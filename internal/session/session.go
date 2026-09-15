// Package session decides whether qilla attaches to an agent's existing main
// Claude session or rotates to a fresh one.
//
// A never-compacting session re-sends its whole transcript on every turn, so
// the cost of resuming grows without bound while the value of the old context
// decays. Past [chat].session_max_idle of inactivity qilla starts a new session
// and seeds it with a small handoff (last exchange + working memory) instead.
package session

import (
	"bufio"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// Store reads and rotates the `sessions` table (agent → resumable session id).
type Store struct {
	DB  *sql.DB
	Now func() time.Time
}

// Open prepares the rotation history table. The live pointer table `sessions`
// is created by the worker.
func Open(db *sql.DB) (*Store, error) {
	_, err := db.Exec(`
CREATE TABLE IF NOT EXISTS sessions_retired(
  agent TEXT NOT NULL,
  session_id TEXT NOT NULL,
  last_used INTEGER NOT NULL,
  superseded INTEGER NOT NULL,
  successor TEXT NOT NULL DEFAULT '',
  PRIMARY KEY(agent, session_id));`)
	if err != nil {
		return nil, err
	}
	return &Store{DB: db, Now: time.Now}, nil
}

func (s *Store) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

// Decision is what the caller should do about the agent's session.
type Decision struct {
	Resume  string        // session id to resume; "" = start fresh
	New     string        // minted session id for the fresh session (rotations only)
	Old     string        // the session rotated away from
	Idle    time.Duration // how long Old had been idle
	Rotated bool
	Handoff string // seed text for the new session; "" when there is nothing to carry
	State   string // the same handoff as terse working-memory state lines; "" when there is nothing to carry
}

// Line is the one-line rotation notice for stderr/logs.
func (d Decision) Line() string {
	return fmt.Sprintf("session rotated: %s → %s (idle %s)", short(d.Old), short(d.New), d.Idle.Round(time.Minute))
}

// Recall returns working memory worth carrying into the new session. Optional.
type Recall func(query string) string

// Attach resolves the agent's session. maxIdle <= 0 never rotates. workdir is
// the directory the session runs in, used to locate its transcript.
//
// On a rotation the live row is repointed at a freshly minted session id, so
// every entry point (chat, ask, web) lands on the same new session; the old row
// is kept in sessions_retired, never deleted.
func (s *Store) Attach(agent, workdir string, maxIdle time.Duration, recall Recall) Decision {
	sid := s.Current(agent)
	if sid == "" || maxIdle <= 0 {
		return Decision{Resume: sid}
	}
	last := s.lastActivity(agent, workdir, sid)
	if last.IsZero() {
		return Decision{Resume: sid}
	}
	idle := s.now().Sub(last)
	if idle <= maxIdle {
		return Decision{Resume: sid}
	}
	d := Decision{New: NewID(), Old: sid, Idle: idle, Rotated: true}
	user, assistant := lastExchange(TranscriptPath(workdir, sid))
	d.Handoff = handoff(user, assistant, recall)
	d.State = StateText(user, assistant, s.now())
	s.rotate(agent, sid, last, d.New)
	return d
}

// Current is the agent's live session id ("" before the first run).
func (s *Store) Current(agent string) string {
	var sid string
	s.DB.QueryRow(`SELECT session_id FROM sessions WHERE agent=?`, agent).Scan(&sid)
	return sid
}

// Set points the agent at a session, backfilling the successor of the row it
// most recently superseded so the history chains.
func (s *Store) Set(agent, sid string) {
	now := s.now().Unix()
	s.DB.Exec(`INSERT INTO sessions(agent,session_id,updated) VALUES(?,?,?)
ON CONFLICT(agent) DO UPDATE SET session_id=excluded.session_id, updated=excluded.updated`, agent, sid, now)
	s.DB.Exec(`UPDATE sessions_retired SET successor=? WHERE agent=? AND successor='' AND session_id<>?`, sid, agent, sid)
}

func (s *Store) rotate(agent, old string, last time.Time, newID string) {
	s.DB.Exec(`INSERT INTO sessions_retired(agent,session_id,last_used,superseded,successor) VALUES(?,?,?,?,?)
ON CONFLICT(agent,session_id) DO UPDATE SET superseded=excluded.superseded, successor=excluded.successor`,
		agent, old, last.Unix(), s.now().Unix(), newID)
	s.DB.Exec(`INSERT INTO sessions(agent,session_id,updated) VALUES(?,?,?)
ON CONFLICT(agent) DO UPDATE SET session_id=excluded.session_id, updated=excluded.updated`, agent, newID, s.now().Unix())
}

// Retired is one superseded session, newest first.
type Retired struct {
	Agent      string
	SessionID  string
	LastUsed   time.Time
	Superseded time.Time
	Successor  string
}

// Rotated lists superseded sessions, newest first.
func (s *Store) Rotated() []Retired {
	rows, err := s.DB.Query(`SELECT agent,session_id,last_used,superseded,successor FROM sessions_retired ORDER BY superseded DESC`)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []Retired
	for rows.Next() {
		var r Retired
		var lu, sup int64
		if rows.Scan(&r.Agent, &r.SessionID, &lu, &sup, &r.Successor) != nil {
			continue
		}
		r.LastUsed, r.Superseded = time.Unix(lu, 0), time.Unix(sup, 0)
		out = append(out, r)
	}
	return out
}

// lastActivity is the transcript's mtime, or the ledger's last-used stamp when
// there is no transcript (a session started elsewhere, or an expired one).
func (s *Store) lastActivity(agent, workdir, sid string) time.Time {
	if fi, err := os.Stat(TranscriptPath(workdir, sid)); err == nil {
		return fi.ModTime()
	}
	var updated int64
	s.DB.QueryRow(`SELECT updated FROM sessions WHERE agent=?`, agent).Scan(&updated)
	if updated == 0 {
		return time.Time{}
	}
	return time.Unix(updated, 0)
}

// handoffChars caps each carried-over turn.
const handoffChars = 2000

// handoff summarises the session being left behind: the last thing the user asked,
// the last thing the agent answered, and whatever working memory recall offers.
func handoff(user, assistant string, recall Recall) string {
	var b strings.Builder
	b.WriteString("<!-- qilla: previous session rotated out (idle); this is the handoff, not live context -->\n")
	if user != "" {
		fmt.Fprintf(&b, "Last request in the previous session:\n%s\n\n", clip(user, handoffChars))
	}
	if assistant != "" {
		fmt.Fprintf(&b, "Your last answer there:\n%s\n\n", clip(assistant, handoffChars))
	}
	if recall != nil {
		if m := strings.TrimSpace(recall(user)); m != "" {
			fmt.Fprintf(&b, "Working memory:\n%s\n\n", m)
		}
	}
	if user == "" && assistant == "" && !strings.Contains(b.String(), "Working memory") {
		return ""
	}
	return b.String()
}

// HandoffKey is the working-memory key a rotated session's handoff is stored
// under, one per agent (kind state, so the last write wins).
func HandoffKey(agent string) string { return agent + "/handoff" }

// Caps on the state entry as a whole and on each of its fields.
const (
	stateChars = 2000
	goalChars  = 300
	lastChars  = 1000
	pendChars  = 300
)

// StateText renders the rotation handoff as the terse lines working memory
// keeps: what the session was for, where it got to, what is still open.
// Empty when there is nothing to carry.
func StateText(user, assistant string, now time.Time) string {
	goal, last := clip(flatten(user), goalChars), clip(flatten(assistant), lastChars)
	if goal == "" && last == "" {
		return ""
	}
	if goal == "" {
		goal = "-"
	}
	if last == "" {
		last = "-"
	}
	return clip(fmt.Sprintf("goal: %s\nlast: %s\npending: %s\nat: %s",
		goal, last, pendingFrom(assistant), now.Format(time.RFC3339)), stateChars)
}

// pendingFrom is the last question or offer left hanging in the final answer,
// or "-" when it closed cleanly.
func pendingFrom(assistant string) string {
	out := "-"
	for _, l := range strings.Split(assistant, "\n") {
		l = strings.TrimLeft(strings.TrimSpace(l), "-*>#0123456789. ")
		l = strings.TrimSpace(l)
		if l == "" {
			continue
		}
		if strings.Contains(l, "?") || offerRe.MatchString(l) {
			out = clip(l, pendChars)
		}
	}
	return out
}

// offerRe matches an answer that ends in an offer rather than a question mark.
var offerRe = regexp.MustCompile(`(?i)^(want me|shall i|should i|let me know|tell me|say the word|i can )`)

// flatten collapses a message to one line: state entries are read line by line.
func flatten(s string) string { return strings.TrimSpace(spaceRe.ReplaceAllString(s, " ")) }

var spaceRe = regexp.MustCompile(`\s+`)

// lastExchange scans a transcript JSONL for the final typed user message and
// the final assistant text, skipping tool traffic and injected context.
func lastExchange(path string) (user, assistant string) {
	f, err := os.Open(path)
	if err != nil {
		return "", ""
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 1<<20), 16<<20)
	for sc.Scan() {
		var l struct {
			Type    string `json:"type"`
			Message struct {
				Content json.RawMessage `json:"content"`
			} `json:"message"`
		}
		if json.Unmarshal(sc.Bytes(), &l) != nil {
			continue
		}
		if l.Type != "user" && l.Type != "assistant" {
			continue
		}
		txt := text(l.Message.Content)
		if txt == "" {
			continue
		}
		if l.Type == "user" {
			user, assistant = txt, "" // a new request invalidates the previous answer
		} else {
			assistant = txt
		}
	}
	return user, assistant
}

// text flattens a message content field to its plain text, dropping tool_use,
// tool_result, thinking and injected <system-reminder>/<command-*> plumbing.
func text(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return typed(s)
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &parts) != nil {
		return ""
	}
	var out []string
	for _, p := range parts {
		if p.Type == "text" {
			if t := typed(p.Text); t != "" {
				out = append(out, t)
			}
		}
	}
	return strings.TrimSpace(strings.Join(out, "\n"))
}

var plumbing = []string{"system-reminder", "command-name", "command-message", "command-args", "local-command-stdout"}

func typed(s string) string {
	for _, tag := range plumbing {
		for {
			i := strings.Index(s, "<"+tag+">")
			j := strings.Index(s, "</"+tag+">")
			if i < 0 || j < i {
				break
			}
			s = s[:i] + s[j+len(tag)+3:]
		}
	}
	return strings.TrimSpace(s)
}

func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

func short(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

// NewID mints a v4 UUID, the shape Claude Code's --session-id accepts.
func NewID() string {
	var b [16]byte
	rand.Read(b[:])
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	h := hex.EncodeToString(b[:])
	return h[:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:]
}

// TranscriptPath is where Claude Code keeps a session's JSONL for a workdir.
func TranscriptPath(workdir, sid string) string {
	dir := os.Getenv("CLAUDE_CONFIG_DIR")
	if dir == "" {
		home, _ := os.UserHomeDir()
		dir = filepath.Join(home, ".claude")
	}
	return filepath.Join(dir, "projects", ProjectSlug(workdir), sid+".jsonl")
}

// ProjectSlug mirrors Claude Code's directory naming: every non-alphanumeric
// byte of the absolute workdir becomes "-" (/home/alice/Notes → -home-alice-Notes).
func ProjectSlug(dir string) string {
	var b strings.Builder
	for _, c := range dir {
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') {
			b.WriteRune(c)
		} else {
			b.WriteByte('-')
		}
	}
	return b.String()
}
