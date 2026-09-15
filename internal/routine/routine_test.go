package routine

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jalberto/qilla/internal/config"
)

func cfg(t *testing.T) *config.Config {
	dir := t.TempDir()
	p := filepath.Join(dir, "qilla.toml")
	os.WriteFile(p, []byte("[agents.chief]\nmodel='claude-sonnet-5'\n"), 0o644)
	c, err := config.Load(p)
	if err != nil {
		t.Fatal(err)
	}
	c.Vault = filepath.Join(dir, "vault")
	return c
}

func TestScriptRoutineFilesAndToml(t *testing.T) {
	c := cfg(t)
	o := Options{Name: "calendar-today", Kind: "script", Schedule: "*-*-* 06:30", Window: "06:00-09:00", MustRun: true, Output: "Desk/Journal/{{date}}.md", Append: true}
	w, err := Write(c, o)
	if err != nil {
		t.Fatal(err)
	}
	if len(w) != 3 { // gather.sh, template.md, qilla.toml
		t.Fatalf("written: %v", w)
	}
	if _, err := os.Stat(c.VaultPath("Qilla/Routines/calendar-today/prompt.md")); err == nil {
		t.Fatal("script kind must not get prompt.md")
	}
	fi, _ := os.Stat(c.VaultPath("Qilla/Routines/calendar-today/gather.sh"))
	if fi.Mode()&0o100 == 0 {
		t.Fatal("gather.sh must be executable")
	}
	c2, err := config.Load(c.Path)
	if err != nil {
		t.Fatalf("appended TOML must load: %v", err)
	}
	r := c2.Routines["calendar-today"]
	if r.Kind != "script" || !r.MustRun || r.Window != "06:00-09:00" || r.Output != "Desk/Journal/{{date}}.md" || !r.Append {
		t.Fatalf("%+v", r)
	}
}

func TestAIRoutineNeedsAgentAndGetsPrompt(t *testing.T) {
	c := cfg(t)
	if err := Validate(c, Options{Name: "brief", Kind: "ai-fresh", Schedule: "daily"}); err == nil || !strings.Contains(err.Error(), "agent") {
		t.Fatalf("want agent error, got %v", err)
	}
	if _, err := Write(c, Options{Name: "brief", Kind: "ai-fresh", Schedule: "daily", Agent: "chief"}); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(c.VaultPath("Qilla/Routines/brief/prompt.md"))
	if !strings.Contains(string(b), "ONE JSON object") {
		t.Fatal("prompt scaffold")
	}
	if _, err := Write(c, Options{Name: "brief", Kind: "ai-fresh", Schedule: "daily", Agent: "chief"}); err == nil {
		t.Fatal("duplicate must be refused")
	}
}

func TestBadNames(t *testing.T) {
	c := cfg(t)
	for _, n := range []string{"Brief", "my routine", "-x", ""} {
		if Validate(c, Options{Name: n, Kind: "script", Schedule: "daily"}) == nil {
			t.Errorf("%q accepted", n)
		}
	}
}
