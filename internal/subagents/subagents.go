// Package subagents ships qilla's generic Claude sub-agent definitions
// (researcher, triager, coder) and merges user-space ones from <vault>/Qilla/Subagents.
// They travel into qilla's own spawns as --agents JSON; a plain claude session
// never sees them.
package subagents

import (
	"embed"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/jalberto/qilla/internal/models"
)

//go:embed files/*.md
var builtin embed.FS

// UserDir is the vault-relative folder for user-defined sub-agents.
const UserDir = "Qilla/Subagents"

// Def is one sub-agent as Claude Code expects it.
type Def struct {
	Description string   `json:"description"`
	Prompt      string   `json:"prompt"`
	Tools       []string `json:"tools,omitempty"`
	Model       string   `json:"model,omitempty"`
}

// Load returns name → definition: built-ins first, user files override by name.
// Tiers become model ids through t; an explicit `model:` wins, a valid `tier:`
// comes next, and a file with neither resolves to the judgment tier — a
// sub-agent never silently inherits the session's (possibly top) model.
// Obsidian folder notes (<Dir>/<Dir>.md) and files whose frontmatter declares a
// `type:` other than `subagent` are skipped: they are notes, not agents.
func Load(vault string, t models.Tiers) (map[string]Def, error) {
	out := map[string]Def{}
	es, _ := builtin.ReadDir("files")
	for _, e := range es {
		b, _ := builtin.ReadFile("files/" + e.Name())
		if n, d, ok := parse(b, t); ok {
			out[n] = d
		}
	}
	if vault != "" {
		dir := filepath.Join(vault, UserDir)
		folderNote := filepath.Base(dir) + ".md"
		if us, err := os.ReadDir(dir); err == nil {
			for _, e := range us {
				if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") || e.Name() == folderNote {
					continue
				}
				b, err := os.ReadFile(filepath.Join(dir, e.Name()))
				if err != nil {
					continue
				}
				if n, d, ok := parse(b, t); ok {
					out[n] = d
				}
			}
		}
	}
	return out, nil
}

// JSON renders the --agents argument.
func JSON(defs map[string]Def) string {
	b, _ := json.Marshal(defs)
	return string(b)
}

// Names lists the loaded sub-agents, sorted.
func Names(defs map[string]Def) []string {
	var n []string
	for k := range defs {
		n = append(n, k)
	}
	sort.Strings(n)
	return n
}

// parse reads the frontmatter (name, description, tools, tier|model) and body.
func parse(b []byte, t models.Tiers) (string, Def, bool) {
	s := string(b)
	if !strings.HasPrefix(s, "---\n") {
		return "", Def{}, false
	}
	end := strings.Index(s[4:], "\n---")
	if end < 0 {
		return "", Def{}, false
	}
	fm, body := s[4:4+end], strings.TrimSpace(s[4+end+4:])
	var name, tier, typ string
	d := Def{Prompt: body}
	for _, line := range strings.Split(fm, "\n") {
		k, v, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		k, v = strings.TrimSpace(k), strings.TrimSpace(v)
		switch k {
		case "name":
			name = v
		case "description":
			d.Description = v
		case "tools":
			for _, x := range strings.Split(v, ",") {
				if x = strings.TrimSpace(x); x != "" {
					d.Tools = append(d.Tools, x)
				}
			}
		case "tier":
			tier = v
		case "model":
			d.Model = v
		case "type":
			typ = v
		}
	}
	if name == "" || d.Description == "" {
		return "", Def{}, false
	}
	if typ != "" && typ != "subagent" {
		return "", Def{}, false
	}
	if d.Model == "" {
		if tier == "" || !models.Valid(tier) {
			tier = "judgment"
		}
		d.Model = t.Model(tier)
	}
	return name, d, true
}
