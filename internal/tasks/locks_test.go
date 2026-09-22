package tasks

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestLockUnlockRoundtrip(t *testing.T) {
	root := t.TempDir()
	now := time.Date(2026, 9, 23, 14, 5, 0, 0, time.Local)

	if err := LockPath(root, "Work/x.md", "midori", now); err != nil {
		t.Fatal(err)
	}
	locks, err := ReadLocks(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(locks) != 1 || locks[0].Path != "Work/x.md" || locks[0].Host != "midori" {
		t.Fatalf("unexpected locks: %+v", locks)
	}

	// relocking the same host+path replaces, not duplicates
	if err := LockPath(root, "Work/x.md", "midori", now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	locks, _ = ReadLocks(root)
	if len(locks) != 1 {
		t.Fatalf("relocking must not duplicate: %+v", locks)
	}

	if err := UnlockPath(root, "Work/x.md", "midori"); err != nil {
		t.Fatal(err)
	}
	locks, _ = ReadLocks(root)
	if len(locks) != 0 {
		t.Fatalf("unlock left a line: %+v", locks)
	}
}

func TestUnlockOnlyOwnLines(t *testing.T) {
	root := t.TempDir()
	now := time.Date(2026, 9, 23, 14, 5, 0, 0, time.Local)
	if err := LockPath(root, "Work/x.md", "midori", now); err != nil {
		t.Fatal(err)
	}
	if err := LockPath(root, "Work/x.md", "akane", now); err != nil {
		t.Fatal(err)
	}
	if err := UnlockPath(root, "Work/x.md", "akane"); err != nil {
		t.Fatal(err)
	}
	locks, _ := ReadLocks(root)
	if len(locks) != 1 || locks[0].Host != "midori" {
		t.Fatalf("unlock removed another host's line: %+v", locks)
	}
}

func TestHeldByOther(t *testing.T) {
	now := time.Date(2026, 9, 23, 14, 5, 0, 0, time.Local)
	locks := []Lock{
		{Path: "Work/x.md", Host: "midori", Since: now.Add(-10 * time.Minute)},
		{Path: "Work/y.md", Host: "akane", Since: now.Add(-10 * time.Minute)},
		{Path: "Work/z.md", Host: "midori", Since: now.Add(-3 * time.Hour)}, // stale
	}
	tests := []struct {
		name string
		path string
		host string
		want bool
	}{
		{"fresh other-host lock blocks", "Work/x.md", "akane", true},
		{"own lock never blocks", "Work/y.md", "akane", false},
		{"stale (>2h) lock is ignorable", "Work/z.md", "akane", false},
		{"unrelated path is unaffected", "Work/other.md", "akane", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, held := HeldByOther(locks, tt.path, tt.host, now)
			if held != tt.want {
				t.Fatalf("HeldByOther(%q, %q) = %v, want %v", tt.path, tt.host, held, tt.want)
			}
		})
	}
}

func TestLockFileHeaderWrittenOnce(t *testing.T) {
	root := t.TempDir()
	now := time.Date(2026, 9, 23, 14, 5, 0, 0, time.Local)
	if err := LockPath(root, "Work/a.md", "akane", now); err != nil {
		t.Fatal(err)
	}
	if err := LockPath(root, "Work/b.md", "akane", now); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(LocksPath)))
	if err != nil {
		t.Fatal(err)
	}
	content := string(b)
	if strings.Count(content, "type: locks") != 1 {
		t.Fatalf("header written more than once:\n%s", content)
	}
	if !strings.Contains(content, "Work/a.md") || !strings.Contains(content, "Work/b.md") {
		t.Fatalf("missing a lock line:\n%s", content)
	}
}
