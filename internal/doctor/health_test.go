package doctor

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jalberto/qilla/internal/config"
)

func at(s string) time.Time {
	t, err := time.ParseInLocation("2006-01-02 15:04", s, time.Local)
	if err != nil {
		panic(err)
	}
	return t
}

func TestParseFreshRule(t *testing.T) {
	for _, tc := range []struct {
		entry           string
		label, path, by string
		remote          string
		max             time.Duration
	}{
		{entry: "Desk/Briefings/Today.md=daily@09:30", label: "Desk/Briefings/Today.md", path: "Desk/Briefings/Today.md", by: "09:30"},
		{entry: "git@nas=7d", label: "git@nas", remote: "nas", max: 7 * 24 * time.Hour},
		{entry: "Desk/Todo.md=6h", label: "Desk/Todo.md", path: "Desk/Todo.md", max: 6 * time.Hour},
		{entry: "Desk/Todo.md=2d", label: "Desk/Todo.md", path: "Desk/Todo.md", max: 48 * time.Hour},
	} {
		r, err := parseFreshRule(tc.entry)
		if err != nil {
			t.Fatalf("%s: %v", tc.entry, err)
		}
		if r.Label != tc.label || r.Path != tc.path || r.By != tc.by || r.Remote != tc.remote || r.Max != tc.max {
			t.Errorf("%s: %+v", tc.entry, r)
		}
	}
	for _, bad := range []string{"nospec", "p=", "=7d", "p=daily@25:99", "p=7x", "p=0d", "git@=7d"} {
		if _, err := parseFreshRule(bad); err == nil {
			t.Errorf("%q must not parse", bad)
		}
	}
}

// healthEnv is a machine with nothing but the health probes wired.
func healthEnv(t *testing.T, now time.Time) (Env, *config.Config) {
	cfg := &config.Config{Vault: t.TempDir()}
	return Env{Stat: os.Stat, Now: func() time.Time { return now }}, cfg
}

func touch(t *testing.T, cfg *config.Config, rel string, mt time.Time) {
	p := cfg.VaultPath(rel)
	os.MkdirAll(filepath.Dir(p), 0o755)
	if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(p, mt, mt); err != nil {
		t.Fatal(err)
	}
}

func TestFreshnessRules(t *testing.T) {
	now := at("2026-09-12 10:00")
	env, cfg := healthEnv(t, now)
	touch(t, cfg, "Desk/Briefings/Today.md", at("2026-09-11 09:31"))
	touch(t, cfg, "Desk/Todo.md", at("2026-09-12 07:00"))
	cfg.Health.Freshness = []string{
		"Desk/Briefings/Today.md=daily@09:30",
		"Desk/Todo.md=6h",
		"Desk/Missing.md=1d",
	}
	cs := Health(cfg, env)
	if c := find(cs, "fresh Desk/Briefings/Today.md"); c.OK || c.Hard || !strings.Contains(c.Info, "expected today by 09:30") {
		t.Errorf("stale daily file: %+v", c)
	}
	if c := find(cs, "fresh Desk/Todo.md"); !c.OK {
		t.Errorf("3h old with a 6h rule is fresh: %+v", c)
	}
	if c := find(cs, "fresh Desk/Missing.md"); c.OK || c.Info != "missing" {
		t.Errorf("%+v", c)
	}

	// before the daily deadline the same untouched file is not yet stale
	env.Now = func() time.Time { return at("2026-09-12 08:00") }
	if c := find(Health(cfg, env), "fresh Desk/Briefings/Today.md"); !c.OK {
		t.Errorf("not due yet: %+v", c)
	}
	// and the 6h rule trips once the file is old enough
	env.Now = func() time.Time { return at("2026-09-12 14:00") }
	if c := find(Health(cfg, env), "fresh Desk/Todo.md"); c.OK || !strings.Contains(c.Info, "older than 6h") {
		t.Errorf("%+v", c)
	}
}

func TestFreshnessGit(t *testing.T) {
	now := at("2026-09-12 10:00")
	env, cfg := healthEnv(t, now)
	cfg.Health.Freshness = []string{"git@nas=7d"}
	log := func(ts ...time.Time) func(string, ...string) (string, error) {
		out := ""
		for _, t := range ts {
			out += strconv.FormatInt(t.Unix(), 10) + "\n"
		}
		return func(string, ...string) (string, error) { return out, nil }
	}
	env.Run = log(now.Add(-time.Hour), now.Add(-9*24*time.Hour)) // git log is newest first
	if c := find(Health(cfg, env), "fresh git@nas"); c.OK || c.Hard || !strings.Contains(c.Info, "2 commits unpushed, oldest 9d old (max 7d)") {
		t.Fatalf("%+v", c)
	}
	env.Run = log(now.Add(-2 * 24 * time.Hour))
	if c := find(Health(cfg, env), "fresh git@nas"); !c.OK {
		t.Fatalf("2d old is within 7d: %+v", c)
	}
	env.Run = log()
	if c := find(Health(cfg, env), "fresh git@nas"); !c.OK || c.Info != "nothing unpushed" {
		t.Fatalf("%+v", c)
	}
	env.Run = func(string, ...string) (string, error) { return "", errors.New("no such remote") }
	if c := find(Health(cfg, env), "fresh git@nas"); !c.OK || !strings.Contains(c.Info, "not probed") {
		t.Fatalf("an unprobeable remote must not raise an alarm: %+v", c)
	}
}

func TestUnitRows(t *testing.T) {
	env, cfg := healthEnv(t, at("2026-09-12 10:00"))
	cfg.Health.WatchedServices = []string{"ok.service", "bad.service", "gone.timer", "idle.service"}
	env.UnitState = func(u string) string {
		switch u {
		case "bad.service":
			return unitFailed
		case "gone.timer":
			return unitNotFound
		case "idle.service":
			return unitInactive
		}
		return unitActive
	}
	cs := Health(cfg, env)
	if c := find(cs, "unit ok.service"); !c.OK || !c.Hard {
		t.Errorf("%+v", c)
	}
	if c := find(cs, "unit bad.service"); c.OK || !c.Hard {
		t.Errorf("%+v", c)
	}
	if c := find(cs, "unit gone.timer"); c.OK || !c.Hard || !strings.Contains(c.Info, "cannot restart") {
		t.Errorf("%+v", c)
	}
	if c := find(cs, "unit idle.service"); !c.OK {
		t.Errorf("an idle oneshot is not a failure: %+v", c)
	}
	if !HardFailure(cs) {
		t.Error("a failed watched unit is a hard failure")
	}
}

func TestJobsRow(t *testing.T) {
	env, cfg := healthEnv(t, at("2026-09-12 10:00"))
	env.Jobs = func(*config.Config) ([]JobProblem, error) {
		return []JobProblem{{3, "brief", "failed", "boom", false}, {7, "learn", "queued", "queued", true}}, nil
	}
	c := find(Health(cfg, env), "jobs")
	if c.OK || c.Hard || !strings.Contains(c.Info, "1 failed: #3 brief — boom") || !strings.Contains(c.Info, "1 parked: #7 learn") {
		t.Fatalf("%+v", c)
	}
	env.Jobs = func(*config.Config) ([]JobProblem, error) { return nil, nil }
	if c := find(Health(cfg, env), "jobs"); !c.OK {
		t.Fatalf("%+v", c)
	}
	env.Jobs = func(*config.Config) ([]JobProblem, error) { return nil, errors.New("db locked") }
	if c := find(Health(cfg, env), "jobs"); c.OK || c.Hard || !strings.Contains(c.Info, "db locked") {
		t.Fatalf("%+v", c)
	}
}

func TestFixRestartsFailedUnits(t *testing.T) {
	env, cfg := healthEnv(t, at("2026-09-12 10:00"))
	cfg.Health.WatchedServices = []string{"ok.service", "flaky.service", "dead.service", "gone.timer"}
	state := map[string]string{"ok.service": unitActive, "flaky.service": unitFailed, "dead.service": unitFailed, "gone.timer": unitNotFound}
	var restarted []string
	env.UnitState = func(u string) string { return state[u] }
	env.Restart = func(u string) error {
		restarted = append(restarted, u)
		if u == "flaky.service" {
			state[u] = unitActive
		}
		return nil
	}
	env.Run = func(name string, args ...string) (string, error) { return "1\n", nil }
	lines, ok := Fix(cfg, env)
	if ok {
		t.Error("dead.service still failed: Fix must report not-ok")
	}
	joined := strings.Join(lines, "\n")
	for _, want := range []string{"flaky.service: recovered by restart", "dead.service: still failed after restart — exit 1", "gone.timer: not-found — unit missing, cannot restart"} {
		if !strings.Contains(joined, want) {
			t.Errorf("missing %q in:\n%s", want, joined)
		}
	}
	if len(restarted) != 2 {
		t.Errorf("only failed units are restarted: %v", restarted)
	}

	for u := range state {
		state[u] = unitActive
	}
	if lines, ok := Fix(cfg, env); !ok || len(lines) != 1 || lines[0] != "all green" {
		t.Errorf("%v %v", lines, ok)
	}
}

func TestHealthRowsAreOptional(t *testing.T) {
	// a config without [health] adds no rows, so --json stays as it was
	env, cfg := fakeEnv(t, map[string]bool{"mise": true, "claude": true, "knap": true, "bwrap": true, "socat": true}, true)
	for _, c := range Run(cfg, nil, env) {
		if strings.HasPrefix(c.Name, "unit ") || strings.HasPrefix(c.Name, "fresh ") || c.Name == "jobs" {
			t.Fatalf("unexpected health row: %+v", c)
		}
	}
}
