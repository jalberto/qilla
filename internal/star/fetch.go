package star

import (
	"context"
	"fmt"
	"os/exec"
	"path/filepath"

	"github.com/jalberto/qilla/internal/decide"
	"github.com/jalberto/qilla/internal/fetch"
	"go.starlark.net/starlark"
)

// bFetch is the `fetch(url, max_rung=4)` builtin: the web-research ladder in
// one call. It returns the same dict `qilla fetch --json` prints —
// {url, rung, kind, conf, via, chars, tried:[{rung, kind, ms[, note]}]} plus
// `text` — and never raises for a blocked page: the script reads `kind`.
//
// It is read-only, but it spawns the ladder's tools, so it is gated exactly
// like run(): with [capabilities] exec declared, each rung's binary must be on
// the list or that rung is not attempted.
//
//	fetch("https://example.com") -> {url, rung, kind, conf, via, chars, text, tried}
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
	d.SetKey(starlark.String("rung"), starlark.MakeInt(res.Rung))
	d.SetKey(starlark.String("kind"), starlark.String(res.Kind))
	d.SetKey(starlark.String("conf"), starlark.Float(res.Conf))
	d.SetKey(starlark.String("via"), starlark.String(res.Via))
	d.SetKey(starlark.String("chars"), starlark.MakeInt(res.Chars))
	d.SetKey(starlark.String("text"), starlark.String(res.Text))
	tried := starlark.NewList(nil)
	for _, t := range res.Tried {
		e := starlark.NewDict(4)
		e.SetKey(starlark.String("rung"), starlark.MakeInt(t.Rung))
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
