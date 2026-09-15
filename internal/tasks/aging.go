package tasks

import (
	"fmt"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

// datedOpen matches an open task line that carries its own date:
//
//   - [ ] 2026-08-26 · Chase the benchmark table
var datedOpen = regexp.MustCompile(`^\s*- \[ \] (\d{4}-\d{2}-\d{2}) · `)

// danglingLink is a wikilink cut in half by truncation. Left in place it would
// emit broken markdown into the briefing, so the remains are dropped.
var danglingLink = regexp.MustCompile(`\[\[[^\]]*$`)

// Commitment is one open dated task line, with its age in days.
type Commitment struct {
	Age  int    // whole days since its date, never negative
	File string // vault-relative
	Line int    // 1-based
	What string // the text after the date, truncated to 110 runes
}

// Aging returns every open dated task in the vault, oldest first.
//
// Desk/Questions.md is excluded on purpose: the briefing surfaces the
// open-questions queue separately, and counting it here would report the same
// debt twice. Archive/ is excluded too — archived promises are not live debt.
func Aging(root string, now time.Time) ([]Commitment, error) {
	var out []Commitment
	err := walk(root, func(path string) error {
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return nil
		}
		rel = filepath.ToSlash(rel)
		if rel == "Desk/Questions.md" || strings.Contains(rel+"/", "/Archive/") {
			return nil
		}
		lines, _, err := readLines(path)
		if err != nil {
			return nil
		}
		for i, l := range lines {
			m := datedOpen.FindStringSubmatch(l)
			if m == nil {
				continue
			}
			d, err := time.ParseInLocation("2006-01-02", m[1], now.Location())
			if err != nil {
				continue
			}
			age := int(now.Sub(d) / (24 * time.Hour))
			if age < 0 {
				age = 0
			}
			out = append(out, Commitment{Age: age, File: rel, Line: i + 1, What: what(l[len(m[0]):])})
		}
		return nil
	})
	sort.Slice(out, func(i, j int) bool {
		if out[i].Age != out[j].Age {
			return out[i].Age > out[j].Age
		}
		if out[i].File != out[j].File {
			return out[i].File < out[j].File
		}
		return out[i].Line < out[j].Line
	})
	return out, err
}

// what is the task text: one line, capped at 110 runes, with a wikilink the cap
// may have sliced in half removed.
func what(text string) string {
	text = truncate(strings.TrimRight(text, "\r"), 110)
	text = danglingLink.ReplaceAllString(text, "")
	return strings.TrimRight(text, " \t")
}

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}

// Flag is the aging marker a commitment carries: 🔴 stale, 🟠 past the warn
// line, · still on schedule.
func (c Commitment) Flag(warn, stale int) string {
	switch {
	case c.Age >= stale:
		return "🔴"
	case c.Age >= warn:
		return "🟠"
	}
	return "·"
}

// Brief is the compact wikilinked line the briefing quotes.
func (c Commitment) Brief(warn, stale int) string {
	return fmt.Sprintf("%s **%dd** — %s ([[%s]])", c.Flag(warn, stale), c.Age, c.What, strings.TrimSuffix(c.File, ".md"))
}

// Row is the human table line.
func (c Commitment) Row(warn, stale int) string {
	base := strings.TrimSuffix(filepath.Base(c.File), ".md")
	return fmt.Sprintf("%s %3dd  %-30s %s", c.Flag(warn, stale), c.Age, base, c.What)
}

// openTask counts every open task line, dated or not.
var openTask = regexp.MustCompile(`^[ \t]*- \[ \] `)

// SummaryLine builds Todo.md's own "> [!summary]" callout from ground truth:
// how many tasks are open, and the two oldest ones with an aging pill. Only
// tasks living in todo itself are pilled — cross-vault commitments (meeting
// notes, project notes) belong in the briefing's commitments line, not here.
func SummaryLine(todoLines []string, own []Commitment, warn, stale int) string {
	total := 0
	for _, l := range todoLines {
		if openTask.MatchString(l) {
			total++
		}
	}
	var pills []string
	for i, c := range own {
		if i >= 2 {
			break
		}
		colour := "muted"
		switch {
		case c.Age >= stale:
			colour = "red"
		case c.Age >= warn:
			colour = "amber"
		}
		short := c.What
		if len([]rune(short)) > 40 {
			short = truncate(short, 37) + "..."
		}
		pills = append(pills, fmt.Sprintf("<span class=\"k-pill %s\">%d d</span> %s", colour, c.Age, short))
	}
	line := fmt.Sprintf("> [!summary] %d open", total)
	if len(pills) > 0 {
		line += " · " + strings.Join(pills, " · ")
	}
	if len(own) > 2 {
		line += " · rest on schedule"
	}
	return line
}

var summaryCallout = regexp.MustCompile(`^> \[!summary\]`)

// Summarize rewrites the first "> [!summary]" callout of the vault-relative
// todo file from ground truth and returns the line it wrote. The write is
// atomic (temp + rename) and preserves the file mode.
func Summarize(root, todoRel string, now time.Time, warn, stale int) (string, error) {
	path := filepath.Join(root, filepath.FromSlash(todoRel))
	lines, mode, err := readLines(path)
	if err != nil {
		return "", fmt.Errorf("no %s", todoRel)
	}
	all, err := Aging(root, now)
	if err != nil {
		return "", err
	}
	var own []Commitment
	for _, c := range all {
		if c.File == todoRel {
			own = append(own, c)
		}
	}
	line := SummaryLine(lines, own, warn, stale)
	found := false
	for i, l := range lines {
		if summaryCallout.MatchString(l) {
			lines[i], found = line, true
			break
		}
	}
	if !found {
		return "", fmt.Errorf("%s has no \"> [!summary]\" callout to rewrite", todoRel)
	}
	if err := writeAtomic(path, strings.Join(lines, "\n"), mode); err != nil {
		return "", err
	}
	return line, nil
}
