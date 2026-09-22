package main

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/jalberto/qilla/internal/config"
	"github.com/jalberto/qilla/internal/tasks"
)

func TestBrainOwnedPathMatcher(t *testing.T) {
	tests := []struct {
		name string
		path string
		want bool
	}{
		{"journal note", "Desk/Journal/2026-09-23.md", true},
		{"todo file exact", "Desk/Todo.md", true},
		{"questions file exact", "Desk/Questions.md", true},
		{"briefing note", "Desk/Briefings/2026-09-23.md", true},
		{"facts note", "Qilla/Facts/whatever.md", true},
		{"people note", "Life/People/Someone.md", true},
		{"config note", "Qilla/Config/Personality.md", true},
		{"improvements file", "Qilla/Improvements.md", true},
		{"context note", "Qilla/Context/JA.md", true},
		{"not owned: work note", "Work/VL/Meetings/2026-09-23.md", false},
		{"not owned: handoff note", "Qilla/Handoff/akane.md", false},
		{"not owned: prefix lookalike", "Desk/Todo.md.bak", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := brainOwned(tt.path); got != tt.want {
				t.Fatalf("brainOwned(%q) = %v, want %v", tt.path, got, tt.want)
			}
		})
	}
}

func TestVaultRel(t *testing.T) {
	vault := "/home/x/Notes"
	tests := []struct {
		name   string
		path   string
		want   string
		wantOK bool
	}{
		{"absolute inside vault", filepath.Join(vault, "Desk/Journal/2026-09-23.md"), "Desk/Journal/2026-09-23.md", true},
		{"already relative", "Desk/Journal/2026-09-23.md", "Desk/Journal/2026-09-23.md", true},
		{"outside the vault", "/home/x/Projects/other/file.md", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := vaultRel(vault, tt.path)
			if ok != tt.wantOK || (ok && got != tt.want) {
				t.Fatalf("vaultRel(%q) = (%q, %v), want (%q, %v)", tt.path, got, ok, tt.want, tt.wantOK)
			}
		})
	}
}

func TestWriteGuardRole(t *testing.T) {
	vault := "/home/x/Notes"
	cfg := &config.Config{Vault: vault}
	now := time.Now()
	if deny, _ := writeGuard(cfg, config.RoleBrain, "midori", filepath.Join(vault, "Desk/Journal/2026-09-23.md"), now); deny {
		t.Fatal("a brain writes anywhere in the vault")
	}
	if deny, reason := writeGuard(cfg, config.RoleWorker, "akane", filepath.Join(vault, "Desk/Journal/2026-09-23.md"), now); !deny || reason == "" {
		t.Fatalf("a worker is denied on brain-owned files: deny=%v reason=%q", deny, reason)
	}
	if deny, _ := writeGuard(cfg, config.RoleWorker, "akane", filepath.Join(vault, "Work/VL/Meetings/x.md"), now); deny {
		t.Fatal("a worker writes freely outside brain-owned prefixes")
	}
	if deny, _ := writeGuard(cfg, config.RoleWorker, "akane", "/home/x/Projects/other/file.md", now); deny {
		t.Fatal("paths outside the vault are not this guard's business")
	}
}

func TestWriteGuardLocks(t *testing.T) {
	vault := t.TempDir()
	cfg := &config.Config{Vault: vault}
	now := time.Date(2026, 9, 23, 14, 5, 0, 0, time.Local)

	if err := tasks.LockPath(vault, "Work/shared.md", "midori", now.Add(-10*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if deny, reason := writeGuard(cfg, config.RoleWorker, "akane", filepath.Join(vault, "Work/shared.md"), now); !deny || reason == "" {
		t.Fatalf("a fresh lock held by another host denies: deny=%v reason=%q", deny, reason)
	}

	// this host's own lock never blocks it
	if err := tasks.LockPath(vault, "Work/mine.md", "akane", now.Add(-10*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if deny, _ := writeGuard(cfg, config.RoleWorker, "akane", filepath.Join(vault, "Work/mine.md"), now); deny {
		t.Fatal("this host's own lock does not block it")
	}

	// a stale lock (>2h) is ignorable
	if err := tasks.LockPath(vault, "Work/stale.md", "midori", now.Add(-3*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if deny, _ := writeGuard(cfg, config.RoleWorker, "akane", filepath.Join(vault, "Work/stale.md"), now); deny {
		t.Fatal("a lock older than 2h is ignorable")
	}
}

func TestBashRedirectTarget(t *testing.T) {
	tests := []struct {
		cmd        string
		wantTarget string
		wantOK     bool
	}{
		{"echo x >> Desk/Todo.md", "Desk/Todo.md", true},
		{"echo x > Desk/Todo.md", "Desk/Todo.md", true},
		{"echo x >>Desk/Todo.md", "Desk/Todo.md", true},
		{"echo x | tee Work/x.md", "Work/x.md", true},
		{"git status", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.cmd, func(t *testing.T) {
			got, ok := bashRedirectTarget(tt.cmd)
			if ok != tt.wantOK || (ok && got != tt.wantTarget) {
				t.Fatalf("bashRedirectTarget(%q) = (%q, %v), want (%q, %v)", tt.cmd, got, ok, tt.wantTarget, tt.wantOK)
			}
		})
	}
}

func TestBashWriteGuard(t *testing.T) {
	vault := "/home/x/Notes"
	cfg := &config.Config{Vault: vault}
	if deny, _ := bashWriteGuard(cfg, config.RoleWorker, "echo x >> Desk/Todo.md"); !deny {
		t.Fatal("a worker redirecting into a brain-owned file is denied")
	}
	if deny, _ := bashWriteGuard(cfg, config.RoleBrain, "echo x >> Desk/Todo.md"); deny {
		t.Fatal("a brain redirects freely")
	}
	if deny, _ := bashWriteGuard(cfg, config.RoleWorker, "echo x >> Work/x.md"); deny {
		t.Fatal("a worker redirecting into its own space is allowed")
	}
}
