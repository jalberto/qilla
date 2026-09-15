package star

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	_ "modernc.org/sqlite"

	"go.starlark.net/starlark"
	"go.starlark.net/starlarkstruct"
)

// maxRows caps a single sqlite.query so a runaway SELECT cannot eat the run.
const maxRows = 10000

// sqliteModule is `sqlite` in the frozen predeclared set: ONE function,
// read-only. The posture of a gather is unchanged — read + run only — so the
// database is opened with mode=ro and only a single SELECT / WITH /
// PRAGMA table_info statement is accepted.
func (r *runner) sqliteModule() *starlarkstruct.Module {
	return &starlarkstruct.Module{Name: "sqlite", Members: starlark.StringDict{
		"query": starlark.NewBuiltin("query", r.bSQLiteQuery),
	}}
}

// readOnlySQL accepts only a single read statement.
func readOnlySQL(q string) error {
	s := strings.TrimSpace(q)
	s = strings.TrimSuffix(s, ";")
	if strings.Contains(s, ";") {
		return fmt.Errorf("sqlite.query: one statement only (found %q)", ";")
	}
	up := strings.ToUpper(s)
	switch {
	case strings.HasPrefix(up, "SELECT"), strings.HasPrefix(up, "WITH"):
		return nil
	case strings.HasPrefix(up, "PRAGMA TABLE_INFO"):
		return nil
	}
	word := up
	if i := strings.IndexAny(word, " \t\n("); i > 0 {
		word = word[:i]
	}
	return fmt.Errorf("sqlite.query is read-only: only SELECT / WITH / PRAGMA table_info, got %q", word)
}

// expandHome resolves a leading ~ (the gather's own $HOME).
func expandHome(p string) string {
	if p == "~" || strings.HasPrefix(p, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, strings.TrimPrefix(strings.TrimPrefix(p, "~"), "/"))
		}
	}
	return p
}

func (r *runner) bSQLiteQuery(_ *starlark.Thread, b *starlark.Builtin, args starlark.Tuple, kw []starlark.Tuple) (starlark.Value, error) {
	var path, query string
	var params starlark.Value = starlark.None
	if err := starlark.UnpackArgs(b.Name(), args, kw, "path", &path, "sql", &query, "params?", &params); err != nil {
		return nil, err
	}
	if err := readOnlySQL(query); err != nil {
		return nil, err
	}
	abs := r.path(expandHome(path))
	if _, err := os.Stat(abs); err != nil {
		return nil, fmt.Errorf("sqlite.query: %w", err)
	}
	ps, err := sqlParams(params)
	if err != nil {
		return nil, err
	}

	db, err := sql.Open("sqlite", "file:"+abs+"?mode=ro&immutable=0")
	if err != nil {
		return nil, fmt.Errorf("sqlite.query: %w", err)
	}
	defer db.Close()

	rows, err := db.QueryContext(r.ctx, query, ps...)
	if err != nil {
		return nil, fmt.Errorf("sqlite.query: %w", err)
	}
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		return nil, fmt.Errorf("sqlite.query: %w", err)
	}

	out := starlark.NewList(nil)
	for rows.Next() {
		if out.Len() >= maxRows {
			return nil, fmt.Errorf("sqlite.query: more than %d rows — narrow the query", maxRows)
		}
		cells := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range cells {
			ptrs[i] = &cells[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return nil, fmt.Errorf("sqlite.query: %w", err)
		}
		d := starlark.NewDict(len(cols))
		for i, c := range cols {
			v, err := sqlValue(cells[i])
			if err != nil {
				return nil, err
			}
			if err := d.SetKey(starlark.String(c), v); err != nil {
				return nil, err
			}
		}
		if err := out.Append(d); err != nil {
			return nil, err
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("sqlite.query: %w", err)
	}
	return out, nil
}

// sqlParams converts the optional bind list.
func sqlParams(v starlark.Value) ([]any, error) {
	if v == nil || v == starlark.None {
		return nil, nil
	}
	it, ok := v.(starlark.Iterable)
	if !ok {
		return nil, fmt.Errorf("sqlite.query: params must be a list")
	}
	iter := it.Iterate()
	defer iter.Done()
	var out []any
	var e starlark.Value
	for iter.Next(&e) {
		switch t := e.(type) {
		case starlark.NoneType:
			out = append(out, nil)
		case starlark.String:
			out = append(out, string(t))
		case starlark.Bool:
			out = append(out, bool(t))
		case starlark.Int:
			i, ok := t.Int64()
			if !ok {
				return nil, fmt.Errorf("sqlite.query: integer param out of range")
			}
			out = append(out, i)
		case starlark.Float:
			out = append(out, float64(t))
		default:
			return nil, fmt.Errorf("sqlite.query: unsupported param type %s", e.Type())
		}
	}
	return out, nil
}

// sqlValue maps a driver value to Starlark: int/float/str/None, bytes → str.
func sqlValue(v any) (starlark.Value, error) {
	switch t := v.(type) {
	case nil:
		return starlark.None, nil
	case int64:
		return starlark.MakeInt64(t), nil
	case float64:
		return starlark.Float(t), nil
	case bool:
		return starlark.Bool(t), nil
	case string:
		return starlark.String(t), nil
	case []byte:
		return starlark.String(t), nil
	}
	return starlark.String(fmt.Sprint(v)), nil
}
