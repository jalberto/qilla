package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/jalberto/qilla/internal/claude"
	"github.com/jalberto/qilla/internal/config"
	"github.com/jalberto/qilla/internal/install"
	"github.com/jalberto/qilla/internal/mem"
	"github.com/jalberto/qilla/internal/queue"
	"github.com/jalberto/qilla/internal/session"
	"github.com/jalberto/qilla/internal/subagents"
)

// cmdChat: qilla chat [agent] [--tier t | --model m] [--new] [--dir path] [opening…]
// The interactive launcher: runs claude in the vault as the agent, with the
// persona as system prompt, the qilla badge, tools from the agent, the model
// from tiers + weekly usage, resuming the agent's session unless --new.
func cmdChat(args []string) error {
	fs := flag.NewFlagSet("chat", flag.ContinueOnError)
	tier := fs.String("tier", "", "override the agent's tier")
	model := fs.String("model", "", "override the model outright")
	fresh := fs.Bool("new", false, "start a new session instead of resuming the agent's")
	effort := fs.String("effort", "", "low | medium | high (default [chat].effort)")
	dir := fs.String("dir", "", "working directory (default: the vault)")
	agent := "chief"
	// model shortcuts: qilla chat opus | sonnet | haiku | fable [agent] [opening…]
	shortModel := ""
	if len(args) > 0 {
		if m, ok := shortcuts[args[0]]; ok {
			shortModel, args = m, args[1:]
		}
	}
	cfg0, err0 := config.Load(config.DefaultPath())
	if err0 != nil {
		return err0
	}
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		switch {
		case args[0] == "new": // `qilla new` — fresh session
			*fresh, args = true, args[1:]
		case cfg0.Routines[args[0]].Kind != "": // `qilla brief` — run the routine headless, now
			return cmdRun(args[:1])
		case cfg0.Agents[args[0]].Model != "" || cfg0.Agents[args[0]].Tier != "" || looksLikeAgent(args[0]) && agentKnown(cfg0, args[0]):
			agent, args = args[0], args[1:]
		}
	}
	if err := fs.Parse(args); err != nil {
		return err
	}
	if shortModel != "" && *model == "" {
		*model = shortModel
		if *effort == "" {
			*effort = "medium"
		}
	}
	opening := strings.Join(fs.Args(), " ")

	cfg := cfg0
	a, ok := cfg.Agents[agent]
	if !ok {
		return fmt.Errorf("agent %q not in %s", agent, cfg.Path)
	}
	// interactive default: [chat].model (fable low) unless a flag says otherwise.
	// a.Model/a.Tier are that agent's HEADLESS routine model — they must not
	// leak into the interactive launcher, or [chat].model can never apply to
	// any agent that also runs scheduled routines (chief always has both).
	base := cfg.Chat.Model
	if *model == "" && *tier == "" && *dir != "" && config.Expand(*dir) != cfg.Vault {
		*tier = "coding" // a session opened on a code repo is a coding session: Opus, medium (the coding tier) unless told otherwise
	}
	if *model == "" && *tier == "" && base != "" {
		*model = base
	}
	choice := cfg.Models.Resolve(*model, *tier, a.Model, a.Tier, time.Now())
	eff := cfg.Chat.Effort
	switch {
	case *effort != "":
		eff = *effort
	case choice.Effort != "":
		eff = choice.Effort // the tier's effort (coding → medium)
	}
	// RC only applies on the engine; an agent host never passes --rc.
	useRC := cfg.IsEngine() && (cfg.Chat.RC == nil || *cfg.Chat.RC)
	q, err := queue.Open(cfg.DBPath())
	if err != nil {
		return err
	}
	workdir := cfg.Vault
	if *dir != "" {
		workdir = config.Expand(*dir)
	}
	// Resume, or rotate when the session has gone cold: see internal/session.
	var sid, newSid, handoff string
	if ss, err := session.Open(q.DB()); err == nil {
		maxIdle, _ := cfg.Chat.MaxIdle() // validated at load
		if *fresh {
			maxIdle = 0 // --new starts fresh anyway; no rotation bookkeeping
		}
		d := ss.Attach(agent, workdir, maxIdle, memRecall(cfg, q.DB(), agent))
		sid, handoff = d.Resume, d.Handoff
		if d.Rotated {
			newSid = d.New
			fmt.Fprintln(os.Stderr, "qilla: "+d.Line())
			saveHandoff(cfg, q.DB(), agent, d.State)
		}
	}
	q.Close()

	bin, err := exec.LookPath(cfg.Claude)
	if err != nil {
		return err
	}
	confDir := filepath.Dir(cfg.Path)
	settings := filepath.Join(confDir, "chat.json")
	merged := map[string]any{}
	if cfg.Chat.Settings != "" {
		if b, err := os.ReadFile(cfg.Chat.Settings); err == nil {
			json.Unmarshal(b, &merged)
		}
	}
	merged["statusLine"] = map[string]any{"type": "command", "command": install.StatusLineCommand(confDir, cfg.Chat.StatusLine), "padding": 0}
	mb, _ := json.Marshal(merged)
	os.WriteFile(settings, mb, 0o644)
	argv := []string{bin, "--settings", settings, "--model", choice.Model, "--effort", eff}
	argv = append(argv, cfg.Chat.ClaudeArgs...)
	if newSid != "" {
		argv = append(argv, "--session-id", newSid)
	}
	if sp := systemPrompt(cfg) + handoff; sp != "" {
		spf := filepath.Join(confDir, "chat.system.md")
		os.WriteFile(spf, []byte(sp), 0o644)
		argv = append(argv, "--append-system-prompt-file", spf)
	}
	if defs, err := subagents.Load(cfg.Vault, cfg.Models); err == nil && len(defs) > 0 {
		argv = append(argv, "--agents", subagents.JSON(defs))
	}
	if len(a.DisallowedTools) > 0 {
		argv = append(argv, "--disallowedTools", strings.Join(a.DisallowedTools, ","))
	}
	if useRC {
		argv = append(argv, "--rc")
	}
	switch {
	case *fresh || sid == "":
		if sid == "" && !*fresh && newSid == "" {
			fmt.Fprintln(os.Stderr, "qilla: no session yet for", agent, "— starting fresh")
		}
	default:
		argv = append(argv, "--resume", sid)
	}
	if opening == "" && !*fresh {
		opening = pickOpening(cfg, sid != "" && usedToday(cfg, agent))
	}
	if opening != "" {
		argv = append(argv, opening)
	}
	if choice.Degraded {
		fmt.Fprintf(os.Stderr, "qilla: weekly usage %d%% — %s degraded to %s\n", choice.Usage, agent, choice.Model)
	}
	env := chatEnv(cfg, agent, os.Environ())
	if err := os.Chdir(workdir); err != nil {
		return err
	}
	if cwd, _ := os.Getwd(); cwd != "" && !strings.HasPrefix(cwd, workdir) {
		fmt.Fprintf(os.Stderr, "\033[34m» qilla runs in %s (your shell stays in %s)\033[0m\n", workdir, cwd)
	}
	fmt.Fprint(os.Stderr, "\033]0;◆ qilla\a") // terminal title
	fmt.Fprintf(os.Stderr, "\033[45;30m ◆ qilla · %s \033[0m %s · %s/%s%s · %s\n", cfg.Role(), agent, choice.Model, eff, map[bool]string{true: " · rc", false: ""}[useRC], workdir)
	return syscall.Exec(bin, argv, env)
}

// chatEnv is the interactive child's environment: the caller's, scrubbed of every
// CLAUDE* var (claude.Scrub — a parent session's vars must not leak into the child),
// plus qilla's own markers. [chat].autocompact_pct is re-added AFTER the scrub on
// purpose: it is the one CLAUDE* var we want the child to see.
func chatEnv(cfg *config.Config, agent string, environ []string) []string {
	env := append(claude.Scrub(environ), "QILLA_RUN=1", "QILLA_ATTACH=1", "QILLA_AGENT="+agent, "QILLA_ARTIFACTS=qilla artifact add")
	if p := cfg.Chat.AutocompactPct; p != nil && *p > 0 {
		env = append(env, fmt.Sprintf("CLAUDE_AUTOCOMPACT_PCT_OVERRIDE=%d", *p))
	}
	return env
}

// systemPrompt is persona + rules from the vault, the same prefix routines get.
// handoffPrompt asks an interactive session to leave its own handoff behind,
// rather than relying on the transcript scrape a rotation does for it.
const handoffPrompt = "At the end of a substantial task run `qilla sessions handoff --set \"goal: … | done: … | pending: … | files: …\"` (one line, ≤400 chars) so a rotated session or a routine can pick up where you left.\n"

func systemPrompt(cfg *config.Config) string {
	var b string
	for _, rel := range []string{cfg.Persona, cfg.Rules} {
		if x, err := os.ReadFile(cfg.VaultPath(rel)); err == nil {
			b += string(x) + "\n\n"
		}
	}
	return b + handoffPrompt + "\n"
}

// cmdAsk: qilla ask [--agent a] [--timeout 10m] "<text>" — headless one-off through the
// queue and the supervisor; waits and prints the answer. Ledgered like any job.
func cmdAsk(args []string) error {
	fs := flag.NewFlagSet("ask", flag.ContinueOnError)
	agent := fs.String("agent", "chief", "agent")
	routine := fs.String("routine", "", "aim the question at a routine (its prompt.md + gather become the frame)")
	to := fs.Duration("timeout", 10*time.Minute, "how long to wait")
	if err := fs.Parse(args); err != nil {
		return err
	}
	text := strings.TrimSpace(strings.Join(fs.Args(), " "))
	if text == "" {
		return fmt.Errorf("usage: qilla ask [--agent a] \"<text>\"")
	}
	cfg, err := config.Load(config.DefaultPath())
	if err != nil {
		return err
	}
	if _, ok := cfg.Agents[*agent]; !ok {
		return fmt.Errorf("agent %q not in config", *agent)
	}
	name := "ask"
	if *routine != "" {
		r, ok := cfg.Routines[*routine]
		if !ok {
			return fmt.Errorf("routine %q not in config", *routine)
		}
		name, *agent = *routine, r.Agent
	}
	q, err := queue.Open(cfg.DBPath())
	if err != nil {
		return err
	}
	defer q.Close()
	ctx, cancel := context.WithTimeout(context.Background(), *to)
	defer cancel()
	id, _, err := q.Enqueue(ctx, queue.Job{Routine: name, Agent: *agent, Text: text, Priority: 5, Due: time.Now()})
	if err != nil {
		return err
	}
	poke()
	fmt.Fprintf(os.Stderr, "\033[45;30m ◆ qilla \033[0m job #%d · waiting…\n", id)
	for {
		j, err := q.Get(ctx, id)
		if err != nil {
			return err
		}
		switch j.State {
		case queue.Done, queue.Failed, queue.Suspended, queue.Expired:
			rec, rerr := os.ReadFile(filepath.Join(cfg.StateDir, "runs", fmt.Sprintf("%d.json", id)))
			if j.State != queue.Done {
				return fmt.Errorf("job #%d %s: %s", id, j.State, j.Error)
			}
			if rerr == nil {
				var r struct {
					Result string `json:"result"`
				}
				if jsonUnmarshal(rec, &r) == nil {
					fmt.Println(strings.TrimSpace(r.Result))
					return nil
				}
			}
			fmt.Println("(done, no text result)")
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("job #%d still %s after %s — it keeps running; see the Jobs tab", id, j.State, *to)
		case <-time.After(2 * time.Second):
		}
	}
}

func jsonUnmarshal(b []byte, v any) error { return json.Unmarshal(b, v) }

var shortcuts = map[string]string{
	"fable": "claude-fable-5-1", "opus": "claude-opus-5", "sonnet": "claude-sonnet-5", "haiku": "claude-haiku-4-5-20251001",
}

// looksLikeAgent: a single lowercase token with no spaces is an agent name; anything else is the opening line.
func looksLikeAgent(s string) bool {
	if s == "" || strings.ContainsAny(s, " \t.,;:!?") {
		return false
	}
	return strings.ToLower(s) == s
}

func agentKnown(cfg *config.Config, name string) bool {
	_, ok := cfg.Agents[name]
	return ok
}

// pickOpening: the resume line when the agent already worked today, else by time of day.
func pickOpening(cfg *config.Config, usedToday bool) string {
	o := cfg.Chat.Openings
	if usedToday {
		return o.Resume
	}
	h := time.Now().Hour()
	switch {
	case h < 14:
		return o.Morning
	case h < 18:
		return o.Afternoon
	default:
		return o.Evening
	}
}

// usedToday: did the agent's session run today (ledger)?
func usedToday(cfg *config.Config, agent string) bool {
	q, err := queue.Open(cfg.DBPath())
	if err != nil {
		return false
	}
	defer q.Close()
	var n int
	q.DB().QueryRow(`SELECT COUNT(*) FROM runs WHERE agent=? AND day=?`, agent, time.Now().Format("2006-01-02")).Scan(&n)
	return n > 0
}

// saveHandoff persists a rotation handoff as the agent's working-memory state,
// so a later session (or a routine) can read where the old one got to. Best
// effort: memory being unavailable never blocks a chat.
func saveHandoff(cfg *config.Config, db *sql.DB, agent, text string) error {
	if strings.TrimSpace(text) == "" {
		return nil
	}
	m, err := mem.Open(cfg, db)
	if err != nil {
		return err
	}
	_, err = m.Add(context.Background(), agent, mem.State, session.HandoffKey(agent), text)
	return err
}

// memRecall is the working-memory lookup that seeds a rotated session, or nil
// when memory is unavailable.
func memRecall(cfg *config.Config, db *sql.DB, agent string) session.Recall {
	return func(q string) string {
		st, err := mem.Open(cfg, db) // lazy: only a rotation pays for it
		if err != nil || st == nil {
			return ""
		}
		txt, _, err := st.Inject(context.Background(), agent, q, 5, cfg.Memory.MaxChars)
		if err != nil {
			return ""
		}
		return txt
	}
}
