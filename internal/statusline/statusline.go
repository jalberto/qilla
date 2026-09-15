// Package statusline renders qilla's Claude Code status line: the bar is built
// from the session JSON Claude Code puts on stdin plus a few cheap counts over
// the vault. Everything here is pure — the gathering lives in cmd/qilla.
package statusline

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"regexp"
	"strconv"
	"strings"
)

// Input is the session JSON Claude Code feeds the status line command.
type Input struct {
	Model struct {
		DisplayName string `json:"display_name"`
	} `json:"model"`
	Workspace struct {
		CurrentDir string `json:"current_dir"`
	} `json:"workspace"`
	ContextWindow struct {
		UsedPercentage float64 `json:"used_percentage"`
	} `json:"context_window"`
	RateLimits struct {
		SevenDay struct {
			UsedPercentage float64 `json:"used_percentage"`
		} `json:"seven_day"`
	} `json:"rate_limits"`
}

// Parse reads the session JSON. A malformed or empty payload still yields a
// usable bar (model "?", no percentages).
func Parse(r io.Reader) Input {
	raw, _ := io.ReadAll(r)
	var in Input
	json.Unmarshal(raw, &in)
	if in.Model.DisplayName == "" {
		in.Model.DisplayName = "?"
	}
	return in
}

// Pct truncates a percentage to an int; 0 or less means the field was absent
// (Claude Code omits it rather than sending a zero), reported as -1.
func Pct(v float64) int {
	if v <= 0 {
		return -1
	}
	return int(v)
}

// View is everything the bar shows. Counts below 1 and percentages below 0 are
// omitted, exactly like the shell version.
type View struct {
	Qilla     bool // QILLA_RUN: wear the purple badge
	Who       string
	Model     string
	Usage     int // weekly usage %, -1 unknown
	Context   int // context window %, -1 unknown
	Services  int // hard-failing doctor checks
	Reminders int
	Questions int
	Jots      int
}

const reset = "\033[0m"

// Render builds the bar (no trailing newline). ANSI matches the shell status
// line it replaces: red above the upper threshold, yellow above the lower one.
func Render(v View) string {
	var b strings.Builder
	if v.Qilla {
		fmt.Fprintf(&b, "\033[45;30m ◆ qilla \033[0m \033[1;35m%s\033[0m · %s", v.Who, v.Model)
	} else {
		fmt.Fprintf(&b, "🌙 \033[1;35mqilla\033[0m · %s", v.Model)
	}
	if v.Usage >= 0 {
		b.WriteString(pctSeg("⚡", v.Usage, 70, 81))
	}
	if v.Context >= 0 {
		b.WriteString(pctSeg("◔", v.Context, 65, 85))
	}
	if v.Services > 0 {
		fmt.Fprintf(&b, " · \033[1;31m🚨%d%s", v.Services, reset)
	}
	if v.Reminders > 0 {
		fmt.Fprintf(&b, " · \033[1;31m🔔%d%s", v.Reminders, reset)
	}
	if v.Questions > 0 {
		fmt.Fprintf(&b, " · ❓%d", v.Questions)
	}
	if v.Jots > 0 {
		fmt.Fprintf(&b, " · 📋%d", v.Jots)
	}
	return b.String()
}

func pctSeg(icon string, pct, yellow, red int) string {
	switch {
	case pct >= red:
		return fmt.Sprintf(" · \033[1;31m%s%d%%%s", icon, pct, reset)
	case pct >= yellow:
		return fmt.Sprintf(" · \033[1;33m%s%d%%%s", icon, pct, reset)
	}
	return fmt.Sprintf(" · %s%d%%", icon, pct)
}

var openTaskRe = regexp.MustCompile(`^\s*- \[ \]`)

// CountOpenTasks counts unchecked task lines (the questions queue); a missing
// file counts zero.
func CountOpenTasks(path string) int {
	return countLines(path, func(line string) bool { return openTaskRe.MatchString(line) })
}

// InboxHeading is the daily-note section jots land in.
const InboxHeading = "## 📥 Inbox"

// CountInboxJots counts bullet lines under the Inbox heading of a daily note,
// up to the next heading.
func CountInboxJots(path string) int {
	in := false
	return countLines(path, func(line string) bool {
		switch {
		case strings.HasPrefix(line, InboxHeading):
			in = true
			return false
		case strings.HasPrefix(line, "## "):
			in = false
			return false
		}
		return in && strings.HasPrefix(line, "- ")
	})
}

func countLines(path string, match func(string) bool) int {
	f, err := os.Open(path)
	if err != nil {
		return 0
	}
	defer f.Close()
	n := 0
	s := bufio.NewScanner(f)
	s.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for s.Scan() {
		if match(s.Text()) {
			n++
		}
	}
	return n
}

// Count parses a helper's stdout as a count; anything unparseable is zero.
func Count(out string) int {
	line := strings.TrimSpace(out)
	if i := strings.IndexByte(line, '\n'); i >= 0 {
		line = strings.TrimSpace(line[:i])
	}
	n, err := strconv.Atoi(line)
	if err != nil || n < 0 {
		return 0
	}
	return n
}
