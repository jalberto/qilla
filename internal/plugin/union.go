package plugin

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Union folds every user plugin dir ([plugins].dirs) into qilla's own plugin
// directory, so Claude Code sees ONE plugin ("qilla") instead of one per dir:
// each user skills/<name> and agents/<name>.md becomes a symlink inside the
// plugin dir, and each dir's hooks/hooks.json is merged into the generated one.
// A real (non-symlink) entry always wins — the embedded skill is never shadowed,
// the clash is reported instead. Symlinks left over from a dir or skill that is
// gone are removed, so the result only ever describes the current config.
// Returns the warnings worth printing.
func Union(pluginDir string, dirs []string) ([]string, error) {
	var warn []string
	w1, err := linkTree(pluginDir, dirs, "skills", true)
	warn = append(warn, w1...)
	if err != nil {
		return warn, err
	}
	w2, err := linkTree(pluginDir, dirs, "agents", false)
	warn = append(warn, w2...)
	if err != nil {
		return warn, err
	}
	return warn, mergeHooks(pluginDir, dirs)
}

// linkTree symlinks every entry of <dir>/<sub> into <pluginDir>/<sub>.
// wantDir picks directories (skills/) or files (agents/*.md).
func linkTree(pluginDir string, dirs []string, sub string, wantDir bool) ([]string, error) {
	var warn []string
	dst := filepath.Join(pluginDir, sub)
	if err := os.MkdirAll(dst, 0o755); err != nil {
		return warn, err
	}
	want := map[string]string{} // name → target
	for _, d := range dirs {
		es, err := os.ReadDir(filepath.Join(d, sub))
		if err != nil {
			continue
		}
		for _, e := range es {
			if e.IsDir() != wantDir || strings.HasPrefix(e.Name(), ".") {
				continue
			}
			if !wantDir && !strings.HasSuffix(e.Name(), ".md") {
				continue
			}
			src := filepath.Join(d, sub, e.Name())
			if fi, err := os.Lstat(filepath.Join(dst, e.Name())); err == nil && fi.Mode()&os.ModeSymlink == 0 {
				warn = append(warn, fmt.Sprintf("%s/%s: %s already ships one — the vault copy at %s is ignored", sub, e.Name(), pluginDir, src))
				continue
			}
			if prev, ok := want[e.Name()]; ok {
				warn = append(warn, fmt.Sprintf("%s/%s: %s ignored — %s came first", sub, e.Name(), src, prev))
				continue
			}
			want[e.Name()] = src
		}
	}
	// drop every symlink that is not (or no longer) wanted, then (re)create the rest
	es, err := os.ReadDir(dst)
	if err != nil {
		return warn, err
	}
	for _, e := range es {
		p := filepath.Join(dst, e.Name())
		fi, err := os.Lstat(p)
		if err != nil || fi.Mode()&os.ModeSymlink == 0 {
			continue
		}
		if tgt, ok := want[e.Name()]; ok {
			if cur, err := os.Readlink(p); err == nil && cur == tgt {
				if _, err := os.Stat(p); err == nil {
					delete(want, e.Name()) // already correct and resolvable
					continue
				}
			}
		}
		if err := os.Remove(p); err != nil {
			return warn, err
		}
	}
	for name, tgt := range want {
		if err := os.Symlink(tgt, filepath.Join(dst, name)); err != nil {
			return warn, err
		}
	}
	return warn, nil
}

// hooksDoc is the shape of a hooks.json: event name → list of matcher groups.
type hooksDoc struct {
	Hooks map[string][]map[string]any `json:"hooks"`
}

// mergeHooks rewrites <pluginDir>/hooks/hooks.json as qilla's own hooks plus,
// per event, the groups of every user dir's hooks/hooks.json. Relative command
// paths are made absolute against the dir they came from: the scripts keep
// running from where they live, whatever the session's working directory is.
func mergeHooks(pluginDir string, dirs []string) error {
	doc := hooksDoc{Hooks: BaseHooks()}
	for _, d := range dirs {
		b, err := os.ReadFile(filepath.Join(d, "hooks", "hooks.json"))
		if err != nil {
			continue
		}
		var user hooksDoc
		if json.Unmarshal(b, &user) != nil {
			continue
		}
		for event, groups := range user.Hooks {
			for _, g := range groups {
				doc.Hooks[event] = append(doc.Hooks[event], absCommands(g, d))
			}
		}
	}
	out, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	p := filepath.Join(pluginDir, "hooks", "hooks.json")
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	return os.WriteFile(p, append(out, '\n'), 0o644)
}

// absCommands resolves relative command paths in one matcher group against base.
func absCommands(group map[string]any, base string) map[string]any {
	hs, _ := group["hooks"].([]any)
	for _, h := range hs {
		m, ok := h.(map[string]any)
		if !ok {
			continue
		}
		cmd, ok := m["command"].(string)
		if !ok {
			continue
		}
		f := strings.Fields(cmd)
		if len(f) == 0 || filepath.IsAbs(f[0]) || strings.HasPrefix(f[0], "$") || strings.HasPrefix(f[0], "~") {
			continue
		}
		if !strings.Contains(f[0], "/") {
			if _, err := os.Stat(filepath.Join(base, f[0])); err != nil {
				continue // a bare binary on PATH, not a script in the dir
			}
		}
		f[0] = filepath.Join(base, f[0])
		m["command"] = strings.Join(f, " ")
	}
	return group
}

// RefreshCache re-materializes the plugin into Claude Code's own install cache.
// `claude plugin install` copies the plugin directory rather than linking it,
// and that copy silently drops symlinked directories — exactly the vault skills
// Union just folded in. So the copy is redone here with links dereferenced.
// A no-op when Claude Code loads the plugin dir in place. Returns where it wrote.
func RefreshCache(pluginDir, claudePluginsDir string) ([]string, error) {
	b, err := os.ReadFile(filepath.Join(claudePluginsDir, "installed_plugins.json"))
	if err != nil {
		return nil, nil // not installed with Claude Code: nothing to refresh
	}
	var doc struct {
		Plugins map[string][]struct {
			InstallPath string `json:"installPath"`
		} `json:"plugins"`
	}
	if err := json.Unmarshal(b, &doc); err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	var done []string
	for _, e := range doc.Plugins["qilla@qilla"] {
		p := e.InstallPath
		if p == "" || p == pluginDir || seen[p] {
			continue
		}
		seen[p] = true
		if _, err := os.Stat(p); err != nil {
			continue // never installed / already gone
		}
		if err := copyDeref(pluginDir, p); err != nil {
			return done, err
		}
		done = append(done, p)
	}
	return done, nil
}

// copyDeref mirrors src onto dst, following symlinks and dropping what src no
// longer has, so the destination is an exact snapshot of the plugin tree.
func copyDeref(src, dst string) error {
	es, err := os.ReadDir(src)
	if err != nil {
		return err
	}
	keep := map[string]bool{}
	for _, e := range es {
		keep[e.Name()] = true
		s, d := filepath.Join(src, e.Name()), filepath.Join(dst, e.Name())
		fi, err := os.Stat(s) // Stat, not e.Type(): follow the symlink
		if err != nil {
			continue // broken link
		}
		if fi.IsDir() {
			if err := os.MkdirAll(d, 0o755); err != nil {
				return err
			}
			if err := copyDeref(s, d); err != nil {
				return err
			}
			continue
		}
		b, err := os.ReadFile(s)
		if err != nil {
			return err
		}
		if err := os.MkdirAll(dst, 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(d, b, 0o644); err != nil {
			return err
		}
	}
	old, err := os.ReadDir(dst)
	if err != nil {
		return nil
	}
	for _, e := range old {
		if !keep[e.Name()] {
			if err := os.RemoveAll(filepath.Join(dst, e.Name())); err != nil {
				return err
			}
		}
	}
	return nil
}
