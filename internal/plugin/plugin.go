// Package plugin materializes qilla's Claude Code plugin: hooks (guard,
// session-start), the skills and the sub-agent definitions, plus a one-plugin
// marketplace so `claude plugin marketplace add <dir>` works. Nothing here
// mentions any particular user; user space adds its own via config.
package plugin

import (
	"embed"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

//go:embed all:skills
var skills embed.FS

// Dir returns the plugin directory for a config dir.
func Dir(configDir string) string { return filepath.Join(configDir, "plugin") }

// Materialize writes/refreshes the plugin tree. agents: name → markdown body.
func Materialize(configDir string, agents map[string]string) (string, error) {
	dir := Dir(configDir)
	w := func(rel, body string) error {
		p := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			return err
		}
		return os.WriteFile(p, []byte(body), 0o644)
	}
	if err := w(".claude-plugin/plugin.json", `{
  "name": "qilla",
  "description": "qilla — the runtime that turns Claude Code + a markdown vault into an always-available right hand: routines, working memory, guard rails, research fan-out.",
  "version": "0.1.0",
  "author": { "name": "qilla" }
}
`); err != nil {
		return dir, err
	}
	if err := w(".claude-plugin/marketplace.json", `{
  "name": "qilla",
  "owner": { "name": "qilla" },
  "plugins": [ { "name": "qilla", "source": "./", "description": "qilla runtime plugin: hooks, skills, sub-agents" } ]
}
`); err != nil {
		return dir, err
	}
	hooks := map[string]any{"hooks": map[string]any{
		"SessionStart": []any{map[string]any{"hooks": []any{map[string]any{"type": "command", "command": "qilla hook session-start", "timeout": 15}}}},
		"PreToolUse": []any{
			map[string]any{"matcher": "Bash", "hooks": []any{map[string]any{"type": "command", "command": "qilla guard bash", "timeout": 10}}},
			map[string]any{"matcher": "Read", "hooks": []any{map[string]any{"type": "command", "command": "qilla guard read", "timeout": 10}}},
		},
		"Stop": []any{map[string]any{"hooks": []any{map[string]any{"type": "command", "command": "qilla hook stop", "timeout": 15}}}},
	}}
	hb, _ := json.MarshalIndent(hooks, "", "  ")
	if err := w("hooks/hooks.json", string(hb)+"\n"); err != nil {
		return dir, err
	}
	// skills shipped with qilla
	if err := copyFS(skills, "skills", filepath.Join(dir, "skills")); err != nil {
		return dir, err
	}
	for name, body := range agents {
		if err := w(filepath.Join("agents", name+".md"), body); err != nil {
			return dir, err
		}
	}
	return dir, nil
}

func copyFS(fsys embed.FS, from, to string) error {
	entries, err := fsys.ReadDir(from)
	if err != nil {
		return err
	}
	for _, e := range entries {
		src := from + "/" + e.Name()
		dst := filepath.Join(to, e.Name())
		if e.IsDir() {
			if err := copyFS(fsys, src, dst); err != nil {
				return err
			}
			continue
		}
		b, err := fsys.ReadFile(src)
		if err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(dst, b, 0o644); err != nil {
			return err
		}
	}
	return nil
}

// InstallCommands are what registers the plugin with Claude Code.
func InstallCommands(dir string) string {
	return strings.Join([]string{
		fmt.Sprintf("claude plugin marketplace add %s", dir),
		"claude plugin install qilla@qilla",
	}, "\n")
}
