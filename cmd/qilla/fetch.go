package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"text/tabwriter"

	"github.com/jalberto/qilla/internal/config"
	"github.com/jalberto/qilla/internal/decide"
	"github.com/jalberto/qilla/internal/fetch"
)

// errBlocked is the exit-3 outcome: every rung ended in something that is not
// content. The message names the last kind so the caller can say which.
type errBlocked struct{ kind string }

func (e errBlocked) Error() string {
	return fmt.Sprintf("no content: last page was %s — every rung tried, see --json", e.kind)
}

const fetchUsage = "usage: qilla fetch <url> [--max-rung N] [--json] | qilla fetch --ledger | qilla fetch --forget <domain>\n" +
	"  --max-rung N stops after position N of the chosen order (1 hister, 2 karakeep-lookup, 3.. the methods)"

// cmdFetch: qilla fetch <url> [--max-rung N] [--json] | --ledger | --forget <domain>
//
// Climbs the web-research ladder: JA's own copies first (hister,
// karakeep-lookup), then the methods (defuddle, curl, ladder, mirror,
// obscura, karakeep-crawl, agent-browser headless, headed) starting where
// the per-domain ledger — or, for an unknown domain, the fetch-route decider —
// says. Each rung's text is classified with decide.PageKind, so a bot check
// or a block page escalates instead of being reported as the page.
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
		case "--ledger":
			return fetchLedgerPrint()
		case "--forget":
			if i+1 >= len(args) {
				return fmt.Errorf("%w: --forget needs a domain", errUsage)
			}
			return fetchLedgerForget(args[i+1])
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
				return fmt.Errorf("%w: %s", errUsage, fetchUsage)
			}
			url = a
		}
	}
	if url == "" {
		return fmt.Errorf("%w: %s", errUsage, fetchUsage)
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

// fetchLedgerPath is {state_dir}/fetch/ledger.json.
func fetchLedgerPath() string {
	stateDir := config.Expand("~/.local/state/qilla")
	if cfg, err := config.Load(config.DefaultPath()); err == nil {
		stateDir = cfg.StateDir
	}
	return fetch.LedgerFile(stateDir)
}

func fetchLedgerPrint() error {
	l, err := fetch.LoadLedger(fetchLedgerPath())
	if err != nil {
		return err
	}
	domains := make([]string, 0, len(l))
	for d := range l {
		domains = append(domains, d)
	}
	sort.Strings(domains)
	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "DOMAIN\tRUNG\tKIND\tOK\tFAIL\tLAST OK\tVIA")
	for _, d := range domains {
		e := l[d]
		fmt.Fprintf(tw, "%s\t%s\t%s\t%d\t%d\t%s\t%s\n", d, e.Rung, e.Kind, e.OKCount, e.FailCount, e.LastOK, e.LastVia)
	}
	return tw.Flush()
}

func fetchLedgerForget(domain string) error {
	path := fetchLedgerPath()
	l, err := fetch.LoadLedger(path)
	if err != nil {
		return err
	}
	d := fetch.Domain("https://" + domain)
	if d == "" {
		d = domain
	}
	if _, ok := l[d]; !ok {
		return fmt.Errorf("fetch ledger: %s not recorded", d)
	}
	delete(l, d)
	if err := l.Save(path); err != nil {
		return err
	}
	fmt.Println("forgot", d)
	return nil
}

// fetchOptions wires the ladder from [browser], [fetch] and [deciders]: a
// missing config leaves the package defaults, and every decider call
// (PageKind's fallback, the fetch-route choice) is traced.
func fetchOptions(maxRung int) fetch.Options {
	o := fetch.Options{MaxRung: maxRung, HisterURL: fetch.DefaultHisterURL, LadderURL: fetch.DefaultLadderURL}
	cfg, err := config.Load(config.DefaultPath())
	if err != nil {
		o.Ask = traceAsk("")
		return o
	}
	o.Profile, o.Session, o.Class = cfg.Browser.Profile, cfg.Browser.Session, cfg.Browser.Class
	o.HisterURL, o.LadderURL = cfg.Fetch.HisterURL, cfg.Fetch.LadderURL
	o.Mirrors, o.Route = cfg.Fetch.MirrorTable(), cfg.Fetch.Route
	o.LedgerPath = fetch.LedgerFile(cfg.StateDir)
	if u := cfg.KarakeepURL(); u != "" {
		if b, err := readSecret("karakeep"); err == nil {
			o.KarakeepURL, o.KarakeepKey = u, strings.TrimSpace(string(b))
		}
	}
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
