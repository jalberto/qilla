package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strings"
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
  send --to <host> [--routine <name>] "<text>"  append a handoff task for <host>
  inbox [--host <h>]                            open handoff tasks addressed to this host, as JSON
  done --host <h> --line <n> [--result "<r>"]   flip that handoff line to done (host/line form)

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
		if len(args) >= 2 && strings.HasPrefix(args[1], "-") {
			return cmdTaskDone(vault, args[1:])
		}
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
	case "send":
		return cmdTaskSend(cfg, args[1:])
	case "inbox":
		return cmdTaskInbox(vault, args[1:])
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

// shortHostname is os.Hostname() with any domain suffix stripped, mirroring
// config.Config.Role()'s own normalisation.
func shortHostname() string {
	name, err := os.Hostname()
	if err != nil {
		return ""
	}
	if i := strings.IndexByte(name, '.'); i >= 0 {
		name = name[:i]
	}
	return name
}

// taskNow is time.Now() in cfg.Timezone, falling back to local time when the
// zone is unset or unknown.
func taskNow(cfg *config.Config) time.Time {
	now := time.Now()
	if cfg.Timezone == "" {
		return now
	}
	loc, err := time.LoadLocation(cfg.Timezone)
	if err != nil {
		return now
	}
	return now.In(loc)
}

// cmdTaskSend: qilla task send --to <host> [--routine <name>] "<text>"
func cmdTaskSend(cfg *config.Config, args []string) error {
	fs := flag.NewFlagSet("task send", flag.ContinueOnError)
	to := fs.String("to", "", "host the task is addressed to")
	routine := fs.String("routine", "", "routine that should act on this task")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *to == "" {
		return fmt.Errorf("usage: qilla task send --to <host> [--routine <name>] \"<text>\"")
	}
	rest := fs.Args()
	if len(rest) == 0 || strings.TrimSpace(rest[0]) == "" {
		return fmt.Errorf("usage: qilla task send --to <host> [--routine <name>] \"<text>\"")
	}
	text := strings.Join(rest, " ")
	from := shortHostname()
	line, err := tasks.Send(cfg.Vault, *to, from, *routine, text, taskNow(cfg))
	if err != nil {
		return err
	}
	fmt.Println(line)
	return nil
}

// cmdTaskInbox: qilla task inbox [--host <h>] — JSON array of open handoff
// tasks addressed to host (default this host's short hostname).
func cmdTaskInbox(vault string, args []string) error {
	fs := flag.NewFlagSet("task inbox", flag.ContinueOnError)
	host := fs.String("host", "", "host whose inbox to read (default this host)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	h := *host
	if h == "" {
		h = shortHostname()
	}
	jots, err := tasks.Inbox(vault, h)
	if err != nil {
		return err
	}
	if jots == nil {
		jots = []tasks.Jot{}
	}
	enc, err := json.Marshal(jots)
	if err != nil {
		return err
	}
	fmt.Println(string(enc))
	return nil
}

// cmdTaskDone: qilla task done --host <h> --line <n> [--result "<r>"]
func cmdTaskDone(vault string, args []string) error {
	fs := flag.NewFlagSet("task done", flag.ContinueOnError)
	host := fs.String("host", "", "host whose inbox file to edit (default this host)")
	line := fs.Int("line", 0, "1-based line number in for-<host>.md")
	result := fs.String("result", "", "wikilink or note appended after the line")
	if err := fs.Parse(args); err != nil {
		return err
	}
	h := *host
	if h == "" {
		h = shortHostname()
	}
	if *line <= 0 {
		return fmt.Errorf("usage: qilla task done --host <h> --line <n> [--result \"<r>\"]")
	}
	if err := tasks.MarkDone(vault, h, *line, *result); err != nil {
		return err
	}
	fmt.Printf("done host=%s line=%d\n", h, *line)
	return nil
}
