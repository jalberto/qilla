// Package star runs a routine's gather step written in Starlark (gather.star)
// in-process, so a bundle author gets a Python-shaped, hermetic, JSON-native
// language with no interpreter to install.
//
// The script must define `def gather(ctx):` returning a dict (or set a
// top-level `result`). The value is converted to plain Go values and the
// worker JSON-encodes it exactly like gather.sh stdout, so digests keep
// working.
//
// The predeclared environment is FROZEN — adding to it is a deliberate,
// documented change (mirror it in Qilla/Plugin/skills/routine/SKILL.md):
//
//	modules: json (encode/decode/indent), time, math, re
//	re:      match, find, findall, sub (Go $1 replacement syntax), split, groups
//	         — RE2 syntax (no backreferences, no lookaround)
//	funcs:   run(cmd, timeout=30, input=None, env=None) -> {rc, out, err}
//	         read(path) -> str        exists(path) -> bool
//	         glob(pattern) -> [str]   listdir(path) -> [str]
//	         mtime(path) -> float     env(name, default="") -> str
//	         ask(kind, text, options=[], question="", floor=0.85) ->
//	         {kind, label, conf, dist, route, model, ms} — one typed decision
//	         from the local decider model; unknown when it is not sure.
//	         decide(tasks, rows) -> [{id, task, label, p, conf, unknown}] —
//	         the trained TF-IDF deciders, in-process and read-only; a task
//	         with no exported model is silently skipped.
//	         now() -> float           fail(msg)             print -> stderr
//	sqlite:  sqlite.query(path, sql, params=[]) -> [dict] — READ-ONLY: the db is
//	         opened mode=ro and only one SELECT / WITH / PRAGMA table_info runs.
//	         Prefer a tool's CLI over its database when it has one.
//	ctx:     .routine .date .vault .env (dict of QILLA_*) .secrets_dir
//	         .dry_run .actions (what a dry run suppressed)
//	globals: dry_run (bool)
//
// Hermetic by default: there is no `open` for writing and no network.
// The posture stays READ + RUN only — even sqlite is read-only.
// State a gather needs to keep goes through run(["qilla", ...]) or a command
// that writes it — never through the Starlark script itself.
//
// DECLARE OR STAY PURE — side effects come only from [capabilities] in the
// bundle's routine.toml, and each declaration injects exactly its helper:
//
//	http  = { hosts = [...], methods = [...] }  → http(method, url, headers={},
//	         json=None, body=None, timeout=30) -> {status, headers, body, json,
//	         error} and secret(name) -> str (reads <secrets_dir>/<name>).
//	         Undeclared host or method is an error; redirects off the
//	         allowlist are refused; methods default to ["GET"].
//	         A transport failure (connection refused, DNS, timeout, TLS, a
//	         redirect refused because it leaves the allowlist) does NOT raise —
//	         Starlark has no try/except: it comes back as {status: 0,
//	         headers: {}, body: "", json: None, error: "<short reason>"} so a
//	         gather degrades one failed call into a row. On success error is
//	         None. Programmer errors still raise: undeclared host or method,
//	         bad arguments, bad JSON in json=.
//	write = { paths = [...] }                   → write(path, text) -> bool
//	         vault-relative, inside a declared prefix, atomic (temp + rename),
//	         False when the content is already identical.
//	exec  = ["msgvault", ...]                   → restricts run() argv[0].
//
// Under a dry run (`qilla gather <routine> --dry`, QILLA_DRY_RUN=1) write()
// and non-GET http() do nothing, append {kind, target} to ctx.actions and the
// worker adds "_dry_actions" to the gather JSON.
package star

import (
	"bytes"
	"context"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/jalberto/qilla/internal/decide"
	"github.com/jalberto/qilla/internal/manifest"
	"go.starlark.net/lib/json"
	starlarkmath "go.starlark.net/lib/math"
	starlarktime "go.starlark.net/lib/time"
	"go.starlark.net/starlark"
	"go.starlark.net/starlarkstruct"
	"go.starlark.net/syntax"
)

// DefaultTimeout matches the worker's gather.sh timeout.
const DefaultTimeout = 5 * time.Minute

// fileOpts enable the dialect a gather author expects: while loops, top-level
// control flow, sets and recursion.
var fileOpts = &syntax.FileOptions{
	Set:             true,
	While:           true,
	TopLevelControl: true,
	GlobalReassign:  true,
	Recursion:       true,
}

// Env is what the runtime hands the script as `ctx`.
type Env struct {
	Routine    string
	Date       string
	Vault      string
	SecretsDir string
	Vars       map[string]string // the QILLA_* variables, also exported to run()
	// Settings is [routines.<name>.settings]: the user-specific configuration
	// a shareable bundle reads instead of hardcoding. Exposed as the global
	// `settings` dict (and as ctx.settings).
	Settings map[string]any
	Timeout  time.Duration // 0 → DefaultTimeout
	// MemSearch and MemState are read-only working-memory access for the
	// `mem` module. nil = the module's calls fail with a clear message.
	MemSearch func(query string, n int) ([]MemEntry, error)
	MemState  func(routine string) ([]MemEntry, error)
	// Caps is [capabilities] from the bundle's routine.toml: nil (or a nil
	// member) means the matching helper is not injected at all.
	Caps *manifest.Capabilities
	// DryRun suppresses mutations: write() and non-GET http() record a
	// {kind, target} action instead of acting.
	DryRun bool
	// DecidersDir is <state_dir>/deciders: where decide() reads the exported
	// models and ask() appends its decision trace. Empty = the decide()
	// builtin reports it is not configured and ask() traces nothing.
	DecidersDir string
	// AskConfig fills ask()'s backend wiring from [deciders] (Lemonade URL,
	// model, floor, the jev route). nil = the package defaults.
	AskConfig func(*decide.AskRequest)
}

// Run executes path and returns the gather result as plain Go values
// (map[string]any). Anything the script printed is returned in prints, to be
// kept with the error like gather.sh stderr.
func Run(ctx context.Context, path string, e Env) (result any, prints string, err error) {
	result, _, prints, err = RunActions(ctx, path, e)
	return result, prints, err
}

// RunActions is Run plus the side effects a dry run suppressed, in order.
// Outside a dry run it is always empty.
func RunActions(ctx context.Context, path string, e Env) (result any, actions []map[string]any, prints string, err error) {
	src, err := os.ReadFile(path)
	if err != nil {
		return nil, nil, "", err
	}
	to := e.Timeout
	if to <= 0 {
		to = DefaultTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, to)
	defer cancel()

	var out bytes.Buffer
	r := &runner{env: e, ctx: ctx, res: make(map[string]*regexp.Regexp)}
	thread := &starlark.Thread{Name: "gather:" + e.Routine, Print: func(_ *starlark.Thread, msg string) {
		out.WriteString(msg)
		out.WriteByte('\n')
	}}
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			thread.Cancel("timeout")
		case <-done:
		}
	}()

	globals, err := starlark.ExecFileOptions(fileOpts, thread, path, src, r.predeclared())
	if err != nil {
		return nil, r.takeActions(), out.String(), starErr(err)
	}
	var val starlark.Value
	if fn, ok := globals["gather"].(starlark.Callable); ok {
		val, err = starlark.Call(thread, fn, starlark.Tuple{r.ctxValue()}, nil)
		if err != nil {
			return nil, r.takeActions(), out.String(), starErr(err)
		}
	} else if v, ok := globals["result"]; ok {
		val = v
	} else {
		return nil, r.takeActions(), out.String(), fmt.Errorf("gather.star must define gather(ctx) or a top-level result")
	}
	if _, ok := val.(*starlark.Dict); !ok {
		return nil, r.takeActions(), out.String(), fmt.Errorf("gather() must return a dict")
	}
	g, err := toGo(val)
	if err != nil {
		return nil, r.takeActions(), out.String(), err
	}
	return g, r.takeActions(), out.String(), nil
}

func starErr(err error) error {
	if ee, ok := err.(*starlark.EvalError); ok {
		return fmt.Errorf("%s", ee.Backtrace())
	}
	return err
}

type runner struct {
	env Env
	ctx context.Context

	mu         sync.Mutex
	res        map[string]*regexp.Regexp
	actions    []map[string]any // side effects a dry run suppressed
	actionsVal *starlark.List   // the same list as ctx.actions
}

// takeActions returns the recorded dry-run actions.
func (r *runner) takeActions() []map[string]any {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.actions
}

func (r *runner) ctxValue() starlark.Value {
	vars := starlark.NewDict(len(r.env.Vars))
	for _, k := range sortedKeys(r.env.Vars) {
		vars.SetKey(starlark.String(k), starlark.String(r.env.Vars[k]))
	}
	vars.Freeze()
	return starlarkstruct.FromStringDict(starlark.String("ctx"), starlark.StringDict{
		"routine":     starlark.String(r.env.Routine),
		"date":        starlark.String(r.env.Date),
		"vault":       starlark.String(r.env.Vault),
		"env":         vars,
		"secrets_dir": starlark.String(r.env.SecretsDir),
		"settings":    r.settingsValue(),
		"dry_run":     starlark.Bool(r.env.DryRun),
		"actions":     r.actionList(),
	})
}

// settingsValue exposes Env.Settings as a frozen Starlark value.
func (r *runner) settingsValue() starlark.Value {
	v, err := toStar(r.env.Settings)
	if err != nil || v == nil {
		d := starlark.NewDict(0)
		d.Freeze()
		return d
	}
	v.Freeze()
	return v
}

func (r *runner) predeclared() starlark.StringDict {
	d := r.frozenPredeclared()
	// capabilities: a helper exists only because routine.toml declares it
	if c := r.env.Caps; c != nil {
		if c.HTTP != nil {
			d["http"] = starlark.NewBuiltin("http", r.bHTTP)
			d["secret"] = starlark.NewBuiltin("secret", r.bSecret)
		}
		if c.Write != nil {
			d["write"] = starlark.NewBuiltin("write", r.bWrite)
		}
	}
	return d
}

func (r *runner) frozenPredeclared() starlark.StringDict {
	return starlark.StringDict{
		"dry_run":  starlark.Bool(r.env.DryRun),
		"json":     json.Module,
		"time":     starlarktime.Module,
		"math":     starlarkmath.Module,
		"re":       r.reModule(),
		"sqlite":   r.sqliteModule(),
		"mem":      r.memModule(),
		"run":      starlark.NewBuiltin("run", r.bRun),
		"read":     starlark.NewBuiltin("read", r.bRead),
		"exists":   starlark.NewBuiltin("exists", r.bExists),
		"glob":     starlark.NewBuiltin("glob", r.bGlob),
		"listdir":  starlark.NewBuiltin("listdir", r.bListdir),
		"mtime":    starlark.NewBuiltin("mtime", r.bMtime),
		"env":      starlark.NewBuiltin("env", r.bEnv),
		"now":      starlark.NewBuiltin("now", r.bNow),
		"decide":   starlark.NewBuiltin("decide", r.bDecide),
		"ask":      starlark.NewBuiltin("ask", r.bAsk),
		"settings": r.settingsValue(),
	}
}

// path resolves a vault-relative path; absolute paths pass through.
func (r *runner) path(p string) string {
	if filepath.IsAbs(p) {
		return p
	}
	return filepath.Join(r.env.Vault, p)
}

// ── files ────────────────────────────────────────────────────────────────

func (r *runner) bRead(_ *starlark.Thread, b *starlark.Builtin, args starlark.Tuple, kw []starlark.Tuple) (starlark.Value, error) {
	var p string
	if err := starlark.UnpackArgs(b.Name(), args, kw, "path", &p); err != nil {
		return nil, err
	}
	data, err := os.ReadFile(r.path(p))
	if err != nil {
		return nil, err
	}
	return starlark.String(data), nil
}

func (r *runner) bExists(_ *starlark.Thread, b *starlark.Builtin, args starlark.Tuple, kw []starlark.Tuple) (starlark.Value, error) {
	var p string
	if err := starlark.UnpackArgs(b.Name(), args, kw, "path", &p); err != nil {
		return nil, err
	}
	_, err := os.Stat(r.path(p))
	return starlark.Bool(err == nil), nil
}

func (r *runner) bGlob(_ *starlark.Thread, b *starlark.Builtin, args starlark.Tuple, kw []starlark.Tuple) (starlark.Value, error) {
	var pat string
	if err := starlark.UnpackArgs(b.Name(), args, kw, "pattern", &pat); err != nil {
		return nil, err
	}
	m, err := filepath.Glob(r.path(pat))
	if err != nil {
		return nil, err
	}
	rel := make([]string, 0, len(m))
	for _, p := range m {
		if q, err := filepath.Rel(r.env.Vault, p); err == nil && !strings.HasPrefix(q, "..") {
			p = q
		}
		rel = append(rel, p)
	}
	sort.Strings(rel)
	return strList(rel), nil
}

func (r *runner) bListdir(_ *starlark.Thread, b *starlark.Builtin, args starlark.Tuple, kw []starlark.Tuple) (starlark.Value, error) {
	var p string
	if err := starlark.UnpackArgs(b.Name(), args, kw, "path", &p); err != nil {
		return nil, err
	}
	ents, err := os.ReadDir(r.path(p))
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(ents))
	for _, e := range ents {
		names = append(names, e.Name())
	}
	sort.Strings(names)
	return strList(names), nil
}

func (r *runner) bMtime(_ *starlark.Thread, b *starlark.Builtin, args starlark.Tuple, kw []starlark.Tuple) (starlark.Value, error) {
	var p string
	if err := starlark.UnpackArgs(b.Name(), args, kw, "path", &p); err != nil {
		return nil, err
	}
	fi, err := os.Stat(r.path(p))
	if err != nil {
		return nil, err
	}
	return starlark.Float(float64(fi.ModTime().UnixNano()) / 1e9), nil
}

func (r *runner) bEnv(_ *starlark.Thread, b *starlark.Builtin, args starlark.Tuple, kw []starlark.Tuple) (starlark.Value, error) {
	var name, def string
	if err := starlark.UnpackArgs(b.Name(), args, kw, "name", &name, "default?", &def); err != nil {
		return nil, err
	}
	if v, ok := r.env.Vars[name]; ok {
		return starlark.String(v), nil
	}
	if v, ok := os.LookupEnv(name); ok {
		return starlark.String(v), nil
	}
	return starlark.String(def), nil
}

func (r *runner) bNow(*starlark.Thread, *starlark.Builtin, starlark.Tuple, []starlark.Tuple) (starlark.Value, error) {
	return starlark.Float(float64(time.Now().UnixNano()) / 1e9), nil
}

// ── run ──────────────────────────────────────────────────────────────────

// bRun executes a command (no shell) with cwd = the vault. A non-zero exit is
// never an error: the script sees rc and decides.
func (r *runner) bRun(_ *starlark.Thread, b *starlark.Builtin, args starlark.Tuple, kw []starlark.Tuple) (starlark.Value, error) {
	var cmdv *starlark.List
	var timeout starlark.Value = starlark.Float(30) // int or float, both mean seconds
	var input starlark.Value = starlark.None
	var extra starlark.Value = starlark.None
	if err := starlark.UnpackArgs(b.Name(), args, kw, "cmd", &cmdv, "timeout?", &timeout, "input?", &input, "env?", &extra); err != nil {
		return nil, err
	}
	argv := make([]string, 0, cmdv.Len())
	for i := 0; i < cmdv.Len(); i++ {
		s, ok := starlark.AsString(cmdv.Index(i))
		if !ok {
			return nil, fmt.Errorf("run: cmd must be a list of strings")
		}
		argv = append(argv, s)
	}
	if len(argv) == 0 {
		return nil, fmt.Errorf("run: empty cmd")
	}
	if c := r.env.Caps; c != nil && len(c.Exec) > 0 {
		base := filepath.Base(argv[0])
		ok := false
		for _, allowed := range c.Exec {
			if allowed == base {
				ok = true
				break
			}
		}
		if !ok {
			return nil, fmt.Errorf("capability exec: %s not declared", base)
		}
	}
	secs, ok := starlark.AsFloat(timeout)
	if !ok {
		return nil, fmt.Errorf("run: timeout must be a number of seconds")
	}
	if secs <= 0 || math.IsNaN(secs) {
		secs = 30
	}
	ctx, cancel := context.WithTimeout(r.ctx, time.Duration(secs*float64(time.Second)))
	defer cancel()
	c := exec.CommandContext(ctx, argv[0], argv[1:]...)
	c.Dir = r.env.Vault
	c.Env = os.Environ()
	for _, k := range sortedKeys(r.env.Vars) {
		c.Env = append(c.Env, k+"="+r.env.Vars[k])
	}
	if d, ok := extra.(*starlark.Dict); ok {
		for _, k := range d.Keys() {
			v, _, _ := d.Get(k)
			ks, _ := starlark.AsString(k)
			vs, _ := starlark.AsString(v)
			c.Env = append(c.Env, ks+"="+vs)
		}
	}
	if input != starlark.None {
		s, ok := starlark.AsString(input)
		if !ok {
			return nil, fmt.Errorf("run: input must be a string")
		}
		c.Stdin = strings.NewReader(s)
	}
	var stdout, stderr bytes.Buffer
	c.Stdout, c.Stderr = &stdout, &stderr
	rc := 0
	if err := c.Run(); err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			rc = ee.ExitCode()
		} else {
			rc = -1
			if stderr.Len() == 0 {
				stderr.WriteString(err.Error())
			}
		}
	}
	d := starlark.NewDict(3)
	d.SetKey(starlark.String("rc"), starlark.MakeInt(rc))
	d.SetKey(starlark.String("out"), starlark.String(stdout.String()))
	d.SetKey(starlark.String("err"), starlark.String(stderr.String()))
	return d, nil
}

// ── re ───────────────────────────────────────────────────────────────────

// compile caches compiled patterns; gathers re-use the same few in loops.
func (r *runner) compile(pat string) (*regexp.Regexp, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if re, ok := r.res[pat]; ok {
		return re, nil
	}
	re, err := regexp.Compile(pat)
	if err != nil {
		return nil, err
	}
	r.res[pat] = re
	return re, nil
}

func (r *runner) reModule() *starlarkstruct.Module {
	two := func(name string, f func(re *regexp.Regexp, s string) (starlark.Value, error)) *starlark.Builtin {
		return starlark.NewBuiltin(name, func(_ *starlark.Thread, b *starlark.Builtin, args starlark.Tuple, kw []starlark.Tuple) (starlark.Value, error) {
			var pat, s string
			if err := starlark.UnpackArgs(b.Name(), args, kw, "pattern", &pat, "s", &s); err != nil {
				return nil, err
			}
			re, err := r.compile(pat)
			if err != nil {
				return nil, err
			}
			return f(re, s)
		})
	}
	return &starlarkstruct.Module{Name: "re", Members: starlark.StringDict{
		"match": two("match", func(re *regexp.Regexp, s string) (starlark.Value, error) {
			return starlark.Bool(re.MatchString(s)), nil
		}),
		"find": two("find", func(re *regexp.Regexp, s string) (starlark.Value, error) {
			if loc := re.FindStringIndex(s); loc != nil {
				return starlark.String(s[loc[0]:loc[1]]), nil
			}
			return starlark.None, nil
		}),
		"findall": two("findall", func(re *regexp.Regexp, s string) (starlark.Value, error) {
			if re.NumSubexp() == 0 {
				return strList(re.FindAllString(s, -1)), nil
			}
			l := starlark.NewList(nil)
			for _, g := range re.FindAllStringSubmatch(s, -1) {
				l.Append(strList(g[1:]))
			}
			return l, nil
		}),
		"split": two("split", func(re *regexp.Regexp, s string) (starlark.Value, error) {
			return strList(re.Split(s, -1)), nil
		}),
		"groups": two("groups", func(re *regexp.Regexp, s string) (starlark.Value, error) {
			m := re.FindStringSubmatch(s)
			if m == nil {
				return starlark.None, nil
			}
			return strList(m[1:]), nil
		}),
		// sub uses Go's replacement syntax: $1 / ${name}, not \1.
		"sub": starlark.NewBuiltin("sub", func(_ *starlark.Thread, b *starlark.Builtin, args starlark.Tuple, kw []starlark.Tuple) (starlark.Value, error) {
			var pat, repl, s string
			if err := starlark.UnpackArgs(b.Name(), args, kw, "pattern", &pat, "repl", &repl, "s", &s); err != nil {
				return nil, err
			}
			re, err := r.compile(pat)
			if err != nil {
				return nil, err
			}
			return starlark.String(re.ReplaceAllString(s, repl)), nil
		}),
	}}
}

// ── conversion ───────────────────────────────────────────────────────────

func strList(ss []string) *starlark.List {
	vs := make([]starlark.Value, len(ss))
	for i, s := range ss {
		vs[i] = starlark.String(s)
	}
	return starlark.NewList(vs)
}

func sortedKeys(m map[string]string) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return ks
}

// toStar converts plain Go values (as decoded from TOML/JSON) into Starlark.
// Anything it cannot map becomes its fmt string, so a settings value never
// breaks a gather.
func toStar(v any) (starlark.Value, error) {
	switch t := v.(type) {
	case nil:
		return starlark.None, nil
	case bool:
		return starlark.Bool(t), nil
	case string:
		return starlark.String(t), nil
	case int:
		return starlark.MakeInt(t), nil
	case int64:
		return starlark.MakeInt64(t), nil
	case float64:
		return starlark.Float(t), nil
	case []any:
		vs := make([]starlark.Value, 0, len(t))
		for _, e := range t {
			sv, err := toStar(e)
			if err != nil {
				return nil, err
			}
			vs = append(vs, sv)
		}
		return starlark.NewList(vs), nil
	case []string:
		return strList(t), nil
	case map[string]any:
		d := starlark.NewDict(len(t))
		ks := make([]string, 0, len(t))
		for k := range t {
			ks = append(ks, k)
		}
		sort.Strings(ks)
		for _, k := range ks {
			sv, err := toStar(t[k])
			if err != nil {
				return nil, err
			}
			if err := d.SetKey(starlark.String(k), sv); err != nil {
				return nil, err
			}
		}
		return d, nil
	default:
		return starlark.String(fmt.Sprint(v)), nil
	}
}

// toGo converts a Starlark value to plain Go values (JSON-encodable).
func toGo(v starlark.Value) (any, error) {
	switch t := v.(type) {
	case starlark.NoneType:
		return nil, nil
	case starlark.Bool:
		return bool(t), nil
	case starlark.Int:
		if i, ok := t.Int64(); ok {
			return i, nil
		}
		f, _ := starlark.AsFloat(t)
		return f, nil
	case starlark.Float:
		return float64(t), nil
	case starlark.String:
		return string(t), nil
	case *starlark.List:
		out := make([]any, 0, t.Len())
		for i := 0; i < t.Len(); i++ {
			g, err := toGo(t.Index(i))
			if err != nil {
				return nil, err
			}
			out = append(out, g)
		}
		return out, nil
	case starlark.Tuple:
		out := make([]any, 0, t.Len())
		for i := 0; i < t.Len(); i++ {
			g, err := toGo(t.Index(i))
			if err != nil {
				return nil, err
			}
			out = append(out, g)
		}
		return out, nil
	case *starlark.Dict:
		out := make(map[string]any, t.Len())
		for _, k := range t.Keys() {
			ks, ok := starlark.AsString(k)
			if !ok {
				return nil, fmt.Errorf("dict keys must be strings, got %s", k.Type())
			}
			val, _, _ := t.Get(k)
			g, err := toGo(val)
			if err != nil {
				return nil, err
			}
			out[ks] = g
		}
		return out, nil
	case *starlark.Set:
		out := make([]any, 0, t.Len())
		iter := t.Iterate()
		defer iter.Done()
		var e starlark.Value
		for iter.Next(&e) {
			g, err := toGo(e)
			if err != nil {
				return nil, err
			}
			out = append(out, g)
		}
		return out, nil
	}
	return nil, fmt.Errorf("cannot convert %s to JSON", v.Type())
}
