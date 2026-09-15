package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/jalberto/qilla/internal/catchup"

	"github.com/jalberto/qilla/internal/config"
	"github.com/jalberto/qilla/internal/mem"
	"github.com/jalberto/qilla/internal/procs"
	"github.com/jalberto/qilla/internal/queue"
	"github.com/jalberto/qilla/internal/session"
)

// cmdSessions: qilla sessions [--count|--count-others|--others|--rotated|handoff]
//
// Live interactive Claude sessions cwd'd inside the vault. No registry file, so
// nothing can go stale: the kernel is the source of truth. --rotated reads the
// one thing the kernel cannot know, the sessions qilla superseded for idleness.
func cmdSessions(args []string) error {
	cfg, err := config.Load(config.DefaultPath())
	if err != nil {
		return err
	}
	mode := "--table"
	if len(args) > 0 {
		mode = args[0]
	}
	if mode == "handoff" {
		return handoffSession(cfg, args[1:])
	}
	switch mode {
	case "--table", "--count", "--count-others", "--others", "--rotated":
	default:
		return fmt.Errorf("usage: qilla sessions [--count | --count-others | --others | --rotated | handoff [--agent a] [--set \"<text>\"]]")
	}
	if mode == "--rotated" {
		return rotatedSessions(cfg)
	}
	// The claude process this command runs under, so it can be excluded.
	own := procs.AncestorClaude(os.Getppid())
	excludeOwn := mode == "--count-others" || mode == "--others"

	var list []procs.Proc
	for _, p := range procs.ClaudeIn(cfg.Vault) {
		if excludeOwn && p.PID == own {
			continue
		}
		list = append(list, p)
	}
	if mode == "--count" || mode == "--count-others" {
		fmt.Println(len(list))
		return nil
	}
	// TSV: pid, start time, cwd, "self" for the session running this command
	fmt.Printf("n=%d\n", len(list))
	for _, p := range list {
		started := "?"
		if !p.Started.IsZero() {
			started = p.Started.Format("15:04:05")
		}
		self := ""
		if p.PID == own {
			self = "\tself"
		}
		fmt.Printf("%d\t%s\t%s%s\n", p.PID, started, p.CWD, self)
	}
	return nil
}

// rotatedSessions lists the sessions qilla superseded: agent, old id, successor,
// when it was last used and when it was retired.
func rotatedSessions(cfg *config.Config) error {
	q, err := queue.Open(cfg.DBPath())
	if err != nil {
		return err
	}
	defer q.Close()
	st, err := session.Open(q.DB())
	if err != nil {
		return err
	}
	rows := st.Rotated()
	fmt.Printf("n=%d\n", len(rows))
	for _, r := range rows {
		succ := r.Successor
		if succ == "" {
			succ = "-"
		}
		fmt.Printf("%s\t%s\t%s\t%s\t%s\n", r.Agent, r.SessionID, succ,
			r.LastUsed.Local().Format("2006-01-02 15:04"), r.Superseded.Local().Format("2006-01-02 15:04"))
	}
	return nil
}

// handoffSession prints (or with --set writes) the agent's handoff: the state
// entry a rotated session leaves behind, or the one-liner a session writes at
// the end of a task. Nothing to hand off prints nothing.
func handoffSession(cfg *config.Config, args []string) error {
	fs := flag.NewFlagSet("sessions handoff", flag.ContinueOnError)
	agent := fs.String("agent", catchup.Agent(), "agent")
	set := fs.String("set", "", "write this text as the agent's handoff")
	if err := fs.Parse(args); err != nil {
		return err
	}
	q, err := queue.Open(cfg.DBPath())
	if err != nil {
		return err
	}
	defer q.Close()
	if strings.TrimSpace(*set) != "" {
		return saveHandoff(cfg, q.DB(), *agent, *set)
	}
	m, err := mem.Open(cfg, q.DB())
	if err != nil {
		return err
	}
	es, err := m.State(context.Background(), *agent, 0)
	if err != nil {
		return err
	}
	for _, e := range es {
		if e.Key == session.HandoffKey(*agent) {
			fmt.Println(strings.TrimSpace(e.Text))
			return nil
		}
	}
	return nil
}
