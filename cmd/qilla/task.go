package main

import (
	"fmt"
	"os"
	"time"

	"github.com/jalberto/qilla/internal/config"
	"github.com/jalberto/qilla/internal/tasks"
)

const taskUsage = `usage: qilla task <command>

  new ["<what>"]   print the next free id for today (or the full task line)
  find <id>        every file:line carrying that id
  done <id>        tick every copy and stamp the completion date
  open             every id-bearing task still open, deduped by id
  aging [--brief]  age open dated tasks ("- [ ] YYYY-MM-DD · …") and flag the stale ones
  summary          rewrite the todo file's "> [!summary]" callout from ground truth

Convention — every synced task line ends with a block id:
  - [ ] 2026-08-26 · Chase the benchmark table ^task-20260826-001
Copies elsewhere carry the SAME ^id; Obsidian links to it with [[note#^id]].

Ids are free until written: nothing reserves one, so calling ` + "`new`" + ` twice
without writing the first line in between returns the same id both times.`

// cmdTask: qilla task new|find|done|open|aging|summary — one task, one id, many places.
func cmdTask(args []string) error {
	cfg, err := config.Load(config.DefaultPath())
	if err != nil {
		return err
	}
	vault := cfg.Vault
	if len(args) == 0 {
		return fmt.Errorf("%s", taskUsage)
	}
	switch args[0] {
	case "new":
		now := time.Now()
		id, err := tasks.NextID(vault, now.Format("20060102"))
		if err != nil {
			return err
		}
		if len(args) > 1 && args[1] != "" {
			fmt.Printf("- [ ] %s · %s ^%s\n", now.Format("2006-01-02"), args[1], id)
		} else {
			fmt.Println(id)
		}
	case "find":
		if len(args) < 2 {
			return fmt.Errorf("usage: qilla task find <id>")
		}
		hits, err := tasks.Find(vault, args[1])
		if err != nil {
			return err
		}
		fmt.Printf("n=%d\n", len(hits))
		for _, h := range hits {
			fmt.Printf("%s\t%d\t%s\n", h.File, h.Line, h.Text)
		}
	case "done":
		if len(args) < 2 {
			return fmt.Errorf("usage: qilla task done <id>")
		}
		changed, err := tasks.Done(vault, args[1], time.Now())
		if err != nil {
			return err
		}
		if len(changed) == 0 {
			return fmt.Errorf("no task carries ^%s", args[1])
		}
		fmt.Printf("ticked=%d\n", len(changed))
		for _, f := range changed {
			fmt.Println(f)
		}
	case "open":
		open, err := tasks.OpenTasks(vault)
		if err != nil {
			return err
		}
		fmt.Printf("n=%d\n", len(open))
		for _, o := range open {
			fmt.Printf("%s\t%s\t%s\n", o.ID, o.File, o.Text)
		}
	case "aging":
		return cmdTaskAging(args[1:])
	case "summary":
		return cmdTaskSummary(args[1:])
	default:
		fmt.Fprintln(os.Stderr, taskUsage)
		return fmt.Errorf("unknown task command %q", args[0])
	}
	return nil
}
