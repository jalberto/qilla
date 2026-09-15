package main

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/jalberto/qilla/internal/artifacts"
	"github.com/jalberto/qilla/internal/config"
	"github.com/jalberto/qilla/internal/doctor"
	"github.com/jalberto/qilla/internal/ledger"
	"github.com/jalberto/qilla/internal/mem"
	"github.com/jalberto/qilla/internal/queue"
	"github.com/jalberto/qilla/internal/reconcile"
)

// cmdReconcile: qilla reconcile — enqueue every must_run routine that has not
// succeeded today. Run by qilla-reconcile.timer every 30 min and at boot.
func cmdReconcile(args []string) error {
	if len(args) != 0 {
		return fmt.Errorf("usage: qilla reconcile")
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
	l, err := ledger.New(q.DB(), nil)
	if err != nil {
		return err
	}
	out, enqueued, err := reconcile.RunWith(context.Background(), cfg, q, l, time.Now(), doctor.NextFire())
	if err != nil {
		return err
	}
	fmt.Print(reconcile.Format(out))
	if st, err := artifacts.New(cfg.ArtifactsDir(), time.Duration(cfg.Artifacts.TTLDays)*24*time.Hour, cfg.Artifacts.MaxMB); err == nil {
		if n := st.Purge(); n > 0 {
			fmt.Printf("artifacts: purged %d expired\n", n)
		}
	}
	if ms, err := mem.Open(cfg, q.DB()); err == nil {
		if n, err := ms.Purge(context.Background()); err == nil && n > 0 {
			fmt.Printf("memory: purged %d expired notes\n", n)
		}
	}
	if enqueued {
		poke()
	}
	return nil
}
