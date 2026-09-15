package main

import (
	"flag"
	"fmt"
	"time"

	"github.com/jalberto/qilla/internal/config"
	"github.com/jalberto/qilla/internal/tasks"
)

// cmdTaskAging: qilla task aging [--brief] [--all] [--count] — age the open
// dated tasks of the vault and flag the ones going stale. Nothing is written.
func cmdTaskAging(args []string) error {
	fs := flag.NewFlagSet("task aging", flag.ContinueOnError)
	brief := fs.Bool("brief", false, "compact wikilinked lines for the briefing")
	all := fs.Bool("all", false, "include commitments under the warn line")
	count := fs.Bool("count", false, "how many are at or past the warn line")
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg, err := config.Load(config.DefaultPath())
	if err != nil {
		return err
	}
	warn, stale := cfg.Tasks.WarnDays, cfg.Tasks.StaleDays
	rows, err := tasks.Aging(cfg.Vault, time.Now())
	if err != nil {
		return err
	}
	if *count {
		n := 0
		for _, c := range rows {
			if c.Age >= warn {
				n++
			}
		}
		fmt.Println(n)
		return nil
	}
	shown := 0
	for _, c := range rows {
		if !*all && c.Age < warn {
			continue
		}
		shown++
		if *brief {
			fmt.Println(c.Brief(warn, stale))
		} else {
			fmt.Println(c.Row(warn, stale))
		}
	}
	if shown == 0 {
		fmt.Printf("No open commitments past %dd.\n", warn)
	}
	return nil
}

// cmdTaskSummary: qilla task summary — rewrite the todo file's own
// "> [!summary]" callout from ground truth.
func cmdTaskSummary(args []string) error {
	if len(args) > 0 {
		return fmt.Errorf("usage: qilla task summary")
	}
	cfg, err := config.Load(config.DefaultPath())
	if err != nil {
		return err
	}
	line, err := tasks.Summarize(cfg.Vault, todoPath(cfg), time.Now(), cfg.Tasks.WarnDays, cfg.Tasks.StaleDays)
	if err != nil {
		return err
	}
	fmt.Println("updated:", line)
	return nil
}

// todoPath is the vault-relative todo file: the top-level `todo` key, which
// the reminders side owns, with the charter's own default as the floor.
func todoPath(cfg *config.Config) string {
	if cfg.Todo != "" {
		return cfg.Todo
	}
	return "Desk/Todo.md"
}
