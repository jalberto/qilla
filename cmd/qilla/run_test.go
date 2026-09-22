package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jalberto/qilla/internal/config"
	"github.com/jalberto/qilla/internal/worker"
)

// onceCfg is a config with one script routine "stub" whose gather.sh runs
// body; role is this host's role.
func onceCfg(t *testing.T, role, body string) *config.Config {
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
	roles := map[string]string{worker.ShortHost(): role}
	return &config.Config{Path: filepath.Join(root, "cfg", "qilla.toml"), Vault: vault, StateDir: filepath.Join(root, "state"),
		Host:     config.Host{Roles: roles},
		Routines: map[string]config.Routine{"stub": {Kind: config.KindScript}}}
}

func TestRunOnceInlineAndExitStatus(t *testing.T) {
	cfg := onceCfg(t, config.RoleBrain, "#!/bin/sh\necho '{\"ok\":true}'\n")
	if err := runRoutine(cfg, "stub", runOpts{once: true}); err != nil {
		t.Fatalf("ok run: %v", err)
	}
	if _, err := os.Stat(filepath.Join(cfg.StateDir, "runs", "0.json")); err != nil {
		t.Fatalf("brain --once should record in the state dir: %v", err)
	}
	bad := onceCfg(t, config.RoleBrain, "#!/bin/sh\necho boom >&2\nexit 3\n")
	if err := runRoutine(bad, "stub", runOpts{once: true}); err == nil {
		t.Fatal("failing gather must fail the run")
	}
}

func TestRunWorkerNeedsOnceLocal(t *testing.T) {
	cfg := onceCfg(t, config.RoleWorker, "#!/bin/sh\necho '{}'\n")
	for _, o := range []runOpts{{}, {once: true}} {
		var r refusal
		if err := runRoutine(cfg, "stub", o); !errors.As(err, &r) || !strings.Contains(string(r), "--once --local") {
			t.Fatalf("%+v: want refusal naming --once --local, got %v", o, err)
		}
	}
	var r refusal
	if err := runRoutine(cfg, "brief", runOpts{once: true, local: true}); !errors.As(err, &r) {
		t.Fatalf("brief on worker: want refusal, got %v", err)
	}
}

func TestRunLocalLedgerToHandoff(t *testing.T) {
	cfg := onceCfg(t, config.RoleWorker, "#!/bin/sh\necho '{\"pending\":[]}'\n")
	if err := runRoutine(cfg, "stub", runOpts{once: true, local: true}); err != nil {
		t.Fatalf("local run: %v", err)
	}
	b, err := os.ReadFile(filepath.Join(cfg.Vault, worker.HandoffPath(worker.ShortHost())))
	if err != nil {
		t.Fatalf("handoff note: %v", err)
	}
	line := string(b)
	if !strings.HasPrefix(line, "- [ ] ") || !strings.Contains(line, " · "+worker.ShortHost()+" · ledger:stub · {") {
		t.Fatalf("handoff line = %q", line)
	}
	if _, err := os.Stat(cfg.StateDir); !os.IsNotExist(err) {
		t.Fatalf("state dir must stay untouched with --local (stat err %v)", err)
	}
}
