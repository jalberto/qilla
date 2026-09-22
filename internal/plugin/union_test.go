package plugin

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// vaultDir builds a user plugin dir with the given skills and a hooks.json.
func vaultDir(t *testing.T, root string, skills []string, hooks string) string {
	t.Helper()
	for _, s := range skills {
		if err := os.MkdirAll(filepath.Join(root, "skills", s), 0o755); err != nil {
			t.Fatal(err)
		}
		os.WriteFile(filepath.Join(root, "skills", s, "SKILL.md"), []byte("# "+s), 0o644)
	}
	os.MkdirAll(filepath.Join(root, "hooks"), 0o755)
	os.WriteFile(filepath.Join(root, "hooks", "hooks.json"), []byte(hooks), 0o644)
	return root
}

func readHooks(t *testing.T, pluginDir string) hooksDoc {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(pluginDir, "hooks", "hooks.json"))
	if err != nil {
		t.Fatal(err)
	}
	var d hooksDoc
	if err := json.Unmarshal(b, &d); err != nil {
		t.Fatal(err)
	}
	return d
}

func TestUnionLinksSkillsAndMergesHooks(t *testing.T) {
	root := t.TempDir()
	pd := filepath.Join(root, "plugin")
	os.MkdirAll(filepath.Join(pd, "skills", "learn"), 0o755) // an embedded skill: a real dir
	vd := vaultDir(t, filepath.Join(root, "vault"), []string{"brief", "diary"}, `{"hooks":{
      "SessionStart":[{"hooks":[{"type":"command","command":"hooks/session-start.sh","timeout":20}]}],
      "Stop":[{"hooks":[{"type":"command","command":"hooks/stop-learn.sh"}]}]}}`)
	os.WriteFile(filepath.Join(vd, "hooks", "session-start.sh"), []byte("#!/bin/sh\n"), 0o755)
	os.WriteFile(filepath.Join(vd, "hooks", "stop-learn.sh"), []byte("#!/bin/sh\n"), 0o755)
	os.MkdirAll(filepath.Join(vd, "agents"), 0o755)
	os.WriteFile(filepath.Join(vd, "agents", "scout.md"), []byte("---\nname: scout\n---\n"), 0o644)

	warn, err := Union(pd, []string{vd})
	if err != nil {
		t.Fatal(err)
	}
	if len(warn) != 0 {
		t.Fatalf("no collisions expected: %v", warn)
	}
	for _, s := range []string{"brief", "diary"} {
		tgt, err := os.Readlink(filepath.Join(pd, "skills", s))
		if err != nil || tgt != filepath.Join(vd, "skills", s) {
			t.Fatalf("skills/%s not symlinked: %v %q", s, err, tgt)
		}
	}
	if tgt, err := os.Readlink(filepath.Join(pd, "agents", "scout.md")); err != nil || tgt != filepath.Join(vd, "agents", "scout.md") {
		t.Fatalf("agent not symlinked: %v %q", err, tgt)
	}
	if fi, err := os.Lstat(filepath.Join(pd, "skills", "learn")); err != nil || fi.Mode()&os.ModeSymlink != 0 {
		t.Fatal("the embedded skill must stay a real directory")
	}

	h := readHooks(t, pd)
	if len(h.Hooks["SessionStart"]) != 2 || len(h.Hooks["Stop"]) != 2 || len(h.Hooks["PreToolUse"]) != 3 {
		t.Fatalf("qilla's own hooks plus the vault's expected: %+v", h.Hooks)
	}
	body, _ := json.Marshal(h.Hooks)
	for _, want := range []string{"qilla hook session-start", "qilla hook stop",
		filepath.Join(vd, "hooks", "session-start.sh"), filepath.Join(vd, "hooks", "stop-learn.sh")} {
		if !strings.Contains(string(body), want) {
			t.Fatalf("hooks.json missing %q: %s", want, body)
		}
	}

	// idempotent: a second run changes nothing
	if _, err := Union(pd, []string{vd}); err != nil {
		t.Fatal(err)
	}
	if h2 := readHooks(t, pd); len(h2.Hooks["SessionStart"]) != 2 {
		t.Fatalf("second union duplicated hooks: %+v", h2.Hooks)
	}
}

func TestUnionCollisionKeepsTheRealDirAndWarns(t *testing.T) {
	root := t.TempDir()
	pd := filepath.Join(root, "plugin")
	os.MkdirAll(filepath.Join(pd, "skills", "research"), 0o755)
	os.WriteFile(filepath.Join(pd, "skills", "research", "SKILL.md"), []byte("embedded"), 0o644)
	vd := vaultDir(t, filepath.Join(root, "vault"), []string{"research"}, `{"hooks":{}}`)

	warn, err := Union(pd, []string{vd})
	if err != nil {
		t.Fatal(err)
	}
	if len(warn) != 1 || !strings.Contains(warn[0], "research") {
		t.Fatalf("the clash must be reported once: %v", warn)
	}
	b, _ := os.ReadFile(filepath.Join(pd, "skills", "research", "SKILL.md"))
	if string(b) != "embedded" {
		t.Fatalf("the embedded skill must win, got %q", b)
	}
}

func TestUnionRemovesStaleSymlinks(t *testing.T) {
	root := t.TempDir()
	pd := filepath.Join(root, "plugin")
	vd := vaultDir(t, filepath.Join(root, "vault"), []string{"brief", "gone"}, `{"hooks":{}}`)
	if _, err := Union(pd, []string{vd}); err != nil {
		t.Fatal(err)
	}
	os.RemoveAll(filepath.Join(vd, "skills", "gone"))
	if _, err := Union(pd, []string{vd}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(pd, "skills", "gone")); err == nil {
		t.Fatal("the symlink of a deleted vault skill must be removed")
	}
	if _, err := os.Lstat(filepath.Join(pd, "skills", "brief")); err != nil {
		t.Fatal("the live skill must survive")
	}
	// the dir itself dropped from the config: every link into it goes
	if _, err := Union(pd, nil); err != nil {
		t.Fatal(err)
	}
	if es, _ := os.ReadDir(filepath.Join(pd, "skills")); len(es) != 0 {
		t.Fatalf("no vault symlink should be left: %v", es)
	}
}

func TestRefreshCacheDereferencesSymlinksAndPrunes(t *testing.T) {
	root := t.TempDir()
	pd := filepath.Join(root, "plugin")
	vd := vaultDir(t, filepath.Join(root, "vault"), []string{"brief"}, `{"hooks":{}}`)
	if _, err := Union(pd, []string{vd}); err != nil {
		t.Fatal(err)
	}
	cache := filepath.Join(root, "cache", "qilla", "qilla", "0.1.0")
	os.MkdirAll(filepath.Join(cache, "skills", "obsolete"), 0o755)
	cp := filepath.Join(root, "claudeplugins")
	os.MkdirAll(cp, 0o755)
	os.WriteFile(filepath.Join(cp, "installed_plugins.json"),
		[]byte(`{"plugins":{"qilla@qilla":[{"installPath":"`+cache+`"}]}}`), 0o644)

	done, err := RefreshCache(pd, cp)
	if err != nil || len(done) != 1 || done[0] != cache {
		t.Fatalf("refresh = %v, %v", done, err)
	}
	fi, err := os.Lstat(filepath.Join(cache, "skills", "brief"))
	if err != nil || !fi.IsDir() || fi.Mode()&os.ModeSymlink != 0 {
		t.Fatalf("the vault skill must land as a real directory: %v", err)
	}
	if b, err := os.ReadFile(filepath.Join(cache, "skills", "brief", "SKILL.md")); err != nil || string(b) != "# brief" {
		t.Fatalf("skill content not copied: %q %v", b, err)
	}
	if _, err := os.Stat(filepath.Join(cache, "skills", "obsolete")); err == nil {
		t.Fatal("what the plugin no longer has must be pruned from the cache")
	}
	if _, err := os.Stat(filepath.Join(cache, "hooks", "hooks.json")); err != nil {
		t.Fatal("the merged hooks.json must reach the cache")
	}
}
