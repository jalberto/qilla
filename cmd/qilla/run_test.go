package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/jalberto/qilla/internal/config"
	"github.com/jalberto/qilla/internal/worker"
)

// onceCfg is a config with one script routine "stub" whose gather.sh runs body.
func onceCfg(t *testing.T, body string) *config.Config {
	t.Helper()
	root := t.TempDir()
	vault := filepath.Join(root, "vault")
	dir := filepath.Join(vault, worker.RoutinesDir, "stub")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "gather.sh"), []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	return &config.Config{Path: filepath.Join(root, "cfg", "qilla.toml"), Vault: vault, StateDir: filepath.Join(root, "state"),
		Routines: map[string]config.Routine{"stub": {Kind: config.KindScript}}}
}

func TestRunOnceInlineAndExitStatus(t *testing.T) {
	cfg := onceCfg(t, "#!/bin/sh\necho '{\"ok\":true}'\n")
	if err := runRoutine(cfg, "stub", runOpts{once: true}); err != nil {
		t.Fatalf("ok run: %v", err)
	}
	if _, err := os.Stat(filepath.Join(cfg.StateDir, "runs", "0.json")); err != nil {
		t.Fatalf("--once should record in the state dir: %v", err)
	}
	bad := onceCfg(t, "#!/bin/sh\necho boom >&2\nexit 3\n")
	if err := runRoutine(bad, "stub", runOpts{once: true}); err == nil {
		t.Fatal("failing gather must fail the run")
	}
}
