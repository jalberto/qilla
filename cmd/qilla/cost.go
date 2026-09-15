package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/jalberto/qilla/internal/config"
	"github.com/jalberto/qilla/internal/ledger"
	"github.com/jalberto/qilla/internal/prices"
	"github.com/jalberto/qilla/internal/queue"
)

// cmdCost: qilla cost [--all] [--by routine|agent]
func cmdCost(args []string) error {
	fs := flag.NewFlagSet("cost", flag.ContinueOnError)
	all := fs.Bool("all", false, "all time instead of today")
	by := fs.String("by", "routine", "group by routine|agent")
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
	l, err := ledger.New(q.DB(), nil)
	if err != nil {
		return err
	}
	day := time.Now().Format("2006-01-02")
	if *all {
		day = ""
	}
	lines, err := l.Summary(context.Background(), day, *by)
	if err != nil {
		return err
	}
	total := 0.0
	fmt.Printf("%-24s %5s %5s %5s %9s\n", *by, "runs", "skip", "fail", "USD")
	for _, ln := range lines {
		mark := ""
		if ln.Unpriced > 0 {
			mark = " (unpriced: run `qilla prices sync`)"
		}
		fmt.Printf("%-24s %5d %5d %5d %9.3f%s\n", ln.Key, ln.Runs, ln.Skipped, ln.Failed, ln.Cost, mark)
		total += ln.Cost
	}
	fmt.Printf("%-24s %23s %9.3f\n", "total", "", total)
	if !*all && cfg.Budget.Block > 0 {
		fmt.Printf("global cap %.2f warn %.2f\n", cfg.Budget.Block, cfg.Budget.Warn)
	}
	return nil
}

// cmdPrices: qilla prices sync — refresh the LiteLLM sheet next to qilla.toml.
func cmdPrices(args []string) error {
	if len(args) != 1 || args[0] != "sync" {
		return fmt.Errorf("usage: qilla prices sync")
	}
	path := prices.File(config.DefaultPath())
	m, err := prices.Fetch(prices.DefaultURL, "anthropic")
	if err != nil {
		return err
	}
	if err := prices.Save(path, m); err != nil {
		return err
	}
	fmt.Fprintf(os.Stdout, "%d anthropic models → %s\n", len(m), path)
	return nil
}
