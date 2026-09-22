package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"

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

// runOpts are `qilla run` flags.
type runOpts struct {
	force bool
	once  bool // synchronous, foreground, exit status = the run's
	local bool // host-local inputs; brain bookkeeping → Qilla/Handoff/<host>.md
}

// refusal is a `qilla run` refusal: one stderr line, exit 2.
type refusal string

func (r refusal) Error() string { return string(r) }

// cmdRun: qilla run <routine> — execute one routine now, bypassing the queue
// (debugging). The run is recorded like any other, except with --local.
func cmdRun(args []string) error {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	var o runOpts
	fs.BoolVar(&o.force, "force", false, "run even when `qilla routine check` fails")
	fs.BoolVar(&o.once, "once", false, "run synchronously in the foreground and exit with the run's status")
	fs.BoolVar(&o.local, "local", false, "with --once: ledger bookkeeping goes to Qilla/Handoff/<host>.md, not the state dir")
	if err := fs.Parse(args); err != nil {
		return err
	}
	args = fs.Args()
	if len(args) != 1 {
		return fmt.Errorf("usage: qilla run [--force] [--once [--local]] <routine>")
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
	if o.local && !o.once {
		return refusal("--local needs --once")
	}
	if !cfg.IsBrain() {
		if brainOnlyRoutines[name] {
			return refusal(name + " is brain-only, refused on a worker")
		}
		if !o.once || !o.local {
			return refusal("this host is a worker: add --once --local (qilla run --once --local " + name + ")")
		}
	}
	if msg := checkBlocks(cfg, name, manifestEnv(cfg)); msg != "" {
		if !o.force {
			return fmt.Errorf("%s; --force to run anyway", msg)
		}
		fmt.Fprintln(os.Stderr, "qilla:", msg, "— running anyway (--force)")
	}
	dbPath := cfg.DBPath()
	if o.local {
		// scratch DB for the worker's digests/sessions: brain state stays untouched
		tmp, err := os.MkdirTemp("", "qilla-once-*")
		if err != nil {
			return err
		}
		defer os.RemoveAll(tmp)
		dbPath = filepath.Join(tmp, "once.db")
	} else if err := os.MkdirAll(cfg.StateDir, 0o755); err != nil {
		return err
	}
	s, err := queue.Open(dbPath)
	if err != nil {
		return err
	}
	defer s.Close()
	w, err := worker.New(cfg, s.DB())
	if err != nil {
		return err
	}
	w.Render = render.Knap{Vault: cfg.Vault}
	record := func(worker.Record) {}
	if o.local {
		w.Sink = worker.HandoffSink{Vault: cfg.Vault, Host: worker.ShortHost()}
	} else {
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
		record = l.Record
	}
	w.OnRun = func(r worker.Record) {
		record(r)
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
