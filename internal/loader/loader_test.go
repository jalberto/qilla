package loader

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func vault(t *testing.T) string {
	t.Helper()
	v := t.TempDir()
	w := func(rel, body string) {
		p := filepath.Join(v, rel)
		os.MkdirAll(filepath.Dir(p), 0o755)
		os.WriteFile(p, []byte(body), 0o644)
	}
	w("Qilla/Persona.md", "# Persona\nYou are Qilla.\n")
	w("Qilla/Rules.md", "# Rules\nNever send.\n")
	w("Qilla/Facts/casa.md", "- 2026-09-01 · router at .1\n")
	w("Desk/Journal/2026-09-12.md", "## 📥 Inbox\n- today\n\n## 📖 Day\nnarrative prose\n\n## 📓 Log\n- 09:00 · today logged\n\n### 🛍️ Market\nroutine output\n")
	w("Desk/Journal/2026-09-11.md", "## 📓 Log\n- 09:00 · yesterday\n")
	w("Desk/Journal/2026-09-10.md", "## 📓 Log\n- 09:00 · two days ago\n")
	w("Life/Casa/Printer.md", "Brother at .60\n")
	return v
}

var day = time.Date(2026, 9, 12, 9, 0, 0, 0, time.UTC)

func newLoader(v string) Loader {
	return Loader{
		Vault: v, Persona: "Qilla/Persona.md", Rules: "Qilla/Rules.md", FactsDir: "Qilla/Facts",
		JournalDir: "Desk/Journal",
		RecallCmd:  "printf 'Life/Casa/Printer.md\\n'", // fake recall: prints one vault-relative path
		Now:        func() time.Time { return day },
	}
}

func TestLoaderOrder(t *testing.T) {
	l := newLoader(vault(t))
	p, err := l.Build([]string{"persona", "rules", "facts:casa", "journal:2d", "recall:5"}, "printer", "Do the brief.", "{\"mail\":3}")
	if err != nil {
		t.Fatal(err)
	}
	idx := func(s string) int { return strings.Index(p.Text, s) }
	order := []string{"You are Qilla", "Never send", "router at .1", "yesterday", "today", "Brother at .60", "Do the brief.", "\"mail\":3"}
	last := -1
	for _, s := range order {
		i := idx(s)
		if i < 0 || i < last {
			t.Fatalf("order broken at %q (idx %d, last %d)\n%s", s, i, last, p.Text)
		}
		last = i
	}
	if strings.Contains(p.Text, "two days ago") {
		t.Fatal("journal:2d must load exactly today and yesterday")
	}
}

func TestStablePrefix(t *testing.T) {
	l := newLoader(vault(t))
	a, _ := l.Build([]string{"persona", "rules"}, "", "prompt A", "input A")
	b, _ := l.Build([]string{"persona", "rules"}, "", "prompt B", "input B")
	if a.Stable != b.Stable || a.Stable == "" {
		t.Fatalf("stable prefix must be byte-identical:\n%q\n%q", a.Stable, b.Stable)
	}
	if !strings.HasPrefix(a.Text, a.Stable) {
		t.Fatal("prompt must start with the stable prefix")
	}
}

func TestRecallCmdMissingIsSoft(t *testing.T) {
	l := newLoader(vault(t))
	l.RecallCmd = "definitely-not-a-command-xyz {{query}} {{n}}"
	p, err := l.Build([]string{"persona", "recall:3"}, "x", "task", "")
	if err != nil {
		t.Fatalf("recall failure must not abort the run: %v", err)
	}
	if len(p.Missing) != 1 || !strings.HasPrefix(p.Missing[0], "recall:3 (") || !strings.Contains(p.Text, "task") {
		t.Fatalf("missing layer recorded with the reason, prompt intact: %+v", p)
	}
}

func TestLoaderSize(t *testing.T) {
	l := newLoader(vault(t))
	p, _ := l.Build([]string{"persona"}, "", "x", "y")
	if p.Chars != len(p.Text) || p.EstTokens <= 0 || p.EstTokens > p.Chars {
		t.Fatalf("size bookkeeping: %+v", p)
	}
}

func TestScopeValidate(t *testing.T) {
	for _, bad := range []string{"brain", "journal:x", "facts:", "recall:-1", "file:"} {
		if ValidateScope([]string{bad}) == nil {
			t.Errorf("%q must be rejected", bad)
		}
	}
	if err := ValidateScope([]string{"persona", "rules", "facts:casa", "journal:3d", "recall:8", "file:Desk/Todo.md"}); err != nil {
		t.Fatal(err)
	}
}

func TestFileLayerAndMissingFactsSoft(t *testing.T) {
	l := newLoader(vault(t))
	p, err := l.Build([]string{"file:Life/Casa/Printer.md", "facts:nonexistent"}, "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(p.Text, "Brother at .60") {
		t.Fatal("file layer missing")
	}
	if len(p.Missing) != 1 || p.Missing[0] != "facts:nonexistent" {
		t.Fatalf("missing layers must be reported, not fatal: %v", p.Missing)
	}
}

func TestScopeMemory(t *testing.T) {
	l := newLoader(vault(t))
	l.Memory = func(q string, n int) (string, error) {
		return "- [heuristic] brief costs 0.5 (" + q + "," + strconv.Itoa(n) + ")", nil
	}
	p, err := l.Build([]string{"persona", "memory:3"}, "brief", "Do the brief.", "")
	if err != nil {
		t.Fatal(err)
	}
	i, j, k := strings.Index(p.Text, "You are Qilla"), strings.Index(p.Text, "working memory"), strings.Index(p.Text, "Do the brief.")
	if !(i < j && j < k) || p.MemoryChars == 0 || !strings.Contains(p.Text, "(brief,3)") {
		t.Fatalf("memory block must sit after vault layers and before the task:\n%s", p.Text)
	}
	l.Memory = nil
	p, _ = l.Build([]string{"memory:3"}, "brief", "x", "")
	if strings.Contains(p.Text, "working memory") {
		t.Fatal("no hook → no block")
	}
}

func TestMemoryDownIsSoft(t *testing.T) {
	l := newLoader(vault(t))
	l.Memory = func(string, int) (string, error) { return "", fmt.Errorf("engram: dial unix: no such file") }
	p, err := l.Build([]string{"persona", "memory:3"}, "brief", "Do the brief.", "")
	if err != nil {
		t.Fatalf("memory failure must not abort the run: %v", err)
	}
	if len(p.Missing) != 1 || !strings.HasPrefix(p.Missing[0], "memory:3") || !strings.Contains(p.Text, "Do the brief.") {
		t.Fatalf("missing layer recorded, prompt intact: %+v", p)
	}
}
