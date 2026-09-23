package engine

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jalberto/qilla/internal/config"
	"github.com/jalberto/qilla/internal/ledger"
	"github.com/jalberto/qilla/internal/queue"
)

func TestLocalReadsJobsRunsAndVault(t *testing.T) {
	dir := t.TempDir()
	vault := filepath.Join(dir, "vault")
	os.MkdirAll(vault, 0o755)
	for _, args := range [][]string{{"init", "-q"}, {"-c", "user.email=t@t", "-c", "user.name=t", "commit", "-q", "--allow-empty", "-m", "x"}} {
		if out, err := exec.Command("git", append([]string{"-C", vault}, args...)...).CombinedOutput(); err != nil {
			t.Skipf("git unavailable: %v %s", err, out)
		}
	}
	os.WriteFile(filepath.Join(vault, "a.md"), []byte("x"), 0o644)

	q, err := queue.Open(filepath.Join(dir, "q.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer q.Close()
	if _, err := ledger.New(q.DB(), nil); err != nil {
		t.Fatal(err)
	}
	now := time.Unix(1_800_000_000, 0)
	db := q.DB()
	db.Exec(`INSERT INTO jobs(routine,due,state,created,updated) VALUES('meetings',0,'running',0,?)`, now.Add(-time.Minute).Unix())
	db.Exec(`INSERT INTO jobs(routine,due,state,created,updated) VALUES('ghost',0,'running',0,?)`, now.Add(-48*time.Hour).Unix())
	db.Exec(`INSERT INTO runs(routine,ok,started,day,duration_ms) VALUES('brief',0,?,'d',1000)`, now.Add(-2*time.Hour).Unix())
	db.Exec(`INSERT INTO runs(routine,ok,started,day,duration_ms) VALUES('brief',1,?,'d',2500)`, now.Add(-time.Hour).Unix())

	st, err := Local(context.Background(), &config.Config{Vault: vault}, db, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Running) != 1 || st.Running[0].Routine != "meetings" {
		t.Fatalf("running = %+v (stale ghost must be dropped)", st.Running)
	}
	if l := st.Last["brief"]; !l.OK || l.DurationS != 2.5 || !l.At.Equal(now.Add(-time.Hour)) {
		t.Fatalf("last = %+v", l)
	}
	if st.Vault.Head == "" || st.Vault.Dirty != 1 || st.Vault.LastCommitAt == "" {
		t.Fatalf("vault = %+v", st.Vault)
	}
	b, _ := json.Marshal(st)
	var m map[string]any
	json.Unmarshal(b, &m)
	for _, k := range []string{"host", "role", "now", "running", "last", "vault"} {
		if _, ok := m[k]; !ok {
			t.Errorf("JSON missing %q: %s", k, b)
		}
	}
	v := m["vault"].(map[string]any)
	for _, k := range []string{"head", "dirty", "last_commit_at"} {
		if _, ok := v[k]; !ok {
			t.Errorf("vault JSON missing %q", k)
		}
	}
}

func TestVaultStateNotARepo(t *testing.T) {
	if v := VaultState(context.Background(), t.TempDir()); v != (Vault{}) {
		t.Fatalf("got %+v, want empty", v)
	}
}

func TestIsLocal(t *testing.T) {
	agent := map[string]string{"midori": "engine"} // "akane" is then an agent
	cases := []struct {
		host, self string
		roles      map[string]string
		want       bool
	}{
		{"", "akane", agent, true},
		{"midori", "akane", agent, false},
		{"akane", "akane", agent, true},
		{"ja@akane.ts.net", "akane", agent, true},
		{"midori", "akane", nil, true}, // empty roles table: this host is the engine
	}
	for _, c := range cases {
		cfg := &config.Config{Engine: config.Engine{Host: c.host}, Host: config.Host{Roles: c.roles}}
		// IsEngine resolves os.Hostname(); only assert role-independent cases
		// when the real hostname would make this host the engine.
		if cfg.IsEngine() && !c.want {
			continue
		}
		if got := IsLocal(cfg, c.self); got != c.want {
			t.Errorf("IsLocal(host=%q self=%q) = %v, want %v", c.host, c.self, got, c.want)
		}
	}
}

// fakeSSH puts an `ssh` script first on PATH.
func fakeSSH(t *testing.T, script string) {
	t.Helper()
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "ssh"), []byte("#!/bin/sh\n"+script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func TestRemoteDecodes(t *testing.T) {
	fakeSSH(t, `[ "$1" = "-o" ] && [ "$2" = "BatchMode=yes" ] || exit 9
echo '{"host":"midori","role":"engine","running":[{"routine":"meetings","since":"2026-09-23T10:00:00Z"}],"last":{"brief":{"at":"2026-09-23T08:30:00Z","ok":true,"duration_s":42}},"vault":{"head":"ab12cd","dirty":0,"last_commit_at":"x"}}'
`)
	st, err := Remote(context.Background(), "midori")
	if err != nil {
		t.Fatal(err)
	}
	if st.Host != "midori" || len(st.Running) != 1 || st.Last["brief"].DurationS != 42 || st.Vault.Head != "ab12cd" {
		t.Fatalf("got %+v", st)
	}
}

func TestRemoteUnreachable(t *testing.T) {
	fakeSSH(t, "echo 'ssh: connect to host midori port 22: No route to host' >&2\necho second >&2\nexit 255\n")
	_, err := Remote(context.Background(), "midori")
	var u *UnreachableError
	if !errors.As(err, &u) {
		t.Fatalf("want *UnreachableError, got %v", err)
	}
	if strings.Contains(err.Error(), "second") || !strings.Contains(err.Error(), "No route") {
		t.Fatalf("detail must be the first stderr line: %q", err)
	}
}

func TestRemoteBadJSON(t *testing.T) {
	fakeSSH(t, "echo not-json\n")
	var u *UnreachableError
	if _, err := Remote(context.Background(), "midori"); !errors.As(err, &u) {
		t.Fatalf("want *UnreachableError, got %v", err)
	}
}

func TestRemoteTimeout(t *testing.T) {
	fakeSSH(t, "exec sleep 5\n")
	old := sshTimeout
	sshTimeout = 200 * time.Millisecond
	defer func() { sshTimeout = old }()
	_, err := Remote(context.Background(), "midori")
	var u *UnreachableError
	if !errors.As(err, &u) || u.Detail != "timed out" {
		t.Fatalf("want timed out, got %v", err)
	}
}

func TestDecide(t *testing.T) {
	now := time.Date(2026, 9, 23, 12, 0, 0, 0, time.UTC)
	st := &Status{Host: "midori",
		Running: []Running{{Routine: "meetings", Since: now.Add(-10 * time.Minute)}},
		Last: map[string]Last{
			"brief":  {At: now.Add(-time.Hour), OK: true},
			"old":    {At: now.Add(-7 * time.Hour), OK: true},
			"failed": {At: now.Add(-time.Hour), OK: false},
			"ask":    {At: now.Add(-time.Hour), OK: true},
		}}
	unreach := &UnreachableError{Host: "midori", Detail: "timed out"}
	cases := []struct {
		name, routine string
		st            *Status
		err           error
		force, manual bool
		stop          bool
		msg, warn     string
	}{
		{"running", "meetings", st, nil, false, false, true, "meetings running on midori since 11:50", ""},
		{"recent", "brief", st, nil, false, false, true, "brief ran on midori at 11:00; --force to run anyway", ""},
		{"stale", "old", st, nil, false, false, false, "", ""},
		{"last failed", "failed", st, nil, false, false, false, "", ""},
		{"never ran", "nope", st, nil, false, false, false, "", ""},
		{"manual skips recent", "ask", st, nil, false, true, false, "", ""},
		{"unreachable", "brief", nil, unreach, false, false, false, "", "engine midori unreachable: timed out; running here"},
		{"force recent", "brief", st, nil, true, false, false, "", ""},
		{"force running", "meetings", st, nil, true, false, false, "", ""},
	}
	for _, c := range cases {
		d := Decide(c.routine, c.st, c.err, c.force, c.manual, now)
		if d.Stop != c.stop || d.Msg != c.msg || d.Warn != c.warn {
			t.Errorf("%s: got %+v", c.name, d)
		}
	}
}

func TestDecideOtherDay(t *testing.T) {
	now := time.Date(2026, 9, 23, 2, 0, 0, 0, time.UTC)
	st := &Status{Host: "midori", Last: map[string]Last{"brief": {At: now.Add(-3 * time.Hour), OK: true}}}
	if d := Decide("brief", st, nil, false, false, now); d.Msg != "brief ran on midori at 2026-09-22 23:00; --force to run anyway" {
		t.Fatalf("got %q", d.Msg)
	}
}

func TestParity(t *testing.T) {
	cases := []struct {
		eng, here Vault
		want      string
		ok        bool
	}{
		{Vault{Head: "ab12cd"}, Vault{Head: "ab12cd"}, "vault in sync (ab12cd)", true},
		{Vault{Head: "ab12cde"}, Vault{Head: "ab12cd"}, "vault in sync (ab12cde)", true},
		{Vault{Head: "ab12", Dirty: 3}, Vault{Head: "cd34"}, "vault out of sync: midori ab12 dirty 3 · here cd34 dirty 0", false},
		{Vault{Head: "ab12"}, Vault{Head: "ab12", Dirty: 1}, "vault out of sync: midori ab12 dirty 0 · here ab12 dirty 1", false},
		{Vault{}, Vault{}, "vault out of sync: midori ? dirty 0 · here ? dirty 0", false},
	}
	for _, c := range cases {
		got, ok := Parity("midori", c.eng, c.here)
		if got != c.want || ok != c.ok {
			t.Errorf("Parity(%+v,%+v) = %q,%v", c.eng, c.here, got, ok)
		}
	}
}
