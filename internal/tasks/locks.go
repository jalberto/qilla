package tasks

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// LocksPath is the vault-relative soft-lock ledger — see
// Projects/qilla/tasks/host-roles-worker.md §3.
const LocksPath = "Qilla/Handoff/locks.md"

const locksHeader = `---
type: locks
tags: [qilla, handoff]
---
# Soft locks

`

// lockLine matches one lock ledger line:
//
//   - Desk/Journal/2026-09-23.md · akane · 2026-09-23 14:05
var lockLine = regexp.MustCompile(`^- (\S.*) · (\S+) · (\d{4}-\d{2}-\d{2} \d{2}:\d{2})$`)

// Lock is one parsed line of Qilla/Handoff/locks.md.
type Lock struct {
	Path  string
	Host  string
	Since time.Time
}

// LockStale is how long a lock line is honored before it is ignorable.
const LockStale = 2 * time.Hour

// ReadLocks parses every well-formed line of <vault>/Qilla/Handoff/locks.md.
// A missing file is not an error — no locks held.
func ReadLocks(root string) ([]Lock, error) {
	abs := filepath.Join(root, filepath.FromSlash(LocksPath))
	lines, _, err := readLines(abs)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []Lock
	for _, l := range lines {
		m := lockLine.FindStringSubmatch(strings.TrimSpace(l))
		if m == nil {
			continue
		}
		since, err := time.ParseInLocation("2006-01-02 15:04", m[3], time.Local)
		if err != nil {
			continue
		}
		out = append(out, Lock{Path: m[1], Host: m[2], Since: since})
	}
	return out, nil
}

// HeldByOther reports the lock (if any) that blocks host from touching
// vaultRelPath: another host's line, not yet stale as of now.
func HeldByOther(locks []Lock, vaultRelPath, host string, now time.Time) (Lock, bool) {
	for _, l := range locks {
		if l.Path == vaultRelPath && l.Host != host && now.Sub(l.Since) < LockStale {
			return l, true
		}
	}
	return Lock{}, false
}

// LockPath appends (or replaces) this host's lock line for vaultRelPath in
// Qilla/Handoff/locks.md, creating the file with its header if missing.
func LockPath(root, vaultRelPath, host string, now time.Time) error {
	abs := filepath.Join(root, filepath.FromSlash(LocksPath))
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		return err
	}
	body, mode, existed := readBody(abs)
	kept := keepOtherLockLines(body, vaultRelPath, host)
	line := fmt.Sprintf("- %s · %s · %s", vaultRelPath, host, now.Format("2006-01-02 15:04"))
	if !existed {
		return writeAtomic(abs, locksHeader+line, mode)
	}
	if strings.TrimSpace(kept) == "" {
		return writeAtomic(abs, kept+line, mode)
	}
	return writeAtomic(abs, kept+"\n"+line, mode)
}

// UnlockPath removes any lock line for vaultRelPath held by host. Lines held
// by other hosts, and everything else in the file, are left untouched.
func UnlockPath(root, vaultRelPath, host string) error {
	abs := filepath.Join(root, filepath.FromSlash(LocksPath))
	body, mode, existed := readBody(abs)
	if !existed {
		return nil
	}
	return writeAtomic(abs, keepOtherLockLines(body, vaultRelPath, host), mode)
}

// readBody reads a file's raw content (sans trailing newline) and mode; a
// missing file is not an error — ("", 0o644, false).
func readBody(abs string) (string, os.FileMode, bool) {
	lines, mode, err := readLines(abs)
	if err != nil {
		return "", 0o644, false
	}
	return strings.TrimRight(strings.Join(lines, "\n"), "\n"), mode, true
}

// keepOtherLockLines returns body with any lock line for vaultRelPath held by
// host removed, everything else (header, other locks, blanks) untouched.
func keepOtherLockLines(body, vaultRelPath, host string) string {
	if body == "" {
		return ""
	}
	var kept []string
	for _, l := range strings.Split(body, "\n") {
		m := lockLine.FindStringSubmatch(strings.TrimSpace(l))
		if m != nil && m[1] == vaultRelPath && m[2] == host {
			continue // superseded (LockPath) or removed (UnlockPath)
		}
		kept = append(kept, l)
	}
	return strings.Join(kept, "\n")
}
