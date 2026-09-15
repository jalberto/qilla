package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/jalberto/qilla/internal/config"
	"github.com/jalberto/qilla/internal/mem"
	"github.com/jalberto/qilla/internal/queue"
)

// errNotSeen makes `qilla mem seen` exit 1 without an error message.
var errNotSeen = errors.New("not seen")

// cmdMem: qilla mem add|search|seen|forget|recent|conflicts|judge|promote|purge|stats
func cmdMem(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: qilla mem add|search|seen|forget|recent|conflicts|judge|promote|purge|stats …")
	}
	cfg, err := config.Load(config.DefaultPath())
	if err != nil {
		return err
	}
	q, err := queue.Open(cfg.DBPath())
	if err != nil {
		return err
	}
	defer q.Close()
	m, err := mem.Open(cfg, q.DB())
	if err != nil {
		return err
	}
	ctx := context.Background()
	sub, rest := args[0], args[1:]
	fs := flag.NewFlagSet("mem "+sub, flag.ContinueOnError)
	project := fs.String("project", "", "routine or agent name (scope)")
	kind := fs.String("kind", "", "heuristic | said | route | note")
	key := fs.String("key", "", "stable key: same key aggregates")
	n := fs.Int("n", 5, "results")
	asJSON := fs.Bool("json", false, "JSON output")
	// positional text/query may come after flags: qilla mem add --project brief "text"
	var pos []string
	for len(rest) > 0 {
		if strings.HasPrefix(rest[0], "-") {
			if err := fs.Parse(rest); err != nil {
				return err
			}
			rest = fs.Args()
			continue
		}
		pos = append(pos, rest[0])
		rest = rest[1:]
	}
	text := strings.Join(pos, " ")
	out := func(v any) {
		if *asJSON {
			json.NewEncoder(os.Stdout).Encode(v)
			return
		}
		switch t := v.(type) {
		case []mem.Entry:
			for _, e := range t {
				fmt.Printf("%s\t%s/%s\tsupport=%d\thits=%d\t%s\n", e.ID, e.Project, e.Kind, e.Support, e.Hits, e.Text)
			}
		case mem.Entry:
			fmt.Printf("%s\t%s/%s\tkey=%s\tsupport=%d\n", t.ID, t.Project, t.Kind, t.Key, t.Support)
		default:
			json.NewEncoder(os.Stdout).Encode(v)
		}
	}
	switch sub {
	case "add":
		if *project == "" || text == "" {
			return fmt.Errorf("usage: qilla mem add --project <r> [--kind k] [--key k] \"<text>\"")
		}
		e, err := m.Add(ctx, *project, *kind, *key, text)
		if err != nil {
			return err
		}
		out(e)
	case "search":
		if text == "" {
			return fmt.Errorf("usage: qilla mem search [--project r] [--kind k] [-n 5] \"<query>\"")
		}
		es, err := m.Search(ctx, *project, *kind, text, *n)
		if err != nil {
			return err
		}
		out(es)
	case "seen":
		if *project == "" || text == "" {
			return fmt.Errorf("usage: qilla mem seen --project <r> \"<text>\"  (exit 0 = seen, 1 = new)")
		}
		seen, prior, err := m.Seen(ctx, *project, text)
		if err != nil {
			return err
		}
		if !seen {
			fmt.Println("new")
			return errNotSeen
		}
		out(*prior)
	case "forget":
		if text == "" {
			return fmt.Errorf("usage: qilla mem forget <id>")
		}
		return m.Forget(ctx, text)
	case "recent":
		es, err := m.B.Recent(ctx, *project, *n)
		if err != nil {
			return err
		}
		out(es)
	case "conflicts":
		cs, err := m.B.Conflicts(ctx, *project)
		if err != nil {
			return err
		}
		if *asJSON {
			// hydrate for the judge routine
			type pair struct {
				mem.Conflict
				Source, Target string
			}
			var ps []pair
			for _, c := range cs {
				p := pair{Conflict: c}
				if e, err := m.B.Get(ctx, c.SourceID); err == nil {
					p.Source = e.Text
				}
				if e, err := m.B.Get(ctx, c.TargetID); err == nil {
					p.Target = e.Text
				}
				ps = append(ps, p)
			}
			json.NewEncoder(os.Stdout).Encode(ps)
			return nil
		}
		for _, c := range cs {
			fmt.Printf("%s\t%s\t%s\t%s\t%s\n", c.ID, c.Relation, c.SourceID, c.TargetID, c.Status)
		}
	case "judge":
		if len(pos) < 2 {
			return fmt.Errorf("usage: qilla mem judge <relation_id> <not_conflict|conflicts_with|supersedes|related> [reason]")
		}
		return m.B.Judge(ctx, pos[0], pos[1], strings.Join(pos[2:], " "))
	case "promote":
		es, err := m.PromotionCandidates(ctx)
		if err != nil {
			return err
		}
		if len(pos) == 1 && pos[0] != "" && !*asJSON {
			return m.MarkPromoted(ctx, pos[0])
		}
		out(es)
	case "purge":
		n, err := m.Purge(ctx)
		if err != nil {
			return err
		}
		fmt.Printf("purged=%d\n", n)
	case "stats":
		st, err := m.Stats(ctx)
		if err != nil {
			return err
		}
		out(st)
	default:
		return fmt.Errorf("unknown mem command %q", sub)
	}
	return nil
}
