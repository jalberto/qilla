package install

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/jalberto/qilla/internal/config"
)

func TestEnsureWritesMissingNeverOverwrites(t *testing.T) {
	root := t.TempDir()
	cfg := &config.Config{Path: filepath.Join(root, "cfg", "qilla.toml"), Vault: filepath.Join(root, "vault"), Persona: "Qilla/Persona.md", Rules: "Qilla/Rules.md",
		Routines: map[string]config.Routine{"r1": {Kind: "script", Output: "x.md"}, "r2": {Kind: "ai-resumed"}}, Plugins: config.Plugins{Dirs: []string{"Qilla/Plugin"}},
		Hooks: config.Hooks{Stop: filepath.Join(root, "hooks", "stop.sh")}}
	os.MkdirAll(filepath.Dir(cfg.Path), 0o755)
	os.MkdirAll(cfg.Vault, 0o755)
	os.MkdirAll(cfg.VaultPath("Qilla"), 0o755)
	os.WriteFile(cfg.VaultPath("Qilla/Persona.md"), []byte("MY PERSONA"), 0o644)
	wrote := Ensure(cfg)
	if len(wrote) < 4 {
		t.Fatalf("expected several defaults, wrote %v", wrote)
	}
	if b, _ := os.ReadFile(cfg.VaultPath("Qilla/Persona.md")); string(b) != "MY PERSONA" {
		t.Fatal("existing persona must not be overwritten")
	}
	for _, rel := range []string{"Qilla/Qilla.md", "Qilla/Rules.md", "Qilla/Research/default.md", "Qilla/Subagents/Subagents.md", ".claude/settings.json", "Qilla/Routines/r1/gather.sh", "Qilla/Routines/r1/template.md", "Qilla/Routines/r2/prompt.md", "Qilla/Plugin/hooks/hooks.json"} {
		if _, err := os.Stat(cfg.VaultPath(rel)); err != nil {
			t.Fatalf("missing %s", rel)
		}
	}
	if fi, err := os.Stat(filepath.Join(root, "hooks", "stop.sh")); err != nil || fi.Mode()&0o100 == 0 {
		t.Fatal("missing hook must be generated executable")
	}
	if _, err := os.Stat(filepath.Join(root, "cfg", "statusline.sh")); err != nil {
		t.Fatal("statusline")
	}
	if _, err := os.Stat(cfg.VaultPath("Qilla/Plugin/.claude-plugin/plugin.json")); err == nil {
		t.Fatal("user plugin dirs must not get a manifest any more")
	}
	if again := Ensure(cfg); len(again) != 0 {
		t.Fatalf("second run must be a no-op: %v", again)
	}
}
