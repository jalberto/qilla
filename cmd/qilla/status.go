package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/jalberto/qilla/internal/config"
	"github.com/jalberto/qilla/internal/ledger"
	"github.com/jalberto/qilla/internal/prices"
	"github.com/jalberto/qilla/internal/queue"
)

// cmdStatus: qilla status [--json] [--short] — the day at a glance for humans,
// hooks and health checks: must_run grid, pending/failed jobs, spend vs cap.
func cmdStatus(args []string) error {
	fs := flag.NewFlagSet("status", flag.ContinueOnError)
	asJSON := fs.Bool("json", false, "machine-readable")
	short := fs.Bool("short", false, "one line (for status lines and session hooks)")
	if err := fs.Parse(args); err != nil {
		return err
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
	pr, _ := prices.Load(prices.File(cfg.Path))
	l, err := ledger.New(q.DB(), pr)
	if err != nil {
		return err
	}
	ctx := context.Background()
	now := time.Now()
	day := now.Format("2006-01-02")
	type mr struct {
		Routine string `json:"routine"`
		Done    bool   `json:"done"`
		Window  string `json:"window"`
	}
	var must []mr
	for name, r := range cfg.Routines {
		if r.MustRun {
			ok, _ := l.SucceededToday(ctx, day, name)
			must = append(must, mr{name, ok, r.Window})
		}
	}
	sort.Slice(must, func(i, j int) bool { return must[i].Routine < must[j].Routine })
	jobs, _ := q.List(ctx, 200)
	var pending, failed, deferred []queue.Job
	for _, j := range jobs {
		switch {
		case j.State == queue.Queued && strings.HasPrefix(j.Error, "deferred"):
			deferred = append(deferred, j)
		case j.State == queue.Queued || j.State == queue.Running:
			pending = append(pending, j)
		case j.State == queue.Suspended && strings.Contains(j.Error, "dropped"):
			// dropped on purpose from the page: not a failure to chase
		case (j.State == queue.Failed || j.State == queue.Suspended) && j.Updated.Format("2006-01-02") == day:
			failed = append(failed, j)
		}
	}
	spent := 0.0
	if lines, err := l.Summary(ctx, day, "routine"); err == nil {
		for _, ln := range lines {
			spent += ln.Cost
		}
	}
	out := map[string]any{"day": day, "must_run": must, "pending": len(pending), "deferred": len(deferred), "failed_today": len(failed), "spent": spent, "cap": cfg.Budget.Block, "warn": cfg.Budget.Warn}
	if *asJSON {
		var fj []map[string]any
		for _, j := range failed {
			fj = append(fj, map[string]any{"id": j.ID, "routine": j.Routine, "error": j.Error})
		}
		out["failed"] = fj
		json.NewEncoder(os.Stdout).Encode(out)
		return nil
	}
	missing := []string{}
	for _, m := range must {
		if !m.Done {
			missing = append(missing, m.Routine)
		}
	}
	if *short {
		parts := []string{}
		if len(missing) > 0 {
			parts = append(parts, "must_run pending: "+strings.Join(missing, ", "))
		}
		if len(failed) > 0 {
			parts = append(parts, fmt.Sprintf("%d failed today", len(failed)))
		}
		if len(pending) > 0 {
			parts = append(parts, fmt.Sprintf("%d queued", len(pending)))
		}
		if len(deferred) > 0 {
			parts = append(parts, fmt.Sprintf("%d deferred (usage window)", len(deferred)))
		}
		if cfg.Budget.Block > 0 {
			parts = append(parts, fmt.Sprintf("%.2f/%.2f USD", spent, cfg.Budget.Block))
		}
		if len(parts) == 0 {
			fmt.Println("qilla: all green")
			return nil
		}
		fmt.Println("qilla: " + strings.Join(parts, " · "))
		if len(failed) > 0 || len(missing) > 0 {
			os.Exit(1)
		}
		return nil
	}
	fmt.Printf("day=%s spent=%.2f cap=%.2f must_run_pending=%d pending=%d deferred=%d failed=%d\n",
		day, spent, cfg.Budget.Block, len(missing), len(pending), len(deferred), len(failed))
	for _, m := range must {
		if !m.Done {
			fmt.Printf("must_run\t%s\t%s\n", m.Routine, m.Window)
		}
	}
	for _, j := range pending {
		fmt.Printf("%s\t%d\t%s\n", j.State, j.ID, j.Routine)
	}
	for _, j := range deferred {
		fmt.Printf("deferred\t%d\t%s\tuntil=%s\t%s\n", j.ID, j.Routine, j.Due.Format("15:04"), firstLine(j.Error))
	}
	for _, j := range failed {
		fmt.Printf("%s\t%d\t%s\t%s\n", j.State, j.ID, j.Routine, firstLine(j.Error))
	}
	return nil
}

func firstLine(s string) string {
	s, _, _ = strings.Cut(strings.TrimSpace(s), "\n")
	if len(s) > 160 {
		s = s[:160] + "…"
	}
	return s
}
