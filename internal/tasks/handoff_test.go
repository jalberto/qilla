package tasks

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestSendCreatesHeaderOnce(t *testing.T) {
	root := t.TempDir()
	now := time.Date(2026, 9, 23, 14, 5, 0, 0, time.UTC)

	line, err := Send(root, "midori", "akane", "research", "look into X", now)
	if err != nil {
		t.Fatal(err)
	}
	want := "- [ ] 2026-09-23 14:05 · from akane · routine:research · look into X"
	if line != want {
		t.Fatalf("line = %q, want %q", line, want)
	}

	if _, err := Send(root, "midori", "akane", "", "second one", now); err != nil {
		t.Fatal(err)
	}

	b, err := os.ReadFile(filepath.Join(root, "Qilla/Handoff/for-midori.md"))
	if err != nil {
		t.Fatal(err)
	}
	content := string(b)
	if strings.Count(content, "type: handoff") != 1 {
		t.Fatalf("header written more than once:\n%s", content)
	}
	if !strings.Contains(content, want) {
		t.Fatalf("missing first line:\n%s", content)
	}
	if !strings.Contains(content, "- [ ] 2026-09-23 14:05 · from akane · second one") {
		t.Fatalf("missing no-routine line:\n%s", content)
	}
}

func TestInboxParsesRoutineAndPlainLines(t *testing.T) {
	root := vault(t, map[string]string{
		"Qilla/Handoff/for-midori.md": `---
type: handoff
host: midori
tags: [qilla, handoff]
---
# Tasks for midori

- [ ] 2026-09-23 14:05 · from akane · routine:research · look into X
- [ ] 2026-09-23 15:00 · from akane · just a note
- [x] 2026-09-22 09:00 · from akane · already done → [[Some/Note]]
`,
	})
	jots, err := Inbox(root, "midori")
	if err != nil {
		t.Fatal(err)
	}
	if len(jots) != 2 {
		t.Fatalf("n=%d, want 2: %+v", len(jots), jots)
	}
	if jots[0].Routine != "research" || jots[0].Text != "look into X" || jots[0].From != "akane" {
		t.Fatalf("jots[0] = %+v", jots[0])
	}
	if jots[0].Line != 8 {
		t.Fatalf("jots[0].Line = %d, want 8", jots[0].Line)
	}
	if jots[1].Routine != "" || jots[1].Text != "just a note" {
		t.Fatalf("jots[1] = %+v", jots[1])
	}
}

func TestMarkDoneFlipsOnlyTargetLine(t *testing.T) {
	root := vault(t, map[string]string{
		"Qilla/Handoff/for-midori.md": `# Tasks for midori

- [ ] 2026-09-23 14:05 · from akane · routine:research · look into X
- [ ] 2026-09-23 15:00 · from akane · another one
`,
	})
	if err := MarkDone(root, "midori", 3, "[[Library/Resources/X]]"); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(root, "Qilla/Handoff/for-midori.md"))
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(string(b), "\n")
	if lines[2] != "- [x] 2026-09-23 14:05 · from akane · routine:research · look into X → [[Library/Resources/X]]" {
		t.Fatalf("line 3 = %q", lines[2])
	}
	if lines[3] != "- [ ] 2026-09-23 15:00 · from akane · another one" {
		t.Fatalf("line 4 should be untouched: %q", lines[3])
	}
}

func TestMarkDoneRejectsNonOpenLine(t *testing.T) {
	root := vault(t, map[string]string{
		"Qilla/Handoff/for-midori.md": "# Tasks for midori\n\n- [x] already done\n",
	})
	if err := MarkDone(root, "midori", 3, ""); err == nil {
		t.Fatal("expected error flipping an already-done line")
	}
}
