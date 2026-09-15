// Package remind implements time-anchored reminders.
//
// Split of responsibility, and it matters: the reminder ITSELF lives in vault
// markdown — durable knowledge the user reads and edits, from mobile via
// LiveSync, versioned in git. Only the DELIVERY STATE ("did we already tell
// him") lives in a JSON file under the state dir; that is telemetry, and it is
// what stops a 5-minute timer from firing the same reminder 288 times a day.
//
// Syntax, in the todo file or any vault note:
//
//   - [ ] 2026-08-26 · ⏰2026-08-27 · Enable the tick timer
//   - [ ] 2026-08-26 · ⏰2026-08-27 09:30 · Call the dentist
//
// The leading date stays the CREATION date; ⏰ is the due date, optionally with
// a time. No time = due at 00:00.
package remind

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// dueRe matches the ⏰ due stamp, with an optional HH:MM.
var dueRe = regexp.MustCompile(`⏰\s*(\d{4}-\d{2}-\d{2})(?:\s+(\d{1,2}):(\d{2}))?`)

// openLine matches an unticked task line.
var openLine = regexp.MustCompile(`^\s*- \[ \]`)

// prefixRe strips the checkbox and the optional creation date from a line.
var prefixRe = regexp.MustCompile(`^\s*- \[ \]\s*(\d{4}-\d{2}-\d{2}\s*·\s*)?`)

// skipDirs are never walked: dot-directories plus the vault's own attics.
var skipDirs = map[string]bool{".trash": true, ".obsidian": true, "Archive": true, ".smart-env": true}

// Reminder is one open task line carrying a due date.
type Reminder struct {
	Due    time.Time
	Text   string
	Source string // vault-relative path:line
}

// Scan returns every open task line in the vault carrying a ⏰ due date,
// soonest first. Unreadable corners are skipped, not fatal.
func Scan(vault string, loc *time.Location) ([]Reminder, error) {
	if loc == nil {
		loc = time.Local
	}
	var out []Reminder
	err := filepath.WalkDir(vault, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			name := d.Name()
			if path != vault && (strings.HasPrefix(name, ".") || skipDirs[name]) {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(d.Name(), ".md") {
			return nil
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return nil
		}
		rel, _ := filepath.Rel(vault, path)
		for i, line := range strings.Split(string(b), "\n") {
			r, ok := parseLine(line, loc)
			if !ok {
				continue
			}
			r.Source = fmt.Sprintf("%s:%d", rel, i+1)
			out = append(out, r)
		}
		return nil
	})
	sort.SliceStable(out, func(i, j int) bool { return out[i].Due.Before(out[j].Due) })
	return out, err
}

// parseLine reads one markdown line; ok is false unless it is an open task
// line with a ⏰ stamp.
func parseLine(line string, loc *time.Location) (Reminder, bool) {
	if !strings.Contains(line, "⏰") || !openLine.MatchString(line) {
		return Reminder{}, false
	}
	m := dueRe.FindStringSubmatch(line)
	if m == nil {
		return Reminder{}, false
	}
	day, err := time.ParseInLocation("2006-01-02", m[1], loc)
	if err != nil {
		return Reminder{}, false
	}
	due := day
	if m[2] != "" {
		h, _ := strconv.Atoi(m[2])
		min, _ := strconv.Atoi(m[3])
		if h > 23 || min > 59 {
			return Reminder{}, false
		}
		due = time.Date(day.Year(), day.Month(), day.Day(), h, min, 0, 0, loc)
	}
	text := dueRe.ReplaceAllString(line, "")
	text = prefixRe.ReplaceAllString(text, "")
	text = strings.TrimSpace(strings.TrimLeft(strings.TrimSpace(text), "·"))
	return Reminder{Due: due, Text: text}, true
}

// Key is stable per (due, text): editing either is a new reminder, which is right.
func Key(r Reminder) string {
	sum := sha256.Sum256([]byte(r.Due.Format("2006-01-02T15:04:05") + "|" + strings.TrimSpace(r.Text)))
	return hex.EncodeToString(sum[:])[:16]
}

// DueNow returns the reminders due at or before now.
func DueNow(rs []Reminder, now time.Time) []Reminder {
	var out []Reminder
	for _, r := range rs {
		if !r.Due.After(now) {
			out = append(out, r)
		}
	}
	return out
}

// When renders the due date the way a human reads it: "today 09:30",
// "tomorrow", "3d overdue", or the plain date.
func When(r Reminder, now time.Time) string {
	d := r.Due
	switch {
	case d.Before(now) && !sameDay(d, now):
		// Same-day-but-earlier is "today", not "overdue" — a reminder set for
		// 00:00 today has not been missed, it has arrived.
		days := int(now.Sub(d) / (24 * time.Hour))
		if days == 0 {
			days = 1
		}
		return fmt.Sprintf("%dd overdue", days)
	case sameDay(d, now):
		if d.Hour() != 0 || d.Minute() != 0 {
			return "today " + d.Format("15:04")
		}
		return "today"
	case sameDay(d, now.AddDate(0, 0, 1)):
		if d.Hour() != 0 || d.Minute() != 0 {
			return "tomorrow " + d.Format("15:04")
		}
		return "tomorrow"
	}
	return d.Format("2006-01-02")
}

func sameDay(a, b time.Time) bool {
	ay, am, ad := a.Date()
	by, bm, bd := b.Date()
	return ay == by && am == bm && ad == bd
}

// ParseAt reads the --at value: YYYY-MM-DD, optionally with a time.
func ParseAt(s string, loc *time.Location) (time.Time, error) {
	if loc == nil {
		loc = time.Local
	}
	s = strings.TrimSpace(s)
	for _, layout := range []string{"2006-01-02", "2006-01-02 15:04", "2006-01-02T15:04", "2006-01-02 15:04:05", "2006-01-02T15:04:05"} {
		if t, err := time.ParseInLocation(layout, s, loc); err == nil {
			return t, nil
		}
	}
	return time.Time{}, fmt.Errorf("--at must be YYYY-MM-DD or 'YYYY-MM-DD HH:MM', got %q", s)
}

// Line renders the markdown line for a new reminder.
func Line(now, due time.Time, text string) string {
	stamp := due.Format("2006-01-02")
	if due.Hour() != 0 || due.Minute() != 0 {
		stamp += " " + due.Format("15:04")
	}
	return fmt.Sprintf("- [ ] %s · ⏰%s · %s", now.Format("2006-01-02"), stamp, text)
}

// Add appends a reminder line to the todo file and returns it. The write is
// atomic (temp + rename in the same directory): the file is LiveSync-hot and
// must never be observed half-written.
func Add(todoPath string, now, due time.Time, text string) (string, error) {
	line := Line(now, due, text)
	var body string
	mode := os.FileMode(0o644)
	if b, err := os.ReadFile(todoPath); err == nil {
		body = string(b)
		if fi, err := os.Stat(todoPath); err == nil {
			mode = fi.Mode()
		}
	} else if !os.IsNotExist(err) {
		return "", err
	}
	if body != "" && !strings.HasSuffix(body, "\n") {
		body += "\n"
	}
	body += line + "\n"
	if err := os.MkdirAll(filepath.Dir(todoPath), 0o755); err != nil {
		return "", err
	}
	if err := writeAtomic(todoPath, body, mode); err != nil {
		return "", err
	}
	return line, nil
}

func writeAtomic(path, content string, mode os.FileMode) error {
	dir := filepath.Dir(path)
	f, err := os.CreateTemp(dir, "."+filepath.Base(path)+".qilla*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if _, err := f.WriteString(content); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmp, mode.Perm()); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// Delivery is one reminder we already notified about.
type Delivery struct {
	Due    string `json:"due"`
	Text   string `json:"text"`
	Source string `json:"source,omitempty"`
	SentAt string `json:"sent_at"`
}

// State is the delivered-reminders file (reminders.json under the state dir).
type State struct {
	Sent map[string]Delivery `json:"sent"`

	path string
}

// StatePath is where the delivery state lives.
func StatePath(stateDir string) string { return filepath.Join(stateDir, "reminders.json") }

// LoadState reads the delivery state; a missing or corrupt file is an empty one.
func LoadState(path string) (*State, error) {
	s := &State{Sent: map[string]Delivery{}, path: path}
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return s, nil
		}
		return nil, err
	}
	if err := json.Unmarshal(b, s); err != nil {
		// A truncated state file would otherwise block every reminder forever;
		// re-notifying once is the cheaper failure.
		return &State{Sent: map[string]Delivery{}, path: path}, nil
	}
	if s.Sent == nil {
		s.Sent = map[string]Delivery{}
	}
	s.path = path
	return s, nil
}

// IsSent reports whether this reminder was already delivered.
func (s *State) IsSent(key string) bool { _, ok := s.Sent[key]; return ok }

// MarkSent records a delivery.
func (s *State) MarkSent(key string, r Reminder, now time.Time) {
	s.Sent[key] = Delivery{
		Due:    r.Due.Format("2006-01-02T15:04"),
		Text:   r.Text,
		Source: r.Source,
		SentAt: now.Format("2006-01-02T15:04:05"),
	}
}

// Reset drops every delivery whose text contains substr, so they fire again.
func (s *State) Reset(substr string) int {
	n := 0
	for k, d := range s.Sent {
		if strings.Contains(d.Text, substr) {
			delete(s.Sent, k)
			n++
		}
	}
	return n
}

// retention is how long a delivery is remembered. Past it the markdown line is
// either ticked (gone from the scan) or so old that one repeat is harmless.
const retention = 180 * 24 * time.Hour

// Save writes the state atomically, pruning deliveries older than the retention.
func (s *State) Save(now time.Time) error {
	for k, d := range s.Sent {
		if t, err := time.Parse("2006-01-02T15:04:05", d.SentAt); err == nil && now.Sub(t) > retention {
			delete(s.Sent, k)
		}
	}
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return err
	}
	return writeAtomic(s.path, string(b)+"\n", 0o644)
}

// Notify sends one desktop notification. QILLA_NOTIFY_CMD replaces notify-send,
// so a future phone transport drops in without touching this logic; the text is
// appended as the last argument.
func Notify(text string) error {
	argv := []string{"notify-send", "--app-name=qilla", "--icon=appointment-soon", "--urgency=normal", "🌙 qilla", text}
	if cmd := os.Getenv("QILLA_NOTIFY_CMD"); cmd != "" {
		fields, err := splitArgs(cmd)
		if err != nil {
			return err
		}
		argv = append(fields, text)
	}
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr // a custom transport's own output stays visible
	return cmd.Run()
}

// splitArgs is a shell-ish split: whitespace separates, single and double
// quotes group (what shlex.split gave the helper this replaces).
func splitArgs(s string) ([]string, error) {
	var out []string
	var cur strings.Builder
	var quote rune
	started := false
	for _, c := range s {
		switch {
		case quote != 0:
			if c == quote {
				quote = 0
			} else {
				cur.WriteRune(c)
			}
		case c == '\'' || c == '"':
			quote, started = c, true
		case c == ' ' || c == '\t':
			if started {
				out = append(out, cur.String())
				cur.Reset()
				started = false
			}
		default:
			cur.WriteRune(c)
			started = true
		}
	}
	if quote != 0 {
		return nil, fmt.Errorf("unbalanced quote in notify command %q", s)
	}
	if started {
		out = append(out, cur.String())
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("empty notify command")
	}
	return out, nil
}
