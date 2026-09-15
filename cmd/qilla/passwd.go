package main

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/jalberto/qilla/internal/auth"
	"github.com/jalberto/qilla/internal/config"
)

// cmdPasswd: qilla passwd — set the web UI password (bcrypt hash into qilla.toml).
func cmdPasswd(args []string) error {
	if len(args) != 0 {
		return fmt.Errorf("usage: qilla passwd  (reads the password from stdin)")
	}
	path := config.DefaultPath()
	cfg, err := config.Load(path)
	if err != nil {
		return err
	}
	fmt.Fprint(os.Stderr, "new web password: ")
	pw, _ := bufio.NewReader(os.Stdin).ReadString('\n')
	pw = strings.TrimRight(pw, "\r\n")
	if len(pw) < 8 {
		return fmt.Errorf("use at least 8 characters")
	}
	h, err := auth.Hash(pw)
	if err != nil {
		return err
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	out := setPasswordHash(string(b), h)
	if err := os.WriteFile(path, []byte(out), 0o600); err != nil {
		return err
	}
	// Force a fresh signing key on next restart, so every existing session
	// (including a leaked cookie) is invalidated by the password change.
	if err := os.Remove(filepath.Join(cfg.StateDir, "web-session.key")); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("password set, but could not invalidate existing sessions: %w", err)
	}
	fmt.Println("password set; restart serve to apply (systemctl --user restart qilla.service) — this also logs out every existing session")
	return nil
}

// setPasswordHash rewrites the password_hash line inside [web]: replaces an
// existing (or commented-out) line, else inserts one right after the [web]
// header, else appends a [web] table. Never duplicates the table.
func setPasswordHash(toml, hash string) string {
	line := fmt.Sprintf("password_hash = %q", hash)
	lines := strings.Split(toml, "\n")
	webAt := -1
	for i, l := range lines {
		t := strings.TrimSpace(l)
		if t == "[web]" {
			webAt = i
			continue
		}
		if strings.HasPrefix(strings.TrimLeft(t, "# "), "password_hash") && webAt >= 0 {
			lines[i] = line
			return strings.Join(lines, "\n")
		}
	}
	if webAt >= 0 {
		lines = append(lines[:webAt+1], append([]string{line}, lines[webAt+1:]...)...)
		return strings.Join(lines, "\n")
	}
	return strings.TrimRight(toml, "\n") + "\n\n[web]\n" + line + "\n"
}
