// trace.go — the decision trace: one append-only JSON line per `decide ask`.
//
// Privacy rule: the raw text never reaches `decisions.jsonl` — only a sha1 and
// a length, so a trace can be joined to a source without carrying its content.
// The retraining companion (`ask-escapes.jsonl`, unknown outcomes only) is the
// one place that keeps a truncated copy, and it stays local.
//
// Same file names as qilla-deciders/deciders/trace.py: Python and Go write to
// the same logs.

package decide

import (
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	// TraceFile is the trace of every ask, whatever the outcome.
	TraceFile = "decisions.jsonl"
	// AskEscapesFile holds unknown outcomes only, with truncated text —
	// retraining material.
	AskEscapesFile = "ask-escapes.jsonl"
	// EscapeTextChars of the original text are kept in AskEscapesFile.
	EscapeTextChars = 500
)

// TraceLine is one trace record — hashes and lengths, never the text.
type TraceLine struct {
	TS            string             `json:"ts"`
	Kind          string             `json:"kind"`
	Options       []string           `json:"options"`
	QuestionSHA1  *string            `json:"question_sha1"`
	TextSHA1      *string            `json:"text_sha1"`
	TextLen       int                `json:"text_len"`
	Label         string             `json:"label"`
	Conf          float64            `json:"conf"`
	Dist          map[string]float64 `json:"dist"`
	Route         string             `json:"route"`
	Model         string             `json:"model"`
	Floor         float64            `json:"floor"`
	PolicyVersion string             `json:"policy_version"`
	MS            int                `json:"ms"`
	Caller        string             `json:"caller"`
	// Tokens are the remote route's billed input tokens; the local route
	// bills none, so the field is left off its lines.
	Tokens int `json:"tokens,omitempty"`
}

// escapeLine is a trace line plus the truncated text, for ask-escapes.jsonl.
type escapeLine struct {
	TraceLine
	Text string `json:"text"`
}

// TraceInput is what TraceRecord needs; Text is hashed, never stored.
type TraceInput struct {
	Kind          string
	Options       []string
	Question      string
	Text          string
	Label         string
	Conf          float64
	Dist          map[string]float64
	Route         string
	Model         string
	Floor         float64
	PolicyVersion string
	MS            int
	Caller        string
	Tokens        int
}

// SHA1Hex is the hex sha1 of a string.
func SHA1Hex(s string) string {
	sum := sha1.Sum([]byte(s))
	return hex.EncodeToString(sum[:])
}

func sha1Ptr(s string) *string {
	if s == "" {
		return nil // Python records a missing question as null
	}
	h := SHA1Hex(s)
	return &h
}

// TraceRecord builds the trace line for one decision.
func TraceRecord(in TraceInput) TraceLine {
	opts := in.Options
	if opts == nil {
		opts = []string{}
	}
	textHash := SHA1Hex(in.Text)
	return TraceLine{
		TS:            time.Now().UTC().Format("2006-01-02T15:04:05Z07:00"),
		Kind:          in.Kind,
		Options:       opts,
		QuestionSHA1:  sha1Ptr(in.Question),
		TextSHA1:      &textHash,
		TextLen:       len([]rune(in.Text)),
		Label:         in.Label,
		Conf:          in.Conf,
		Dist:          in.Dist,
		Route:         in.Route,
		Model:         in.Model,
		Floor:         in.Floor,
		PolicyVersion: in.PolicyVersion,
		MS:            in.MS,
		Caller:        in.Caller,
		Tokens:        in.Tokens,
	}
}

// CountRouteToday counts today's trace lines for one route — what the jev
// daily cap is checked against. A missing or unreadable trace counts zero:
// the cap never blocks a decision because a file is absent.
func CountRouteToday(dir, route string) int {
	data, err := os.ReadFile(filepath.Join(dir, TraceFile))
	if err != nil {
		return 0
	}
	day := time.Now().UTC().Format("2006-01-02")
	n := 0
	for _, ln := range strings.Split(string(data), "\n") {
		if strings.TrimSpace(ln) == "" {
			continue
		}
		var row struct {
			TS    string `json:"ts"`
			Route string `json:"route"`
		}
		if err := json.Unmarshal([]byte(ln), &row); err != nil {
			continue
		}
		if row.Route == route && strings.HasPrefix(row.TS, day) {
			n++
		}
	}
	return n
}

func appendJSONL(path string, v any) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err := f.Write(append(b, '\n')); err != nil {
		return err
	}
	return nil
}

// AppendTrace writes the trace line under dir and — when the outcome is
// unknown — the escape with the text truncated. `line` itself must already be
// free of raw text.
func AppendTrace(dir string, line TraceLine, text string) error {
	if err := appendJSONL(filepath.Join(dir, TraceFile), line); err != nil {
		return err
	}
	if line.Label != Unknown {
		return nil
	}
	r := []rune(text)
	if len(r) > EscapeTextChars {
		r = r[:EscapeTextChars]
	}
	return appendJSONL(filepath.Join(dir, AskEscapesFile), escapeLine{TraceLine: line, Text: string(r)})
}

// TraceTail returns the last n raw trace lines under dir, oldest first; n <= 0
// returns all of them, and no trace is no lines, not an error.
func TraceTail(dir string, n int) ([]string, error) {
	data, err := os.ReadFile(filepath.Join(dir, TraceFile))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var lines []string
	for _, ln := range strings.Split(string(data), "\n") {
		if strings.TrimSpace(ln) != "" {
			lines = append(lines, ln)
		}
	}
	if n > 0 && len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return lines, nil
}
