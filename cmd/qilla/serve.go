package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/jalberto/qilla/internal/config"
	"github.com/jalberto/qilla/internal/ledger"
	"github.com/jalberto/qilla/internal/mem"
	"github.com/jalberto/qilla/internal/prices"
	"github.com/jalberto/qilla/internal/queue"
	"github.com/jalberto/qilla/internal/render"
	"github.com/jalberto/qilla/internal/serve"
	"github.com/jalberto/qilla/internal/worker"
)

// cmdServe: qilla serve — the supervisor. Normally started by qilla.socket.
func cmdServe(args []string) error {
	if len(args) != 0 {
		return fmt.Errorf("usage: qilla serve")
	}
	cfg, err := config.Load(config.DefaultPath())
	if err != nil {
		return err
	}
	if err := os.MkdirAll(cfg.StateDir, 0o755); err != nil {
		return err
	}
	q, err := queue.Open(cfg.DBPath())
	if err != nil {
		return err
	}
	defer q.Close()
	// one holder for the whole process: the supervisor re-reads qilla.toml
	// before each drain and the worker sees the same swap.
	hold := config.NewHolder(cfg)
	w, err := worker.New(hold, q.DB())
	if err != nil {
		return err
	}
	w.Render = render.Knap{Vault: cfg.Vault}
	if ms, err := mem.Open(cfg, q.DB()); err == nil {
		w.Mem = ms
	} else {
		fmt.Fprintln(os.Stderr, "qilla: working memory disabled:", err)
	}
	pr, _ := prices.Load(prices.File(cfg.Path))
	l, err := ledger.New(q.DB(), pr)
	if err != nil {
		return err
	}
	w.OnRun = l.Record
	s := serve.New(hold, q, w, l)
	ls, err := serve.Listen(cfg, socketPath())
	if err != nil {
		return err
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGTERM, syscall.SIGINT)
	go func() {
		<-sig
		s.Log("signal: finishing the current job, then exiting")
		s.Stop()
	}()
	// SIGHUP is the explicit "re-read qilla.toml" trigger; it never stops the
	// server, and it goes through the same path as the mtime check.
	hup := make(chan os.Signal, 1)
	signal.Notify(hup, syscall.SIGHUP)
	go func() {
		for range hup {
			hold.Force(s.Log)
		}
	}()
	s.Log("qilla serve on %s (poke %s)", ls.HTTP.Addr(), ls.Poke.Addr())
	return s.Run(ctx, ls)
}
