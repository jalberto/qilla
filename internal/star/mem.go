package star

import (
	"fmt"

	"go.starlark.net/starlark"
	"go.starlark.net/starlarkstruct"
)

// MemEntry is one working-memory note as a gather script sees it.
type MemEntry struct {
	Project string
	Kind    string
	Key     string
	Text    string
	Updated string // RFC3339
}

func (e MemEntry) value() starlark.Value {
	d := starlark.NewDict(5)
	d.SetKey(starlark.String("project"), starlark.String(e.Project))
	d.SetKey(starlark.String("kind"), starlark.String(e.Kind))
	d.SetKey(starlark.String("key"), starlark.String(e.Key))
	d.SetKey(starlark.String("text"), starlark.String(e.Text))
	d.SetKey(starlark.String("updated"), starlark.String(e.Updated))
	d.Freeze()
	return d
}

// memModule exposes read-only working memory so a gather can skip what the
// routine already handled instead of re-diffing the whole vault.
//
//	mem.search("offer 41", n=5)   -> [ {project,kind,key,text,updated}, … ]
//	mem.state()                   -> this routine's state entries, newest first
//	mem.state("market")         -> another routine's state entries
func (r *runner) memModule() *starlarkstruct.Module {
	return &starlarkstruct.Module{Name: "mem", Members: starlark.StringDict{
		"search": starlark.NewBuiltin("mem.search", r.bMemSearch),
		"state":  starlark.NewBuiltin("mem.state", r.bMemState),
	}}
}

func (r *runner) bMemSearch(_ *starlark.Thread, b *starlark.Builtin, args starlark.Tuple, kw []starlark.Tuple) (starlark.Value, error) {
	var query string
	n := 5
	if err := starlark.UnpackArgs(b.Name(), args, kw, "query", &query, "n?", &n); err != nil {
		return nil, err
	}
	if r.env.MemSearch == nil {
		return nil, fmt.Errorf("mem.search: working memory is not available to this run")
	}
	es, err := r.env.MemSearch(query, n)
	if err != nil {
		return nil, fmt.Errorf("mem.search: %w", err)
	}
	return memList(es), nil
}

func (r *runner) bMemState(_ *starlark.Thread, b *starlark.Builtin, args starlark.Tuple, kw []starlark.Tuple) (starlark.Value, error) {
	routine := ""
	if err := starlark.UnpackArgs(b.Name(), args, kw, "routine?", &routine); err != nil {
		return nil, err
	}
	if routine == "" {
		routine = r.env.Routine
	}
	if r.env.MemState == nil {
		return nil, fmt.Errorf("mem.state: working memory is not available to this run")
	}
	es, err := r.env.MemState(routine)
	if err != nil {
		return nil, fmt.Errorf("mem.state: %w", err)
	}
	return memList(es), nil
}

func memList(es []MemEntry) *starlark.List {
	vs := make([]starlark.Value, 0, len(es))
	for _, e := range es {
		vs = append(vs, e.value())
	}
	l := starlark.NewList(vs)
	l.Freeze()
	return l
}
