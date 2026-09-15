package render

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jalberto/qilla/internal/config"
	"github.com/jalberto/qilla/internal/worker"
)

func needKnap(t *testing.T) {
	if _, err := exec.LookPath("knap"); err != nil {
		t.Skip("knap not on PATH")
	}
}

func TestRenderAppendWithDate(t *testing.T) {
	needKnap(t)
	vault := t.TempDir()
	dir := filepath.Join(vault, worker.RoutinesDir, "cal")
	os.MkdirAll(dir, 0o755)
	os.WriteFile(filepath.Join(dir, "template.md"), []byte("## 📅 {{ date }}\n{% for e in gathered.events %}- {{ e.time }} · {{ e.title }}\n{% endfor %}"), 0o644)
	k := Knap{Vault: vault, Now: func() time.Time { return time.Date(2026, 9, 12, 8, 0, 0, 0, time.UTC) }}
	r := config.Routine{Kind: "script", Output: "Desk/Journal/{{date}}.md", Append: true}
	data := worker.Data{Routine: "cal", Date: "2026-09-12", Gathered: map[string]any{"events": []map[string]any{{"time": "10:00", "title": "Standup"}, {"time": "13:00", "title": "Lunch"}}}}
	out := filepath.Join(vault, "Desk", "Journal", "2026-09-12.md")
	os.MkdirAll(filepath.Dir(out), 0o755)
	os.WriteFile(out, []byte("# Day\n"), 0o644)
	if err := k.Render(context.Background(), "cal", r, data); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(out)
	s := string(b)
	if !strings.HasPrefix(s, "# Day\n") || !strings.Contains(s, "## 📅 2026-09-12") || !strings.Contains(s, "- 13:00 · Lunch") {
		t.Fatalf("rendered:\n%s", s)
	}
}

func TestRenderFailsOnBadTemplate(t *testing.T) {
	needKnap(t)
	vault := t.TempDir()
	dir := filepath.Join(vault, worker.RoutinesDir, "x")
	os.MkdirAll(dir, 0o755)
	os.WriteFile(filepath.Join(dir, "template.md"), []byte("{% if %}"), 0o644)
	k := Knap{Vault: vault}
	err := k.Render(context.Background(), "x", config.Routine{Output: "o.md"}, worker.Data{})
	if err == nil || !strings.Contains(err.Error(), "knap") {
		t.Fatalf("want knap diagnostics, got %v", err)
	}
	if err := Validate("", filepath.Join(dir, "template.md")); err == nil {
		t.Fatal("validate must fail")
	}
}

func TestNoOutputNoRender(t *testing.T) {
	k := Knap{Vault: t.TempDir(), Bin: "definitely-missing"}
	if err := k.Render(context.Background(), "x", config.Routine{}, worker.Data{}); err != nil {
		t.Fatal("routines without output render nothing, never touch knap")
	}
}
