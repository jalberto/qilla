// Package doctor checks everything that would make tomorrow morning fail.
package doctor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jalberto/qilla/internal/config"
	"github.com/jalberto/qilla/internal/install"
	"github.com/jalberto/qilla/internal/loader"
	"github.com/jalberto/qilla/internal/manifest"
	"github.com/jalberto/qilla/internal/mem"
	"github.com/jalberto/qilla/internal/models"
	"github.com/jalberto/qilla/internal/plugin"
	"github.com/jalberto/qilla/internal/prices"
	"github.com/jalberto/qilla/internal/render"
	"github.com/jalberto/qilla/internal/subagents"
)

// Check is one row of the report.
type Check struct {
	Name string `json:"name"`
	OK   bool   `json:"ok"`
	Hard bool   `json:"hard"` // a failing hard check exits 1
	Info string `json:"info"`
}

// Env is what the checks probe; tests substitute it.
type Env struct {
	LookPath      func(string) (string, error)
	Stat          func(string) (os.FileInfo, error)
	Run           func(name string, args ...string) (string, error) // stdout, err
	UnitActive    func(unit string) bool
	Validate      func(tmplPath string) error                // knap validate; nil = skip
	ListTimers    func() ([]Timer, error)                    // systemctl --user list-timers; nil = skip
	EngramHealth  func(url string) error                     // probe the memory sidecar; nil = skip
	SecurityScore func(unit string) (float64, error)         // systemd-analyze security; nil = skip
	Calendar      func(spec string) error                    // systemd-analyze calendar; nil = skip
	MiseMissing   func(configDir string) ([]string, error)   // tools in <configDir>/mise.toml not installed; nil = skip
	MiseWarnings  func() (int, string, error)                // `mise doctor`: warning count + the first one; nil = skip
	UnitState     func(unit string) string                   // watched unit: active | inactive | failed | not-found; nil = skip the watch
	Restart       func(unit string) error                    // systemctl --user restart, for `qilla doctor fix`; nil = skip
	Sleep         func(time.Duration)                        // settle time between restart and re-check; nil = do not wait
	Jobs          func(*config.Config) ([]JobProblem, error) // failed/parked queue jobs; nil = skip
	Now           func() time.Time                           // fixed clock in tests; nil = time.Now
	// RoutineCheck evaluates a routine bundle's routine.toml requirements;
	// nil = skip (and legacy bundles without a manifest return no results).
	RoutineCheck func(name string) ([]manifest.Result, error)
	// mise shims as the units see them: the generated shim names, which of a
	// candidate set has no version inside the unit, the backend spec to pin, and
	// whether a system binary of that name would satisfy the shim's fallback.
	ShimNames      func() (map[string]bool, error)
	ShimMissing    func(configDir string, names []string) []string
	ShimSpecs      func(names []string) map[string]string
	SystemLookPath func(name string) bool
	Home           string
	ConfigDir      string
}

// Default probes the real machine.
func Default(configPath string) Env {
	home, _ := os.UserHomeDir()
	return Env{
		LookPath: exec.LookPath, Stat: os.Stat, Home: home, ConfigDir: filepath.Dir(configPath),
		Run: func(name string, args ...string) (string, error) {
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			out, err := exec.CommandContext(ctx, name, args...).Output()
			return string(out), err
		},
		Validate:       func(p string) error { return render.Validate("", p) },
		ListTimers:     listTimers,
		MiseMissing:    miseMissing,
		MiseWarnings:   func() (int, string, error) { return miseWarnings(filepath.Dir(configPath)) },
		ShimNames:      shimNames,
		ShimMissing:    shimMissing,
		SystemLookPath: systemLookPath,
		ShimSpecs:      shimSpecs,
		SecurityScore:  securityScore,
		Calendar: func(spec string) error {
			out, err := exec.Command("systemd-analyze", "calendar", spec).CombinedOutput()
			if err != nil {
				return fmt.Errorf("%s", strings.TrimSpace(string(out)))
			}
			return nil
		},
		EngramHealth: func(u string) error {
			b, err := mem.NewEngram(u)
			if err != nil {
				return err
			}
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			return b.Health(ctx)
		},
		UnitState: unitState,
		Restart: func(unit string) error {
			return exec.Command("systemctl", "--user", "restart", unit).Run()
		},
		Sleep: time.Sleep,
		Jobs:  queueProblems,
		UnitActive: func(unit string) bool {
			return exec.Command("systemctl", "--user", "is-active", "-q", unit).Run() == nil
		},
	}
}

// Run evaluates every check. cfg may be nil when the config failed to load
// (then loadErr explains why and only that check is reported).
func Run(cfg *config.Config, loadErr error, env Env) []Check {
	if loadErr != nil {
		return []Check{{"config", false, true, loadErr.Error()}}
	}
	var out []Check
	add := func(name string, ok, hard bool, info string) { out = append(out, Check{name, ok, hard, info}) }
	add("config", true, true, cfg.Path)

	if fi, err := env.Stat(cfg.Vault); err != nil || !fi.IsDir() {
		add("vault", false, true, cfg.Vault+" not a directory")
	} else {
		add("vault", true, true, cfg.Vault)
	}
	for _, f := range []struct{ name, rel string }{{"persona", cfg.Persona}, {"rules", cfg.Rules}} {
		if _, err := env.Stat(cfg.VaultPath(f.rel)); err != nil {
			add(f.name, false, false, f.rel+" missing in vault — AI runs will lack it")
		} else {
			add(f.name, true, false, f.rel)
		}
	}
	if err := os.MkdirAll(cfg.StateDir, 0o755); err != nil {
		add("state dir", false, true, err.Error())
	} else {
		add("state dir", true, true, cfg.StateDir)
	}

	// harness
	if p, err := env.LookPath(cfg.Claude); err != nil {
		add("claude", false, true, cfg.Claude+" not on PATH — `mise install -C "+env.ConfigDir+"` or npm i -g @anthropic-ai/claude-code")
	} else {
		v, _ := env.Run(p, "--version")
		add("claude", true, true, strings.TrimSpace(firstLine(v)))
		st, err := env.Run(p, "auth", "status")
		switch {
		case err == nil && strings.Contains(st, `"loggedIn": true`):
			add("claude login", true, true, "logged in")
		case fileExists(env, filepath.Join(env.Home, ".claude", ".credentials.json")):
			add("claude login", true, true, "credentials present")
		default:
			add("claude login", false, true, "run `claude` once and /login")
		}
	}

	// recall + renderer tools
	recallBin := strings.Fields(cfg.RecallCmd)
	if len(recallBin) > 0 {
		if _, err := env.LookPath(recallBin[0]); err != nil {
			add("recall", false, hasScope(cfg, "recall:"), recallBin[0]+" not on PATH (recall_cmd)")
		} else {
			add("recall", true, false, recallBin[0])
		}
	}
	if _, err := env.LookPath("knap"); err != nil {
		add("knap", false, hasOutput(cfg), "knap not on PATH — routines with output/template cannot render")
	} else {
		add("knap", true, false, "knap")
		for name, r := range cfg.Routines {
			if r.Output == "" {
				continue
			}
			tmpl := r.Template
			if tmpl == "" {
				tmpl = filepath.Join("Qilla", "Routines", name, "template.md")
			}
			if _, err := env.Stat(cfg.VaultPath(tmpl)); err != nil {
				add("template "+name, false, true, tmpl+" missing")
			} else if env.Validate != nil {
				if err := env.Validate(cfg.VaultPath(tmpl)); err != nil {
					add("template "+name, false, true, err.Error())
				} else {
					add("template "+name, true, false, tmpl)
				}
			}
		}
	}

	// working memory
	if cfg.Memory.Backend == "engram" {
		if env.EngramHealth == nil {
			add("memory", true, false, "engram (not probed)")
		} else if err := env.EngramHealth(cfg.Memory.EngramURL); err != nil {
			add("memory", false, false, "engram unreachable at "+cfg.Memory.EngramURL+" — systemctl --user start qilla-engram.service (AI runs continue without working memory)")
		} else {
			add("memory", true, false, "engram at "+cfg.Memory.EngramURL)
		}
	} else {
		add("memory", true, false, "sqlite (own FTS5 tables)")
	}

	// gmail
	if cfg.Gmail.ClientSecrets != "" || len(cfg.Gmail.Accounts) > 0 {
		if _, err := env.Stat(cfg.Gmail.ClientSecrets); err != nil {
			add("gmail", false, false, cfg.Gmail.ClientSecrets+" missing — `qilla gmail auth` cannot run (gmail.client_secrets)")
		} else {
			add("gmail", true, false, fmt.Sprintf("%d account(s), stage %s", len(cfg.Gmail.Accounts), cfg.Gmail.StageFile))
		}
	}

	// prices
	if _, err := env.Stat(prices.File(cfg.Path)); err != nil {
		add("prices", false, false, "prices.json missing — costs show as unpriced until `qilla prices sync`")
	} else {
		add("prices", true, false, prices.File(cfg.Path))
	}

	// scopes validate at doctor time, never at run time
	for name, r := range cfg.Routines {
		if err := loader.ValidateScope(r.Scope); err != nil {
			add("routine "+name, false, true, err.Error())
		}
		if r.Kind != config.KindScript {
			if _, err := env.Stat(filepath.Join(cfg.Vault, "Qilla", "Routines", name, "prompt.md")); err != nil {
				add("routine "+name, false, true, "Qilla/Routines/"+name+"/prompt.md missing")
			}
		}
		// the gather step is gather.sh (any language) or gather.star (built-in
		// Starlark); with both, gather.sh wins and the other is dead weight.
		_, shErr := env.Stat(filepath.Join(cfg.Vault, "Qilla", "Routines", name, "gather.sh"))
		_, starErr := env.Stat(filepath.Join(cfg.Vault, "Qilla", "Routines", name, "gather.star"))
		if shErr == nil && starErr == nil {
			add("routine "+name, false, false, "Qilla/Routines/"+name+": both gather.sh and gather.star — gather.sh wins")
		}
		if r.Kind == config.KindScript && shErr != nil && starErr != nil {
			add("routine "+name, false, true, "Qilla/Routines/"+name+"/gather.sh or gather.star missing (kind script)")
		}
	}

	// routine manifests: one soft row per routine whose requirements are unmet
	if env.RoutineCheck != nil {
		names := make([]string, 0, len(cfg.Routines))
		for n := range cfg.Routines {
			names = append(names, n)
		}
		sort.Strings(names)
		for _, n := range names {
			rs, err := env.RoutineCheck(n)
			if err != nil {
				add("routine "+n, false, false, "routine "+n+": "+err.Error())
				continue
			}
			if f := manifest.FirstFailure(rs); f != "" {
				add("routine "+n, false, false, "routine "+n+": "+f)
			}
		}
	}

	// sandbox
	if env.SecurityScore != nil {
		if score, err := env.SecurityScore("qilla.service"); err == nil {
			ok := score <= 4.0
			add("sandbox", ok, false, fmt.Sprintf("systemd-analyze security exposure %.1f (≤ 4.0 wanted)", score))
		}
	}
	if _, err := env.LookPath("bwrap"); err != nil {
		add("bwrap", false, !cfg.Sandbox.Disabled, "bubblewrap not on PATH — Claude Code's Bash sandbox cannot run (dnf install bubblewrap socat, or sandbox.disabled = true)")
	} else {
		add("bwrap", true, false, "bubblewrap present")
	}
	if _, err := env.LookPath("socat"); err != nil {
		add("socat", false, false, "socat not on PATH — sandbox network allowlist cannot be enforced (dnf install socat)")
	} else {
		add("socat", true, false, "socat present")
	}
	if _, err := env.LookPath("systemd-creds"); err != nil {
		add("systemd-creds", false, len(install.SecretNames(env.ConfigDir)) > 0, "systemd-creds not on PATH — `qilla secret` cannot encrypt/decrypt")
	} else {
		add("systemd-creds", true, false, "present")
	}
	if _, err := env.LookPath("mise"); err != nil {
		add("mise", false, true, "mise not on PATH — runtime tools (claude, engram, knap, qmd) are pinned in "+filepath.Join(env.ConfigDir, "mise.toml"))
	} else if env.MiseMissing != nil {
		if missing, err := env.MiseMissing(env.ConfigDir); err != nil {
			add("mise", true, false, "installed (tool list not probed: "+firstLine(err.Error())+")")
		} else if len(missing) > 0 {
			add("mise", false, false, "missing tools: "+strings.Join(missing, ", ")+" — MISE_GLOBAL_CONFIG_FILE="+filepath.Join(env.ConfigDir, "mise.toml")+" mise install")
		} else if n, first, err := miseDoctor(env); err == nil && n > 0 {
			// mise doctor catches environment problems the pinned-tool list
			// can't see (PATH shadowing above all); soft, never hard.
			add("mise", false, false, fmt.Sprintf("%d mise doctor warning%s: %s", n, plural(n), first))
		} else {
			add("mise", true, false, "every pinned tool installed")
		}
	} else {
		add("mise", true, false, "present")
	}
	// A helper installed through the user's own global mise config is invisible
	// inside the unit (MISE_GLOBAL_CONFIG_FILE points at the runtime mise.toml)
	// and dies there with "No version is set for shim: <name>".
	if env.ShimNames != nil && env.ShimMissing != nil {
		if shims, err := env.ShimNames(); err == nil && len(shims) > 0 {
			cands := shimCandidates(allowedTools(cfg), routineScripts(cfg.Vault), shims)
			if len(cands) > 0 {
				// A shim with no version still works when a system binary of the
				// same name is on the unit PATH (jq, rg): the shim falls back to it.
				missing := shimsThatBreak(env.ShimMissing(env.ConfigDir, cands), env.SystemLookPath)
				if len(missing) == 0 {
					add("mise shims", true, false, fmt.Sprintf("%d helper shim%s resolve inside the unit", len(cands), plural(len(cands))))
				} else {
					specs := map[string]string{}
					if env.ShimSpecs != nil {
						specs = env.ShimSpecs(missing)
					}
					add("mise shims", false, false, formatShimHint(missing, specs, filepath.Join(env.ConfigDir, "mise.toml")))
				}
			}
		}
	}
	if name, _ := env.Run("git", "config", "--get", "user.name"); strings.TrimSpace(name) == "" {
		add("git identity", false, anyCommit(cfg), "git user.name unset — routines with commit = true cannot commit (git config --global user.name …)")
	} else {
		add("git identity", true, false, strings.TrimSpace(name))
	}
	// user space files every run relies on
	for _, h := range []struct{ name, cmd string }{{"hook session_start", cfg.Hooks.SessionStart}, {"hook stop", cfg.Hooks.Stop}} {
		f := strings.Fields(h.cmd)
		if len(f) == 0 || !filepath.IsAbs(f[0]) {
			continue
		}
		if fi, err := env.Stat(f[0]); err != nil {
			add(h.name, false, false, f[0]+" missing — `qilla init` regenerates a no-op")
		} else if fi.Mode()&0o100 == 0 {
			add(h.name, false, false, f[0]+" not executable")
		} else {
			add(h.name, true, false, f[0])
		}
	}
	if _, err := env.Stat(cfg.VaultPath(".claude/settings.json")); err != nil {
		add("vault settings", false, false, ".claude/settings.json missing in vault — `qilla init` writes one enabling the qilla plugin for this project")
	} else {
		add("vault settings", true, false, "project-scoped Claude settings present")
	}

	// identity: qilla never touches user-level Claude settings; sub-agents travel per spawn
	if b, err := os.ReadFile(filepath.Join(env.Home, ".claude", "settings.json")); err == nil {
		// the only user-wide entries qilla may own: the statusline dispatcher and its plugin marketplace
		stripped := strings.NewReplacer(`"qilla statusline"`, "", `"qilla@qilla"`, "", `"qilla":`, "", "/qilla/plugin", "", "qilla/plugin", "").Replace(string(b))
		if strings.Contains(stripped, "qilla") {
			add("identity", false, false, "~/.claude/settings.json has qilla entries beyond `qilla statusline` and the qilla plugin — qilla installs nothing else user-wide")
		} else {
			add("identity", true, false, "user-level Claude settings: only the statusline dispatcher and the plugin; badge + markers per spawn")
		}
	} else {
		add("identity", true, false, "no user-level Claude settings")
	}
	if defs, err := subagents.Load(cfg.Vault, cfg.Models); err == nil {
		add("subagents", true, false, strings.Join(subagents.Names(defs), ", ")+" (built-in + "+subagents.UserDir+")")
	}

	// browser
	hb := cfg.Browser.Headless
	if hb == "" {
		hb = "agent-browser"
	}
	if _, err := env.LookPath(hb); err != nil {
		add("browser", false, false, hb+" not on PATH — routines cannot browse (npm i -g agent-browser)")
	} else if cfg.Browser.Headed == "" {
		add("browser", true, false, hb+" headless; no headed fallback configured ([browser].headed)")
	} else {
		add("browser", true, false, hb+" headless · handoff: "+firstLine(cfg.Browser.Headed))
	}

	if len(cfg.Plugins.Dirs) > 0 {
		if dirs := cfg.PluginDirs(); len(dirs) == len(cfg.Plugins.Dirs) {
			// the dirs are not loaded as plugins any more: their skills are
			// symlinked into the one qilla plugin, so a missing link is the failure.
			if missing := unlinkedSkills(plugin.Dir(env.ConfigDir), dirs); len(missing) > 0 {
				add("plugins", false, false, fmt.Sprintf("%d skill(s) not merged into the qilla plugin (%s) — run `qilla plugin install`", len(missing), strings.Join(missing, ", ")))
			} else {
				add("plugins", true, false, strings.Join(dirs, ", ")+" (merged into the qilla plugin)")
			}
		} else {
			add("plugins", false, false, fmt.Sprintf("%d of %d plugin dirs missing: %v", len(cfg.Plugins.Dirs)-len(dirs), len(cfg.Plugins.Dirs), cfg.Plugins.Dirs))
		}
	}

	// model tiers + usage
	if u := models.Usage(cfg.Models.UsageCache, time.Now()); u < 0 {
		add("usage", true, false, "seven-day usage unknown — tiers run undegraded")
	} else if u >= cfg.Models.DegradeAt {
		add("usage", false, false, fmt.Sprintf("seven-day usage %d%% ≥ %d%% — every tier degraded one step", u, cfg.Models.DegradeAt))
	} else {
		add("usage", true, false, fmt.Sprintf("seven-day usage %d%% (degrade at %d%%)", u, cfg.Models.DegradeAt))
	}

	// secrets
	if names := install.SecretNames(env.ConfigDir); len(names) > 0 {
		add("secrets", true, false, fmt.Sprintf("%d encrypted (%s) — gather.sh only", len(names), strings.Join(names, ", ")))
	} else {
		add("secrets", true, false, "none configured (qilla secret set <name>)")
	}

	// schedules: every OnCalendar part must parse (systemd-analyze calendar)
	if env.Calendar != nil {
		for name, r := range cfg.Routines {
			if strings.EqualFold(strings.TrimSpace(r.Schedule), "manual") {
				continue
			}
			for _, part := range strings.Split(r.Schedule, ";") {
				part = strings.TrimSpace(part)
				if part == "" {
					continue
				}
				if err := env.Calendar(part); err != nil {
					add("schedule "+name, false, true, fmt.Sprintf("%q: %s (use systemd OnCalendar syntax; several slots → separate with ';')", part, firstLine(err.Error())))
				}
			}
		}
	}

	// units
	if env.UnitActive != nil {
		if env.UnitActive("qilla.socket") {
			add("qilla.socket", true, false, "active")
		} else {
			add("qilla.socket", false, false, "not active — systemctl --user enable --now qilla.socket")
		}
		units := install.Units(cfg, install.Paths{})
		inactive := []string{}
		for _, t := range install.TimerNames(units) {
			if !env.UnitActive(t) {
				inactive = append(inactive, t)
			}
		}
		if len(inactive) == 0 {
			add("timers", true, false, fmt.Sprintf("%d active", len(install.TimerNames(units))))
		} else {
			add("timers", false, false, "inactive: "+strings.Join(inactive, " "))
		}
	}

	// the stack watch: units, freshness, queue
	out = append(out, Health(cfg, env)...)
	return out
}

func anyCommit(cfg *config.Config) bool {
	for _, r := range cfg.Routines {
		if r.Commit {
			return true
		}
	}
	return false
}

// miseMissing lists tools pinned in <configDir>/mise.toml that mise reports as
// not installed. --current restricts the listing to the tools the active config
// asks for, and MISE_IGNORED_CONFIG_PATHS drops the user's own global config —
// without both, every installed-but-broken tool on the machine is reported (and
// the unit's tmpfs home makes several of them unreachable anyway).
func miseMissing(configDir string) ([]string, error) {
	cmd := exec.Command("mise", "ls", "--current", "--missing", "--json")
	cmd.Env = unitEnv(configDir)
	out, err := cmd.Output()
	if err != nil {
		return nil, err
	}
	return parseMiseMissing(out)
}

// parseMiseMissing reads `mise ls --current --missing --json`: an object keyed
// by tool name (empty — "{}" — when nothing is missing).
func parseMiseMissing(out []byte) ([]string, error) {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(out, &m); err != nil {
		return nil, err
	}
	var names []string
	for k := range m {
		names = append(names, k)
	}
	sort.Strings(names)
	return names, nil
}

// userMiseConfig is the path of the user's own global mise config.
func userMiseConfig() string {
	if xdg := os.Getenv("XDG_CONFIG_HOME"); xdg != "" {
		return filepath.Join(xdg, "mise", "config.toml")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".config", "mise", "config.toml")
}

// unitEnv is the environment the systemd units run with: only the runtime
// mise.toml is visible, nothing auto-installs, shims come first on PATH.
func unitEnv(configDir string) []string {
	home, _ := os.UserHomeDir()
	return append(os.Environ(),
		"MISE_GLOBAL_CONFIG_FILE="+filepath.Join(configDir, "mise.toml"),
		"MISE_IGNORED_CONFIG_PATHS="+userMiseConfig(),
		"MISE_AUTO_INSTALL=false", "MISE_YES=1", "MISE_QUIET=1",
		"PATH="+strings.Join([]string{
			filepath.Join(home, ".local/share/mise/shims"),
			filepath.Join(home, ".local/bin"),
			"/usr/local/bin", "/usr/bin", "/bin",
		}, ":"))
}

// shimNames lists the shims mise has generated (one file per tool binary).
func shimNames() (map[string]bool, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	ents, err := os.ReadDir(filepath.Join(home, ".local/share/mise/shims"))
	if err != nil {
		return nil, err
	}
	names := map[string]bool{}
	for _, e := range ents {
		if !e.IsDir() {
			names[e.Name()] = true
		}
	}
	return names, nil
}

// allowedTools is every allowed_tools entry of every agent in cfg.
func allowedTools(cfg *config.Config) []string {
	var out []string
	for _, a := range cfg.Agents {
		out = append(out, a.AllowedTools...)
	}
	sort.Strings(out)
	return out
}

// routineScripts reads every routine gather.sh and bin/* in the vault; their
// text is scanned for helper names the agent config never mentions.
func routineScripts(vault string) []string {
	var out []string
	pats := []string{
		filepath.Join(vault, "Qilla", "Routines", "*", "gather.sh"),
		filepath.Join(vault, "Qilla", "Routines", "*", "bin", "*"),
	}
	for _, p := range pats {
		paths, _ := filepath.Glob(p)
		for _, f := range paths {
			if b, err := os.ReadFile(f); err == nil && len(b) < 1<<20 {
				out = append(out, string(b))
			}
		}
	}
	return out
}

var shimWord = regexp.MustCompile(`[A-Za-z0-9_.+-]+`)

// shimCandidates picks the helper names worth probing: the command of every
// Bash(...) allowed tool (the wrapped command when it goes through rtk) plus
// every word of the routine scripts — kept only when mise has a shim for it.
func shimCandidates(allowedTools []string, scripts []string, shimNames map[string]bool) []string {
	seen := map[string]bool{}
	var out []string
	keep := func(n string) {
		if n != "" && shimNames[n] && !seen[n] {
			seen[n] = true
			out = append(out, n)
		}
	}
	for _, t := range allowedTools {
		i := strings.IndexByte(t, '(')
		if i < 0 || !strings.EqualFold(strings.TrimSpace(t[:i]), "Bash") {
			continue
		}
		inner := strings.TrimSuffix(t[i+1:], ")")
		f := shimWord.FindAllString(inner, 3)
		if len(f) == 0 {
			continue
		}
		if f[0] == "rtk" && len(f) > 1 {
			keep(f[1])
			continue
		}
		keep(f[0])
	}
	for _, s := range scripts {
		for _, w := range shimWord.FindAllString(s, -1) {
			keep(w)
		}
	}
	sort.Strings(out)
	return out
}

// shimMissing runs `mise which <name>` with the unit env: a non-zero exit means
// the shim has no version there, however well it works in an interactive shell.
func shimMissing(configDir string, names []string) []string {
	var out []string
	for _, n := range names {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		cmd := exec.CommandContext(ctx, "mise", "-C", configDir, "which", n)
		cmd.Env = unitEnv(configDir)
		err := cmd.Run()
		cancel()
		if err != nil {
			out = append(out, n)
		}
	}
	return out
}

// shimsThatBreak drops the names a system binary covers: `mise which` failing
// only breaks the routine when the shim has nothing to fall back to.
func shimsThatBreak(unresolved []string, onSystemPath func(string) bool) []string {
	if onSystemPath == nil {
		return unresolved
	}
	var out []string
	for _, n := range unresolved {
		if !onSystemPath(n) {
			out = append(out, n)
		}
	}
	return out
}

// systemLookPath reports whether name is an executable on the unit PATH with
// the mise shims directory removed — what the shim itself falls back to.
func systemLookPath(name string) bool {
	home, _ := os.UserHomeDir()
	for _, dir := range []string{filepath.Join(home, ".local/bin"), "/usr/local/bin", "/usr/bin", "/bin"} {
		if fi, err := os.Stat(filepath.Join(dir, name)); err == nil && !fi.IsDir() && fi.Mode()&0o111 != 0 {
			return true
		}
	}
	return false
}

// shimSpecs maps a tool binary name to the backend spec that installed it, read
// from `mise ls --json` WITHOUT the unit env (that is where the user's own
// global config, the one holding these tools, is visible).
func shimSpecs(names []string) map[string]string {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "mise", "ls", "--json").Output()
	if err != nil {
		return nil
	}
	return parseShimSpecs(out, names)
}

// parseShimSpecs picks, for each wanted name, the installed `mise ls --json`
// key whose last path segment matches it.
func parseShimSpecs(out []byte, names []string) map[string]string {
	var m map[string][]struct {
		Installed bool `json:"installed"`
	}
	if err := json.Unmarshal(out, &m); err != nil {
		return nil
	}
	want := map[string]bool{}
	for _, n := range names {
		want[n] = true
	}
	specs := map[string]string{}
	for spec, vers := range m {
		short := spec
		if i := strings.LastIndexAny(short, "/:"); i >= 0 {
			short = short[i+1:]
		}
		if !want[short] {
			continue
		}
		for _, v := range vers {
			if v.Installed {
				specs[short] = spec
				break
			}
		}
	}
	return specs
}

// formatShimHint is the red `mise shims` row: what breaks, and the exact lines
// to paste into the runtime mise.toml.
func formatShimHint(missing []string, specs map[string]string, tomlPath string) string {
	var pins []string
	for _, n := range missing {
		spec := specs[n]
		if spec == "" {
			spec = n
		}
		pins = append(pins, fmt.Sprintf("%q = \"latest\"", spec))
	}
	return "shims without a version inside the unit: " + strings.Join(missing, ", ") +
		" — pin them in " + tomlPath + " (from mise ls --json: " + strings.Join(pins, ", ") + ")"
}

// miseDoctor calls the injected MiseWarnings probe, if any.
func miseDoctor(env Env) (int, string, error) {
	if env.MiseWarnings == nil {
		return 0, "", errors.New("not probed")
	}
	return env.MiseWarnings()
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

// miseWarnings runs `mise doctor` and parses its "N warning(s) found:" section,
// returning the count and the first warning (its headline, plus the detail line
// when the headline ends in a colon). It runs with the unit's environment and
// PATH, so the row describes the units, not the interactive shell.
func miseWarnings(configDir string) (int, string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "mise", "-C", configDir, "doctor")
	cmd.Env = unitEnv(configDir)
	out, err := cmd.CombinedOutput()
	if len(out) == 0 && err != nil {
		return 0, "", err // mise missing or unusable; a non-zero exit with output is normal
	}
	return parseMiseWarnings(string(out))
}

var miseWarnCount = regexp.MustCompile(`(?m)^(\d+) warnings? found:`)

func parseMiseWarnings(out string) (int, string, error) {
	m := miseWarnCount.FindStringSubmatchIndex(out)
	if m == nil {
		return 0, "", nil
	}
	n, _ := strconv.Atoi(out[m[2]:m[3]])
	lines := strings.Split(out[m[1]:], "\n")
	for i, ln := range lines {
		t := strings.TrimSpace(ln)
		if !strings.HasPrefix(t, "1. ") {
			continue
		}
		first := strings.TrimSpace(strings.TrimPrefix(t, "1. "))
		if strings.HasSuffix(first, ":") && i+1 < len(lines) {
			first += " " + strings.TrimSpace(lines[i+1])
		}
		if len(first) > 120 {
			first = first[:120] + "…"
		}
		return n, first, nil
	}
	return n, "", nil
}

// HardFailure reports whether any hard check failed.
func HardFailure(cs []Check) bool {
	for _, c := range cs {
		if c.Hard && !c.OK {
			return true
		}
	}
	return false
}

// Format renders the report for a terminal.
func Format(cs []Check) string {
	var b strings.Builder
	for _, c := range cs {
		info := c.Info
		if c.OK {
			info = dropHint(info) // a passing check does not need the fix-it hint
		}
		mark := "✓"
		if !c.OK {
			mark = "✗"
			if !c.Hard {
				mark = "!"
			}
		}
		fmt.Fprintf(&b, "%s %-16s %s\n", mark, c.Name, info)
	}
	return b.String()
}

// dropHint removes a trailing "(qilla …)" remediation note: useful when the
// check fails, noise on every line that already passed.
func dropHint(info string) string {
	i := strings.LastIndex(info, "(qilla ")
	if i <= 0 || !strings.HasSuffix(info, ")") {
		return info
	}
	return strings.TrimSpace(info[:i])
}

// JSON renders the report as JSON.
func JSON(cs []Check) []byte {
	b, _ := json.Marshal(cs)
	return b
}

func hasScope(cfg *config.Config, prefix string) bool {
	for _, r := range cfg.Routines {
		for _, s := range r.Scope {
			if strings.HasPrefix(s, prefix) {
				return true
			}
		}
	}
	for _, a := range cfg.Agents {
		for _, s := range a.Scope {
			if strings.HasPrefix(s, prefix) {
				return true
			}
		}
	}
	return false
}

func hasOutput(cfg *config.Config) bool {
	for _, r := range cfg.Routines {
		if r.Output != "" {
			return true
		}
	}
	return false
}

func fileExists(env Env, p string) bool { _, err := env.Stat(p); return err == nil }

func firstLine(s string) string { s, _, _ = strings.Cut(s, "\n"); return s }

// Timer is one row of `systemctl --user list-timers --output=json`.
type Timer struct {
	Unit     string `json:"unit"`
	Next     string `json:"next"`
	Last     string `json:"last"`
	Activate string `json:"activates"`
}

func listTimers() ([]Timer, error) {
	out, err := exec.Command("systemctl", "--user", "list-timers", "--all", "--output=json", "qilla-*").Output()
	if err != nil {
		return nil, err
	}
	var raw []struct {
		Unit      string `json:"unit"`
		Next      any    `json:"next"`
		Last      any    `json:"last"`
		Activates string `json:"activates"`
	}
	if err := json.Unmarshal(out, &raw); err != nil {
		return nil, err
	}
	var ts []Timer
	for _, r := range raw {
		ts = append(ts, Timer{Unit: r.Unit, Next: usec(r.Next), Last: usec(r.Last), Activate: r.Activates})
	}
	return ts, nil
}

// usec renders systemd's microsecond timestamps (or "-") as local time.
func usec(v any) string {
	f, ok := v.(float64)
	if !ok || f == 0 {
		return ""
	}
	return time.UnixMicro(int64(f)).Local().Format("Mon 15:04")
}

// Report is everything the status page shows besides checks.
type Report struct {
	Checks  []Check           `json:"checks"`
	Timers  []Timer           `json:"timers"`
	MCP     []string          `json:"mcp_servers"`
	Skills  []string          `json:"skills"`
	Version string            `json:"claude_version"`
	Extra   map[string]string `json:"extra,omitempty"`
}

// Full runs the checks and gathers timers, MCP servers and skills.
func Full(cfg *config.Config, loadErr error, env Env) Report {
	rep := Report{Checks: Run(cfg, loadErr, env)}
	if cfg == nil {
		return rep
	}
	if env.ListTimers != nil {
		rep.Timers, _ = env.ListTimers()
	}
	if p := cfg.MCPConfig(); p != "" {
		if b, err := os.ReadFile(p); err == nil {
			var m struct {
				Servers map[string]any `json:"mcpServers"`
			}
			if json.Unmarshal(b, &m) == nil {
				for k := range m.Servers {
					rep.MCP = append(rep.MCP, k)
				}
			}
		}
	}
	for _, dir := range []string{filepath.Join(cfg.Vault, ".claude", "skills"), filepath.Join(env.Home, ".claude", "skills")} {
		if es, err := os.ReadDir(dir); err == nil {
			for _, e := range es {
				if e.IsDir() {
					rep.Skills = append(rep.Skills, e.Name())
				}
			}
		}
	}
	for _, c := range rep.Checks {
		if c.Name == "claude" && c.OK {
			rep.Version = c.Info
		}
	}
	return rep
}

// securityScore parses "Overall exposure level for <unit>: 4.2 OK" from
// systemd-analyze security --user.
func securityScore(unit string) (float64, error) {
	out, err := exec.Command("systemd-analyze", "security", "--user", "--no-pager", unit).CombinedOutput()
	if err != nil {
		return 0, err
	}
	for _, line := range strings.Split(string(out), "\n") {
		if strings.Contains(line, "Overall exposure level") {
			f := strings.Fields(strings.TrimSpace(line[strings.Index(line, ":")+1:]))
			if len(f) > 0 {
				var v float64
				fmt.Sscanf(f[0], "%f", &v)
				return v, nil
			}
		}
	}
	return 0, fmt.Errorf("no exposure line")
}

// NextFire reads `systemctl --user list-timers` once and answers per routine.
func NextFire() func(routine string) time.Time {
	out, err := exec.Command("systemctl", "--user", "list-timers", "--all", "--output=json", "qilla-*").Output()
	next := map[string]time.Time{}
	if err == nil {
		var raw []struct {
			Unit string `json:"unit"`
			Next any    `json:"next"`
		}
		if json.Unmarshal(out, &raw) == nil {
			for _, r := range raw {
				if f, ok := r.Next.(float64); ok && f > 0 {
					name := strings.TrimSuffix(strings.TrimPrefix(r.Unit, "qilla-"), ".timer")
					next[name] = time.UnixMicro(int64(f))
				}
			}
		}
	}
	return func(routine string) time.Time { return next[routine] }
}

// unlinkedSkills lists <dir>/skills/<name> entries with no symlink in the qilla
// plugin: the union is stale and the skill is invisible to Claude Code.
func unlinkedSkills(pluginDir string, dirs []string) []string {
	var out []string
	for _, d := range dirs {
		es, err := os.ReadDir(filepath.Join(d, "skills"))
		if err != nil {
			continue
		}
		for _, e := range es {
			if !e.IsDir() || strings.HasPrefix(e.Name(), ".") {
				continue
			}
			link := filepath.Join(pluginDir, "skills", e.Name())
			if tgt, err := os.Readlink(link); err != nil || tgt != filepath.Join(d, "skills", e.Name()) {
				out = append(out, e.Name())
			}
		}
	}
	sort.Strings(out)
	return out
}
