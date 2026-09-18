// Package config loads qilla.toml: one file, the whole system.
package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
	"github.com/jalberto/qilla/internal/models"
)

// Budget is a daily cap in USD.
type Budget struct {
	Warn  float64 `toml:"warn"`
	Block float64 `toml:"block"`
}

// Agent is a named Claude persona with its own session.
type Agent struct {
	Model           string   `toml:"model"`
	Tier            string   `toml:"tier"` // classify | extract | research | synthesis | judgment — resolved through [models]
	Budget          Budget   `toml:"budget"`
	AllowedTools    []string `toml:"allowed_tools"`
	DisallowedTools []string `toml:"disallowed_tools"` // passed as --disallowedTools (deny wins)
	Scope           []string `toml:"scope"`
}

// Routine kinds.
const (
	KindScript  = "script"
	KindFresh   = "ai-fresh"
	KindResumed = "ai-resumed"
)

// Routine is one scheduled unit of work.
type Routine struct {
	Kind            string   `toml:"kind"`
	Agent           string   `toml:"agent"`
	Schedule        string   `toml:"schedule"`
	Window          string   `toml:"window"`
	MustRun         bool     `toml:"must_run"`
	Scope           []string `toml:"scope"`
	Model           string   `toml:"model"`
	Tier            string   `toml:"tier"`
	AllowedTools    []string `toml:"allowed_tools"`
	DisallowedTools []string `toml:"disallowed_tools"`
	Budget          Budget   `toml:"budget"`
	Output          string   `toml:"output"`
	AllowedDomains  []string `toml:"allowed_domains"` // extra sandbox egress for this routine
	Template        string   `toml:"template"`
	Append          bool     `toml:"append"`
	MaxAttempts     int      `toml:"max_attempts"`
	Input           string   `toml:"input"`  // gather (default) | runs (ledger rows since the last run: learn) | pending (items queued with `qilla enqueue <r> --text`: judge)
	Rules           string   `toml:"rules"`  // vault-relative rules file injected before the task (judge)
	Commit          bool     `toml:"commit"` // after a successful run qilla commits the vault (git add -A minus .obsidian); the model never runs git
}

// Web is the UI listener.
type Web struct {
	Listen       string `toml:"listen"`
	PasswordHash string `toml:"password_hash"`
	IdleExit     string `toml:"idle_exit"`
}

// Memory is the working-memory layer: what qilla needs and the user never reads.
type Memory struct {
	Backend            string `toml:"backend"`            // engram | sqlite
	SharedProject      string `toml:"shared_project"`     // notes every routine and agent sees (default "shared")
	EngramURL          string `toml:"engram_url"`         // http://127.0.0.1:7437 or unix:///run/user/1000/qilla-engram.sock
	EngramVersion      string `toml:"engram_version"`     // pinned release for the runtime mise.toml
	MaxChars           int    `toml:"memory_max_chars"`   // cap on injected memory per prompt
	SaidTTLDays        int    `toml:"said_ttl_days"`      // TTL floor for kind=said
	ExpireUnusedDays   int    `toml:"expire_unused_days"` // demotion by use
	PromoteSupport     int    `toml:"promote_support"`    // support needed to propose a vault fact
	PromoteDays        int    `toml:"promote_days"`       // stable for this long
	ConflictAutoDays   int    `toml:"conflict_auto_days"`
	ConsolidateDryRuns int    `toml:"consolidate_dry_run_weeks"`
	Vectors            bool   `toml:"vectors"`
	EmbedURL           string `toml:"embed_url"`
	EmbedModel         string `toml:"embed_model"`
}

// Sandbox controls Claude Code's own Bash sandbox (bubblewrap) for runs.
type Sandbox struct {
	Disabled       bool     `toml:"disabled"`        // default on
	AllowedDomains []string `toml:"allowed_domains"` // network egress the sandbox permits
	BindRO         []string `toml:"bind_ro"`         // extra paths visible read-only inside the unit (helpers, tool data)
	BindRW         []string `toml:"bind_rw"`         // extra paths visible read-write inside the unit (caches, state)
}

// Chat is the interactive launcher's defaults (qilla chat).
type Chat struct {
	Model      string   `toml:"model"`       // default claude-fable-5-1; degrades with usage via [models].ladder
	Effort     string   `toml:"effort"`      // low | medium | high
	RC         *bool    `toml:"rc"`          // Remote Control on by default
	StatusLine string   `toml:"statusline"`  // the user's own status line command qilla wraps with its badge (default: `statusline` on PATH)
	Settings   string   `toml:"settings"`    // extra Claude settings JSON merged into the chat settings (theme, remoteControlAtStartup…)
	ClaudeArgs []string `toml:"claude_args"` // extra flags for interactive sessions, e.g. ["--disallowed-tools", "Artifact", "--no-chrome"]
	// AutocompactPct auto-compacts the interactive session at this % of the
	// context window (nil = 70, the qilla default; 0 = leave Claude Code's own,
	// which triggers very late). Survives claude.Scrub as an explicit env var.
	AutocompactPct *int `toml:"autocompact_pct"`
	// SessionMaxIdle rotates the agent's main session instead of resuming it
	// once it has been idle this long: a marathon session re-sends its whole
	// context every turn, so resuming a cold one is pure cost. Go duration;
	// empty = 6h, "0" = never rotate.
	SessionMaxIdle string   `toml:"session_max_idle"`
	Openings       Openings `toml:"openings"` // first message by time of day when qilla chat is started bare
}

// DefaultSessionMaxIdle applies when [chat].session_max_idle is unset.
const DefaultSessionMaxIdle = 6 * time.Hour

// MaxIdle parses SessionMaxIdle; 0 means never rotate.
func (c Chat) MaxIdle() (time.Duration, error) {
	if strings.TrimSpace(c.SessionMaxIdle) == "" {
		return DefaultSessionMaxIdle, nil
	}
	d, err := time.ParseDuration(strings.TrimSpace(c.SessionMaxIdle))
	if err != nil {
		return 0, err
	}
	if d < 0 {
		return 0, fmt.Errorf("must not be negative")
	}
	return d, nil
}

// Openings are the bare `qilla chat` first messages; empty = just resume.
type Openings struct {
	Morning   string `toml:"morning"`   // before 14:00
	Afternoon string `toml:"afternoon"` // 14:00–18:00
	Evening   string `toml:"evening"`   // after 18:00
	Resume    string `toml:"resume"`    // when the agent's session was already used today
}

// Browser is the headless/headed browser pair routines may use.
type Browser struct {
	Headless string `toml:"headless"` // CLI, default agent-browser
	Headed   string `toml:"headed"`   // command with %u (url) and %p (profile dir); empty = no handoff
}

// Artifacts are HTML files runs and chats produce for the page.
type Artifacts struct {
	TTLDays int   `toml:"ttl_days"` // default 30
	MaxMB   int64 `toml:"max_mb"`   // default 5
}

// Tasks are the aging thresholds for open dated tasks (`qilla task aging`).
type Tasks struct {
	WarnDays  int `toml:"warn_days"`  // default 3
	StaleDays int `toml:"stale_days"` // default 7
}

// Gmail is the OAuth client and the accounts behind `qilla gmail` (staging
// ledger + trash applier). Nothing here is a secret: tokens live in
// `qilla secret` as gmail-<account>.
type Gmail struct {
	ClientSecrets string   `toml:"client_secrets"` // Google Cloud installed-app client JSON
	Accounts      []string `toml:"accounts"`       // addresses qilla may act on
	StageFile     string   `toml:"stage_file"`     // default ~/.local/state/qilla/gmail-stage.jsonl
}

// Guard is the PreToolUse rail set. Patterns are RE2 regexes matched against the Bash command.
type Guard struct {
	Scope        string   `toml:"scope"`          // vault (default: only when cwd is inside the vault) | all
	Ask          []string `toml:"ask"`            // commands that need the user's explicit OK per use (default: ssh/scp/sftp/rsync-to-host)
	Deny         []string `toml:"deny"`           // commands never allowed
	ReadMaxLines int      `toml:"read_max_lines"` // whole-file Read above this is denied (default 400; 0 = off)
}

// Hooks are user extensions qilla's hooks call after their own work.
type Hooks struct {
	SessionStart string `toml:"session_start"` // extra command whose stdout is appended to the session context
	Stop         string `toml:"stop"`          // extra command run at Stop (e.g. a learn reminder); empty = nothing
}

// Health is the stack watch: systemd user units that must not be failed and
// artifacts that must stay fresh (each entry "<vault-relative path>=<spec>",
// spec: daily@HH:MM | Nh | Nd, or "git@<remote>=Nd" for unpushed commits).
type Health struct {
	WatchedServices []string `toml:"watched_services"`
	Freshness       []string `toml:"freshness"`
}

// Plugins are Claude Code plugin directories loaded only into qilla's own spawns
// (vault-relative or absolute). User-space skills live here, never user-wide.
type Plugins struct {
	Dirs []string `toml:"dirs"`
}

// Config is the whole file.
type Config struct {
	Vault         string             `toml:"vault"`
	StateDir      string             `toml:"state_dir"`
	QueueDir      string             `toml:"queue_dir"`
	Persona       string             `toml:"persona"`
	Rules         string             `toml:"rules"`
	FactsDir      string             `toml:"facts_dir"`
	JournalDir    string             `toml:"journal_dir"`
	Todo          string             `toml:"todo"`
	Questions     string             `toml:"questions"`      // vault-relative queue of open questions; its "- [ ]" lines are the statusline ❓ count
	DailyTemplate string             `toml:"daily_template"` // vault-relative template a missing daily note is created from
	ReplyMarker   string             `toml:"reply_marker"`   // prefix of an owner's reply line under a queued question
	RecallCmd     string             `toml:"recall_cmd"`
	RecallTimeout string             `toml:"recall_timeout"` // default 45s
	Claude        string             `toml:"claude"`
	Timezone      string             `toml:"timezone"`
	Budget        Budget             `toml:"budget"`
	Web           Web                `toml:"web"`
	Memory        Memory             `toml:"memory"`
	Sandbox       Sandbox            `toml:"sandbox"`
	Models        models.Tiers       `toml:"models"`
	Chat          Chat               `toml:"chat"`
	Browser       Browser            `toml:"browser"`
	Artifacts     Artifacts          `toml:"artifacts"`
	Guard         Guard              `toml:"guard"`
	Gmail         Gmail              `toml:"gmail"`
	Tasks         Tasks              `toml:"tasks"`
	Hooks         Hooks              `toml:"hooks"`
	Plugins       Plugins            `toml:"plugins"`
	Health        Health             `toml:"health"`
	Agents        map[string]Agent   `toml:"agents"`
	Routines      map[string]Routine `toml:"routines"`

	Path string `toml:"-"` // where it was loaded from
}

// MCPConfig is the optional Claude Code MCP servers file next to qilla.toml.
func (c *Config) MCPConfig() string {
	p := filepath.Join(filepath.Dir(c.Path), "mcp.json")
	if _, err := os.Stat(p); err != nil {
		return ""
	}
	return p
}

// DefaultPath is ~/.config/qilla/qilla.toml (or $QILLA_CONFIG).
func DefaultPath() string {
	if p := os.Getenv("QILLA_CONFIG"); p != "" {
		return p
	}
	base, err := os.UserConfigDir()
	if err != nil {
		base = Expand("~/.config")
	}
	return filepath.Join(base, "qilla", "qilla.toml")
}

// Load reads, defaults and validates a config file.
func Load(path string) (*Config, error) {
	var c Config
	md, err := toml.DecodeFile(path, &c)
	if err != nil {
		return nil, fmt.Errorf("config %s: %w", path, err)
	}
	if undecoded := md.Undecoded(); len(undecoded) > 0 {
		return nil, fmt.Errorf("config %s: unknown keys: %v", path, undecoded)
	}
	c.Path = path
	c.applyDefaults()
	if err := c.Validate(); err != nil {
		return nil, fmt.Errorf("config %s: %w", path, err)
	}
	return &c, nil
}

func (c *Config) applyDefaults() {
	def := func(p *string, v string) {
		if *p == "" {
			*p = v
		}
	}
	def(&c.Vault, "~/Notes")
	def(&c.StateDir, "~/.local/state/qilla")
	def(&c.QueueDir, "~/.cache/qilla/helpers/queue")
	def(&c.Persona, "Qilla/Persona.md")
	def(&c.Rules, "Qilla/Rules.md")
	def(&c.FactsDir, "Qilla/Facts")
	def(&c.JournalDir, "Desk/Journal")
	def(&c.Todo, "Desk/Todo.md")
	def(&c.Questions, "Desk/Questions.md")
	def(&c.DailyTemplate, "Qilla/Templates/Daily.md")
	def(&c.ReplyMarker, "💬 owner:")
	def(&c.RecallCmd, "qmd query {{query}} --files -n {{n}} --no-rerank")
	def(&c.Claude, "claude")
	def(&c.Web.Listen, "127.0.0.1:7433")
	def(&c.Web.IdleExit, "10m")
	def(&c.Memory.Backend, "engram")
	def(&c.Memory.SharedProject, "shared")
	def(&c.Memory.EngramVersion, "v2.0.0-rc.10")
	if c.Memory.EngramURL == "" {
		c.Memory.EngramURL = "unix://" + filepath.Join(runtimeDir(), "qilla-engram.sock")
	}
	defInt := func(p *int, v int) {
		if *p == 0 {
			*p = v
		}
	}
	defInt(&c.Memory.MaxChars, 2000)
	defInt(&c.Memory.SaidTTLDays, 90)
	defInt(&c.Memory.ExpireUnusedDays, 90)
	defInt(&c.Memory.PromoteSupport, 5)
	defInt(&c.Memory.PromoteDays, 14)
	defInt(&c.Memory.ConflictAutoDays, 7)
	defInt(&c.Memory.ConsolidateDryRuns, 3)
	defInt(&c.Tasks.WarnDays, 3)
	defInt(&c.Tasks.StaleDays, 7)
	c.Vault, c.StateDir, c.QueueDir = Expand(c.Vault), Expand(c.StateDir), Expand(c.QueueDir)
	for i, p := range c.Sandbox.BindRO {
		c.Sandbox.BindRO[i] = Expand(p)
	}
	for i, p := range c.Sandbox.BindRW {
		c.Sandbox.BindRW[i] = Expand(p)
	}
	c.Models.Defaults()
	def(&c.Gmail.StageFile, "~/.local/state/qilla/gmail-stage.jsonl")
	c.Gmail.StageFile = Expand(c.Gmail.StageFile)
	c.Gmail.ClientSecrets = Expand(c.Gmail.ClientSecrets)
	def(&c.Guard.Scope, "vault")
	if len(c.Guard.Ask) == 0 {
		c.Guard.Ask = []string{`(^|[;&|` + "`" + `$([[:space:]])(ssh|scp|sftp|ssh-copy-id|sshpass)([[:space:]]|$)`, `(^|[;&|[:space:]])rsync[^|;&]*[[:space:]][^[:space:]]+:`, `(^|[;&|[:space:]])qilla[[:space:]]+gmail[[:space:]]+(apply|untrash)([[:space:]]|$)`}
	}
	if c.Guard.ReadMaxLines == 0 {
		c.Guard.ReadMaxLines = 400
	}
	c.Hooks.SessionStart = Expand(c.Hooks.SessionStart)
	c.Hooks.Stop = Expand(c.Hooks.Stop)
	if c.Artifacts.TTLDays == 0 {
		c.Artifacts.TTLDays = 30
	}
	if c.Artifacts.MaxMB == 0 {
		c.Artifacts.MaxMB = 5
	}
	if c.Chat.AutocompactPct == nil {
		n := 70
		c.Chat.AutocompactPct = &n
	}
	def(&c.Chat.Model, "claude-fable-5-1")
	def(&c.Chat.Effort, "low")
	c.Chat.StatusLine = Expand(c.Chat.StatusLine)
	c.Chat.Settings = Expand(c.Chat.Settings)
	if c.Chat.RC == nil {
		t := true
		c.Chat.RC = &t
	}
	c.Models.UsageCache = Expand(c.Models.UsageCache)
	for name, r := range c.Routines {
		if r.MaxAttempts == 0 {
			r.MaxAttempts = 3
		}
		if r.Kind != KindScript && r.Agent == "" {
			r.Agent = "chief"
		}
		c.Routines[name] = r
	}
}

var (
	nameRe  = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)
	scopeRe = regexp.MustCompile(`^(persona|rules|facts:[A-Za-z0-9_-]+|journal:[0-9]+d|recall:[0-9]+|memory:[0-9]+|file:.+)$`)
)

// Validate reports the first thing that would make a run fail.
func (c *Config) Validate() error {
	if c.Budget.Block > 0 && c.Budget.Warn > c.Budget.Block {
		return errors.New("budget.warn above budget.block")
	}
	for name, r := range c.Routines {
		if !nameRe.MatchString(name) {
			return fmt.Errorf("routine %q: name must be lowercase [a-z0-9-]", name)
		}
		switch r.Kind {
		case KindScript, KindFresh, KindResumed:
		default:
			return fmt.Errorf("routine %s: kind must be %s | %s | %s", name, KindScript, KindFresh, KindResumed)
		}
		if r.Schedule == "" {
			return fmt.Errorf("routine %s: schedule required (systemd OnCalendar, or \"manual\" for ask/enqueue-only routines)", name)
		}
		switch r.Input {
		case "", "gather", "runs", "pending":
		default:
			return fmt.Errorf("routine %s: input must be gather | runs | pending", name)
		}
		if r.Kind != KindScript {
			if _, ok := c.Agents[r.Agent]; !ok {
				return fmt.Errorf("routine %s: agent %q not defined", name, r.Agent)
			}
		}
		if r.Tier != "" && !models.Valid(r.Tier) {
			return fmt.Errorf("routine %s: unknown tier %q (%v)", name, r.Tier, models.Names)
		}
		for _, s := range r.Scope {
			if !scopeRe.MatchString(s) {
				return fmt.Errorf("routine %s: bad scope token %q", name, s)
			}
		}
		if r.Window != "" && !windowRe.MatchString(r.Window) {
			return fmt.Errorf("routine %s: window must be HH:MM-HH:MM", name)
		}
	}
	if p := c.Chat.AutocompactPct; p != nil && *p != 0 && (*p < 10 || *p > 95) {
		return fmt.Errorf("chat.autocompact_pct must be 0 or 10..95, got %d", *p)
	}
	if _, err := c.Chat.MaxIdle(); err != nil {
		return fmt.Errorf("chat.session_max_idle: %w", err)
	}
	if len(c.Gmail.Accounts) > 0 && c.Gmail.ClientSecrets == "" {
		return errors.New("gmail.accounts set but gmail.client_secrets missing")
	}
	if b := c.Memory.Backend; b != "engram" && b != "sqlite" {
		return fmt.Errorf("memory.backend must be engram | sqlite")
	}
	for name, a := range c.Agents {
		if a.Tier != "" && !models.Valid(a.Tier) {
			return fmt.Errorf("agent %s: unknown tier %q (%v)", name, a.Tier, models.Names)
		}
		for _, s := range a.Scope {
			if !scopeRe.MatchString(s) {
				return fmt.Errorf("agent %s: bad scope token %q", name, s)
			}
		}
	}
	return nil
}

var windowRe = regexp.MustCompile(`^([01][0-9]|2[0-3]):[0-5][0-9]-([01][0-9]|2[0-3]):[0-5][0-9]$`)

// Expand replaces a leading ~ with the home directory.
func Expand(p string) string {
	if strings.HasPrefix(p, "~") {
		home, _ := os.UserHomeDir()
		return filepath.Join(home, p[1:])
	}
	return p
}

// VaultPath resolves a vault-relative path.
func (c *Config) VaultPath(rel string) string { return filepath.Join(c.Vault, rel) }

// PluginDirs resolves [plugins].dirs to absolute paths (vault-relative unless absolute or ~).
func (c *Config) PluginDirs() []string {
	var out []string
	for _, d := range c.Plugins.Dirs {
		d = Expand(d)
		if !filepath.IsAbs(d) {
			d = filepath.Join(c.Vault, d)
		}
		if _, err := os.Stat(d); err == nil {
			out = append(out, d)
		}
	}
	return out
}

// ArtifactsDir is where served HTML lives.
func (c *Config) ArtifactsDir() string { return filepath.Join(c.StateDir, "artifacts") }

// DBPath is the SQLite file holding jobs, ledger and sessions.
func (c *Config) DBPath() string { return filepath.Join(c.StateDir, "qilla.db") }

// runtimeDir is $XDG_RUNTIME_DIR or a per-user temp dir.
func runtimeDir() string {
	if d := os.Getenv("XDG_RUNTIME_DIR"); d != "" {
		return d
	}
	return filepath.Join(os.TempDir(), fmt.Sprintf("qilla-%d", os.Getuid()))
}
