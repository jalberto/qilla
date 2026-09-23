package main

import (
	"context"
	"errors"
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

// runOpts are `qilla run` flags.
type runOpts struct {
	force bool
	once  bool // no-op; kept for scripts. Runs are always inline/synchronous.
}

// refusal is a `qilla run` refusal: one stderr line, exit 2.
type refusal string

func (r refusal) Error() string { return string(r) }

// cmdRun: qilla run <routine> — execute one routine now, bypassing the queue
// (debugging). The run is recorded like any other.
func cmdRun(args []string) error {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	var o runOpts
	fs.BoolVar(&o.force, "force", false, "run even when `qilla routine check` fails")
	fs.BoolVar(&o.once, "once", false, "run inline (default; kept for scripts)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	args = fs.Args()
	if len(args) != 1 {
		return fmt.Errorf("usage: qilla run [--force] [--once] <routine>")
	}
	cfg, err := config.Load(config.DefaultPath())
	if err != nil {
		return err
	}
	err = runRoutine(cfg, args[0], o)
	var r refusal
	if errors.As(err, &r) {
		fmt.Fprintln(os.Stderr, "qilla:", r)
		os.Exit(2)
	}
	return err
}

// runRoutine runs one routine inline (no jobs queue, no qilla.socket).
func runRoutine(cfg *config.Config, name string, o runOpts) error {
	if msg := checkBlocks(cfg, name, manifestEnv(cfg)); msg != "" {
		if !o.force {
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
	return w.Run(context.Background(), &queue.Job{Routine: name, Agent: cfg.Routines[name].Agent})
}
