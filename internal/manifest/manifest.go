// Package manifest reads a routine bundle's routine.toml: what the routine
// needs before it can be enabled (commands, secrets, settings), which features
// are optional, which files the user must supply and which signals prove the
// routine has something to work with.
//
// A bundle is generic code; everything user-specific lives in
// [routines.<name>.settings] in qilla.toml and in user data files. The
// manifest is what makes that contract checkable: `qilla routine check`.
package manifest

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
)

// File is the manifest's name inside a bundle.
const File = "routine.toml"

// SignalTimeout caps one signal command.
const SignalTimeout = 20 * time.Second

// Requires is the hard contract: anything missing fails the check.
type Requires struct {
	Commands []string `toml:"commands"` // binaries on PATH
	Secrets  []string `toml:"secrets"`  // names under `qilla secret`
	Settings []string `toml:"settings"` // keys in [routines.<name>.settings]
}

// Optional is a feature the routine degrades without: missing ⇒ disabled.
type Optional struct {
	Name     string   `toml:"name"`
	Commands []string `toml:"commands"`
	Secrets  []string `toml:"secrets"`
	Settings []string `toml:"settings"`
	Hint     string   `toml:"hint"`
}

// UserFile is data the user provides, relative to the bundle.
type UserFile struct {
	Path     string `toml:"path"`
	Required bool   `toml:"required"`
	Hint     string `toml:"hint"`
}

// Signal is evidence the routine has input: a command whose output is judged
// by Expect. Run may contain {{settings.key}} placeholders.
type Signal struct {
	Name   string   `toml:"name"`
	Run    []string `toml:"run"`
	Expect string   `toml:"expect"` // nonempty | ok | json-nonempty
	Hint   string   `toml:"hint"`
}

// HTTPCap scopes the `http` helper: which hosts (host[:port], exactly as the
// URL spells them) and which methods a gather.star may call.
type HTTPCap struct {
	Hosts   []string `toml:"hosts" json:"hosts,omitempty"`
	Methods []string `toml:"methods" json:"methods,omitempty"` // empty → ["GET"]
}

// WriteCap scopes the `write` helper: vault-relative prefixes. A prefix ending
// in "/" is a directory, anything else is an exact file.
type WriteCap struct {
	Paths []string `toml:"paths" json:"paths,omitempty"`
}

// Capabilities are the side effects a bundle declares under [capabilities].
// Nothing declared ⇒ the gather runtime stays at its frozen, pure set.
type Capabilities struct {
	HTTP  *HTTPCap  `toml:"http" json:"http,omitempty"`
	Write *WriteCap `toml:"write" json:"write,omitempty"`
	Exec  []string  `toml:"exec" json:"exec,omitempty"` // when non-empty, restricts run() argv[0]
}

// MethodList returns the declared methods, uppercased, defaulting to GET.
func (c *HTTPCap) MethodList() []string {
	if c == nil || len(c.Methods) == 0 {
		return []string{"GET"}
	}
	out := make([]string, 0, len(c.Methods))
	for _, m := range c.Methods {
		out = append(out, strings.ToUpper(strings.TrimSpace(m)))
	}
	return out
}

// KnownMethods are the HTTP verbs a bundle may declare.
var KnownMethods = map[string]bool{"GET": true, "POST": true, "PUT": true, "PATCH": true, "DELETE": true}

// Manifest is routine.toml.
type Manifest struct {
	Name      string     `toml:"name"`
	Summary   string     `toml:"summary"`
	Requires  Requires   `toml:"requires"`
	Optional  []Optional `toml:"optional"`
	UserFiles []UserFile `toml:"user_files"`
	Signals   []Signal   `toml:"signals"`

	// Capabilities is [capabilities]: the side effects gather.star is allowed.
	Capabilities *Capabilities `toml:"capabilities"`

	Dir string `toml:"-"` // bundle directory, set by Load
}

// Load reads <dir>/routine.toml. A missing manifest is not an error: legacy
// bundles have none, and Load returns (nil, nil).
func Load(dir string) (*Manifest, error) {
	p := filepath.Join(dir, File)
	b, err := os.ReadFile(p)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var m Manifest
	if err := toml.Unmarshal(b, &m); err != nil {
		return nil, fmt.Errorf("%s: %w", p, err)
	}
	m.Dir = dir
	for _, s := range m.Signals {
		switch s.Expect {
		case "", ExpectNonempty, ExpectOK, ExpectJSONNonempty:
		default:
			return nil, fmt.Errorf("%s: signal %q: unknown expect %q (nonempty | ok | json-nonempty)", p, s.Name, s.Expect)
		}
	}
	return &m, nil
}

// Expect values.
const (
	ExpectNonempty     = "nonempty"
	ExpectOK           = "ok"
	ExpectJSONNonempty = "json-nonempty"
)

// Statuses a Result carries.
const (
	StatusOK       = "ok"
	StatusMissing  = "missing"
	StatusEnabled  = "enabled"
	StatusDisabled = "disabled"
	StatusFailed   = "failed"
)

// Result is one row of the check.
type Result struct {
	Item   string `json:"item"`   // "command msgvault", "setting accounts", "signal newsletter label"
	Status string `json:"status"` // ok | missing | enabled | disabled | failed
	OK     bool   `json:"ok"`
	Hard   bool   `json:"hard"` // a failing hard row means the routine must not be enabled
	Hint   string `json:"hint,omitempty"`
}

// Env is what the check probes; tests substitute it.
type Env struct {
	LookPath  func(string) (string, error)                      // nil → skip command checks
	HasSecret func(name string) bool                            // nil → assume present
	Stat      func(string) (os.FileInfo, error)                 // nil → os.Stat
	Run       func(name string, args ...string) (string, error) // signal runner; nil → skip signals
}

// DefaultEnv probes the real machine. hasSecret answers whether a secret of
// that name is stored.
func DefaultEnv(hasSecret func(string) bool) Env {
	return Env{
		LookPath:  exec.LookPath,
		HasSecret: hasSecret,
		Stat:      os.Stat,
		Run: func(name string, args ...string) (string, error) {
			ctx, cancel := context.WithTimeout(context.Background(), SignalTimeout)
			defer cancel()
			out, err := exec.CommandContext(ctx, name, args...).Output()
			return string(out), err
		},
	}
}

// Check evaluates the manifest against the machine and the routine's settings.
func (m *Manifest) Check(env Env, settings map[string]any) []Result {
	var out []Result
	add := func(r Result) { out = append(out, r) }

	for _, c := range m.Requires.Commands {
		add(cmdResult(env, c, true, "install "+c+" and put it on PATH"))
	}
	for _, s := range m.Requires.Secrets {
		ok := env.HasSecret == nil || env.HasSecret(s)
		add(Result{Item: "secret " + s, Status: status(ok, StatusOK, StatusMissing), OK: ok, Hard: true,
			Hint: hintIf(ok, "`qilla secret set "+s+"`")})
	}
	for _, k := range m.Requires.Settings {
		_, ok := settings[k]
		add(Result{Item: "setting " + k, Status: status(ok, StatusOK, StatusMissing), OK: ok, Hard: true,
			Hint: hintIf(ok, fmt.Sprintf("set %s in [routines.%s.settings]", k, m.Name))})
	}

	for _, o := range m.Optional {
		missing := o.missing(env, settings)
		if len(missing) == 0 {
			add(Result{Item: "optional " + o.Name, Status: StatusEnabled, OK: true})
			continue
		}
		hint := o.Hint
		if hint == "" {
			hint = "missing " + strings.Join(missing, ", ")
		} else {
			hint = fmt.Sprintf("%s (missing %s)", hint, strings.Join(missing, ", "))
		}
		add(Result{Item: "optional " + o.Name, Status: StatusDisabled, OK: true, Hint: hint})
	}

	for _, f := range m.UserFiles {
		ok := statFn(env)(filepath.Join(m.Dir, f.Path)) == nil
		r := Result{Item: "file " + f.Path, Status: status(ok, StatusOK, StatusMissing), OK: ok || !f.Required, Hard: f.Required}
		if !ok {
			r.Hint = f.Hint
			if !f.Required && r.Hint != "" {
				r.Hint = "optional: " + r.Hint
			}
		}
		add(r)
	}

	for _, s := range m.Signals {
		add(signalResult(env, s, settings))
	}

	for _, r := range capResults(m.Capabilities) {
		add(r)
	}
	return out
}

// capResults renders one row per declared capability, and a failing row when a
// declaration is malformed (the bundle must not run with a broken scope).
func capResults(c *Capabilities) []Result {
	if c == nil {
		return nil
	}
	var out []Result
	fail := func(item, hint string) {
		out = append(out, Result{Item: item, Status: StatusFailed, OK: false, Hard: true, Hint: hint})
	}
	if h := c.HTTP; h != nil {
		methods := h.MethodList()
		item := fmt.Sprintf("cap http  hosts=%s methods=%s", strings.Join(h.Hosts, ","), strings.Join(methods, ","))
		switch {
		case len(h.Hosts) == 0:
			fail("cap http", "capabilities.http.hosts is empty — list the hosts the gather may reach")
		default:
			bad := ""
			for _, mm := range methods {
				if !KnownMethods[mm] {
					bad = mm
					break
				}
			}
			if bad != "" {
				fail(item, fmt.Sprintf("method %q is not one of GET, POST, PUT, PATCH, DELETE", bad))
			} else {
				out = append(out, Result{Item: item, Status: StatusOK, OK: true})
			}
		}
	}
	if w := c.Write; w != nil {
		item := "cap write paths=" + strings.Join(w.Paths, ",")
		switch {
		case len(w.Paths) == 0:
			fail("cap write", "capabilities.write.paths is empty — list the vault-relative prefixes")
		default:
			bad := ""
			for _, p := range w.Paths {
				if filepath.IsAbs(p) || strings.Contains(p, "..") {
					bad = p
					break
				}
			}
			if bad != "" {
				fail(item, fmt.Sprintf("path %q must be vault-relative and free of ..", bad))
			} else {
				out = append(out, Result{Item: item, Status: StatusOK, OK: true})
			}
		}
	}
	if len(c.Exec) > 0 {
		out = append(out, Result{Item: "cap exec " + strings.Join(c.Exec, ","), Status: StatusOK, OK: true})
	}
	return out
}

// missing lists the pieces an optional feature lacks.
func (o Optional) missing(env Env, settings map[string]any) []string {
	var miss []string
	for _, c := range o.Commands {
		if env.LookPath != nil {
			if _, err := env.LookPath(c); err != nil {
				miss = append(miss, "command "+c)
			}
		}
	}
	for _, s := range o.Secrets {
		if env.HasSecret != nil && !env.HasSecret(s) {
			miss = append(miss, "secret "+s)
		}
	}
	for _, k := range o.Settings {
		if _, ok := settings[k]; !ok {
			miss = append(miss, "setting "+k)
		}
	}
	return miss
}

func signalResult(env Env, s Signal, settings map[string]any) Result {
	r := Result{Item: "signal " + s.Name, Hard: true}
	if env.Run == nil || len(s.Run) == 0 {
		r.Status, r.OK = StatusOK, true
		return r
	}
	args, err := Expand(s.Run, settings)
	if err != nil {
		r.Status, r.Hint = StatusFailed, err.Error()
		return r
	}
	out, err := env.Run(args[0], args[1:]...)
	ok := signalOK(s.Expect, out, err)
	r.OK = ok
	r.Status = status(ok, StatusOK, StatusFailed)
	if !ok {
		r.Hint = s.Hint
		if err != nil {
			if r.Hint == "" {
				r.Hint = err.Error()
			} else {
				r.Hint += " (" + err.Error() + ")"
			}
		}
	}
	return r
}

func signalOK(expect, out string, err error) bool {
	if err != nil {
		return false
	}
	switch expect {
	case ExpectOK:
		return true
	case ExpectJSONNonempty:
		var v any
		if json.Unmarshal([]byte(out), &v) != nil {
			return false
		}
		switch t := v.(type) {
		case []any:
			return len(t) > 0
		case map[string]any:
			return len(t) > 0
		default:
			return v != nil
		}
	default: // nonempty
		return strings.TrimSpace(out) != ""
	}
}

// Expand substitutes {{settings.key}} placeholders in a command line.
func Expand(args []string, settings map[string]any) ([]string, error) {
	out := make([]string, len(args))
	for i, a := range args {
		for {
			start := strings.Index(a, "{{")
			if start < 0 {
				break
			}
			end := strings.Index(a[start:], "}}")
			if end < 0 {
				break
			}
			end += start
			key := strings.TrimSpace(a[start+2 : end])
			if !strings.HasPrefix(key, "settings.") {
				return nil, fmt.Errorf("unknown placeholder {{%s}}", key)
			}
			k := strings.TrimPrefix(key, "settings.")
			v, ok := settings[k]
			if !ok {
				return nil, fmt.Errorf("setting %q referenced by the signal is not set", k)
			}
			a = a[:start] + scalar(v) + a[end+2:]
		}
		out[i] = a
	}
	return out, nil
}

func scalar(v any) string {
	switch t := v.(type) {
	case string:
		return t
	default:
		b, err := json.Marshal(v)
		if err != nil {
			return fmt.Sprint(v)
		}
		return strings.Trim(string(b), `"`)
	}
}

// Failed reports whether any hard requirement failed.
func Failed(rs []Result) bool {
	for _, r := range rs {
		if r.Hard && !r.OK {
			return true
		}
	}
	return false
}

// FirstFailure returns "<item>: <hint>" for the first failing hard row, "" if
// all pass — the one line doctor shows.
func FirstFailure(rs []Result) string {
	for _, r := range rs {
		if r.Hard && !r.OK {
			if r.Hint == "" {
				return r.Item + " " + r.Status
			}
			return fmt.Sprintf("%s %s — %s", r.Item, r.Status, r.Hint)
		}
	}
	return ""
}

func cmdResult(env Env, c string, hard bool, hint string) Result {
	ok := true
	if env.LookPath != nil {
		_, err := env.LookPath(c)
		ok = err == nil
	}
	return Result{Item: "command " + c, Status: status(ok, StatusOK, StatusMissing), OK: ok, Hard: hard,
		Hint: hintIf(ok, hint)}
}

func statFn(env Env) func(string) error {
	st := env.Stat
	if st == nil {
		st = os.Stat
	}
	return func(p string) error { _, err := st(p); return err }
}

func status(ok bool, yes, no string) string {
	if ok {
		return yes
	}
	return no
}

func hintIf(ok bool, hint string) string {
	if ok {
		return ""
	}
	return hint
}
