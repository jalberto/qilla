package session

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jalberto/qilla/internal/queue"
)

const transcript = `{"type":"user","message":{"role":"user","content":"<system-reminder>ctx</system-reminder>\nfix the briefing"}}
{"type":"assistant","message":{"role":"assistant","content":[{"type":"thinking","thinking":"hmm"},{"type":"text","text":"fixed it"}]}}
{"type":"user","message":{"role":"user","content":[{"type":"tool_result","content":"x"}]}}
`

// store opens a Store on a temp DB with the live sessions table the worker owns.
func store(t *testing.T, now time.Time) *Store {
	t.Helper()
	q, err := queue.Open(filepath.Join(t.TempDir(), "q.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { q.Close() })
	if _, err := q.DB().Exec(`CREATE TABLE IF NOT EXISTS sessions(agent TEXT PRIMARY KEY, session_id TEXT NOT NULL, updated INTEGER NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	s, err := Open(q.DB())
	if err != nil {
		t.Fatal(err)
	}
	s.Now = func() time.Time { return now }
	return s
}

// seed writes a transcript for sid under a fake CLAUDE_CONFIG_DIR, aged to mtime.
func seed(t *testing.T, workdir, sid string, mtime time.Time) {
	t.Helper()
	root := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", root)
	p := TranscriptPath(workdir, sid)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(transcript), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(p, mtime, mtime); err != nil {
		t.Fatal(err)
	}
}

func TestIdleSessionRotates(t *testing.T) {
	now := time.Now()
	s := store(t, now)
	s.Set("chief", "old-session-id-1234")
	seed(t, "/vault", "old-session-id-1234", now.Add(-7*time.Hour-12*time.Minute))

	d := s.Attach("chief", "/vault", 6*time.Hour, func(string) string { return "recalled fact" })
	if !d.Rotated {
		t.Fatalf("want rotation, got %+v", d)
	}
	if d.Resume != "" {
		t.Errorf("rotation must not resume, got %q", d.Resume)
	}
	if d.New == "" || d.New == d.Old {
		t.Errorf("want a fresh id, got %q", d.New)
	}
	if got := s.Current("chief"); got != d.New {
		t.Errorf("live row = %q, want the new id %q", got, d.New)
	}
	for _, want := range []string{"fix the briefing", "fixed it", "recalled fact"} {
		if !strings.Contains(d.Handoff, want) {
			t.Errorf("handoff missing %q:\n%s", want, d.Handoff)
		}
	}
	if strings.Contains(d.Handoff, "system-reminder") || strings.Contains(d.Handoff, "hmm") {
		t.Errorf("handoff carried plumbing:\n%s", d.Handoff)
	}
	r := s.Rotated()
	if len(r) != 1 || r[0].SessionID != "old-session-id-1234" || r[0].Successor != d.New {
		t.Fatalf("retired row wrong: %+v", r)
	}
	if !strings.HasPrefix(d.Line(), "session rotated: old-sess → ") || !strings.Contains(d.Line(), "idle 7h12m") {
		t.Errorf("line = %q", d.Line())
	}
}

func TestFreshSessionResumes(t *testing.T) {
	now := time.Now()
	s := store(t, now)
	s.Set("chief", "warm-session")
	seed(t, "/vault", "warm-session", now.Add(-20*time.Minute))

	d := s.Attach("chief", "/vault", 6*time.Hour, nil)
	if d.Rotated || d.Resume != "warm-session" {
		t.Fatalf("want resume, got %+v", d)
	}
	if len(s.Rotated()) != 0 {
		t.Error("nothing should be retired")
	}
}

func TestZeroNeverRotates(t *testing.T) {
	now := time.Now()
	s := store(t, now)
	s.Set("chief", "ancient-session")
	seed(t, "/vault", "ancient-session", now.Add(-30*24*time.Hour))

	d := s.Attach("chief", "/vault", 0, nil)
	if d.Rotated || d.Resume != "ancient-session" {
		t.Fatalf("max_idle 0 must never rotate, got %+v", d)
	}
}

// With no transcript on disk the ledger stamp is the fallback clock.
func TestLedgerFallbackWhenNoTranscript(t *testing.T) {
	now := time.Now()
	s := store(t, now)
	s.Now = func() time.Time { return now.Add(-9 * time.Hour) }
	s.Set("chief", "no-transcript")
	s.Now = func() time.Time { return now }
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())

	d := s.Attach("chief", "/vault", 6*time.Hour, nil)
	if !d.Rotated {
		t.Fatalf("stale ledger stamp should rotate, got %+v", d)
	}
	if d.Handoff != "" {
		t.Errorf("no transcript, no handoff: %q", d.Handoff)
	}
}

func TestNoSessionYet(t *testing.T) {
	d := store(t, time.Now()).Attach("chief", "/vault", 6*time.Hour, nil)
	if d.Rotated || d.Resume != "" {
		t.Fatalf("want a plain fresh start, got %+v", d)
	}
}

// A rotation leaves a terse state entry behind, not just the prose handoff.
func TestRotationStateLines(t *testing.T) {
	now := time.Now()
	s := store(t, now)
	s.Set("chief", "old-session-id-1234")
	seed(t, "/vault", "old-session-id-1234", now.Add(-7*time.Hour))

	d := s.Attach("chief", "/vault", 6*time.Hour, nil)
	for _, want := range []string{"goal: fix the briefing", "last: ", "pending: ", "at: "} {
		if !strings.Contains(d.State, want) {
			t.Errorf("state missing %q:\n%s", want, d.State)
		}
	}
	if len(d.State) > 2000 {
		t.Errorf("state must stay under 2000 chars, got %d", len(d.State))
	}
	if HandoffKey("chief") != "chief/handoff" {
		t.Errorf("handoff key = %q", HandoffKey("chief"))
	}
}

func TestStateTextPending(t *testing.T) {
	at := time.Date(2026, 9, 12, 10, 0, 0, 0, time.UTC)
	got := StateText("ship it", "Done.\nWant me to push it too?", at)
	if !strings.Contains(got, "pending: Want me to push it too?") {
		t.Errorf("an open offer is pending:\n%s", got)
	}
	if !strings.Contains(got, "at: 2026-09-12T10:00:00Z") {
		t.Errorf("state stamps RFC3339:\n%s", got)
	}
	if got := StateText("ship it", "Done. Pushed.", at); !strings.Contains(got, "pending: -") {
		t.Errorf("a closed answer has nothing pending:\n%s", got)
	}
	if got := StateText("", "", at); got != "" {
		t.Errorf("nothing to carry, no state: %q", got)
	}
}
