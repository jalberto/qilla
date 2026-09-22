package main

import (
	"context"
	"database/sql"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/jalberto/qilla/internal/catchup"
	"github.com/jalberto/qilla/internal/config"
	"github.com/jalberto/qilla/internal/mem"
	"github.com/jalberto/qilla/internal/queue"
	"github.com/jalberto/qilla/internal/remind"
	"github.com/jalberto/qilla/internal/vault"
)

const catchupUsage = `usage: qilla catchup [--all]

What changed in the vault since this agent last caught up (agent = $QILLA_AGENT,
default "chief"). Header first, then one TSV row per item; a quiet vault prints
only the header.

  n=<total> jots=<n> answers=<n> open=<n> log=<n> due=<n> state=<n>
  state<TAB><key><TAB><text>         working memory the agent carries in (state kind)
  jot<TAB><date><TAB><text>          unrouted 📥 Inbox bullet (today + yesterday)
  answer<TAB><stem><TAB><reply>      a question the owner has replied to
  open<TAB><stem>                    still-open question (counted always, listed with --all)
  log<TAB><date><TAB><text>          📓 Log lines added since the last catchup
  remind<TAB><when><TAB><text>       reminders due now

--all ignores the stored bookmark. State is updated after printing either way.`

// cmdCatchup: qilla catchup [--all].
func cmdCatchup(args []string) error {
	fs := flag.NewFlagSet("catchup", flag.ContinueOnError)
	all := fs.Bool("all", false, "ignore the stored bookmark: print everything")
	fs.Usage = func() { fmt.Fprintln(os.Stderr, catchupUsage) }
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("%s", catchupUsage)
	}
	cfg, err := config.Load(config.DefaultPath())
	if err != nil {
		return err
	}
	return runCatchup(cfg, os.Stdout, *all)
}

// runCatchup is the catchup body: scan, print, move the bookmark. The
// session-start hook calls it in-process instead of shelling out.
func runCatchup(cfg *config.Config, out io.Writer, all bool) error {
	loc := time.Local
	if cfg.Timezone != "" {
		if l, err := time.LoadLocation(cfg.Timezone); err == nil {
			loc = l
		}
	}
	now := time.Now().In(loc)

	q, err := queue.Open(cfg.DBPath())
	if err != nil {
		return err
	}
	defer q.Close()
	st, err := catchup.OpenState(q.DB())
	if err != nil {
		return err
	}
	agent := catchup.Agent()
	_, seen, err := st.Load(agent)
	if err != nil {
		return err
	}

	res := catchup.Scan(cfg.Vault, cfg.JournalDir, cfg.Questions, cfg.ReplyMarker, now, seen, all)
	state := agentState(cfg, q.DB(), agent)
	var due []remind.Reminder
	if rs, err := remind.Scan(cfg.Vault, loc); err == nil {
		due = remind.DueNow(rs, now)
	}
	writeCatchup(out, res, due, state, now, all)
	return st.Save(agent, now, res.LogSeen)
}

// agentState is the agent's working-memory state: its own entries plus the
// shared project's. Best effort — memory being off only costs the state rows.
func agentState(cfg *config.Config, db *sql.DB, agent string) []catchup.StateRow {
	m, err := mem.Open(cfg, db)
	if err != nil {
		return nil
	}
	var out []catchup.StateRow
	seen := map[string]bool{"": true}
	for _, project := range []string{agent, cfg.Memory.SharedProject} {
		if seen[project] {
			continue
		}
		seen[project] = true
		es, err := m.State(context.Background(), project, 0)
		if err != nil {
			continue
		}
		for _, e := range es {
			out = append(out, catchup.StateRow{Key: e.Key, Text: vault.Truncate(flattenState(e.Text), catchup.StateMax)})
		}
	}
	return out
}

var stateNL = strings.NewReplacer("\n", " · ", "\r", " ")

func flattenState(s string) string { return stateNL.Replace(s) }

// writeCatchup prints the header plus the TSV rows, nothing else. State comes
// first: it is what the agent already knows, before what is new.
func writeCatchup(w io.Writer, res catchup.Result, due []remind.Reminder, state []catchup.StateRow, now time.Time, all bool) {
	fmt.Fprintf(w, "n=%d jots=%d answers=%d open=%d log=%d due=%d state=%d\n",
		res.Total()+len(due), len(res.Jots), len(res.Answers), len(res.Opens), len(res.Logs), len(due), len(state))
	for _, s := range state {
		fmt.Fprintf(w, "state\t%s\t%s\n", s.Key, s.Text)
	}
	for _, j := range res.Jots {
		fmt.Fprintf(w, "jot\t%s\t%s\n", j.Date, j.Text)
	}
	for _, a := range res.Answers {
		fmt.Fprintf(w, "answer\t%s\t%s\n", a.Stem, a.Reply)
	}
	if all {
		for _, o := range res.Opens {
			fmt.Fprintf(w, "open\t%s\n", o)
		}
	}
	for _, l := range res.Logs {
		fmt.Fprintf(w, "log\t%s\t%s\n", l.Date, l.Text)
	}
	for _, r := range due {
		fmt.Fprintf(w, "remind\t%s\t%s\n", remind.When(r, now), r.Text)
	}
}
