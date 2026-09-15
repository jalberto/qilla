package catchup

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

var now = time.Date(2026, 9, 12, 9, 0, 0, 0, time.UTC)

func vaultDir(t *testing.T, files map[string]string) string {
	t.Helper()
	v := t.TempDir()
	for rel, body := range files {
		p := filepath.Join(v, rel)
		os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return v
}

const today = `---
type: daily
---

# 2026-09-12

## 📥 Inbox
- call the notary
- ordered the router → [[Home/Network]]

## ✅ Today
- ship the brief

## 📖 Day
a long narrative nobody needs twice

## 📓 Log
- 08:30 · brief ran
- 09:00 · market offer judged

### 🛍️ Market · 2026-09-12
- routine output, not a log line
`

const questionsNote = `# Questions

- [ ] 2026-09-01 · demo:brief · Keep the newsletter deletes? **Default taken: kept as-is.**
  > 💬 owner: yes, keep them
- [ ] 2026-09-02 · demo:retro · Install hunk? **Default taken: not installed.**
  > 💬 owner:
`

func TestScanDelta(t *testing.T) {
	v := vaultDir(t, map[string]string{
		"Desk/Journal/2026-09-12.md": today,
		"Desk/Questions.md":          questionsNote,
	})
	r := Scan(v, "Desk/Journal", "Desk/Questions.md", "💬 owner:", now, nil, false)
	if len(r.Jots) != 1 || r.Jots[0].Text != "call the notary" {
		t.Fatalf("routed jots must be skipped: %+v", r.Jots)
	}
	if len(r.Logs) != 2 {
		t.Fatalf("log lines stop at the routine sub-heading: %+v", r.Logs)
	}
	if len(r.Answers) != 1 || r.Answers[0].Reply != "yes, keep them" {
		t.Fatalf("answers: %+v", r.Answers)
	}
	if len(r.Opens) != 1 {
		t.Fatalf("opens: %+v", r.Opens)
	}
	if got := r.Opens[0]; got != "2026-09-02 · demo:retro · Install hunk?" {
		t.Fatalf("open stems drop the default text: %q", got)
	}
	if r.LogSeen["2026-09-12"] != 2 {
		t.Fatalf("log bookmark: %+v", r.LogSeen)
	}

	// second call with the bookmark: nothing new in the log
	r2 := Scan(v, "Desk/Journal", "Desk/Questions.md", "💬 owner:", now, r.LogSeen, false)
	if len(r2.Logs) != 0 {
		t.Fatalf("already-seen log lines must not repeat: %+v", r2.Logs)
	}
	// --all ignores the bookmark
	r3 := Scan(v, "Desk/Journal", "Desk/Questions.md", "💬 owner:", now, r.LogSeen, true)
	if len(r3.Logs) != 2 {
		t.Fatalf("--all must replay everything: %+v", r3.Logs)
	}
}

func TestQuietVault(t *testing.T) {
	v := vaultDir(t, map[string]string{"Desk/Journal/2026-09-12.md": "# 2026-09-12\n\n## 📥 Inbox\n\n## 📓 Log\n"})
	r := Scan(v, "Desk/Journal", "Desk/Questions.md", "💬 owner:", now, nil, false)
	if r.Total() != 0 {
		t.Fatalf("a quiet vault has nothing to report: %+v", r)
	}
}
