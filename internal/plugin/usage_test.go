package plugin

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func writePlugin(t *testing.T, root, dirName, declaredName string, skills ...string) string {
	t.Helper()
	dir := filepath.Join(root, dirName)
	for _, s := range skills {
		if err := os.MkdirAll(filepath.Join(dir, "skills", s), 0o755); err != nil {
			t.Fatal(err)
		}
		os.WriteFile(filepath.Join(dir, "skills", s, "SKILL.md"), []byte("---\nname: "+s+"\n---\n"), 0o644)
	}
	if declaredName != "" {
		os.MkdirAll(filepath.Join(dir, ".claude-plugin"), 0o755)
		os.WriteFile(filepath.Join(dir, ".claude-plugin", "plugin.json"), []byte(`{"name":"`+declaredName+`"}`), 0o644)
	}
	return dir
}

func writeClaudeJSON(t *testing.T, dir string, usage map[string]any) string {
	t.Helper()
	p := filepath.Join(dir, "claude.json")
	b, _ := json.Marshal(map[string]any{"skillUsage": usage})
	if err := os.WriteFile(p, b, 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestUsageSumsQualifiedAndBareKeys(t *testing.T) {
	root := t.TempDir()
	dir := writePlugin(t, root, "Plugin", "demo", "brief", "retro", "scout")
	cj := writeClaudeJSON(t, root, map[string]any{
		"demo:brief": map[string]any{"usageCount": 3, "lastUsedAt": 1756000000000},
		"brief":      map[string]any{"usageCount": 2, "lastUsedAt": 1757000000000},
		"retro":      map[string]any{"usageCount": 1, "lastUsedAt": 1755000000000},
	})
	rows, err := Usage([]string{dir}, cj)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 3 {
		t.Fatalf("want 3 skills, got %+v", rows)
	}
	if rows[0].Name != "demo:brief" || rows[0].Uses != 5 {
		t.Fatalf("both keys must be summed: %+v", rows[0])
	}
	if rows[0].Last != "2025-09-04" { // the later of the two stamps
		t.Fatalf("last use: %q", rows[0].Last)
	}
	if rows[1].Name != "demo:retro" || rows[1].Uses != 1 {
		t.Fatalf("sorted by uses: %+v", rows)
	}
	if rows[2].Name != "demo:scout" || rows[2].Uses != 0 || rows[2].Last != "never" {
		t.Fatalf("never-used skill: %+v", rows[2])
	}
}

func TestPluginNamePrefersPluginJSONThenLowercasedDir(t *testing.T) {
	root := t.TempDir()
	declared := writePlugin(t, root, "Plugin", "demo", "brief")
	if got := PluginName(declared); got != "demo" {
		t.Fatalf("declared name: %q", got)
	}
	bare := writePlugin(t, root, "Qilla", "", "brief")
	if got := PluginName(bare); got != "qilla" {
		t.Fatalf("dir basename lowercased: %q", got)
	}
}

func TestUsageSkipsMissingDirsAndBrokenClaudeJSON(t *testing.T) {
	root := t.TempDir()
	dir := writePlugin(t, root, "Plugin", "demo", "brief")
	broken := filepath.Join(root, "broken.json")
	os.WriteFile(broken, []byte("not json"), 0o644)
	rows, err := Usage([]string{dir, filepath.Join(root, "nope")}, broken)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Uses != 0 {
		t.Fatalf("%+v", rows)
	}
	rows, err = Usage([]string{filepath.Join(root, "nope")}, filepath.Join(root, "missing.json"))
	if err != nil || len(rows) != 0 {
		t.Fatalf("%v %+v", err, rows)
	}
}
