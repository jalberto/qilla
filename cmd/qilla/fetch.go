package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"

	"github.com/jalberto/qilla/internal/config"
	"github.com/jalberto/qilla/internal/decide"
	"github.com/jalberto/qilla/internal/fetch"
)

// errBlocked is the exit-3 outcome: every rung ended in something that is not
// content. The message names the last kind so the caller can say which.
type errBlocked struct{ kind string }

func (e errBlocked) Error() string {
	return fmt.Sprintf("no content: last page was %s — try a hister/karakeep mirror", e.kind)
}

// cmdFetch: qilla fetch <url> [--max-rung N] [--json]
//
// Climbs the web-research ladder (defuddle/curl → obscura --stealth →
// agent-browser headless → agent-browser headed) and classifies each rung's
// text with decide.PageKind, so a bot check or a block page escalates instead
// of being reported as the page.
func cmdFetch(args []string) error {
	var (
		url     string
		maxRung int
		asJSON  bool
	)
	for i := 0; i < len(args); i++ {
		switch a := args[i]; a {
		case "--json":
			asJSON = true
		case "--max-rung":
			if i+1 >= len(args) {
				return fmt.Errorf("%w: --max-rung needs a number", errUsage)
			}
			n, err := strconv.Atoi(args[i+1])
			if err != nil || n < 1 || n > fetch.MaxRung {
				return fmt.Errorf("%w: --max-rung must be 1..%d", errUsage, fetch.MaxRung)
			}
			maxRung, i = n, i+1
		default:
			if url != "" || len(a) > 1 && a[0] == '-' {
				return fmt.Errorf("%w: usage: qilla fetch <url> [--max-rung N] [--json]", errUsage)
			}
			url = a
		}
	}
	if url == "" {
		return fmt.Errorf("%w: usage: qilla fetch <url> [--max-rung N] [--json]", errUsage)
	}
	res, err := fetch.Fetch(context.Background(), url, fetchOptions(maxRung))
	if err != nil {
		return err
	}
	if asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetEscapeHTML(false)
		if err := enc.Encode(res); err != nil {
			return err
		}
	} else if res.OK() {
		fmt.Print(res.Text)
		if len(res.Text) > 0 && res.Text[len(res.Text)-1] != '\n' {
			fmt.Println()
		}
	}
	if !res.OK() {
		return errBlocked{kind: res.Kind}
	}
	return nil
}

// fetchOptions wires the ladder from [browser] and [deciders]: a missing
// config leaves the package defaults, and PageKind's model fallback traces
// every verdict it makes with caller "fetch".
func fetchOptions(maxRung int) fetch.Options {
	o := fetch.Options{MaxRung: maxRung}
	cfg, err := config.Load(config.DefaultPath())
	if err != nil {
		o.Ask = traceAsk("")
		return o
	}
	o.Profile, o.Session, o.Class = cfg.Browser.Profile, cfg.Browser.Session, cfg.Browser.Class
	o.Ask = traceAsk(filepath.Join(cfg.StateDir, "deciders"))
	return o
}

// traceAsk is PageKind's backend: decide.AskAndTrace with the [deciders]
// wiring, so the model verdicts land in the decisions trace (rules do not).
func traceAsk(dir string) decide.PageKindAsk {
	return func(ctx context.Context, req decide.AskRequest) (decide.AskResult, error) {
		if err := applyDecidersConfig(&req); err != nil {
			return decide.AskResult{}, err
		}
		res, err := decide.AskAndTrace(ctx, dir, req)
		if err != nil && res.Kind == "" {
			return res, err
		}
		if err != nil {
			fmt.Fprintln(os.Stderr, "qilla fetch: trace:", err)
		}
		return res, nil
	}
}
