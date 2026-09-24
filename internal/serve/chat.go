package serve

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/jalberto/qilla/internal/procs"
	"github.com/jalberto/qilla/internal/session"
)

// The web Chat tab is a view on the agent's real session — the transcript
// Claude Code appends to at ~/.claude/projects/<cwd-slug>/<session>.jsonl —
// so turns typed in `qilla chat`, from Remote Control, or queued from the
// page all show up in one place. qilla only reads this file; it never writes it.

// Turn is one rendered conversation entry.
type Turn struct {
	UUID     string     `json:"uuid"`
	Role     string     `json:"role"` // user | assistant
	Text     string     `json:"text"`
	Thinking string     `json:"thinking,omitempty"` // reasoning blocks, shown collapsed
	Tools    int        `json:"tools,omitempty"`    // tool calls in this assistant turn (text may be empty)
	Calls    []ToolCall `json:"calls,omitempty"`    // what those calls were, shown collapsed
	Model    string     `json:"model,omitempty"`
	Time     time.Time  `json:"time"`
}

// ToolCall is one tool_use, reduced to what a human skims: the tool and a
// short slice of its input.
type ToolCall struct {
	Name  string `json:"name"`
	Input string `json:"input,omitempty"`
}

// transcriptPath is where Claude Code keeps the session for a workdir.
func transcriptPath(workdir, sid string) string { return session.TranscriptPath(workdir, sid) }

// projectSlug mirrors Claude Code's directory naming.
func projectSlug(dir string) string { return session.ProjectSlug(dir) }

type transcriptLine struct {
	Type      string    `json:"type"`
	UUID      string    `json:"uuid"`
	Timestamp time.Time `json:"timestamp"`
	Message   struct {
		Role    string          `json:"role"`
		Model   string          `json:"model"`
		Content json.RawMessage `json:"content"`
	} `json:"message"`
}

type contentPart struct {
	Type     string          `json:"type"`
	Text     string          `json:"text"`
	Thinking string          `json:"thinking"`
	Name     string          `json:"name"`
	Input    json.RawMessage `json:"input"`
}

// callSummary flattens a tool input to one short line: the command for Bash,
// the path for file tools, the first values otherwise.
func callSummary(in json.RawMessage) string {
	var m map[string]any
	if json.Unmarshal(in, &m) != nil || len(m) == 0 {
		return ""
	}
	for _, k := range []string{"command", "file_path", "pattern", "query", "url", "prompt", "description"} {
		if v, ok := m[k].(string); ok && v != "" {
			return clip(v, 160)
		}
	}
	b, _ := json.Marshal(m)
	return clip(string(b), 160)
}

func clip(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}

// parseTurn turns one JSONL line into a Turn; ok=false for anything that is
// not a user/assistant message with something to show (tool results, system
// events, cost-state, mode changes…).
func parseTurn(line []byte) (Turn, bool) {
	var l transcriptLine
	if json.Unmarshal(line, &l) != nil || (l.Type != "user" && l.Type != "assistant") {
		return Turn{}, false
	}
	t := Turn{UUID: l.UUID, Role: l.Type, Time: l.Timestamp, Model: l.Message.Model}
	var s string
	if json.Unmarshal(l.Message.Content, &s) == nil {
		t.Text = s
	} else {
		var parts []contentPart
		if json.Unmarshal(l.Message.Content, &parts) != nil {
			return Turn{}, false
		}
		var texts, thoughts []string
		for _, p := range parts {
			switch p.Type {
			case "text":
				if strings.TrimSpace(p.Text) != "" {
					texts = append(texts, p.Text)
				}
			case "thinking":
				if strings.TrimSpace(p.Thinking) != "" {
					thoughts = append(thoughts, strings.TrimSpace(p.Thinking))
				}
			case "tool_use":
				t.Tools++
				t.Calls = append(t.Calls, ToolCall{Name: p.Name, Input: callSummary(p.Input)})
			}
		}
		t.Text = strings.Join(texts, "\n")
		t.Thinking = strings.Join(thoughts, "\n\n")
	}
	t.Text = strings.TrimSpace(t.Text)
	if t.Role == "user" {
		t.Text = stripInjected(t.Text)
		if t.Text == "" {
			return Turn{}, false // tool results and injected context, not something the user typed
		}
	}
	if t.Role == "assistant" && t.Text == "" && t.Tools == 0 && t.Thinking == "" {
		return Turn{}, false
	}
	return t, true
}

// injectedTags open the blocks Claude Code prepends to a user turn (context,
// slash-command plumbing); they are not what the person typed.
var injectedTags = []string{"<system-reminder", "<command-name", "<command-message", "<command-args", "<local-command", "<bash-input", "<bash-stdout", "<bash-stderr", "<task-notification"}

// stripInjected removes leading injected blocks and returns what remains; a
// turn that is nothing but injected context comes back empty. A message that
// merely starts with "<" (pasted XML, a <tag>-style token) is kept.
func stripInjected(s string) string {
	for {
		s = strings.TrimSpace(s)
		tag := ""
		for _, t := range injectedTags {
			if strings.HasPrefix(s, t) {
				tag = t
				break
			}
		}
		if tag == "" {
			return s
		}
		name := strings.TrimPrefix(tag, "<")
		end := strings.Index(s, "</"+name+">")
		if end < 0 {
			return "" // unterminated injected block: nothing typed follows it
		}
		s = s[end+len("</"+name+">"):]
	}
}

// readTurns returns the last n turns of a transcript; consecutive tool-only
// assistant turns collapse into one so a long tool loop is one muted line.
func readTurns(path string, n int) ([]Turn, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var turns []Turn
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 1<<20), 16<<20)
	for sc.Scan() {
		t, ok := parseTurn(sc.Bytes())
		if !ok {
			continue
		}
		turns = appendTurn(turns, t)
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	if len(turns) > n {
		turns = turns[len(turns)-n:]
	}
	return turns, nil
}

func appendTurn(turns []Turn, t Turn) []Turn {
	if n := len(turns); n > 0 && t.Role == "assistant" && t.Text == "" && turns[n-1].Role == "assistant" && turns[n-1].Text == "" {
		turns[n-1].Tools += t.Tools
		turns[n-1].Calls = append(turns[n-1].Calls, t.Calls...)
		if t.Thinking != "" {
			turns[n-1].Thinking = strings.TrimSpace(turns[n-1].Thinking + "\n\n" + t.Thinking)
		}
		turns[n-1].Time = t.Time
		return turns
	}
	return append(turns, t)
}

// attached reports whether an interactive claude holds the session right now.
func attached(sid string) bool { return procs.Attached(sid) }

// subagentFresh is how recently a sub-agent transcript must have been written
// to count as live: a working sub-agent appends continuously.
const subagentFresh = 30 * time.Second

// openSubagents counts the sub-agents currently working for a session: Agent/Task
// tool_use blocks in the main transcript that have no tool_result yet, and
// sub-agent transcripts (<dir>/<sid>/subagents/*.jsonl) touched in the last 30 s.
// A running Agent call usually has both, so the two are combined with max, not
// a sum, to avoid double counting.
func openSubagents(path, sid string) int {
	open := map[string]bool{}
	if f, err := os.Open(path); err == nil {
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 0, 1<<20), 16<<20)
		for sc.Scan() {
			var l struct {
				Message struct {
					Content []struct {
						Type      string `json:"type"`
						ID        string `json:"id"`
						Name      string `json:"name"`
						ToolUseID string `json:"tool_use_id"`
					} `json:"content"`
				} `json:"message"`
			}
			if json.Unmarshal(sc.Bytes(), &l) != nil {
				continue
			}
			for _, p := range l.Message.Content {
				switch {
				case p.Type == "tool_use" && (p.Name == "Agent" || p.Name == "Task") && p.ID != "":
					open[p.ID] = true
				case p.Type == "tool_result" && p.ToolUseID != "":
					delete(open, p.ToolUseID)
				}
			}
		}
		f.Close()
	}
	live := 0
	if sid != "" {
		entries, _ := os.ReadDir(filepath.Join(filepath.Dir(path), sid, "subagents"))
		for _, e := range entries {
			if e.IsDir() || !strings.HasSuffix(e.Name(), ".jsonl") {
				continue
			}
			if fi, err := e.Info(); err == nil && time.Since(fi.ModTime()) < subagentFresh {
				live++
			}
		}
	}
	if live > len(open) {
		return live
	}
	return len(open)
}

// runningJobs counts queue jobs currently executing.
func (s *Server) runningJobs(ctx context.Context) int {
	js, err := s.Q.List(ctx, 50)
	if err != nil {
		return 0
	}
	n := 0
	for _, j := range js {
		if j.State == "running" {
			n++
		}
	}
	return n
}

type chatState struct {
	Agent     string `json:"agent"`
	SessionID string `json:"session_id"`
	Model     string `json:"model"`
	Effort    string `json:"effort"`
	Attached  bool   `json:"attached"`
	Running   int    `json:"running"`   // qilla jobs in state "running"
	Subagents int    `json:"subagents"` // sub-agents working for this session
	Turns     []Turn `json:"turns"`
}

// chatHistory: GET /api/chat/history?agent=chief&n=40
func (s *Server) chatHistory(w http.ResponseWriter, r *http.Request) {
	agent := r.URL.Query().Get("agent")
	if agent == "" {
		agent = "chief"
	}
	if _, ok := s.Cfg().Agents[agent]; !ok {
		http.Error(w, "unknown agent", 400)
		return
	}
	n, _ := strconv.Atoi(r.URL.Query().Get("n"))
	if n <= 0 || n > 500 {
		n = 40
	}
	sid := s.W.Session(agent)
	st := chatState{Agent: agent, SessionID: sid, Model: s.Cfg().Chat.Model, Effort: s.Cfg().Chat.Effort,
		Attached: attached(sid), Running: s.runningJobs(r.Context()), Turns: []Turn{}}
	if sid != "" {
		path := transcriptPath(s.Cfg().Vault, sid)
		if turns, err := readTurns(path, n); err == nil {
			st.Turns = turns
		}
		st.Subagents = openSubagents(path, sid)
	}
	writeJSON(w, st)
}

// tailTranscripts watches every agent's session transcript and publishes new
// turns as SSE "chat" events, plus "chat-state" when a terminal attaches or
// leaves. Polling (1 s) keeps it dependency-free; serve exits when idle anyway.
func (s *Server) tailTranscripts(ctx context.Context) {
	type cursor struct {
		sid       string
		off       int64
		rest      []byte
		attached  bool
		running   int
		subagents int
	}
	cur := map[string]*cursor{}
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	i := 0
	for {
		select {
		case <-ctx.Done():
			return
		case <-s.hub.closing:
			return
		case <-tick.C:
		}
		i++
		for agent := range s.Cfg().Agents {
			sid := s.W.Session(agent)
			if sid == "" {
				continue
			}
			c := cur[agent]
			path := transcriptPath(s.Cfg().Vault, sid)
			if c == nil || c.sid != sid {
				// new or rotated session: start at the current end, history is the endpoint's job
				st, err := os.Stat(path)
				if err != nil {
					continue
				}
				c = &cursor{sid: sid, off: st.Size()}
				cur[agent] = c
			}
			if i%5 == 0 {
				// every 5 s: attached + activity counters (the full-transcript
				// scan for sub-agents is why this is not on every tick)
				a, run, sub := attached(sid), s.runningJobs(ctx), openSubagents(path, sid)
				if a != c.attached || run != c.running || sub != c.subagents {
					c.attached, c.running, c.subagents = a, run, sub
					s.hub.publish("chat-state", "", map[string]any{"agent": agent, "attached": a, "running": run, "subagents": sub})
				}
			}
			st, err := os.Stat(path)
			if err != nil {
				continue
			}
			if st.Size() < c.off { // truncated/rewritten: do not replay
				c.off, c.rest = st.Size(), nil
				continue
			}
			if st.Size() == c.off {
				continue
			}
			f, err := os.Open(path)
			if err != nil {
				continue
			}
			f.Seek(c.off, io.SeekStart)
			b, err := io.ReadAll(io.LimitReader(f, 8<<20))
			f.Close()
			if err != nil {
				continue
			}
			c.off += int64(len(b))
			data := append(c.rest, b...)
			lines := bytes.Split(data, []byte("\n"))
			c.rest = append([]byte(nil), lines[len(lines)-1]...) // partial last line waits for the next tick
			for _, ln := range lines[:len(lines)-1] {
				if t, ok := parseTurn(ln); ok {
					s.hub.publish("chat", "", map[string]any{"agent": agent, "turn": t})
				}
			}
		}
	}
}
