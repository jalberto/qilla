package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/jalberto/qilla/internal/config"
	"github.com/jalberto/qilla/internal/queue"
	"github.com/jalberto/qilla/internal/worker"
)

// socketPath is the supervisor's activation socket.
func socketPath() string {
	if d := os.Getenv("XDG_RUNTIME_DIR"); d != "" {
		return filepath.Join(d, "qilla.sock")
	}
	return filepath.Join(os.TempDir(), fmt.Sprintf("qilla-%d.sock", os.Getuid()))
}

// poke wakes the supervisor. Failure is fine: the next timer or reconcile will.
func poke() {
	c, err := net.DialTimeout("unix", socketPath(), time.Second)
	if err != nil {
		return
	}
	c.Write([]byte("poke\n"))
	c.Close()
}

// cmdEnqueue: qilla enqueue <routine> [--priority N] [--late]
//
//	qilla enqueue ask --text "…" [--agent chief] [--priority N]
func cmdEnqueue(args []string) error {
	fs := flag.NewFlagSet("enqueue", flag.ContinueOnError)
	prio := fs.Int("priority", 0, "higher runs first (asks default 5)")
	text := fs.String("text", "", "an ask's text (routine name becomes 'ask')")
	agent := fs.String("agent", "", "agent for an ask (default chief)")
	late := fs.Bool("late", false, "set by reconcile after the window closed")
	// routine name first, flags after: qilla enqueue brief --priority 3
	if len(args) == 0 || strings.HasPrefix(args[0], "-") {
		return fmt.Errorf("usage: qilla enqueue <routine> [--priority N] | qilla enqueue ask --text …")
	}
	name := args[0]
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	cfg, err := config.Load(config.DefaultPath())
	if err != nil {
		return err
	}
	j := queue.Job{Routine: name, Priority: *prio, Late: *late, Due: time.Now()}
	if name == "ask" {
		if *text == "" {
			return fmt.Errorf("ask needs --text")
		}
		j.Text, j.Agent = *text, *agent
		if j.Agent == "" {
			j.Agent = "chief"
		}
		if *prio == 0 {
			j.Priority = 5
		}
	} else {
		r, ok := cfg.Routines[name]
		if !ok {
			return fmt.Errorf("routine %q not in %s", name, cfg.Path)
		}
		j.Agent = r.Agent
		if r.Input == "pending" && *text != "" {
			// a candidate for a judge routine: park it; the routine's timer (or a poke) judges the batch
			p := worker.PendingPath(cfg.StateDir, name)
			os.MkdirAll(filepath.Dir(p), 0o755)
			f, err := os.OpenFile(p, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
			if err != nil {
				return err
			}
			id := fmt.Sprintf("%d", time.Now().UnixNano())
			line, _ := json.Marshal(map[string]any{"id": id, "text": *text, "at": time.Now().Format(time.RFC3339)})
			f.Write(append(line, '\n'))
			f.Close()
			fmt.Printf("pending for %s: %s\n", name, id)
			*text = ""
		}
	}
	if err := os.MkdirAll(cfg.StateDir, 0o755); err != nil {
		return err
	}
	s, err := queue.Open(cfg.DBPath())
	if err != nil {
		return err
	}
	defer s.Close()
	id, dup, err := s.Enqueue(context.Background(), j)
	if err != nil {
		return err
	}
	poke()
	if dup {
		fmt.Printf("already queued #%d %s\n", id, name)
	} else {
		fmt.Printf("queued #%d %s\n", id, name)
	}
	return nil
}
