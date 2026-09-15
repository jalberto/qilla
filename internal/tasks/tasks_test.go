package tasks

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func vault(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	for name, body := range files {
		p := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func TestNextIDSequencing(t *testing.T) {
	root := vault(t, map[string]string{
		"Desk/Todo.md":       "- [ ] a ^task-20260826-001\n- [x] b ^task-20260826-004\n",
		"Work/Meet.md":       "- [ ] c ^task-20260826-002\n",
		"Old.md":             "- [ ] d ^task-20250101-099\n",
		".obsidian/skip.md":  "- [ ] x ^task-20260826-900\n",
		".trash/deleted.md":  "- [ ] x ^task-20260826-901\n",
		"node_modules/n.md":  "- [ ] x ^task-20260826-902\n",
		"Life/notes.txt.bak": "^task-20260826-903",
	})
	got, err := NextID(root, "20260826")
	if err != nil {
		t.Fatal(err)
	}
	if got != "task-20260826-005" {
		t.Fatalf("NextID = %s, want task-20260826-005", got)
	}
	// Ids are free until written: a second call without a write repeats.
	again, _ := NextID(root, "20260826")
	if again != got {
		t.Fatalf("second NextID = %s, want the same id %s", again, got)
	}
	// A fresh day starts at 001.
	fresh, _ := NextID(root, "20260901")
	if fresh != "task-20260901-001" {
		t.Fatalf("fresh day = %s", fresh)
	}
}

func TestFindAcrossFiles(t *testing.T) {
	root := vault(t, map[string]string{
		"Desk/Todo.md": "header\n- [ ] chase ^task-20260826-001\n",
		"Work/Meet.md": "- [ ] chase ^task-20260826-001\nother\n- [ ] no ^task-20260826-0012\n",
		"Other.md":     "nothing here\n",
	})
	hits, err := Find(root, "task-20260826-001")
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 2 {
		t.Fatalf("hits = %+v", hits)
	}
	if hits[0].File != filepath.Join("Desk", "Todo.md") || hits[0].Line != 2 {
		t.Fatalf("first hit = %+v", hits[0])
	}
	if hits[1].File != filepath.Join("Work", "Meet.md") || hits[1].Line != 1 {
		t.Fatalf("second hit = %+v", hits[1])
	}
	// A leading ^ is accepted too.
	if h2, _ := Find(root, "^task-20260826-001"); len(h2) != 2 {
		t.Fatalf("caret-prefixed id = %+v", h2)
	}
}

func TestDoneTicksEveryCopyAndStamps(t *testing.T) {
	root := vault(t, map[string]string{
		"Desk/Todo.md":    "- [ ] chase ^task-20260826-001\n- [ ] keep ^task-20260826-002\n",
		"Life/Person.md":  "  - [ ] chase ^task-20260826-001\n",
		"Done/Already.md": "- [x] chase ✅2026-08-27 ^task-20260826-001\n",
		"Unrelated/Un.md": "- [ ] other\n",
		".git/hidden.md":  "- [ ] chase ^task-20260826-001\n",
	})
	if err := os.Chmod(filepath.Join(root, "Life/Person.md"), 0o600); err != nil {
		t.Fatal(err)
	}
	changed, err := Done(root, "task-20260826-001", time.Date(2026, 9, 12, 10, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if len(changed) != 3 {
		t.Fatalf("changed = %v", changed)
	}
	todo, _ := os.ReadFile(filepath.Join(root, "Desk/Todo.md"))
	want := "- [x] chase ✅2026-09-12 ^task-20260826-001\n- [ ] keep ^task-20260826-002\n"
	if string(todo) != want {
		t.Fatalf("Todo.md =\n%q\nwant\n%q", todo, want)
	}
	person, _ := os.ReadFile(filepath.Join(root, "Life/Person.md"))
	if string(person) != "  - [x] chase ✅2026-09-12 ^task-20260826-001\n" {
		t.Fatalf("Person.md = %q", person)
	}
	fi, _ := os.Stat(filepath.Join(root, "Life/Person.md"))
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode not preserved: %v", fi.Mode())
	}
	// An already-stamped line is left alone (no double stamp).
	already, _ := os.ReadFile(filepath.Join(root, "Done/Already.md"))
	if strings.Count(string(already), "✅") != 1 {
		t.Fatalf("double stamp: %q", already)
	}
	// Unknown id changes nothing.
	if c, _ := Done(root, "task-20260826-999", time.Now()); len(c) != 0 {
		t.Fatalf("unknown id changed %v", c)
	}
}

func TestOpenDedupes(t *testing.T) {
	root := vault(t, map[string]string{
		"Desk/Todo.md":   "- [ ] 2026-08-26 · chase ^task-20260826-002\n- [x] done ^task-20260826-003\n",
		"Life/Person.md": "- [ ] 2026-08-26 · chase ^task-20260826-002\n- [ ] plain task no id\n",
		"Work/A.md":      "- [ ] early ^task-20260825-001\n",
	})
	open, err := OpenTasks(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(open) != 2 {
		t.Fatalf("open = %+v", open)
	}
	if open[0].ID != "task-20260825-001" || open[1].ID != "task-20260826-002" {
		t.Fatalf("order = %+v", open)
	}
	if open[1].File != filepath.Join("Desk", "Todo.md") {
		t.Fatalf("first file = %s", open[1].File)
	}
	if open[1].Text != "2026-08-26 · chase" {
		t.Fatalf("text = %q", open[1].Text)
	}
}
