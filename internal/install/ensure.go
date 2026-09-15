package install

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/jalberto/qilla/internal/config"
)

// DefaultResearchPreset is written to <vault>/Qilla/Research/default.md when missing.
const DefaultResearchPreset = `---
type: config
tags: [qilla, research, preset]
---
# Research preset · default

- **Destination**: a note next to the topic it serves, frontmatter ` + "`type: research`, `date`, `status: researched`, `decision`, `tags`" + `.
- **Verdict**: one line — what is true, what is uncertain, what changes for you.
- **Sources**: every claim with url · date; volatile facts (prices, versions) marked.
- **Recall first**: search the vault for an existing note on the exact topic — extend, never duplicate.
`

// SubagentsReadme explains the user-space sub-agent folder.
const SubagentsReadme = `---
type: config
tags: [qilla, config]
---
# Sub-agents

Drop ` + "`<name>.md`" + ` files here to add or override sub-agents qilla passes into its sessions:

` + "```" + `
---
name: cook
description: plans meals from what is in the fridge
tools: Read, WebSearch
tier: research
---
You plan meals…
` + "```" + `

Built-ins: ` + "`researcher`" + ` (tier research) and ` + "`triager`" + ` (tier classify). A file with the same name replaces the built-in.
`

// Ensure writes every missing default for a loaded config: vault files
// (index, persona, rules, Qilla folders, research preset, sub-agents readme,
// plugin skeleton, per-routine scaffold, user hooks, project Claude settings),
// the runtime mise.toml, chat settings and the status line. Existing files are
// never touched. Returns what it wrote.
func Ensure(cfg *config.Config) []string {
	var wrote []string
	dir := filepath.Dir(cfg.Path)
	w := func(path, body string, mode os.FileMode) {
		if ok, err := WriteFile(path, body, false); err == nil && ok {
			os.Chmod(path, mode)
			wrote = append(wrote, path)
		}
	}
	w(filepath.Join(dir, "mise.toml"), fmt.Sprintf(MiseTomlTemplate, cfg.Memory.EngramVersion), 0o644)
	w(filepath.Join(dir, "statusline.sh"), StatusLine, 0o755)
	if cfg.Chat.Settings != "" {
		w(cfg.Chat.Settings, "{}\n", 0o644)
	}
	if fi, err := os.Stat(cfg.Vault); err == nil && fi.IsDir() {
		for _, d := range []string{"Qilla", "Qilla/Routines", "Qilla/Subagents", "Qilla/Research"} {
			os.MkdirAll(cfg.VaultPath(d), 0o755)
		}
		w(cfg.VaultPath("Qilla/Qilla.md"), DefaultIndex, 0o644)
		w(cfg.VaultPath(cfg.Persona), DefaultPersona, 0o644)
		w(cfg.VaultPath(cfg.Rules), DefaultRules, 0o644)
		w(cfg.VaultPath("Qilla/Research/default.md"), DefaultResearchPreset, 0o644)
		w(cfg.VaultPath("Qilla/Subagents/Subagents.md"), SubagentsReadme, 0o644)
		w(cfg.VaultPath(".claude/settings.json"), VaultSettings, 0o644)
		// every configured routine has the files its kind needs
		for name, r := range cfg.Routines {
			rd := cfg.VaultPath(filepath.Join("Qilla", "Routines", name))
			script := r.Kind == config.KindScript
			if script || r.Kind == config.KindFresh {
				// a bundle with a gather.star already has its gather: adding a
				// gather.sh would silently win over it.
				if _, err := os.Stat(filepath.Join(rd, "gather.star")); err != nil {
					w(filepath.Join(rd, "gather.sh"), RoutineGatherSh(name), 0o755)
				}
			}
			if !script {
				w(filepath.Join(rd, "prompt.md"), RoutinePromptMd(name), 0o644)
			}
			if r.Output != "" && r.Template == "" {
				w(filepath.Join(rd, "template.md"), RoutineTemplateMd(name, script), 0o644)
			}
		}
		// user plugin dirs: a loadable skeleton
		for _, d := range cfg.Plugins.Dirs {
			d = config.Expand(d)
			if !filepath.IsAbs(d) {
				d = cfg.VaultPath(d)
			}
			os.MkdirAll(filepath.Join(d, "skills"), 0o755)
			os.MkdirAll(filepath.Join(d, "hooks"), 0o755)
			w(filepath.Join(d, ".claude-plugin", "plugin.json"), PluginManifest, 0o644)
			w(filepath.Join(d, "hooks", "hooks.json"), PluginHooks, 0o644)
		}
	}
	// user hooks: configured but missing → no-op script, never a broken session
	for _, h := range []string{cfg.Hooks.SessionStart, cfg.Hooks.Stop} {
		if f := strings.Fields(h); len(f) > 0 && filepath.IsAbs(f[0]) {
			w(f[0], NoopHook, 0o755)
		}
	}
	// statusline may be stale from an older version: refresh when it differs from the shipped one
	if b, err := os.ReadFile(filepath.Join(dir, "statusline.sh")); err == nil && string(b) != StatusLine {
		os.WriteFile(filepath.Join(dir, "statusline.sh"), []byte(StatusLine), 0o755)
		wrote = append(wrote, filepath.Join(dir, "statusline.sh")+" (updated)")
	}
	return wrote
}
