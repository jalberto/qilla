package doctor

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jalberto/qilla/internal/config"
)

func fakeEnv(t *testing.T, present map[string]bool, loggedIn bool) (Env, *config.Config) {
	vault := t.TempDir()
	os.MkdirAll(filepath.Join(vault, "Qilla", "Routines", "brief"), 0o755)
	os.WriteFile(filepath.Join(vault, "Qilla", "Persona.md"), []byte("p"), 0o644)
	os.WriteFile(filepath.Join(vault, "Qilla", "Routines", "brief", "prompt.md"), []byte("x"), 0o644)
	os.WriteFile(filepath.Join(vault, "Qilla", "Routines", "brief", "template.md"), []byte("# {{ date }}"), 0o644)
	cfg := &config.Config{Path: filepath.Join(t.TempDir(), "qilla.toml"), Vault: vault, StateDir: t.TempDir(),
		Persona: "Qilla/Persona.md", Rules: "Qilla/Rules.md", Claude: "claude", RecallCmd: "qmd query {{query}}",
		Agents:   map[string]config.Agent{"chief": {}},
		Routines: map[string]config.Routine{"brief": {Kind: "ai-fresh", Agent: "chief", Scope: []string{"recall:5"}, Output: "x.md"}}}
	env := Env{
		LookPath: func(n string) (string, error) {
			if present[n] {
				return "/bin/" + n, nil
			}
			return "", errors.New("nope")
		},
		Stat: os.Stat, Home: t.TempDir(), ConfigDir: filepath.Dir(cfg.Path),
		Run: func(name string, args ...string) (string, error) {
			if name == "git" {
				return "alice\n", nil
			}
			if len(args) > 0 && args[0] == "auth" {
				if loggedIn {
					return `{"loggedIn": true}`, nil
				}
				return `{"loggedIn": false}`, nil
			}
			return "2.1.268 (Claude Code)", nil
		},
		UnitActive:    func(u string) bool { return u == "qilla.socket" },
		Validate:      func(string) error { return nil },
		SecurityScore: func(string) (float64, error) { return 3.2, nil },
		Calendar: func(spec string) error {
			if strings.Contains(spec, ",") && strings.Contains(spec, ":") && strings.Count(spec, ":") > 1 {
				return errors.New("Failed to parse calendar specification")
			}
			return nil
		},
	}
	return env, cfg
}

func find(cs []Check, name string) Check {
	for _, c := range cs {
		if c.Name == name {
			return c
		}
	}
	return Check{Name: "missing:" + name}
}

func TestHealthyMachine(t *testing.T) {
	env, cfg := fakeEnv(t, map[string]bool{"mise": true, "claude": true, "qmd": true, "knap": true, "bwrap": true, "socat": true}, true)
	cs := Run(cfg, nil, env)
	for _, n := range []string{"config", "vault", "persona", "claude", "claude login", "recall", "knap", "bwrap", "socat"} {
		if c := find(cs, n); !c.OK {
			t.Errorf("%s should pass: %+v", n, c)
		}
	}
	if find(cs, "rules").OK || find(cs, "rules").Hard {
		t.Error("missing rules is a soft failure")
	}
	if find(cs, "timers").OK {
		t.Error("brief timer inactive must be reported")
	}
	if HardFailure(cs) {
		t.Fatalf("no hard failures expected:\n%s", Format(cs))
	}
	if c := find(cs, "template brief"); !c.OK {
		t.Fatalf("template validated: %+v", c)
	}
}

func TestMissingTemplateIsHard(t *testing.T) {
	env, cfg := fakeEnv(t, map[string]bool{"mise": true, "claude": true, "knap": true}, true)
	os.Remove(filepath.Join(cfg.Vault, "Qilla", "Routines", "brief", "template.md"))
	cs := Run(cfg, nil, env)
	if c := find(cs, "template brief"); c.OK || !c.Hard {
		t.Fatalf("%+v", c)
	}
}

func TestMissingClaudeIsHard(t *testing.T) {
	env, cfg := fakeEnv(t, map[string]bool{"qmd": true}, false)
	cs := Run(cfg, nil, env)
	if c := find(cs, "claude"); c.OK || !c.Hard {
		t.Fatalf("%+v", c)
	}
	if c := find(cs, "knap"); c.OK || !c.Hard {
		t.Fatalf("knap hard when a routine has output: %+v", c)
	}
	if !HardFailure(cs) {
		t.Fatal("hard failure expected")
	}
}

func TestNotLoggedIn(t *testing.T) {
	env, cfg := fakeEnv(t, map[string]bool{"mise": true, "claude": true}, false)
	cs := Run(cfg, nil, env)
	if c := find(cs, "claude login"); c.OK || !strings.Contains(c.Info, "/login") {
		t.Fatalf("%+v", c)
	}
	if c := find(cs, "recall"); c.OK || !c.Hard {
		t.Fatalf("recall is hard when a scope uses recall: %+v", c)
	}
}

func TestConfigError(t *testing.T) {
	cs := Run(nil, errors.New("bad toml"), Env{})
	if len(cs) != 1 || cs[0].OK || !cs[0].Hard {
		t.Fatalf("%+v", cs)
	}
}

func TestSandboxRows(t *testing.T) {
	env, cfg := fakeEnv(t, map[string]bool{"mise": true, "claude": true, "knap": true, "bwrap": true, "socat": true}, true)
	cs := Run(cfg, nil, env)
	if c := find(cs, "sandbox"); !c.OK || !strings.Contains(c.Info, "3.2") {
		t.Fatalf("%+v", c)
	}
	if c := find(cs, "bwrap"); !c.OK {
		t.Fatalf("%+v", c)
	}
	env.SecurityScore = func(string) (float64, error) { return 7.9, nil }
	env.LookPath = func(n string) (string, error) {
		if n == "bwrap" {
			return "", errors.New("no")
		}
		return "/bin/" + n, nil
	}
	cs = Run(cfg, nil, env)
	if c := find(cs, "sandbox"); c.OK || c.Hard {
		t.Fatalf("high exposure is a soft failure: %+v", c)
	}
	if c := find(cs, "bwrap"); c.OK || !c.Hard {
		t.Fatalf("missing bwrap with sandbox on is hard: %+v", c)
	}
}

func TestBadScheduleIsHard(t *testing.T) {
	env, cfg := fakeEnv(t, map[string]bool{"mise": true, "claude": true, "knap": true, "bwrap": true, "socat": true}, true)
	r := cfg.Routines["brief"]
	r.Schedule = "*-*-* 08:30,13:00"
	cfg.Routines["brief"] = r
	cs := Run(cfg, nil, env)
	if c := find(cs, "schedule brief"); c.OK || !c.Hard {
		t.Fatalf("%+v", c)
	}
	r.Schedule = "*-*-* 08:30; *-*-* 13:00"
	cfg.Routines["brief"] = r
	if c := find(Run(cfg, nil, env), "schedule brief"); c.Name != "missing:schedule brief" {
		t.Fatalf("valid parts must not report: %+v", c)
	}
}

const miseDoctorOut = `settings:
  experimental true

1 warning found:

1. mise tool paths are not first in PATH. These paths take precedence:
     ~/Projects/misc/niri-ror
   This may cause system-installed tools to be used instead.

No problems found
`

func TestMiseDoctorWarningIsSoft(t *testing.T) {
	if n, first, _ := parseMiseWarnings(miseDoctorOut); n != 1 || first != "mise tool paths are not first in PATH. These paths take precedence: ~/Projects/misc/niri-ror" {
		t.Fatalf("n=%d first=%q", n, first)
	}
	if n, _, _ := parseMiseWarnings("No problems found\n"); n != 0 {
		t.Fatalf("clean mise doctor must report 0, got %d", n)
	}
	env, cfg := fakeEnv(t, map[string]bool{"mise": true, "claude": true}, true)
	env.MiseMissing = func(string) ([]string, error) { return nil, nil }
	env.MiseWarnings = func() (int, string, error) { return parseMiseWarnings(miseDoctorOut) }
	c := find(Run(cfg, nil, env), "mise")
	if c.OK || c.Hard || !strings.Contains(c.Info, "1 mise doctor warning:") {
		t.Fatalf("%+v", c)
	}
	env.MiseWarnings = func() (int, string, error) { return 0, "", nil }
	if c := find(Run(cfg, nil, env), "mise"); !c.OK || c.Info != "every pinned tool installed" {
		t.Fatalf("no warnings must keep the pinned-tools text: %+v", c)
	}
}

func TestParseMiseMissing(t *testing.T) {
	names, err := parseMiseMissing([]byte(`{}`))
	if err != nil || len(names) != 0 {
		t.Fatalf("empty payload: %v %v", names, err)
	}
	names, err = parseMiseMissing([]byte(`{"ubi:Gentleman-Programming/engram":[{"version":"1.2.3","installed":false,"source":{"type":"mise.toml","path":"/c/mise.toml"}}]}`))
	if err != nil || len(names) != 1 || names[0] != "ubi:Gentleman-Programming/engram" {
		t.Fatalf("one-tool payload: %v %v", names, err)
	}
	if _, err := parseMiseMissing([]byte("not json")); err == nil {
		t.Fatal("bad payload must error")
	}
}

func TestShimCandidates(t *testing.T) {
	shims := map[string]bool{"msgvault": true, "agent-browser": true, "hister": true, "node": true}
	for _, tc := range []struct {
		name    string
		allowed []string
		scripts []string
		want    []string
	}{
		{"bash command", []string{"Bash(msgvault list:*)"}, nil, []string{"msgvault"}},
		{"rtk wrapper", []string{"Bash(rtk hister search:*)"}, nil, []string{"hister"}},
		{"non-shim dropped", []string{"Bash(cat /etc/hosts)", "Read", "Bash(git status)"}, nil, nil},
		{"non-bash tool dropped", []string{"WebFetch(msgvault)"}, nil, nil},
		{"script words", nil, []string{"#!/bin/sh\nagent-browser open \"$URL\"\n"}, []string{"agent-browser"}},
		{"dedup and sort", []string{"Bash(node:*)", "Bash(msgvault:*)"}, []string{"node --version"}, []string{"msgvault", "node"}},
	} {
		got := shimCandidates(tc.allowed, tc.scripts, shims)
		if strings.Join(got, ",") != strings.Join(tc.want, ",") {
			t.Errorf("%s: got %v want %v", tc.name, got, tc.want)
		}
	}
}

func TestFormatShimHintAndRow(t *testing.T) {
	specs := map[string]string{"msgvault": "github:wesm/msgvault", "agent-browser": "npm:agent-browser"}
	got := formatShimHint([]string{"msgvault", "agent-browser", "hister"}, specs, "/c/mise.toml")
	want := `shims without a version inside the unit: msgvault, agent-browser, hister — pin them in /c/mise.toml (from mise ls --json: "github:wesm/msgvault" = "latest", "npm:agent-browser" = "latest", "hister" = "latest")`
	if got != want {
		t.Fatalf("got  %s\nwant %s", got, want)
	}
	if s := parseShimSpecs([]byte(`{"github:wesm/msgvault":[{"installed":true}],"npm:other":[{"installed":false}]}`), []string{"msgvault", "other"}); len(s) != 1 || s["msgvault"] != "github:wesm/msgvault" {
		t.Fatalf("specs: %v", s)
	}

	env, cfg := fakeEnv(t, map[string]bool{"mise": true, "claude": true}, true)
	cfg.Agents = map[string]config.Agent{"chief": {AllowedTools: []string{"Bash(msgvault:*)", "Bash(hister:*)"}}}
	env.MiseMissing = func(string) ([]string, error) { return nil, nil }
	env.MiseWarnings = func() (int, string, error) { return 0, "", nil }
	env.ShimNames = func() (map[string]bool, error) { return map[string]bool{"msgvault": true, "hister": true}, nil }
	env.ShimSpecs = func([]string) map[string]string { return specs }
	env.ShimMissing = func(string, []string) []string { return []string{"msgvault"} }
	if c := find(Run(cfg, nil, env), "mise shims"); c.OK || c.Hard || !strings.Contains(c.Info, `"github:wesm/msgvault" = "latest"`) {
		t.Fatalf("%+v", c)
	}
	env.ShimMissing = func(string, []string) []string { return nil }
	if c := find(Run(cfg, nil, env), "mise shims"); !c.OK || c.Info != "2 helper shims resolve inside the unit" {
		t.Fatalf("%+v", c)
	}
	// a shim with no version but a system binary behind it still works (jq, rg)
	env.ShimMissing = func(string, []string) []string { return []string{"msgvault", "hister"} }
	env.SystemLookPath = func(string) bool { return true }
	if c := find(Run(cfg, nil, env), "mise shims"); !c.OK {
		t.Fatalf("system fallback must not report: %+v", c)
	}
	env.SystemLookPath = func(n string) bool { return n == "hister" }
	if c := find(Run(cfg, nil, env), "mise shims"); c.OK || !strings.Contains(c.Info, "unit: msgvault —") {
		t.Fatalf("%+v", c)
	}
	if got := shimsThatBreak([]string{"a", "b"}, nil); len(got) != 2 {
		t.Fatalf("no probe means no filtering: %v", got)
	}
	env.ShimNames = nil
	if c := find(Run(cfg, nil, env), "mise shims"); c.Name != "missing:mise shims" {
		t.Fatalf("no shim dir must skip the row: %+v", c)
	}
}
