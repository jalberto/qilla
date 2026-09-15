package subagents

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/jalberto/qilla/internal/models"
)

func TestBuiltinsAndUserOverride(t *testing.T) {
	var tiers models.Tiers
	tiers.Defaults()
	vault := t.TempDir()
	os.MkdirAll(filepath.Join(vault, UserDir), 0o755)
	os.WriteFile(filepath.Join(vault, UserDir, "triager.md"), []byte("---\nname: triager\ndescription: my triager\ntools: Read\nmodel: claude-opus-5\n---\nbody"), 0o644)
	os.WriteFile(filepath.Join(vault, UserDir, "cook.md"), []byte("---\nname: cook\ndescription: plans meals\ntier: research\n---\nYou cook."), 0o644)
	defs, err := Load(vault, tiers)
	if err != nil {
		t.Fatal(err)
	}
	if defs["researcher"].Model != tiers.Research || len(defs["researcher"].Tools) == 0 {
		t.Fatalf("builtin researcher: %+v", defs["researcher"])
	}
	if defs["triager"].Description != "my triager" || defs["triager"].Model != "claude-opus-5" {
		t.Fatalf("user file must override the builtin: %+v", defs["triager"])
	}
	if defs["cook"].Model != tiers.Research || defs["cook"].Prompt != "You cook." {
		t.Fatalf("user agent with tier: %+v", defs["cook"])
	}
	// coder: the coding tier (Opus by default), can edit and run the gate, never commits
	if c := defs["coder"]; c.Model != tiers.Coding || c.Model != "claude-opus-5" || len(c.Tools) < 5 {
		t.Fatalf("builtin coder: %+v", c)
	}
	var back map[string]Def
	if err := json.Unmarshal([]byte(JSON(defs)), &back); err != nil || len(back) != 4 {
		t.Fatalf("json: %v %d", err, len(back))
	}
	if n := Names(defs); n[0] != "coder" || n[1] != "cook" || n[3] != "triager" {
		t.Fatalf("%v", n)
	}
}

// A user agent with neither model: nor tier: must land on the judgment tier,
// never on whatever model the session happens to run.
func TestTierlessAgentGetsTheJudgmentTier(t *testing.T) {
	var tiers models.Tiers
	tiers.Defaults()
	vault := t.TempDir()
	os.MkdirAll(filepath.Join(vault, UserDir), 0o755)
	os.WriteFile(filepath.Join(vault, UserDir, "plain.md"), []byte("---\nname: plain\ndescription: no model, no tier\n---\nbody"), 0o644)
	os.WriteFile(filepath.Join(vault, UserDir, "bogus.md"), []byte("---\nname: bogus\ndescription: unknown tier\ntier: nonesuch\n---\nbody"), 0o644)
	defs, err := Load(vault, tiers)
	if err != nil {
		t.Fatal(err)
	}
	if defs["plain"].Model != tiers.Judgment {
		t.Fatalf("tierless agent: %+v (want %s)", defs["plain"], tiers.Judgment)
	}
	if defs["bogus"].Model != tiers.Judgment {
		t.Fatalf("invalid tier must fall back to judgment: %+v", defs["bogus"])
	}
}

// Obsidian folder notes and non-subagent notes living in the folder are notes,
// not agents, even when they carry name/description frontmatter.
func TestFolderNotesAndTypedNotesAreNotAgents(t *testing.T) {
	var tiers models.Tiers
	tiers.Defaults()
	vault := t.TempDir()
	os.MkdirAll(filepath.Join(vault, UserDir), 0o755)
	folder := filepath.Base(UserDir) + ".md" // Subagents/Subagents.md
	os.WriteFile(filepath.Join(vault, UserDir, folder), []byte("---\nname: Subagents\ndescription: index of the sub-agents\n---\nThe folder note."), 0o644)
	os.WriteFile(filepath.Join(vault, UserDir, "notes.md"), []byte("---\nname: notes\ndescription: a plain note\ntype: note\n---\nbody"), 0o644)
	os.WriteFile(filepath.Join(vault, UserDir, "typed.md"), []byte("---\nname: typed\ndescription: an explicit sub-agent\ntype: subagent\n---\nbody"), 0o644)
	defs, err := Load(vault, tiers)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := defs["Subagents"]; ok {
		t.Fatal("the folder note must not be loaded as an agent")
	}
	if _, ok := defs["notes"]; ok {
		t.Fatal("type: note must not be loaded as an agent")
	}
	if _, ok := defs["typed"]; !ok {
		t.Fatal("type: subagent must still load")
	}
}
