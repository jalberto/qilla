package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jalberto/qilla/internal/config"
)

func logVault(t *testing.T, files map[string]string) *config.Config {
	t.Helper()
	v := t.TempDir()
	for rel, body := range files {
		p := filepath.Join(v, rel)
		os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return &config.Config{Vault: v, JournalDir: "Desk/Journal", DailyTemplate: "Qilla/Templates/Daily.md"}
}

func read(t *testing.T, cfg *config.Config, rel string) string {
	t.Helper()
	b, err := os.ReadFile(cfg.VaultPath(rel))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// The line lands at the end of the Log section, above the routine sub-sections,
// and nothing else in the note moves.
func TestLogAppendsInSection(t *testing.T) {
	note := "# 2026-09-12\n\n## 📥 Inbox\n- a jot\n\n## 📓 Log\n- 08:30 · brief ran\n\n### 🛍️ Market\n- routine output\n"
	cfg := logVault(t, map[string]string{"Desk/Journal/2026-09-12.md": note})
	if err := appendLog(cfg, "2026-09-12", "12:00", "notary called"); err != nil {
		t.Fatal(err)
	}
	got := read(t, cfg, "Desk/Journal/2026-09-12.md")
	want := "# 2026-09-12\n\n## 📥 Inbox\n- a jot\n\n## 📓 Log\n- 08:30 · brief ran\n- 12:00 · notary called\n\n### 🛍️ Market\n- routine output\n"
	if got != want {
		t.Fatalf("note rewritten:\n%q\nwant\n%q", got, want)
	}
}

func TestLogCreatesMissingSection(t *testing.T) {
	cfg := logVault(t, map[string]string{"Desk/Journal/2026-09-12.md": "# 2026-09-12\n\n## 📥 Inbox\n- a jot\n"})
	if err := appendLog(cfg, "2026-09-12", "12:00", "notary called"); err != nil {
		t.Fatal(err)
	}
	got := read(t, cfg, "Desk/Journal/2026-09-12.md")
	if !strings.HasSuffix(got, "## 📓 Log\n- 12:00 · notary called\n") || !strings.Contains(got, "- a jot") {
		t.Fatalf("section appended at the end, note kept:\n%q", got)
	}
}

func TestLogCreatesMissingNoteFromTemplate(t *testing.T) {
	cfg := logVault(t, map[string]string{"Qilla/Templates/Daily.md": "# {{date}}\n\n## 📥 Inbox\n\n## 📓 Log\n"})
	if err := appendLog(cfg, "2026-09-12", "12:00", "notary called"); err != nil {
		t.Fatal(err)
	}
	got := read(t, cfg, "Desk/Journal/2026-09-12.md")
	if !strings.Contains(got, "# 2026-09-12") || !strings.Contains(got, "- 12:00 · notary called") {
		t.Fatalf("note created from the template:\n%q", got)
	}
}
