package tasks

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

var sweepNow = time.Date(2026, 9, 30, 9, 0, 0, 0, time.Local)

func sweepVault(t *testing.T) string {
	t.Helper()
	return vault(t, map[string]string{
		"Desk/Todo.md": "# Todo\n\n" +
			"> [!summary] 2 open\n\n" +
			"## Security\n\n" +
			"- [x] 2026-08-23 · Rotate the secret ^task-20260823-001\n" +
			"  > 💬 JA: done\n" +
			"- [ ] 2026-09-01 · Still open, keep me\n\n" +
			"## Tech\n\n" +
			"- [x] Buy dongle · 2026-09-15 · [[Qilla/Facts/Tech]]\n" +
			"- [ ] 2026-09-06 · Pair fans\n",
	})
}

func TestSweepMovesDoneWithChildren(t *testing.T) {
	root := sweepVault(t)
	n, archiveRel, moved, err := Sweep(root, "Desk/Todo.md", sweepNow, false)
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 || len(moved) != 2 {
		t.Fatalf("n=%d, len(moved)=%d, want 2 and 2", n, len(moved))
	}
	if archiveRel != "Desk/Archive/Todo.md" {
		t.Fatalf("archiveRel=%q", archiveRel)
	}

	todo, err := os.ReadFile(filepath.Join(root, "Desk/Todo.md"))
	if err != nil {
		t.Fatal(err)
	}
	todoText := string(todo)
	if strings.Contains(todoText, "Rotate the secret") {
		t.Fatal("done item still in todo file")
	}
	if strings.Contains(todoText, "Buy dongle") {
		t.Fatal("done item still in todo file")
	}
	if !strings.Contains(todoText, "Still open, keep me") {
		t.Fatal("open item was removed")
	}
	if !strings.Contains(todoText, "## Security") || !strings.Contains(todoText, "## Tech") {
		t.Fatal("section headings should survive even when emptied")
	}

	archive, err := os.ReadFile(filepath.Join(root, "Desk/Archive/Todo.md"))
	if err != nil {
		t.Fatal(err)
	}
	archiveText := string(archive)
	if !strings.HasPrefix(archiveText, "---\ntype: note\ntags:\n  - desk\n  - archive\n---\n\n# Todo — archive\n") {
		t.Fatalf("archive header not seeded:\n%s", archiveText)
	}
	if !strings.Contains(archiveText, "## Swept 2026-09-30") {
		t.Fatal("missing today's Swept heading")
	}
	if !strings.Contains(archiveText, "  > 💬 JA: done") {
		t.Fatal("child line did not move with its parent")
	}
	// the block id must stay the LAST token on its line.
	wantLine := "- [x] 2026-08-23 · Rotate the secret · §Security ^task-20260823-001"
	if !strings.Contains(archiveText, wantLine) {
		t.Fatalf("archive missing %q, got:\n%s", wantLine, archiveText)
	}
	if !strings.Contains(archiveText, "- [x] Buy dongle · 2026-09-15 · [[Qilla/Facts/Tech]] · §Tech") {
		t.Fatal("section marker missing for the id-less item")
	}
	if moved[0] != wantLine {
		t.Fatalf("moved[0] = %q, want %q", moved[0], wantLine)
	}
}

func TestSweepUppercaseX(t *testing.T) {
	root := vault(t, map[string]string{
		"Desk/Todo.md": "## Tech\n\n- [X] 2026-09-01 · Uppercase done\n",
	})
	n, _, _, err := Sweep(root, "Desk/Todo.md", sweepNow, false)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("n=%d, want 1", n)
	}
	archive, err := os.ReadFile(filepath.Join(root, "Desk/Archive/Todo.md"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(archive), "Uppercase done") {
		t.Fatal("[X] item was not swept")
	}
}

func TestSweepBlankThenIndentedContinuation(t *testing.T) {
	root := vault(t, map[string]string{
		"Desk/Todo.md": "## Tech\n\n" +
			"- [x] 2026-09-01 · Parent with a blank before its child\n" +
			"\n" +
			"  > 💬 JA: still part of the block\n" +
			"- [ ] 2026-09-02 · Sibling, must stay\n",
	})
	n, _, _, err := Sweep(root, "Desk/Todo.md", sweepNow, false)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("n=%d, want 1", n)
	}
	todo, err := os.ReadFile(filepath.Join(root, "Desk/Todo.md"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(todo), "still part of the block") {
		t.Fatal("blank-then-indented child did not move with its parent")
	}
	if !strings.Contains(string(todo), "Sibling, must stay") {
		t.Fatal("sibling open item was removed")
	}
	archive, err := os.ReadFile(filepath.Join(root, "Desk/Archive/Todo.md"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(archive), "still part of the block") {
		t.Fatal("blank-then-indented child missing from archive")
	}
}

func TestSweepMidLineWikilinkIDSurvives(t *testing.T) {
	root := vault(t, map[string]string{
		"Desk/Todo.md": "## Tech\n\n" +
			"- [x] 2026-09-30 · See [[Desk/Todo#^task-20260901-001]] for context ^task-20260930-005\n",
	})
	n, _, moved, err := Sweep(root, "Desk/Todo.md", sweepNow, true)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("n=%d, want 1", n)
	}
	want := "- [x] 2026-09-30 · See [[Desk/Todo#^task-20260901-001]] for context · §Tech ^task-20260930-005"
	if moved[0] != want {
		t.Fatalf("moved[0] =\n%q\nwant\n%q", moved[0], want)
	}
}

func TestSweepOpenItemsUntouched(t *testing.T) {
	root := vault(t, map[string]string{
		"Desk/Todo.md": "## Tech\n\n- [ ] 2026-09-06 · Pair fans\n  > 💬 JA: still pending\n",
	})
	n, _, _, err := Sweep(root, "Desk/Todo.md", sweepNow, false)
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("n=%d, want 0", n)
	}
	if _, err := os.Stat(filepath.Join(root, "Desk/Archive/Todo.md")); !os.IsNotExist(err) {
		t.Fatal("archive should not be created when there is nothing to sweep")
	}
	b, err := os.ReadFile(filepath.Join(root, "Desk/Todo.md"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), "still pending") {
		t.Fatal("open item's children were touched")
	}
}

func TestSweepReusesTodaysHeading(t *testing.T) {
	root := sweepVault(t)
	if _, _, _, err := Sweep(root, "Desk/Todo.md", sweepNow, false); err != nil {
		t.Fatal(err)
	}
	// a second done item appears later the same day
	if err := os.WriteFile(filepath.Join(root, "Desk/Todo.md"),
		[]byte("## More\n\n- [x] 2026-09-30 · Second sweep item\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	n, _, _, err := Sweep(root, "Desk/Todo.md", sweepNow, false)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("n=%d, want 1", n)
	}
	archive, err := os.ReadFile(filepath.Join(root, "Desk/Archive/Todo.md"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(archive), "## Swept 2026-09-30") != 1 {
		t.Fatalf("heading should be reused once, got:\n%s", archive)
	}
	if !strings.Contains(string(archive), "Second sweep item") {
		t.Fatal("second sweep's item missing from archive")
	}
}

func TestSweepDryRunWritesNothing(t *testing.T) {
	root := sweepVault(t)
	before, err := os.ReadFile(filepath.Join(root, "Desk/Todo.md"))
	if err != nil {
		t.Fatal(err)
	}
	n, archiveRel, moved, err := Sweep(root, "Desk/Todo.md", sweepNow, true)
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 || len(moved) != 2 {
		t.Fatalf("n=%d, len(moved)=%d, want 2 and 2", n, len(moved))
	}
	if archiveRel != "Desk/Archive/Todo.md" {
		t.Fatalf("archiveRel=%q", archiveRel)
	}
	after, err := os.ReadFile(filepath.Join(root, "Desk/Todo.md"))
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Fatal("dry run modified the todo file")
	}
	if _, err := os.Stat(filepath.Join(root, "Desk/Archive/Todo.md")); !os.IsNotExist(err) {
		t.Fatal("dry run created the archive file")
	}
}
