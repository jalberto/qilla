package star

import (
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
			ID:       dictStr(d, "id"),
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
		e.SetKey(starlark.String("id"), starlark.String(res.ID))
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

// dictStr reads a string key, tolerating a missing key or a non-string value.
func dictStr(d *starlark.Dict, key string) string {
	v, found, err := d.Get(starlark.String(key))
	if err != nil || !found {
		return ""
	}
	s, _ := starlark.AsString(v)
	return s
}
