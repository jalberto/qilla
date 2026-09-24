package star

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/jalberto/qilla/internal/decide"
	"github.com/jalberto/qilla/internal/fetch"
	"go.starlark.net/starlark"
)

// bFetch is the `fetch(url, max_rung=0)` builtin: the web-research ladder in
// one call. It returns the same dict `qilla fetch --json` prints —
// {url, rung, pos, kind, conf, via, chars, order, route, needs_human,
// tried:[{rung, pos, kind, ms[, note]}]} plus `text` (rung is the rung name,
// max_rung a position in the chosen order) — and never raises for a blocked page: the script reads `kind`.
//
// It is read-only, but it spawns the ladder's tools, so it is gated exactly
// like run(): with [capabilities] exec declared, each rung's binary must be on
// the list or that rung is not attempted.
//
//	fetch("https://example.com") -> {url, rung, pos, kind, conf, via, chars, order, route, text, tried}
func (r *runner) bFetch(_ *starlark.Thread, b *starlark.Builtin, args starlark.Tuple, kw []starlark.Tuple) (starlark.Value, error) {
	var url string
	maxRung := 0
	if err := starlark.UnpackArgs(b.Name(), args, kw, "url", &url, "max_rung?", &maxRung); err != nil {
		return nil, err
	}
	o := fetch.Options{
		MaxRung: maxRung,
		Profile: r.env.BrowserProfile,
		Session: r.env.BrowserSession,
		Class:   r.env.BrowserClass,
		Ask:     r.pageKindAsk(),

		HisterURL:  r.env.Fetch.HisterURL,
		LadderURL:  r.env.Fetch.LadderURL,
		Mirrors:    r.env.Fetch.Mirrors,
		Route:      r.env.Fetch.Route,
		LedgerPath: r.env.Fetch.LedgerPath,
		// Routines run unattended: never a headed window unless the
		// routine's own [fetch] headed says so.
		Headed: fetch.HeadedNever,
		// karakeep-crawl creates a bookmark: never on a dry run.
		ReadOnly: r.env.DryRun,
	}
	if h := r.env.Fetch.Headed; h != "" {
		o.Headed = h
	}
	if u, ok := r.env.Settings["karakeep_url"].(string); ok && u != "" && r.env.SecretsDir != "" {
		if b, err := os.ReadFile(filepath.Join(r.env.SecretsDir, "karakeep")); err == nil {
			o.KarakeepURL, o.KarakeepKey = u, strings.TrimSpace(string(b))
		}
	}
	if c := r.env.Caps; c != nil && len(c.Exec) > 0 {
		allowed := map[string]bool{}
		for _, e := range c.Exec {
			allowed[filepath.Base(e)] = true
		}
		o.Look = func(name string) (string, error) {
			if !allowed[name] {
				return "", fmt.Errorf("capability exec: %s not declared", name)
			}
			return exec.LookPath(name)
		}
	}
	res, err := fetch.Fetch(r.ctx, url, o)
	if err != nil {
		return nil, fmt.Errorf("fetch: %w", err)
	}
	return fetchDict(res), nil
}

// bPageKind is `page_kind(text)`: classify already-fetched text without
// fetching anything. -> {kind, conf, via}
func (r *runner) bPageKind(_ *starlark.Thread, b *starlark.Builtin, args starlark.Tuple, kw []starlark.Tuple) (starlark.Value, error) {
	var text string
	if err := starlark.UnpackArgs(b.Name(), args, kw, "text", &text); err != nil {
		return nil, err
	}
	kind, conf, via := decide.PageKindWith(r.ctx, text, r.pageKindAsk())
	d := starlark.NewDict(3)
	d.SetKey(starlark.String("kind"), starlark.String(kind))
	d.SetKey(starlark.String("conf"), starlark.Float(conf))
	d.SetKey(starlark.String("via"), starlark.String(via))
	return d, nil
}

// pageKindAsk is PageKind's model fallback on the script's wiring: the same
// [deciders] backend ask() uses, traced with caller "fetch".
func (r *runner) pageKindAsk() decide.PageKindAsk {
	return func(ctx context.Context, req decide.AskRequest) (decide.AskResult, error) {
		if r.env.AskConfig != nil {
			r.env.AskConfig(&req)
		}
		return decide.AskAndTrace(ctx, r.env.DecidersDir, req)
	}
}

func fetchDict(res fetch.Result) *starlark.Dict {
	d := starlark.NewDict(8)
	d.SetKey(starlark.String("url"), starlark.String(res.URL))
	d.SetKey(starlark.String("rung"), starlark.String(res.Rung))
	d.SetKey(starlark.String("pos"), starlark.MakeInt(res.Pos))
	d.SetKey(starlark.String("route"), starlark.String(res.Route))
	order := starlark.NewList(nil)
	for _, n := range res.Order {
		order.Append(starlark.String(n))
	}
	d.SetKey(starlark.String("order"), order)
	d.SetKey(starlark.String("kind"), starlark.String(res.Kind))
	d.SetKey(starlark.String("conf"), starlark.Float(res.Conf))
	d.SetKey(starlark.String("via"), starlark.String(res.Via))
	d.SetKey(starlark.String("chars"), starlark.MakeInt(res.Chars))
	d.SetKey(starlark.String("text"), starlark.String(res.Text))
	d.SetKey(starlark.String("needs_human"), starlark.Bool(res.NeedsHuman))
	tried := starlark.NewList(nil)
	for _, t := range res.Tried {
		e := starlark.NewDict(4)
		e.SetKey(starlark.String("rung"), starlark.String(t.Rung))
		e.SetKey(starlark.String("pos"), starlark.MakeInt(t.Pos))
		e.SetKey(starlark.String("kind"), starlark.String(t.Kind))
		e.SetKey(starlark.String("ms"), starlark.MakeInt(t.MS))
		if t.Note != "" {
			e.SetKey(starlark.String("note"), starlark.String(t.Note))
		}
		tried.Append(e)
	}
	d.SetKey(starlark.String("tried"), tried)
	return d
}
