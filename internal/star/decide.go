package star

import (
	"context"
	"fmt"
	"os"

	"github.com/jalberto/qilla/internal/decide"
	"go.starlark.net/starlark"
)

// bDecide is the `decide(tasks, rows)` builtin: TF-IDF + logistic regression
// in-process, so a gather buckets its rows without a subprocess.
//
// It needs no capability — it only reads the exported models under
// <state_dir>/deciders/models and computes. A task with no model is skipped
// (the caller sees no rows for it and keeps asking the model), never an error.
//
//	decide(["newsletter","trash"], unread) -> [{id, task, label, p, conf, unknown}, …]
func (r *runner) bDecide(_ *starlark.Thread, b *starlark.Builtin, args starlark.Tuple, kw []starlark.Tuple) (starlark.Value, error) {
	var tasksv, rowsv starlark.Value
	if err := starlark.UnpackArgs(b.Name(), args, kw, "tasks", &tasksv, "rows", &rowsv); err != nil {
		return nil, err
	}
	tasks, err := strSlice("decide: tasks", tasksv)
	if err != nil {
		return nil, err
	}
	if r.env.DecidersDir == "" {
		return nil, fmt.Errorf("decide: no deciders directory configured")
	}
	rowsIter, ok := rowsv.(starlark.Iterable)
	if !ok {
		return nil, fmt.Errorf("decide: rows must be a list of dicts")
	}
	var records []decide.Record
	it := rowsIter.Iterate()
	defer it.Done()
	var v starlark.Value
	for it.Next(&v) {
		d, ok := v.(*starlark.Dict)
		if !ok {
			return nil, fmt.Errorf("decide: rows must be a list of dicts, got %s", v.Type())
		}
		records = append(records, decide.Record{
			ID:       dictID(d, "id"),
			Subject:  dictStr(d, "subject"),
			From:     dictStr(d, "from"),
			FromName: dictStr(d, "from_name"),
			Body:     dictStr(d, "body"),
		})
	}
	set, err := decide.LoadSet(r.env.DecidersDir, tasks, os.Stderr)
	if err != nil {
		return nil, err
	}
	out := starlark.NewList(nil)
	for _, res := range set.Classify(records) {
		e := starlark.NewDict(6)
		idv, err := toStar(res.ID.Value())
		if err != nil {
			idv = starlark.String(res.ID.String())
		}
		e.SetKey(starlark.String("id"), idv)
		e.SetKey(starlark.String("task"), starlark.String(res.Task))
		e.SetKey(starlark.String("label"), starlark.String(res.Label))
		e.SetKey(starlark.String("p"), starlark.Float(res.P))
		e.SetKey(starlark.String("conf"), starlark.Float(res.Conf))
		e.SetKey(starlark.String("unknown"), starlark.Bool(res.Unknown))
		out.Append(e)
	}
	return out, nil
}

func strSlice(what string, v starlark.Value) ([]string, error) {
	if s, ok := starlark.AsString(v); ok {
		return []string{s}, nil
	}
	it, ok := v.(starlark.Iterable)
	if !ok {
		return nil, fmt.Errorf("%s must be a string or a list of strings", what)
	}
	iter := it.Iterate()
	defer iter.Done()
	var out []string
	var e starlark.Value
	for iter.Next(&e) {
		s, ok := starlark.AsString(e)
		if !ok {
			return nil, fmt.Errorf("%s must be a list of strings", what)
		}
		out = append(out, s)
	}
	return out, nil
}

// dictID reads the row id, which may be an int (msgvault) or a string. It is
// carried through unchanged so the caller can match it against its own rows.
func dictID(d *starlark.Dict, key string) decide.ID {
	v, found, err := d.Get(starlark.String(key))
	if err != nil || !found {
		return decide.StringID("")
	}
	if i, ok := v.(starlark.Int); ok {
		if n, ok := i.Int64(); ok {
			return decide.IntID(n)
		}
	}
	s, _ := starlark.AsString(v)
	return decide.StringID(s)
}

// dictStr reads a string key, tolerating a missing key or a non-string value.
func dictStr(d *starlark.Dict, key string) string {
	v, found, err := d.Get(starlark.String(key))
	if err != nil || !found {
		return ""
	}
	s, _ := starlark.AsString(v)
	return s
}

// bAsk is the `ask(kind, text, …)` builtin: one typed decision answered by the
// local decider model, in-process and synchronous.
//
// It needs no capability — it posts to the local Lemonade on 127.0.0.1 and
// appends a trace line under <state_dir>/deciders. A backend that is down is
// never an error: the script gets label "unknown" and an "error" key, and
// **must not act on an unknown**.
//
// `floor` defaults to [deciders] conf_floor (0.85 out of the box).
//
//	ask("choice", body, options=["respond","archive"], question="…") ->
//	  {kind, label, conf, dist, route, model, ms[, error][, dist_from]}
func (r *runner) bAsk(_ *starlark.Thread, b *starlark.Builtin, args starlark.Tuple, kw []starlark.Tuple) (starlark.Value, error) {
	var (
		kind, text, question, route, caller string
		optionsv                            starlark.Value
		// floor 0 = the configured [deciders] conf_floor (default 0.85).
		floor  float64
		public bool
	)
	if err := starlark.UnpackArgs(b.Name(), args, kw,
		"kind", &kind, "text", &text,
		"options?", &optionsv, "question?", &question, "floor?", &floor,
		"route?", &route, "public?", &public, "caller?", &caller); err != nil {
		return nil, err
	}
	var options []string
	if optionsv != nil {
		var err error
		if options, err = strSlice("ask: options", optionsv); err != nil {
			return nil, err
		}
	}
	if route == "" {
		route = "local"
	}
	req := decide.AskRequest{
		Kind: kind, Options: options, Question: question, Text: text,
		Floor: floor, Route: route, Public: public, Caller: caller,
	}
	if req.Caller == "" {
		req.Caller = r.env.Routine
	}
	if r.env.AskConfig != nil {
		r.env.AskConfig(&req)
	}
	res, err := decide.AskAndTrace(context.Background(), r.env.DecidersDir, req)
	if err != nil && res.Kind == "" {
		return nil, fmt.Errorf("ask: %w", err)
	}
	return askDict(res), nil
}

// askDict is the result as the script sees it — the same keys as the CLI JSON.
func askDict(res decide.AskResult) *starlark.Dict {
	d := starlark.NewDict(9)
	d.SetKey(starlark.String("kind"), starlark.String(res.Kind))
	if res.Kind == "score" {
		if res.Value == nil {
			d.SetKey(starlark.String("value"), starlark.None)
		} else {
			d.SetKey(starlark.String("value"), starlark.MakeInt(*res.Value))
		}
	} else {
		d.SetKey(starlark.String("label"), starlark.String(res.Label))
		if res.Dist == nil {
			d.SetKey(starlark.String("dist"), starlark.None)
		} else {
			dist := starlark.NewDict(len(res.Dist))
			for o, p := range res.Dist {
				dist.SetKey(starlark.String(o), starlark.Float(p))
			}
			d.SetKey(starlark.String("dist"), dist)
		}
	}
	d.SetKey(starlark.String("conf"), starlark.Float(res.Conf))
	if res.DistFrom != "" {
		d.SetKey(starlark.String("dist_from"), starlark.String(res.DistFrom))
	}
	d.SetKey(starlark.String("route"), starlark.String(res.Route))
	d.SetKey(starlark.String("model"), starlark.String(res.Model))
	d.SetKey(starlark.String("ms"), starlark.MakeInt(res.MS))
	if res.Error != "" {
		d.SetKey(starlark.String("error"), starlark.String(res.Error))
	}
	return d
}
