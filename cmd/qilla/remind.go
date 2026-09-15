package main

import (
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/jalberto/qilla/internal/config"
	"github.com/jalberto/qilla/internal/remind"
)

const remindUsage = `usage: qilla remind <command>

  add "<what>" --at "<YYYY-MM-DD [HH:MM]>"   append the reminder to the todo file
  list                                       every pending reminder, soonest first
  due [--notify]                             due and not yet delivered (--notify sends and marks)
  count                                      how many are due or overdue (statusline)
  reset "<substring>"                        clear delivered state so they fire again

The reminder itself lives in vault markdown and is the source of truth:
  - [ ] 2026-08-26 · ⏰2026-08-27 09:30 · Call the dentist
Only "already notified" is state (reminders.json under state_dir), which is what
stops the 5-minute timer from firing the same reminder all day.`

// cmdRemind: qilla remind add|list|due|count|reset — markdown is the truth.
func cmdRemind(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("%s", remindUsage)
	}
	cfg, err := config.Load(config.DefaultPath())
	if err != nil {
		return err
	}
	loc := time.Local
	if cfg.Timezone != "" {
		l, err := time.LoadLocation(cfg.Timezone)
		if err != nil {
			return fmt.Errorf("timezone %q: %w", cfg.Timezone, err)
		}
		loc = l
	}
	now := time.Now().In(loc)

	sub, rest := args[0], args[1:]
	if sub == "add" {
		fs := flag.NewFlagSet("remind add", flag.ContinueOnError)
		at := fs.String("at", "", "due date: YYYY-MM-DD [HH:MM]")
		var text string
		if len(rest) > 0 && rest[0] != "" && rest[0][0] != '-' {
			text, rest = rest[0], rest[1:]
		}
		if err := fs.Parse(rest); err != nil {
			return err
		}
		if text == "" && fs.NArg() > 0 {
			text = fs.Arg(0)
		}
		if text == "" || *at == "" {
			return fmt.Errorf(`usage: qilla remind add "<what>" --at "<YYYY-MM-DD [HH:MM]>"`)
		}
		due, err := remind.ParseAt(*at, loc)
		if err != nil {
			return err
		}
		line, err := remind.Add(cfg.VaultPath(cfg.Todo), now, due, text)
		if err != nil {
			return err
		}
		fmt.Println(line)
		return nil
	}

	rs, err := remind.Scan(cfg.Vault, loc)
	if err != nil {
		return err
	}

	switch sub {
	case "count":
		fmt.Println(len(remind.DueNow(rs, now)))
		return nil
	case "list":
		fmt.Printf("n=%d due=%d\n", len(rs), len(remind.DueNow(rs, now)))
		for _, r := range rs {
			state := "pending"
			if !r.Due.After(now) {
				state = "due"
			}
			fmt.Printf("%s\t%s\t%s\t%s\n", state, remind.When(r, now), r.Text, r.Source)
		}
		return nil
	case "reset":
		if len(rest) == 0 || rest[0] == "" {
			return fmt.Errorf(`usage: qilla remind reset "<substring>"`)
		}
		st, err := remind.LoadState(remind.StatePath(cfg.StateDir))
		if err != nil {
			return err
		}
		n := st.Reset(rest[0])
		if err := st.Save(now); err != nil {
			return err
		}
		fmt.Printf("reset=%d\n", n)
		return nil
	case "due", "send":
		fs := flag.NewFlagSet("remind due", flag.ContinueOnError)
		notify := fs.Bool("notify", sub == "send", "send a desktop notification and mark delivered")
		if err := fs.Parse(rest); err != nil {
			return err
		}
		st, err := remind.LoadState(remind.StatePath(cfg.StateDir))
		if err != nil {
			return err
		}
		var pending []remind.Reminder
		for _, r := range remind.DueNow(rs, now) {
			if !st.IsSent(remind.Key(r)) {
				pending = append(pending, r)
			}
		}
		if !*notify {
			fmt.Printf("n=%d\n", len(pending))
			for _, r := range pending {
				fmt.Printf("%s\t%s\n", remind.When(r, now), r.Text)
			}
			return nil
		}
		if len(pending) == 0 {
			return nil
		}
		for _, r := range pending {
			when := remind.When(r, now)
			if err := remind.Notify(fmt.Sprintf("%s  (%s)", r.Text, when)); err != nil {
				// Leave it undelivered so the next tick retries rather than losing it.
				fmt.Fprintf(os.Stderr, "FAILED to send: %s\n", r.Text)
				continue
			}
			st.MarkSent(remind.Key(r), r, now)
			fmt.Printf("sent\t%s\n", r.Text)
		}
		return st.Save(now)
	}
	fmt.Fprintln(os.Stderr, remindUsage)
	return fmt.Errorf("unknown remind command %q", sub)
}
