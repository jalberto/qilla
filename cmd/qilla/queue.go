package main

import (
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/jalberto/qilla/internal/candidates"
	"github.com/jalberto/qilla/internal/config"
)

const queueUsage = `usage: qilla queue <command>

  add <family> --text "<verbatim>" [--source S] [--people "a,b"] [--date-hint H]
                       queue a candidate, print its id (exact-text duplicates are skipped)
  list [family]        pending candidates as JSON lines (all families when omitted)
  count [family]       "<family> <n>" per non-empty family; exit 1 when anything is queued
  drop <id>...         remove judged candidates from whatever family holds them

Flows detect, the judge decides: a flow that stumbles on a candidate (a
time-bound promise, a fact with no obvious home) queues one line here and moves
on; the judge routine drains the family in batch. Families are free-form names
matching ^[a-z][a-z0-9-]*$, one <family>.jsonl each under queue_dir.`

// cmdQueue: qilla queue add|list|count|drop — the judge candidate queues.
func cmdQueue(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("%s", queueUsage)
	}
	cfg, err := config.Load(config.DefaultPath())
	if err != nil {
		return err
	}
	root := cfg.QueueDir
	if d := os.Getenv("QILLA_QUEUE_DIR"); d != "" { // the shell helpers' override
		root = d
	}
	s := candidates.New(root)

	switch args[0] {
	case "add":
		if len(args) < 2 || strings.HasPrefix(args[1], "-") {
			return fmt.Errorf("usage: qilla queue add <family> --text \"<verbatim>\" [--source S] [--people a,b] [--date-hint H]")
		}
		family := args[1]
		fs := flag.NewFlagSet("queue add", flag.ContinueOnError)
		text := fs.String("text", "", "the verbatim mention (required)")
		source := fs.String("source", "", "where it was seen (note, thread, meeting)")
		people := fs.String("people", "", "comma-separated names")
		hint := fs.String("date-hint", "", "the date words as said (\"next week\", 2026-09-04)")
		if err := fs.Parse(args[2:]); err != nil {
			return err
		}
		c := candidates.Candidate{Source: *source, Text: *text, DateHint: *hint, People: splitList(*people)}
		id, dup, err := s.Add(family, c, time.Now())
		if err != nil {
			return err
		}
		if dup {
			fmt.Fprintln(os.Stderr, "duplicate, skipped")
		}
		fmt.Println(id)
	case "list":
		fams, err := families(s, args[1:])
		if err != nil {
			return err
		}
		for _, f := range fams {
			items, err := s.List(f)
			if err != nil {
				return err
			}
			for _, it := range items {
				fmt.Println(it.Raw)
			}
		}
	case "count":
		fams, err := families(s, args[1:])
		if err != nil {
			return err
		}
		total := 0
		for _, f := range fams {
			n, err := s.Count(f)
			if err != nil {
				return err
			}
			if n == 0 {
				continue
			}
			total += n
			fmt.Printf("%s=%d\n", f, n)
		}
		if total > 0 {
			os.Exit(1) // a plain "anything queued?" test for scripts
		}
	case "drop":
		if len(args) < 2 {
			return fmt.Errorf("usage: qilla queue drop <id>...")
		}
		n, err := s.DropAll(args[1:])
		if err != nil {
			return err
		}
		fmt.Printf("dropped=%d\n", n)
	default:
		fmt.Fprintln(os.Stderr, queueUsage)
		return fmt.Errorf("unknown queue command %q", args[0])
	}
	return nil
}

// families is the one named on the command line, or every family with a file.
func families(s *candidates.Store, args []string) ([]string, error) {
	if len(args) > 0 && args[0] != "" {
		if !candidates.ValidFamily(args[0]) {
			return nil, fmt.Errorf("bad family %q: want ^[a-z][a-z0-9-]*$", args[0])
		}
		return []string{args[0]}, nil
	}
	return s.Families()
}

func splitList(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
