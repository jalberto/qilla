package main

import (
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/jalberto/qilla/internal/config"
	"github.com/jalberto/qilla/internal/tasks"
)

// brainOwnedPrefixes are the vault-relative directories/files a worker never
// writes directly — see Projects/qilla/tasks/host-roles-worker.md §2.
var brainOwnedPrefixes = []string{
	"Desk/Journal/",
	"Desk/Todo.md",
	"Desk/Questions.md",
	"Desk/Briefings/",
	"Qilla/Facts/",
	"Life/People/",
	"Qilla/Config/",
	"Qilla/Improvements.md",
	"Qilla/Context/",
}

const brainOwnedReason = "brain-owned on a worker — append your line to Qilla/Handoff/<host>.md instead"

// brainOwned reports whether vaultRelPath (forward-slash, vault-relative)
// falls under a brain-owned prefix.
func brainOwned(vaultRelPath string) bool {
	for _, pfx := range brainOwnedPrefixes {
		if strings.HasSuffix(pfx, "/") {
			if strings.HasPrefix(vaultRelPath, pfx) {
				return true
			}
		} else if vaultRelPath == pfx {
			return true
		}
	}
	return false
}

// vaultRel resolves a tool's file path (absolute or already vault-relative)
// against the vault root. ok is false when the path is outside the vault.
func vaultRel(vault, p string) (string, bool) {
	if p == "" || vault == "" {
		return "", false
	}
	if !filepath.IsAbs(p) {
		return filepath.ToSlash(filepath.Clean(p)), true
	}
	absVault, err := filepath.Abs(vault)
	if err != nil {
		return "", false
	}
	absP, err := filepath.Abs(p)
	if err != nil {
		return "", false
	}
	rel, err := filepath.Rel(absVault, absP)
	if err != nil || strings.HasPrefix(rel, "..") {
		return "", false
	}
	return filepath.ToSlash(rel), true
}

// writeGuard decides an Edit/Write/MultiEdit/NotebookEdit for role/host: a
// brain always writes; a worker is denied on a brain-owned prefix or a path
// another host holds a fresh lock on. host and now are injected for testability
// (role and hostname would otherwise make this untestable without a real host).
func writeGuard(cfg *config.Config, role, host, path string, now time.Time) (deny bool, reason string) {
	if role == config.RoleBrain {
		return false, ""
	}
	rel, ok := vaultRel(cfg.Vault, path)
	if !ok {
		return false, "" // outside the vault: not this guard's business
	}
	if brainOwned(rel) {
		return true, brainOwnedReason
	}
	locks, err := tasks.ReadLocks(cfg.Vault)
	if err != nil {
		return false, ""
	}
	if l, held := tasks.HeldByOther(locks, rel, host, now); held {
		return true, fmt.Sprintf("locked by %s since %s — queue the change in your handoff instead", l.Host, l.Since.Format("2006-01-02 15:04"))
	}
	return false, ""
}

// bashRedirectTarget returns the vault-relative path a `>`, `>>` or `tee`
// redirect writes to, if the command has one. Deliberately simple: a string
// match on the token after the redirect operator, not a shell parse.
func bashRedirectTarget(cmd string) (string, bool) {
	fields := strings.Fields(cmd)
	for i, f := range fields {
		switch {
		case f == ">" || f == ">>":
			if i+1 < len(fields) {
				return fields[i+1], true
			}
		case strings.HasPrefix(f, ">>") || strings.HasPrefix(f, ">"):
			t := strings.TrimLeft(f, ">")
			if t != "" {
				return t, true
			}
		case f == "tee":
			for j := i + 1; j < len(fields); j++ {
				if !strings.HasPrefix(fields[j], "-") {
					return fields[j], true
				}
			}
		}
	}
	return "", false
}

// bashWriteGuard extends the bash guard on a worker: deny a command whose
// redirect target is a brain-owned vault path.
func bashWriteGuard(cfg *config.Config, role, cmd string) (deny bool, reason string) {
	if role == config.RoleBrain {
		return false, ""
	}
	target, ok := bashRedirectTarget(cmd)
	if !ok {
		return false, ""
	}
	rel, ok := vaultRel(cfg.Vault, target)
	if !ok {
		return false, ""
	}
	if brainOwned(rel) {
		return true, brainOwnedReason
	}
	return false, ""
}
