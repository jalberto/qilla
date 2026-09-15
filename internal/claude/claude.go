// Package claude runs Claude Code headless (`claude -p`) and parses its JSON.
package claude

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Request is one headless turn.
type Request struct {
	Prompt           string
	SystemPromptFile string   // file appended to Claude Code's system prompt (persona + rules): the cache-stable prefix
	Agents           string   // --agents JSON (sub-agent definitions) or ""
	PluginDirs       []string // --plugin-dir per entry: skills/hooks/agents loaded only into this spawn
	SystemPrompt     string   // appended to Claude Code's system prompt (persona + rules): the cache-stable prefix
	Model            string
	Effort           string // low | medium | high; empty = Claude Code's default
	Resume           string // session id to resume; empty = fresh
	SessionID        string // mint the fresh session with this id (session rotation); ignored when Resume is set
	AllowedTools     []string
	DisallowedTools  []string
	Workdir          string
	MCPConfig        string   // path to an mcpServers JSON; empty = none
	Settings         string   // path to a settings JSON (sandbox etc.); empty = none
	Env              []string // extra KEY=VALUE markers (QILLA_RUN, QILLA_ROUTINE…) so hooks and the status line know whose session this is
	Timeout          time.Duration
}

// Usage is what Claude Code reports per turn.
type Usage struct {
	Input         int `json:"input_tokens"`
	Output        int `json:"output_tokens"`
	CacheCreation int `json:"cache_creation_input_tokens"`
	CacheRead     int `json:"cache_read_input_tokens"`
	// 1h cache writes are billed higher; present on recent Claude Code versions
	CacheCreation1h int `json:"-"`
}

// Result is the parsed `--output-format json` envelope.
type Result struct {
	Type       string                     `json:"type"`
	Subtype    string                     `json:"subtype"`
	IsError    bool                       `json:"is_error"`
	Result     string                     `json:"result"`
	SessionID  string                     `json:"session_id"`
	DurationMS int                        `json:"duration_ms"`
	NumTurns   int                        `json:"num_turns"`
	CostUSD    float64                    `json:"total_cost_usd"` // 0 on subscriptions; ledger recomputes
	Usage      Usage                      `json:"usage"`
	ModelUsage map[string]json.RawMessage `json:"modelUsage"`
	Denials    []Denial                   `json:"permission_denials"`
	Raw        json.RawMessage            `json:"-"`
}

// Denial is a tool call refused by the allowlist.
type Denial struct {
	Tool  string          `json:"tool_name"`
	Input json.RawMessage `json:"tool_input"`
}

// DeniedDetail renders "Tool(input…)" for the first few denials so a run
// record says what was refused, not just which tool.
func (r Result) DeniedDetail(max int) string {
	var out []string
	for _, d := range r.Denials {
		in := strings.TrimSpace(string(d.Input))
		if len(in) > 120 {
			in = in[:120] + "…"
		}
		out = append(out, d.Tool+" "+in)
		if len(out) == max {
			break
		}
	}
	return strings.Join(out, " | ")
}

// DeniedTools lists the refused tool names, deduped.
func (r Result) DeniedTools() []string {
	seen := map[string]bool{}
	var out []string
	for _, d := range r.Denials {
		if !seen[d.Tool] {
			seen[d.Tool] = true
			out = append(out, d.Tool)
		}
	}
	return out
}

// Run executes one headless turn with a scrubbed environment.
func Run(ctx context.Context, bin string, req Request) (*Result, error) {
	if req.Timeout == 0 {
		req.Timeout = 30 * time.Minute
	}
	ctx, cancel := context.WithTimeout(ctx, req.Timeout)
	defer cancel()
	args := []string{"-p", "--output-format", "json", "--permission-mode", "default"}
	if req.SystemPromptFile != "" {
		args = append(args, "--append-system-prompt-file", req.SystemPromptFile)
	}
	if req.Agents != "" {
		args = append(args, "--agents", req.Agents)
	}
	for _, d := range req.PluginDirs {
		args = append(args, "--plugin-dir", d)
	}
	if req.SessionID != "" && req.Resume == "" {
		args = append(args, "--session-id", req.SessionID)
	}
	if req.Model != "" {
		args = append(args, "--model", req.Model)
	}
	if req.Effort != "" {
		args = append(args, "--effort", req.Effort)
	}
	if req.Resume != "" {
		args = append(args, "--resume", req.Resume)
	}
	if req.Settings != "" {
		args = append(args, "--settings", req.Settings)
	}
	if req.MCPConfig != "" {
		args = append(args, "--mcp-config", req.MCPConfig, "--strict-mcp-config")
	}
	if len(req.AllowedTools) > 0 {
		args = append(args, "--allowedTools", strings.Join(req.AllowedTools, ","))
	}
	if len(req.DisallowedTools) > 0 {
		args = append(args, "--disallowedTools", strings.Join(req.DisallowedTools, ","))
	}
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Dir = req.Workdir
	cmd.Env = append(Scrub(os.Environ()), req.Env...)
	cmd.Stdin = strings.NewReader(req.Prompt)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		if stdout.Len() == 0 {
			return nil, fmt.Errorf("claude: %v: %s", err, strings.TrimSpace(stderr.String()))
		}
	}
	res, perr := Parse(stdout.Bytes())
	if perr != nil {
		return nil, fmt.Errorf("claude: unparseable output: %v: %s", perr, firstLine(stdout.String()))
	}
	return res, nil
}

// Parse decodes the JSON envelope. Claude Code prints one object for
// --output-format json; be tolerant of a trailing newline or leading noise.
func Parse(b []byte) (*Result, error) {
	b = bytes.TrimSpace(b)
	if i := bytes.IndexByte(b, '{'); i > 0 {
		b = b[i:]
	}
	var r Result
	if err := json.Unmarshal(b, &r); err != nil {
		return nil, err
	}
	r.Raw = append(json.RawMessage(nil), b...)
	// 1h cache tier lives under usage.cache_creation.ephemeral_1h_input_tokens
	var aux struct {
		Usage struct {
			CacheCreation struct {
				H1 int `json:"ephemeral_1h_input_tokens"`
			} `json:"cache_creation"`
		} `json:"usage"`
	}
	if json.Unmarshal(b, &aux) == nil {
		r.Usage.CacheCreation1h = aux.Usage.CacheCreation.H1
	}
	return &r, nil
}

// Scrub drops every CLAUDE* variable and ZMX_SESSION (a leaked
// CLAUDE_CODE_CHILD_SESSION disables transcript saving and breaks --resume)
// and the credentials directory: secrets are for gather.sh, never the model.
func Scrub(env []string) []string {
	out := make([]string, 0, len(env))
	for _, kv := range env {
		k, _, _ := strings.Cut(kv, "=")
		if strings.HasPrefix(k, "CLAUDE") || k == "ZMX_SESSION" || k == "CREDENTIALS_DIRECTORY" || k == "QILLA_SECRETS_DIR" {
			continue
		}
		out = append(out, kv)
	}
	return out
}

func firstLine(s string) string {
	s, _, _ = strings.Cut(strings.TrimSpace(s), "\n")
	if len(s) > 200 {
		s = s[:200] + "…"
	}
	return s
}

var resetRe = regexp.MustCompile(`resets? (?:at )?(\d{1,2})(?::(\d{2}))?\s*([ap]m)?`)

// RateLimited reports whether the result text is Claude's usage-limit
// message and when the window resets (local time; +1h fallback when unparsed).
func (r Result) RateLimited(now time.Time) (bool, time.Time) {
	t := strings.ToLower(r.Result)
	if !strings.Contains(t, "hit your") || !strings.Contains(t, "limit") {
		return false, time.Time{}
	}
	m := resetRe.FindStringSubmatch(t)
	if m == nil {
		return true, now.Add(time.Hour)
	}
	h, _ := strconv.Atoi(m[1])
	min := 0
	if m[2] != "" {
		min, _ = strconv.Atoi(m[2])
	}
	if m[3] == "pm" && h < 12 {
		h += 12
	}
	if m[3] == "am" && h == 12 {
		h = 0
	}
	at := time.Date(now.Year(), now.Month(), now.Day(), h, min, 0, 0, now.Location())
	if !at.After(now) {
		at = at.Add(24 * time.Hour)
	}
	return true, at.Add(2 * time.Minute) // a little slack past the reset
}
