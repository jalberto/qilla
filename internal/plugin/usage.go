package plugin

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// ClaudeJSONPath is Claude Code's own config, which keeps the skill counters.
// Tests point it elsewhere.
func ClaudeJSONPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".claude.json")
}

// SkillUse is one skill of one plugin with its usage counters.
type SkillUse struct {
	Plugin string `json:"plugin"`
	Skill  string `json:"skill"`
	Name   string `json:"name"` // "<plugin>:<skill>", how Claude names it
	Uses   int    `json:"uses"`
	Last   string `json:"last"` // YYYY-MM-DD, or "never"
}

// usageEntry is one ~/.claude.json .skillUsage value.
type usageEntry struct {
	UsageCount int   `json:"usageCount"`
	LastUsedAt int64 `json:"lastUsedAt"`
}

// PluginName is the plugin's declared name (.claude-plugin/plugin.json), or the
// lowercased basename of its directory.
func PluginName(dir string) string {
	var meta struct {
		Name string `json:"name"`
	}
	if b, err := os.ReadFile(filepath.Join(dir, ".claude-plugin", "plugin.json")); err == nil {
		if json.Unmarshal(b, &meta) == nil && strings.TrimSpace(meta.Name) != "" {
			return strings.TrimSpace(meta.Name)
		}
	}
	return strings.ToLower(filepath.Base(dir))
}

// readSkillUsage reads the .skillUsage map, tolerating a missing or broken file.
func readSkillUsage(claudeJSON string) map[string]usageEntry {
	var doc struct {
		SkillUsage map[string]usageEntry `json:"skillUsage"`
	}
	b, err := os.ReadFile(claudeJSON)
	if err != nil || json.Unmarshal(b, &doc) != nil {
		return map[string]usageEntry{}
	}
	if doc.SkillUsage == nil {
		return map[string]usageEntry{}
	}
	return doc.SkillUsage
}

// Usage lists the skills under every plugin dir with their use count and last
// use. A skill is recorded under either "<plugin>:<skill>" or the bare
// "<skill>", so both keys are summed before anything is called unused.
// Result order: most used first, then never-used, alphabetical within each.
func Usage(dirs []string, claudeJSON string) ([]SkillUse, error) {
	u := readSkillUsage(claudeJSON)
	var out []SkillUse
	for _, dir := range dirs {
		name := PluginName(dir)
		es, err := os.ReadDir(filepath.Join(dir, "skills"))
		if err != nil {
			continue
		}
		for _, e := range es {
			// Stat, not e.IsDir(): a vault skill folded into the plugin is a symlink.
			if fi, err := os.Stat(filepath.Join(dir, "skills", e.Name())); err != nil || !fi.IsDir() {
				continue
			}
			// the vault skills were counted under the old "killa:" plugin name
			// until they were merged into this one: same skill, same history.
			qualified, bare, legacy := u[name+":"+e.Name()], u[e.Name()], u["killa:"+e.Name()]
			last := qualified.LastUsedAt
			for _, o := range []int64{bare.LastUsedAt, legacy.LastUsedAt} {
				if o > last {
					last = o
				}
			}
			out = append(out, SkillUse{
				Plugin: name, Skill: e.Name(), Name: name + ":" + e.Name(),
				Uses: qualified.UsageCount + bare.UsageCount + legacy.UsageCount, Last: fmtDate(last),
			})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Uses != out[j].Uses {
			return out[i].Uses > out[j].Uses
		}
		return out[i].Name < out[j].Name
	})
	return out, nil
}

// fmtDate turns an epoch-ms stamp into YYYY-MM-DD; 0 means never used.
func fmtDate(ms int64) string {
	if ms <= 0 {
		return "never"
	}
	return time.UnixMilli(ms).Format("2006-01-02")
}
