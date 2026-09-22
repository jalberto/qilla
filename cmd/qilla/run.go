package main

import (
	"context"
	"flag"
	"fmt"
	"os"

	"github.com/jalberto/qilla/internal/config"
	"github.com/jalberto/qilla/internal/ledger"
	"github.com/jalberto/qilla/internal/mem"
	"github.com/jalberto/qilla/internal/prices"
	"github.com/jalberto/qilla/internal/queue"
	"github.com/jalberto/qilla/internal/render"
	"github.com/jalberto/qilla/internal/worker"
)

// brainOnlyRoutines are refused on a worker host (host-roles-worker §5).
var brainOnlyRoutines = map[string]bool{
	"brief": true, "wallapop": true, "newsletters": true, "newsletters-act": true,
	"learn": true, "retro": true, "remind": true, "notion-poll": true,
	"notion-tasks": true, "reconcile": true,
}

// cmdRun: qilla run <routine> — execute one routine now, bypassing the queue
// (debugging). The run is recorded like any other.
func cmdRun(args []string) error {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	force := fs.Bool("force", false, "run even when `qilla routine check` fails")
	if err := fs.Parse(args); err != nil {
		return err
	}
	args = fs.Args()
	if len(args) != 1 {
		return fmt.Errorf("usage: qilla run [--force] <routine>")
	}
	cfg, err := config.Load(config.DefaultPath())
	if err != nil {
		return err
	}
	if !cfg.IsBrain() && brainOnlyRoutines[args[0]] {
		fmt.Fprintf(os.Stderr, "qilla: %s is brain-only, refused on a worker\n", args[0])
		os.Exit(2)
	}
	if msg := checkBlocks(cfg, args[0], manifestEnv(cfg)); msg != "" {
		if !*force {
			return fmt.Errorf("%s; --force to run anyway", msg)
		}
		fmt.Fprintln(os.Stderr, "qilla:", msg, "— running anyway (--force)")
	}
	if err := os.MkdirAll(cfg.StateDir, 0o755); err != nil {
		return err
	}
	s, err := queue.Open(cfg.DBPath())
	if err != nil {
		return err
	}
	defer s.Close()
	w, err := worker.New(cfg, s.DB())
	if err != nil {
		return err
	}
	w.Render = render.Knap{Vault: cfg.Vault}
	if ms, err := mem.Open(cfg, s.DB()); err == nil {
		w.Mem = ms
	} else {
		fmt.Fprintln(os.Stderr, "qilla: working memory disabled:", err)
	}
	pr, _ := prices.Load(prices.File(cfg.Path))
	l, err := ledger.New(s.DB(), pr)
	if err != nil {
		return err
	}
	w.OnRun = func(r worker.Record) {
		l.Record(r)
		switch {
		case r.Skipped:
			fmt.Printf("%s: unchanged input, skipped (digest %s)\n", r.Routine, r.Digest)
		case r.Error != "":
			fmt.Fprintf(os.Stderr, "%s: failed after %s: %s\n", r.Routine, r.Duration.Round(1e9), r.Error)
		default:
			fmt.Printf("%s: ok in %s · %d in / %d out tokens · prompt %d chars\n", r.Routine, r.Duration.Round(1e9), r.Usage.Input+r.Usage.CacheRead+r.Usage.CacheCreation, r.Usage.Output, r.PromptChars)
			if r.Result != "" {
				fmt.Println(r.Result)
			}
		}
	}
	return w.Run(context.Background(), &queue.Job{Routine: args[0], Agent: cfg.Routines[args[0]].Agent})
}
