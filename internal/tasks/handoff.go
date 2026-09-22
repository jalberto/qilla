package tasks

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// HandoffDir is the vault-relative directory the brain/worker handoff notes
// live in — see Projects/qilla/tasks/host-roles-worker.md §4.
const HandoffDir = "Qilla/Handoff"

// handoffLine matches an open or done handoff task line:
//
//   - [ ] 2026-09-23 14:05 · from akane · routine:research · did the thing
//   - [x] 2026-09-23 14:05 · from akane · did the thing → [[result]]
var handoffLine = regexp.MustCompile(
	`^- \[([ x])\] (\d{4}-\d{2}-\d{2} \d{2}:\d{2}) · from (\S+) · (?:routine:(\S+) · )?(.*)$`)

// handoffPath is the vault-relative path of the handoff file addressed to host.
func handoffPath(host string) string {
	return filepath.Join(HandoffDir, fmt.Sprintf("for-%s.md", host))
}

// handoffHeader is written once, the first time a host's handoff file is created.
func handoffHeader(host string) string {
	return fmt.Sprintf(`---
type: handoff
host: %s
tags: [qilla, handoff]
---
# Tasks for %s

`, host, host)
}

// Send appends one task line to <vault>/Qilla/Handoff/for-<host>.md, creating
// the file with its header if it does not exist yet. It never rewrites the
// file — only O_APPEND. Returns the line written (without its trailing
// newline).
func Send(root, host, from, routine, text string, now time.Time) (string, error) {
	rel := handoffPath(host)
	abs := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		return "", err
	}
	needHeader := false
	if fi, err := os.Stat(abs); err != nil {
		if !os.IsNotExist(err) {
			return "", err
		}
		needHeader = true
	} else if fi.Size() == 0 {
		needHeader = true
	}
	f, err := os.OpenFile(abs, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return "", err
	}
	defer f.Close()
	if needHeader {
		if _, err := f.WriteString(handoffHeader(host)); err != nil {
			return "", err
		}
	}
	seg := ""
	if routine != "" {
		seg = "routine:" + routine + " · "
	}
	line := fmt.Sprintf("- [ ] %s · from %s · %s%s", now.Format("2006-01-02 15:04"), from, seg, text)
	if _, err := f.WriteString(line + "\n"); err != nil {
		return "", err
	}
	return line, nil
}

// Jot is one parsed handoff task line.
type Jot struct {
	Line    int    `json:"line"` // 1-based, within its file
	Date    string `json:"date"`
	From    string `json:"from"`
	Routine string `json:"routine"`
	Text    string `json:"text"`
}

// Inbox returns the open ("- [ ]") task lines in <vault>/Qilla/Handoff/for-<host>.md,
// in file order. A missing file is not an error — an empty inbox.
func Inbox(root, host string) ([]Jot, error) {
	abs := filepath.Join(root, filepath.FromSlash(handoffPath(host)))
	lines, _, err := readLines(abs)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []Jot
	for i, l := range lines {
		m := handoffLine.FindStringSubmatch(l)
		if m == nil || m[1] != " " {
			continue
		}
		out = append(out, Jot{Line: i + 1, Date: m[2], From: m[3], Routine: m[4], Text: strings.TrimRight(m[5], " \t")})
	}
	return out, nil
}

// MarkDone flips line n (1-based) of for-<host>.md from "- [ ]" to "- [x]" and
// appends " → <result>". Only that line is touched; the write is atomic.
func MarkDone(root, host string, n int, result string) error {
	abs := filepath.Join(root, filepath.FromSlash(handoffPath(host)))
	lines, mode, err := readLines(abs)
	if err != nil {
		return err
	}
	if n < 1 || n > len(lines) {
		return fmt.Errorf("no line %d in %s", n, handoffPath(host))
	}
	l := lines[n-1]
	if !strings.HasPrefix(strings.TrimLeft(l, " \t"), "- [ ] ") {
		return fmt.Errorf("line %d of %s is not an open task: %q", n, handoffPath(host), l)
	}
	out := strings.Replace(l, "- [ ] ", "- [x] ", 1)
	if result != "" {
		out += " → " + result
	}
	lines[n-1] = out
	return writeAtomic(abs, strings.Join(lines, "\n"), mode)
}
