// Package catchup answers one question for an agent starting work: what
// changed in the vault since I last looked? It reads the daily notes and the
// questions queue and returns only the deltas, so a session spends a few
// hundred characters instead of re-reading a 50k daily note.
package catchup

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/jalberto/qilla/internal/vault"
)

// Section headings of the daily note, matched emoji- and case-insensitively.
const (
	InboxHeading = "Inbox"
	LogHeading   = "Log"
	TodayHeading = "Today"
)

// Limits on what a line may cost the reader.
const (
	answerStemMax = 120
	openStemMax   = 100
)

// StateMax caps a state row's text: enough to orient, never a paragraph.
const StateMax = 200

// StateRow is one working-memory state entry the agent carries between
// sessions (a routine's last outcome, a session's handoff).
type StateRow struct {
	Key  string
	Text string
}

// Jot is an unrouted 📥 Inbox bullet.
type Jot struct {
	Date string
	Text string
}

// Answer is a question the owner has replied to.
type Answer struct {
	Stem  string
	Reply string
}

// LogLine is a 📓 Log bullet not yet seen by this agent.
type LogLine struct {
	Date string
	Text string
}

// Result is everything new, already truncated for printing.
type Result struct {
	Jots    []Jot
	Answers []Answer
	Opens   []string // open-question stems, default text stripped
	Logs    []LogLine
	// LogSeen is the new per-date Log line count to persist after printing.
	LogSeen map[string]int
}

// Total is the header's n=.
func (r Result) Total() int {
	return len(r.Jots) + len(r.Answers) + len(r.Opens) + len(r.Logs)
}

var routedRe = regexp.MustCompile(`→ \[\[`)

// DefaultReplyMarker is used when the config leaves reply_marker empty.
const DefaultReplyMarker = "💬 owner:"

// replyPattern builds the "> <marker> <text>" matcher for a configured marker.
func replyPattern(marker string) *regexp.Regexp {
	if strings.TrimSpace(marker) == "" {
		marker = DefaultReplyMarker
	}
	return regexp.MustCompile(`^\s*>\s*` + regexp.QuoteMeta(marker) + `\s*(.*)$`)
}

// Days returns the vault-relative daily notes to scan: today and yesterday.
func Days(journalDir string, now time.Time) []string {
	return []string{
		filepath.Join(journalDir, now.Format("2006-01-02")+".md"),
		filepath.Join(journalDir, now.AddDate(0, 0, -1).Format("2006-01-02")+".md"),
	}
}

// Scan collects the delta. seen is the per-date Log line count from the last
// run (nil or all=true means "everything is new").
// replyMarker is the prefix of an owner reply under a queued question
// (config reply_marker); "" falls back to DefaultReplyMarker.
func Scan(vaultDir, journalDir, questionsRel, replyMarker string, now time.Time, seen map[string]int, all bool) Result {
	r := Result{LogSeen: map[string]int{}}
	for _, rel := range Days(journalDir, now) {
		date := strings.TrimSuffix(filepath.Base(rel), ".md")
		lines := readLines(filepath.Join(vaultDir, rel))
		if lines == nil {
			continue
		}
		for _, b := range vault.Bullets(lines, InboxHeading) {
			if routedRe.MatchString(b) {
				continue
			}
			r.Jots = append(r.Jots, Jot{Date: date, Text: bulletText(b)})
		}
		logs := vault.Bullets(lines, LogHeading)
		r.LogSeen[date] = len(logs)
		from := 0
		if !all {
			from = seen[date]
		}
		if from > len(logs) { // the note was edited down; show nothing rather than lie
			from = len(logs)
		}
		for _, l := range logs[from:] {
			r.Logs = append(r.Logs, LogLine{Date: date, Text: bulletText(l)})
		}
	}
	r.Answers, r.Opens = questions(filepath.Join(vaultDir, questionsRel), replyMarker)
	return r
}

var (
	openTaskRe  = regexp.MustCompile(`^- \[ \]\s*`)
	quoteRe     = regexp.MustCompile(`^\s*>\s?(.*)$`)
	defaultRe   = regexp.MustCompile(`\*\*Default taken:.*?\*\*`)
	whitespaceR = regexp.MustCompile(`\s+`)
)

// questions splits the queue into answered (stem + reply) and still-open stems.
func questions(path, replyMarker string) ([]Answer, []string) {
	replyRe := replyPattern(replyMarker)
	lines := readLines(path)
	var answers []Answer
	var opens []string
	for i := 0; i < len(lines); i++ {
		if !openTaskRe.MatchString(lines[i]) {
			continue
		}
		stem := openTaskRe.ReplaceAllString(lines[i], "")
		reply := ""
		for j := i + 1; j < len(lines); j++ {
			m := replyRe.FindStringSubmatch(lines[j])
			if m == nil {
				if strings.TrimSpace(lines[j]) == "" || openTaskRe.MatchString(lines[j]) {
					break
				}
				continue
			}
			reply = strings.TrimSpace(m[1])
			// a reply may continue on further quoted lines
			for k := j + 1; k < len(lines); k++ {
				c := quoteRe.FindStringSubmatch(lines[k])
				if c == nil || replyRe.MatchString(lines[k]) {
					break
				}
				reply = strings.TrimSpace(reply + " " + strings.TrimSpace(c[1]))
			}
			break
		}
		if reply != "" {
			answers = append(answers, Answer{
				Stem:  vault.Truncate(clean(stem), answerStemMax),
				Reply: clean(reply),
			})
			continue
		}
		opens = append(opens, vault.Truncate(clean(defaultRe.ReplaceAllString(stem, "")), openStemMax))
	}
	return answers, opens
}

// bulletText strips the "- " marker and flattens the line for TSV.
func bulletText(b string) string { return clean(strings.TrimPrefix(b, "- ")) }

func clean(s string) string {
	return strings.TrimSpace(whitespaceR.ReplaceAllString(strings.ReplaceAll(s, "\t", " "), " "))
}

func readLines(path string) []string {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	_, body := vault.SplitFrontmatter(string(b))
	return vault.Lines(body)
}
