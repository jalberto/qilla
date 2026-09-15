package tasks

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

var agingNow = time.Date(2026, 9, 12, 10, 0, 0, 0, time.Local)

func agingVault(t *testing.T) string {
	t.Helper()
	return vault(t, map[string]string{
		"Desk/Todo.md": "> [!summary] stale text nobody maintained\n\n" +
			"- [ ] 2026-09-01 · Send the Acme benchmark table to [[People/Sam|Sam]] before the board call\n" +
			"- [ ] 2026-09-09 · Pay the gestoría invoice\n" +
			"- [ ] 2026-09-12 · Book the flights\n" +
			"- [ ] no date here, not a commitment\n" +
			"- [x] 2026-08-01 · Done, never aged\n",
		"Work/VL/Meetings/Kickoff.md": "- [ ] 2026-09-05 · Chase the SLA draft\n",
		"Desk/Questions.md":           "- [ ] 2026-01-01 · counted by the questions queue, not here\n",
		"Work/Archive/Old.md":         "- [ ] 2026-01-01 · archived, not live debt\n",
		".obsidian/plug.md":           "- [ ] 2026-01-01 · plugin noise\n",
	})
}

func TestAgingOrderAndScope(t *testing.T) {
	got, err := Aging(agingVault(t), agingNow)
	if err != nil {
		t.Fatal(err)
	}
	want := []struct {
		age  int
		file string
	}{
		{11, "Desk/Todo.md"},
		{7, "Work/VL/Meetings/Kickoff.md"},
		{3, "Desk/Todo.md"},
		{0, "Desk/Todo.md"},
	}
	if len(got) != len(want) {
		t.Fatalf("got %d commitments, want %d: %+v", len(got), len(want), got)
	}
	for i, w := range want {
		if got[i].Age != w.age || got[i].File != w.file {
			t.Fatalf("row %d = %dd %s, want %dd %s", i, got[i].Age, got[i].File, w.age, w.file)
		}
	}
	if f := got[0].Flag(3, 7); f != "🔴" {
		t.Fatalf("11d flag %q, want 🔴", f)
	}
	if f := got[2].Flag(3, 7); f != "🟠" {
		t.Fatalf("3d flag %q, want 🟠", f)
	}
	if f := got[3].Flag(3, 7); f != "·" {
		t.Fatalf("0d flag %q, want ·", f)
	}
	wantBrief := "🔴 **11d** — Send the Acme benchmark table to [[People/Sam|Sam]] before the board call ([[Desk/Todo]])"
	if b := got[0].Brief(3, 7); b != wantBrief {
		t.Fatalf("brief =\n%s\nwant\n%s", b, wantBrief)
	}
	if r := got[1].Row(3, 7); r != "🔴   7d  Kickoff                        Chase the SLA draft" {
		t.Fatalf("row = %q", r)
	}
}

func TestAgingTruncationDropsHalfWikilink(t *testing.T) {
	root := vault(t, map[string]string{
		"Desk/Todo.md": "- [ ] 2026-09-01 · " + strings.Repeat("x", 100) + " [[Some/Long/Note|label]]\n",
	})
	got, err := Aging(root, agingNow)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(got[0].What, "[[") {
		t.Fatalf("dangling wikilink left in %q", got[0].What)
	}
	if got[0].What != strings.Repeat("x", 100) {
		t.Fatalf("what = %q", got[0].What)
	}
}

func TestSummarizeRewritesCallout(t *testing.T) {
	root := agingVault(t)
	line, err := Summarize(root, "Desk/Todo.md", agingNow, 3, 7)
	if err != nil {
		t.Fatal(err)
	}
	want := `> [!summary] 4 open · <span class="k-pill red">11 d</span> Send the Acme benchmark table to [[Pe... · ` +
		`<span class="k-pill amber">3 d</span> Pay the gestoría invoice · rest on schedule`
	if line != want {
		t.Fatalf("summary =\n%s\nwant\n%s", line, want)
	}
	b, err := os.ReadFile(filepath.Join(root, "Desk/Todo.md"))
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(string(b), "\n")
	if lines[0] != want {
		t.Fatalf("file head = %q", lines[0])
	}
	if !strings.Contains(string(b), "- [ ] 2026-09-12 · Book the flights") {
		t.Fatal("rest of the file was not preserved")
	}
}

func TestSummarizeNeedsACallout(t *testing.T) {
	root := vault(t, map[string]string{"Desk/Todo.md": "# Todo\n\n- [ ] 2026-09-01 · a\n"})
	if _, err := Summarize(root, "Desk/Todo.md", agingNow, 3, 7); err == nil {
		t.Fatal("want an error when there is no summary callout")
	}
	if _, err := Summarize(root, "Desk/Missing.md", agingNow, 3, 7); err == nil {
		t.Fatal("want an error for a missing todo file")
	}
}

func TestSummaryLineWithoutCommitments(t *testing.T) {
	got := SummaryLine([]string{"- [ ] plain", "  - [ ] nested", "text"}, nil, 3, 7)
	if got != "> [!summary] 2 open" {
		t.Fatalf("got %q", got)
	}
}
