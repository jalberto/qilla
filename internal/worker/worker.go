// Package worker executes one job: gather → digest → context → claude -p →
// render → record. Kinds: script (no model), ai-fresh, ai-resumed.
package worker

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/jalberto/qilla/internal/artifacts"
	"github.com/jalberto/qilla/internal/claude"
	"github.com/jalberto/qilla/internal/config"
	"github.com/jalberto/qilla/internal/decide"
	"github.com/jalberto/qilla/internal/install"
	"github.com/jalberto/qilla/internal/loader"
	"github.com/jalberto/qilla/internal/manifest"
	"github.com/jalberto/qilla/internal/mem"
	"github.com/jalberto/qilla/internal/queue"
	"github.com/jalberto/qilla/internal/session"
	"github.com/jalberto/qilla/internal/star"
	"github.com/jalberto/qilla/internal/subagents"
)

// RoutinesDir is where routine bundles live inside the vault.
const RoutinesDir = "Qilla/Routines"

// Renderer turns a run's data into a vault note (knap). Nop by default.
type Renderer interface {
	Render(ctx context.Context, name string, r config.Routine, data Data) error
}

// Data is the envelope every template receives.
type Data struct {
	Routine  string `json:"routine"`
	Date     string `json:"date"`
	Gathered any    `json:"gathered"` // parsed JSON when gather.sh printed JSON, else the raw string
	Result   any    `json:"result"`   // parsed JSON when the model returned JSON, else the raw string
	// Settings is [routines.<name>.settings]: the user-specific half of a
	// shareable bundle, available to templates as `settings`.
	Settings map[string]any `json:"settings,omitempty"`
}

// Record is what a run leaves behind for the ledger and the UI.
type Record struct {
	JobID       int64         `json:"job_id"`
	Routine     string        `json:"routine"`
	Agent       string        `json:"agent"`
	Kind        string        `json:"kind"`
	Model       string        `json:"model"`
	Tier        string        `json:"tier,omitempty"`
	Degraded    bool          `json:"degraded,omitempty"`
	Skipped     bool          `json:"skipped"` // digest unchanged
	Digest      string        `json:"digest"`
	PromptChars int           `json:"prompt_chars"`
	MemoryChars int           `json:"memory_chars,omitempty"`
	Remembered  int           `json:"remembered,omitempty"`
	Artifacts   []string      `json:"artifacts,omitempty"` // artifact ids imported from the result
	Warnings    []string      `json:"warnings,omitempty"`
	Missing     []string      `json:"missing_layers,omitempty"`
	Usage       claude.Usage  `json:"usage"`
	SessionID   string        `json:"session_id,omitempty"`
	Denied      []string      `json:"denied_tools,omitempty"`
	Result      string        `json:"result"`
	Started     time.Time     `json:"started"`
	Duration    time.Duration `json:"duration"`
	Error       string        `json:"error,omitempty"`
}

// Worker runs jobs.
type Worker struct {
	Cfg    *config.Config
	DB     *sql.DB
	Render Renderer
	Mem    *mem.Store   // working memory; nil = disabled
	OnRun  func(Record) // ledger hook; may be nil
	Now    func() time.Time
}

// New prepares the worker's tables.
func New(cfg *config.Config, db *sql.DB) (*Worker, error) {
	if _, err := db.Exec(`
CREATE TABLE IF NOT EXISTS sessions(agent TEXT PRIMARY KEY, session_id TEXT NOT NULL, updated INTEGER NOT NULL);
CREATE TABLE IF NOT EXISTS digests(routine TEXT PRIMARY KEY, digest TEXT NOT NULL, updated INTEGER NOT NULL);`); err != nil {
		return nil, err
	}
	return &Worker{Cfg: cfg, DB: db, Render: nopRenderer{}, Now: time.Now}, nil
}

type nopRenderer struct{}

func (nopRenderer) Render(context.Context, string, config.Routine, Data) error { return nil }

// Run executes the job. Errors are the job's failure reason.
func (w *Worker) Run(ctx context.Context, j *queue.Job) error {
	rec := Record{JobID: j.ID, Routine: j.Routine, Agent: j.Agent, Started: w.Now()}
	err := w.run(ctx, j, &rec)
	rec.Duration = w.Now().Sub(rec.Started)
	if err != nil {
		rec.Error = err.Error()
	}
	w.save(rec)
	if w.OnRun != nil {
		w.OnRun(rec)
	}
	return err
}

func (w *Worker) run(ctx context.Context, j *queue.Job, rec *Record) error {
	var r config.Routine
	prompt := ""
	if j.Routine == "ask" {
		// an ask is the user's own conversation with the agent's main session — it
		// follows the interactive [chat] model, not the agent's headless
		// routine model, so the session never switches models by entry point.
		r = config.Routine{Kind: config.KindResumed, Agent: j.Agent, Model: w.Cfg.Chat.Model}
		prompt = j.Text
	} else {
		var ok bool
		if r, ok = w.Cfg.Routines[j.Routine]; !ok {
			return queue.Terminal{Err: fmt.Errorf("routine %q not in config", j.Routine)}
		}
	}
	rec.Kind, rec.Agent = r.Kind, r.Agent
	dir := filepath.Join(w.Cfg.Vault, RoutinesDir, j.Routine)

	var gathered string
	var hasGather bool
	var err error
	switch r.Input {
	case "runs":
		gathered, err = w.inputRuns(ctx, j.Routine)
		hasGather = err == nil && gathered != ""
	case "pending":
		gathered, err = w.inputPending(j.Routine)
		hasGather = err == nil
	default:
		gathered, hasGather, err = w.gather(ctx, dir, j.Routine, false)
	}
	if err != nil {
		return err
	}
	if r.Input == "pending" && strings.TrimSpace(gathered) == "[]" {
		rec.Skipped = true // nothing queued for this judge routine
		return nil
	}
	if j.Text != "" && j.Routine != "ask" {
		// a question aimed at a routine (qilla ask --routine r "…"): it is the input
		if gathered == "" {
			gathered = j.Text
		} else {
			gathered += "\n\n<!-- ask -->\n" + j.Text
		}
	}
	if hasGather {
		sum := sha256.Sum256([]byte(gathered))
		rec.Digest = hex.EncodeToString(sum[:8])
	}
	data := Data{Routine: j.Routine, Date: w.Now().Format("2006-01-02"), Gathered: jsonOrString(gathered), Settings: r.Settings}

	if r.Kind == config.KindScript {
		if !hasGather {
			return queue.Terminal{Err: fmt.Errorf("routine %s: kind script needs %s/gather.sh or gather.star", j.Routine, filepath.Join(RoutinesDir, j.Routine))}
		}
		rec.Result = gathered
		return w.Render.Render(ctx, j.Routine, r, data)
	}

	// ai-*: skip when nothing changed since the last success
	if r.Kind == config.KindFresh && hasGather && rec.Digest == w.lastDigest(j.Routine) {
		rec.Skipped = true
		return nil
	}
	if prompt == "" {
		b, err := os.ReadFile(filepath.Join(dir, "prompt.md"))
		if err != nil {
			return queue.Terminal{Err: fmt.Errorf("routine %s: %s/prompt.md missing", j.Routine, filepath.Join(RoutinesDir, j.Routine))}
		}
		prompt = string(b)
	}
	if r.Rules != "" { // judge routines: the rules file frames the task
		if b, err := os.ReadFile(w.Cfg.VaultPath(r.Rules)); err == nil {
			prompt = "<!-- rules: " + r.Rules + " -->\n" + string(b) + "\n\n" + prompt
		} else {
			return queue.Terminal{Err: fmt.Errorf("routine %s: rules %s missing", j.Routine, r.Rules)}
		}
	}
	agent, ok := w.Cfg.Agents[r.Agent]
	if !ok {
		return queue.Terminal{Err: fmt.Errorf("agent %q not in config", r.Agent)}
	}
	scope, tools, deny := agent.Scope, agent.AllowedTools, agent.DisallowedTools
	if len(r.Scope) > 0 {
		scope = r.Scope
	}
	if len(r.AllowedTools) > 0 {
		tools = r.AllowedTools
	}
	if len(r.DisallowedTools) > 0 {
		deny = append(append([]string{}, deny...), r.DisallowedTools...)
	}
	choice := w.Cfg.Models.Resolve(r.Model, r.Tier, agent.Model, agent.Tier, w.Now())
	model := choice.Model
	rec.Model, rec.Tier, rec.Degraded = model, choice.Tier, choice.Degraded

	ld := loader.Loader{Vault: w.Cfg.Vault, Persona: w.Cfg.Persona, Rules: w.Cfg.Rules, FactsDir: w.Cfg.FactsDir,
		JournalDir: w.Cfg.JournalDir, RecallCmd: w.Cfg.RecallCmd, RecallTimeout: recallTimeout(w.Cfg.RecallTimeout), Now: w.Now}
	if w.Mem != nil {
		project := j.Routine
		if j.Routine == "ask" {
			project = r.Agent // the chief's memory accumulates across asks
		}
		ld.Memory = func(q string, n int) (string, error) {
			txt, _, err := w.Mem.Inject(ctx, project, q+" "+firstLine(gathered), n, w.Cfg.Memory.MaxChars)
			return txt, err
		}
	}
	p, err := ld.Build(scope, firstLine(prompt), prompt, gathered)
	if err != nil {
		return err
	}
	rec.PromptChars, rec.Missing, rec.MemoryChars = p.Chars, p.Missing, p.MemoryChars

	spf, err := w.systemPromptFile(j.Routine, p.Stable)
	if err != nil {
		return err
	}
	// plugins are useless if the Skill tool is not allowed: add it when plugin dirs are configured
	if dirs := w.Cfg.PluginDirs(); len(dirs) > 0 && len(tools) > 0 {
		hasSkill := false
		for _, t := range tools {
			if t == "Skill" {
				hasSkill = true
			}
		}
		if !hasSkill {
			tools = append(append([]string{}, tools...), "Skill")
		}
	}
	agentsJSON := ""
	if defs, err := subagents.Load(w.Cfg.Vault, w.Cfg.Models); err == nil && len(defs) > 0 {
		agentsJSON = subagents.JSON(defs)
	}
	req := claude.Request{Prompt: p.Dynamic, SystemPromptFile: spf, Agents: agentsJSON, PluginDirs: w.Cfg.PluginDirs(), Model: model, Effort: choice.Effort, AllowedTools: tools, DisallowedTools: deny, Workdir: w.Cfg.Vault, MCPConfig: w.Cfg.MCPConfig(),
		Env: []string{"QILLA_RUN=1", "QILLA_ROUTINE=" + j.Routine, "QILLA_AGENT=" + r.Agent}}
	switch sf, err := w.settingsFile(j.Routine, r); {
	case err == nil:
		req.Settings = sf
	case errors.Is(err, errSandboxOff):
	default:
		return fmt.Errorf("sandbox settings: %w", err) // never run unsandboxed by accident
	}
	if r.Kind == config.KindResumed {
		maxIdle, _ := w.Cfg.Chat.MaxIdle() // validated at load
		d := w.sessions().Attach(r.Agent, w.Cfg.Vault, maxIdle, w.recall(ctx, r.Agent))
		req.Resume = d.Resume
		if d.Rotated {
			// the new session is already the agent's live one: claude must use
			// that id, not mint its own, or chat and web would attach elsewhere.
			req.SessionID = d.New
			log.Print("qilla: " + d.Line())
			rec.Warnings = append(rec.Warnings, d.Line())
			if d.Handoff != "" {
				req.Prompt = d.Handoff + "\n" + req.Prompt
			}
			if w.Mem != nil && d.State != "" {
				w.Mem.Add(ctx, r.Agent, mem.State, session.HandoffKey(r.Agent), d.State)
			}
		}
	}
	if j.Routine == "ask" {
		req.Effort = w.Cfg.Chat.Effort // same session, same settings as qilla chat
	}
	res, err := claude.Run(ctx, w.Cfg.Claude, req)
	if err != nil {
		return err
	}
	rec.Usage, rec.SessionID, rec.Result = res.Usage, res.SessionID, res.Result
	if limited, until := res.RateLimited(w.Now()); limited {
		return queue.Deferred{Err: fmt.Errorf("claude usage limit: %s", firstLine(res.Result)), Until: until}
	}
	if r.Kind == config.KindResumed && res.SessionID != "" {
		w.setSession(r.Agent, res.SessionID)
	}
	if res.IsError {
		return fmt.Errorf("claude reported an error: %s", firstLine(res.Result))
	}
	if d := res.DeniedTools(); len(d) > 0 {
		rec.Denied = d
		rec.Warnings = append(rec.Warnings, fmt.Sprintf("tools denied by allowlist: %s [%s]", strings.Join(d, ", "), res.DeniedDetail(3)))
		if strings.TrimSpace(res.Result) == "" {
			return queue.Terminal{Err: fmt.Errorf("tools denied by allowlist and no result: %s — widen allowed_tools [%s]", strings.Join(d, ", "), res.DeniedDetail(3))}
		}
		// the model finished with a result despite the denials: keep the work, surface the denial
		if w.Mem != nil {
			w.Mem.Add(ctx, "shared", "heuristic", "denied-"+j.Routine, fmt.Sprintf("%s run hit the allowlist: %s", j.Routine, res.DeniedDetail(1)))
		}
	}
	data.Result = jsonOrString(res.Result)
	memProject := j.Routine
	if j.Routine == "ask" {
		memProject = r.Agent
	}
	rec.Remembered = w.remember(ctx, memProject, data.Result)
	w.judge(ctx, data.Result)
	rec.Artifacts = w.importArtifacts(j, data.Result)
	if r.Commit {
		if msg, err := w.commitVault(ctx, j.Routine); err != nil {
			rec.Warnings = append(rec.Warnings, "commit: "+err.Error())
		} else if msg != "" {
			rec.Warnings = append(rec.Warnings, msg)
		}
	}
	if r.Input == "pending" {
		if m, ok := data.Result.(map[string]any); ok {
			var ids []string
			for _, v := range asList(m["drop"]) {
				if id, ok := v.(string); ok {
					ids = append(ids, id)
				}
			}
			if items, ok := m["items"].([]any); ok && len(ids) == 0 { // judged items are done
				for _, it := range items {
					if e, ok := it.(map[string]any); ok {
						if id, ok := e["id"].(string); ok {
							ids = append(ids, id)
						}
					}
				}
			}
			w.dropPending(j.Routine, ids)
		}
	}
	if err := w.Render.Render(ctx, j.Routine, r, data); err != nil {
		return err
	}
	if hasGather {
		w.setDigest(j.Routine, rec.Digest)
	}
	return nil
}

// gatherTimeout caps the gather step, whichever language it is written in.
const gatherTimeout = 5 * time.Minute

// GatherEnv is the environment a gather step sees, whichever language it is
// written in: shell reads it from the process env, Starlark from ctx.env.
func (w *Worker) GatherEnv(name string) map[string]string {
	e := map[string]string{
		"QILLA_VAULT":         w.Cfg.Vault,
		"QILLA_ROUTINE":       name,
		"QILLA_DATE":          w.Now().Format("2006-01-02"),
		"QILLA_MEM_SEARCH":    "qilla mem search --project " + name,
		"QILLA_BROWSER":       "qilla browser --agent " + name,
		"QILLA_ARTIFACTS_DIR": filepath.Join(w.Cfg.StateDir, "artifacts-inbox"),
	}
	// the routine's settings: the user-specific half of a shareable bundle
	if st := w.Cfg.Routines[name].Settings; len(st) > 0 {
		if b, err := json.Marshal(st); err == nil {
			e["QILLA_SETTINGS"] = string(b)
		}
	}
	// secrets (systemd LoadCredentialEncrypted) reach the gather only, never the model
	if cd := os.Getenv("CREDENTIALS_DIRECTORY"); cd != "" {
		e["QILLA_SECRETS_DIR"] = cd
	}
	return e
}

// gatherEnv is GatherEnv plus the secrets a gather is entitled to. Under
// systemd the credentials directory is already mounted and is used as is;
// otherwise (`qilla run` from a terminal) qilla mounts the secrets itself, so
// both paths hand the gather the same QILLA_SECRETS_DIR contract. The returned
// cleanup removes anything qilla mounted.
func (w *Worker) gatherEnv(dir, name string) (map[string]string, func()) {
	e := w.GatherEnv(name)
	if e["QILLA_SECRETS_DIR"] != "" {
		return e, func() {}
	}
	mnt, cleanup := w.mountSecrets(dir, name)
	if mnt == "" {
		return e, cleanup
	}
	e["QILLA_SECRETS_DIR"] = mnt
	return e, cleanup
}

// Gather runs the routine's gather step and returns its JSON, whether it
// exists (false = the routine has no gather) and any failure. It is exported
// for `qilla gather <routine>`, the cheap way to test a bundle.
func (w *Worker) Gather(ctx context.Context, name string) (string, bool, error) {
	return w.GatherDry(ctx, name, false)
}

// GatherDry is Gather with the dry-run switch of `qilla gather <name> --dry`:
// a gather.star's declared write/http mutations are recorded as
// "_dry_actions" in the JSON instead of being performed.
func (w *Worker) GatherDry(ctx context.Context, name string, dry bool) (string, bool, error) {
	return w.gather(ctx, filepath.Join(w.Cfg.Vault, RoutinesDir, name), name, dry)
}

// gather runs the routine's gather step in the vault: <dir>/gather.sh (any
// language, its stdout is the JSON) or, when there is no gather.sh,
// <dir>/gather.star (built-in Starlark, its dict JSON-encoded here). Neither
// present → ("", false, nil).
func (w *Worker) gather(ctx context.Context, dir, name string, dry bool) (string, bool, error) {
	script := filepath.Join(dir, "gather.sh")
	if _, err := os.Stat(script); errors.Is(err, os.ErrNotExist) {
		return w.gatherStar(ctx, dir, name, dry)
	}
	ctx, cancel := context.WithTimeout(ctx, gatherTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "sh", script)
	cmd.Dir = w.Cfg.Vault
	env, cleanup := w.gatherEnv(dir, name)
	defer cleanup()
	cmd.Env = claude.Scrub(os.Environ())
	for _, k := range sortedEnvKeys(env) {
		cmd.Env = append(cmd.Env, k+"="+env[k])
	}
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		return "", true, fmt.Errorf("gather.sh: %v: %s", err, strings.TrimSpace(errb.String()))
	}
	return out.String(), true, nil
}

// starMem maps working-memory entries to the gather runtime's view.
func starMem(es []mem.Entry) []star.MemEntry {
	out := make([]star.MemEntry, 0, len(es))
	for _, e := range es {
		out = append(out, star.MemEntry{Project: e.Project, Kind: e.Kind, Key: e.Key, Text: e.Text,
			Updated: e.Updated.Format(time.RFC3339)})
	}
	return out
}

// gatherStar runs <dir>/gather.star through the in-process Starlark runtime.
// The dict it returns is JSON-encoded with encoding/json (map keys sorted), so
// the digest is as stable as gather.sh's stdout.
func (w *Worker) gatherStar(ctx context.Context, dir, name string, dry bool) (string, bool, error) {
	script := filepath.Join(dir, "gather.star")
	if _, err := os.Stat(script); errors.Is(err, os.ErrNotExist) {
		return "", false, nil
	}
	env, cleanup := w.gatherEnv(dir, name)
	defer cleanup()
	if env["QILLA_DRY_RUN"] == "1" || os.Getenv("QILLA_DRY_RUN") == "1" {
		dry = true
	}
	senv := star.Env{
		Routine: name, Date: env["QILLA_DATE"], Vault: w.Cfg.Vault,
		SecretsDir: env["QILLA_SECRETS_DIR"], Vars: env, Timeout: gatherTimeout,
		Settings: w.Cfg.Routines[name].Settings,
		DryRun:   dry,
		// decide() needs no capability: it only reads the exported models.
		DecidersDir: filepath.Join(w.Cfg.StateDir, "deciders"),
		// ask() talks to the local Lemonade; [deciders] says where and how.
		AskConfig: w.askConfig,
	}
	// [capabilities] from the bundle's routine.toml scopes the side effects
	// the script may have; no manifest ⇒ the frozen, pure runtime.
	if m, err := manifest.Load(dir); err != nil {
		return "", true, fmt.Errorf("gather.star: %v", err)
	} else if m != nil {
		senv.Caps = m.Capabilities
	}
	if w.Mem != nil {
		senv.MemSearch = func(q string, n int) ([]star.MemEntry, error) {
			es, err := w.Mem.Search(ctx, name, "", q, n)
			return starMem(es), err
		}
		senv.MemState = func(routine string) ([]star.MemEntry, error) {
			es, err := w.Mem.State(ctx, routine, 0)
			return starMem(es), err
		}
	}
	res, actions, prints, err := star.RunActions(ctx, script, senv)
	if err != nil {
		if p := strings.TrimSpace(prints); p != "" {
			return "", true, fmt.Errorf("gather.star: %v\n%s", err, p)
		}
		return "", true, fmt.Errorf("gather.star: %v", err)
	}
	// only a dry run that actually suppressed something adds a key: the
	// digest of a normal run stays exactly what it was
	if len(actions) > 0 {
		if m, ok := res.(map[string]any); ok {
			m["_dry_actions"] = actions
		}
	}
	b, err := json.Marshal(res)
	if err != nil {
		return "", true, fmt.Errorf("gather.star: %v", err)
	}
	return string(b), true, nil
}

func sortedEnvKeys(m map[string]string) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return ks
}

func (w *Worker) lastDigest(routine string) string {
	var d string
	w.DB.QueryRow(`SELECT digest FROM digests WHERE routine=?`, routine).Scan(&d)
	return d
}

func (w *Worker) setDigest(routine, d string) {
	w.DB.Exec(`INSERT INTO digests(routine,digest,updated) VALUES(?,?,?) ON CONFLICT(routine) DO UPDATE SET digest=excluded.digest, updated=excluded.updated`,
		routine, d, w.Now().Unix())
}

// recall is the cheap working-memory lookup a rotated session is seeded with;
// nil when memory is disabled.
func (w *Worker) recall(ctx context.Context, agent string) session.Recall {
	if w.Mem == nil {
		return nil
	}
	return func(q string) string {
		txt, _, err := w.Mem.Inject(ctx, agent, q, 5, w.Cfg.Memory.MaxChars)
		if err != nil {
			return ""
		}
		return txt
	}
}

func (w *Worker) session(agent string) string { return w.sessions().Current(agent) }

// sessions is the rotation store over the same DB handle.
func (w *Worker) sessions() *session.Store {
	s, err := session.Open(w.DB)
	if err != nil {
		return &session.Store{DB: w.DB, Now: w.Now}
	}
	s.Now = w.Now
	return s
}

// Session is the agent's current resumable session id ("" before the first run).
func (w *Worker) Session(agent string) string { return w.session(agent) }

func (w *Worker) setSession(agent, id string) { w.sessions().Set(agent, id) }

// save writes the record under <state_dir>/runs/<job>.json for the UI and debugging.
func (w *Worker) save(rec Record) {
	dir := filepath.Join(w.Cfg.StateDir, "runs")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return
	}
	b, _ := json.MarshalIndent(rec, "", "  ")
	os.WriteFile(filepath.Join(dir, fmt.Sprintf("%d.json", rec.JobID)), b, 0o644)
}

func jsonOrString(s string) any {
	t := stripFence(strings.TrimSpace(s))
	if strings.HasPrefix(t, "{") || strings.HasPrefix(t, "[") {
		var v any
		if json.Unmarshal([]byte(t), &v) == nil {
			return v
		}
	}
	return s
}

// stripFence removes a markdown code fence (```, optionally with a language
// tag) wrapping the whole text; anything else is returned untouched, so JSON
// embedded in prose still fails to parse and stays a string.
func stripFence(s string) string {
	if !strings.HasPrefix(s, "```") || !strings.HasSuffix(s, "```") || len(s) < 6 {
		return s
	}
	body := strings.TrimSuffix(s, "```")
	// drop the opening fence line (``` plus an optional language tag)
	if i := strings.IndexByte(body, '\n'); i >= 0 {
		if tag := strings.TrimSpace(body[3:i]); !strings.ContainsAny(tag, " \t`") {
			return strings.TrimSpace(body[i+1:])
		}
	}
	return s
}

func firstLine(s string) string {
	s, _, _ = strings.Cut(strings.TrimSpace(s), "\n")
	if len(s) > 120 {
		s = s[:120]
	}
	return s
}

// remember stores result.remember entries: [{kind,key,text}] — the worker
// writes them, the model never touches the store directly.
func (w *Worker) remember(ctx context.Context, project string, result any) int {
	if w.Mem == nil {
		return 0
	}
	m, ok := result.(map[string]any)
	if !ok {
		return 0
	}
	items, ok := m["remember"].([]any)
	if !ok {
		return 0
	}
	n := 0
	for _, it := range items {
		e, ok := it.(map[string]any)
		if !ok {
			continue
		}
		kind, _ := e["kind"].(string)
		key, _ := e["key"].(string)
		text, _ := e["text"].(string)
		if _, err := w.Mem.Add(ctx, project, kind, key, text); err == nil {
			n++
		}
	}
	return n
}

// judge applies result.judge entries: [{id, relation, reason}] to pending
// working-memory conflicts (the weekly mem-consolidate routine).
func (w *Worker) judge(ctx context.Context, result any) {
	if w.Mem == nil {
		return
	}
	m, ok := result.(map[string]any)
	if !ok {
		return
	}
	items, ok := m["judge"].([]any)
	if !ok {
		return
	}
	for _, it := range items {
		e, ok := it.(map[string]any)
		if !ok {
			continue
		}
		id, _ := e["id"].(string)
		rel, _ := e["relation"].(string)
		reason, _ := e["reason"].(string)
		if id != "" && rel != "" {
			w.Mem.B.Judge(ctx, id, rel, reason)
		}
	}
}

// settingsFile writes the per-routine Claude Code settings (sandbox on, egress
// allowlist = global + routine) under state_dir/settings/<routine>.json.
// Content is deterministic so the file is stable across runs.
var errSandboxOff = errors.New("sandbox disabled")

func (w *Worker) settingsFile(name string, r config.Routine) (string, error) {
	if w.Cfg.Sandbox.Disabled {
		return "", errSandboxOff
	}
	domains := append(append([]string{}, w.Cfg.Sandbox.AllowedDomains...), r.AllowedDomains...)
	sort.Strings(domains)
	// secrets are for gather.sh: deny the credentials dir and the encrypted store to sandboxed Bash
	deny := []string{filepath.Join(filepath.Dir(w.Cfg.Path), "creds"), w.secretsRoot()}
	if cd := os.Getenv("CREDENTIALS_DIRECTORY"); cd != "" {
		deny = append(deny, cd)
	}
	sort.Strings(deny)
	set := map[string]any{
		// a qilla session must look like one: purple badge, routine name, never the plain claude line
		"statusLine": map[string]any{"type": "command", "command": install.StatusLineCommand(filepath.Dir(w.Cfg.Path), w.Cfg.Chat.StatusLine), "padding": 0},
		"sandbox": map[string]any{
			"enabled": true, "allowUnsandboxedCommands": false,
			"filesystem": map[string]any{"denyRead": deny, "denyWrite": deny},
			"network":    map[string]any{"allowedDomains": domains}}}
	b, _ := json.MarshalIndent(set, "", "  ")
	dir := filepath.Join(w.Cfg.StateDir, "settings")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	path := filepath.Join(dir, name+".json")
	if old, err := os.ReadFile(path); err == nil && bytes.Equal(old, b) {
		return path, nil
	}
	return path, os.WriteFile(path, b, 0o644)
}

// systemPromptFile persists the stable prefix (persona + rules) under
// state_dir/system/<routine>.md, rewritten only when it changes.
func (w *Worker) systemPromptFile(name, stable string) (string, error) {
	if strings.TrimSpace(stable) == "" {
		return "", nil
	}
	dir := filepath.Join(w.Cfg.StateDir, "system")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	path := filepath.Join(dir, name+".md")
	if old, err := os.ReadFile(path); err == nil && string(old) == stable {
		return path, nil
	}
	return path, os.WriteFile(path, []byte(stable), 0o644)
}

func recallTimeout(s string) time.Duration {
	d, err := time.ParseDuration(s)
	if err != nil || d <= 0 {
		return 0
	}
	return d
}

// inputRuns feeds a learn routine the ledger rows since its own last run.
func (w *Worker) inputRuns(ctx context.Context, routine string) (string, error) {
	var since int64
	w.DB.QueryRowContext(ctx, `SELECT COALESCE(MAX(started),0) FROM runs WHERE routine=? AND ok=1`, routine).Scan(&since)
	rows, err := w.DB.QueryContext(ctx, `SELECT routine,agent,kind,model,ok,skipped,cost,input+output+cache_read+cache_write+cache_write_1h,duration_ms,error,started,tier,degraded FROM runs WHERE started>? AND routine!=? ORDER BY started`, since, routine)
	if err != nil {
		return "", err
	}
	defer rows.Close()
	var out []map[string]any
	for rows.Next() {
		var rt, ag, kd, md, er, tier string
		var ok, sk, dg int
		var cost float64
		var tok, dur, st int64
		if err := rows.Scan(&rt, &ag, &kd, &md, &ok, &sk, &cost, &tok, &dur, &er, &st, &tier, &dg); err != nil {
			return "", err
		}
		out = append(out, map[string]any{"routine": rt, "agent": ag, "kind": kd, "model": md, "tier": tier, "degraded": dg == 1, "ok": ok == 1, "skipped": sk == 1,
			"cost": cost, "tokens": tok, "seconds": dur / 1000, "error": firstLine(er), "at": time.Unix(st, 0).Format(time.RFC3339)})
	}
	if len(out) == 0 {
		return "", nil
	}
	b, _ := json.Marshal(map[string]any{"since": time.Unix(since, 0).Format(time.RFC3339), "runs": out})
	return string(b), nil
}

// PendingPath is where `qilla enqueue <routine> --text` parks items for a judge routine.
func PendingPath(stateDir, routine string) string {
	return filepath.Join(stateDir, "pending", routine+".jsonl")
}

// inputPending returns the queued items as a JSON array (and leaves the file;
// the routine's result may `drop` ids).
func (w *Worker) inputPending(routine string) (string, error) {
	b, err := os.ReadFile(PendingPath(w.Cfg.StateDir, routine))
	if errors.Is(err, os.ErrNotExist) {
		return "[]", nil
	}
	if err != nil {
		return "", err
	}
	var items []json.RawMessage
	for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		if strings.TrimSpace(line) != "" {
			items = append(items, json.RawMessage(line))
		}
	}
	out, _ := json.Marshal(items)
	return string(out), nil
}

// dropPending removes judged ids from the pending file.
func (w *Worker) dropPending(routine string, ids []string) {
	p := PendingPath(w.Cfg.StateDir, routine)
	b, err := os.ReadFile(p)
	if err != nil {
		return
	}
	drop := map[string]bool{}
	for _, id := range ids {
		drop[id] = true
	}
	var keep []string
	for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		var it struct {
			ID string `json:"id"`
		}
		if json.Unmarshal([]byte(line), &it) == nil && drop[it.ID] {
			continue
		}
		if strings.TrimSpace(line) != "" {
			keep = append(keep, line)
		}
	}
	os.WriteFile(p, []byte(strings.Join(keep, "\n")+"\n"), 0o644)
}

func asList(v any) []any {
	l, _ := v.([]any)
	return l
}

// importArtifacts copies result.artifacts [{path,title}] (vault- or state-relative, or absolute)
// into the artifact store and returns their ids.
func (w *Worker) importArtifacts(j *queue.Job, result any) []string {
	m, ok := result.(map[string]any)
	if !ok {
		return nil
	}
	items, ok := m["artifacts"].([]any)
	if !ok || len(items) == 0 {
		return nil
	}
	st, err := artifacts.New(w.Cfg.ArtifactsDir(), time.Duration(w.Cfg.Artifacts.TTLDays)*24*time.Hour, w.Cfg.Artifacts.MaxMB)
	if err != nil {
		return nil
	}
	var ids []string
	for _, it := range items {
		e, ok := it.(map[string]any)
		if !ok {
			continue
		}
		path, _ := e["path"].(string)
		title, _ := e["title"].(string)
		if path == "" {
			continue
		}
		cands := []string{path, filepath.Join(w.Cfg.Vault, path), filepath.Join(w.Cfg.StateDir, path)}
		for _, c := range cands {
			if _, err := os.Stat(c); err == nil {
				if meta, err := st.Add(c, title, j.Routine, j.ID, 0); err == nil {
					ids = append(ids, meta.ID)
				}
				break
			}
		}
	}
	return ids
}

// commitVault commits the vault's pending changes (everything but .obsidian) as
// "qilla: <routine> <date>". Nothing to commit → ("", nil).
func (w *Worker) commitVault(ctx context.Context, routine string) (string, error) {
	git := func(args ...string) (string, error) {
		c := exec.CommandContext(ctx, "git", append([]string{"-C", w.Cfg.Vault}, args...)...)
		c.Env = claude.Scrub(os.Environ())
		out, err := c.CombinedOutput()
		return strings.TrimSpace(string(out)), err
	}
	if _, err := git("rev-parse", "--git-dir"); err != nil {
		return "", nil // not a repo: nothing to do
	}
	// never sweep editor/agent state: Obsidian churn and Claude Code local settings (permission grants)
	if _, err := git("add", "-A", "--", ".", ":(exclude).obsidian", ":(exclude).claude/settings.local.json", ":(exclude).claude/settings.json"); err != nil {
		return "", fmt.Errorf("git add: %w", err)
	}
	if out, _ := git("diff", "--cached", "--quiet"); out == "" {
		if _, err := git("diff", "--cached", "--quiet"); err == nil {
			return "", nil // clean
		}
	}
	msg := fmt.Sprintf("qilla: %s %s", routine, w.Now().Format("2006-01-02 15:04"))
	if out, err := git("commit", "-q", "-m", msg); err != nil {
		return "", fmt.Errorf("git commit: %v %s", err, firstLine(out))
	}
	return "committed: " + msg, nil
}

// askConfig fills the star `ask()` builtin's backend wiring from [deciders].
// The jev key is a secret: it is only read when the routine actually has a
// secrets directory and the remote route is on.
func (w *Worker) askConfig(req *decide.AskRequest) {
	req.URL = w.Cfg.Deciders.LemonadeURL
	req.Model = w.Cfg.Deciders.AskModel
	req.PolicyVersion = w.Cfg.Deciders.PolicyVersion
	if req.Floor == 0 {
		req.Floor = w.Cfg.Deciders.ConfFloor
	}
	req.JevEnabled = w.Cfg.Deciders.JevEnabled
	req.JevURL = w.Cfg.Deciders.JevURL
	if req.Route == "jev" && req.JevEnabled {
		if d := os.Getenv("QILLA_SECRETS_DIR"); d != "" {
			if b, err := os.ReadFile(filepath.Join(d, "jev_key")); err == nil {
				req.JevKey = strings.TrimSpace(string(b))
			}
		}
	}
}
