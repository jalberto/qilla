package main

import (
	"bufio"
	"time"

	"encoding/json"
	"flag"
	"fmt"
	"github.com/jalberto/qilla/internal/candidates"
	"github.com/jalberto/qilla/internal/procs"
	"github.com/jalberto/qilla/internal/remind"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/jalberto/qilla/internal/config"
	"github.com/jalberto/qilla/internal/plugin"
	"github.com/jalberto/qilla/internal/subagents"
)

// hookInput is the subset of Claude Code's hook JSON we read.
type hookInput struct {
	Cwd       string `json:"cwd"`
	ToolInput struct {
		Command  string `json:"command"`
		FilePath string `json:"file_path"`
		Limit    any    `json:"limit"`
	} `json:"tool_input"`
	StopHookActive bool `json:"stop_hook_active"`
}

func readHook() (hookInput, []byte) {
	b, _ := io.ReadAll(os.Stdin)
	var in hookInput
	json.Unmarshal(b, &in)
	if in.Cwd == "" {
		in.Cwd, _ = os.Getwd()
	}
	return in, b
}

func inScope(cfg *config.Config, cwd string) bool {
	return cfg.Guard.Scope == "all" || strings.HasPrefix(cwd, cfg.Vault)
}

func emit(v any) {
	b, _ := json.Marshal(v)
	fmt.Println(string(b))
}

func decision(dec, reason string) {
	emit(map[string]any{"hookSpecificOutput": map[string]any{"hookEventName": "PreToolUse", "permissionDecision": dec, "permissionDecisionReason": reason}})
}

// cmdGuard: qilla guard bash|read — PreToolUse rails (hook). Always prints valid JSON, always exits 0.
func cmdGuard(args []string) error {
	cfg, err := config.Load(config.DefaultPath())
	in, _ := readHook()
	if err != nil || len(args) != 1 || !inScope(cfg, in.Cwd) {
		emit(map[string]any{})
		return nil
	}
	switch args[0] {
	case "bash":
		cmd := in.ToolInput.Command
		if cmd == "" {
			emit(map[string]any{})
			return nil
		}
		for _, pat := range cfg.Guard.Deny {
			if re, err := regexp.Compile(pat); err == nil && re.MatchString(cmd) {
				decision("deny", "qilla guard: this command is on the deny list ("+pat+")")
				return nil
			}
		}
		for _, pat := range cfg.Guard.Ask {
			if re, err := regexp.Compile(pat); err == nil && re.MatchString(cmd) {
				decision("ask", "qilla guard: remote connection / guarded action — needs the user's explicit approval for THIS use. Do not look for workarounds.")
				return nil
			}
		}
	case "read":
		p := in.ToolInput.FilePath
		if p == "" || in.ToolInput.Limit != nil || cfg.Guard.ReadMaxLines <= 0 {
			break
		}
		switch strings.ToLower(filepath.Ext(p)) {
		case ".pdf", ".png", ".jpg", ".jpeg", ".gif", ".webp", ".ipynb":
			break
		default:
			if f, err := os.Open(p); err == nil {
				n := 0
				sc := bufio.NewScanner(f)
				sc.Buffer(make([]byte, 1<<20), 1<<20)
				for sc.Scan() {
					n++
					if n > cfg.Guard.ReadMaxLines {
						break
					}
				}
				f.Close()
				if n > cfg.Guard.ReadMaxLines {
					decision("deny", fmt.Sprintf("qilla guard: %s has more than %d lines — read the part you need (offset/limit, or grep -n / sed -n 'X,Yp'); every later turn pays for what you load now.", p, cfg.Guard.ReadMaxLines))
					return nil
				}
			}
		}
	}
	emit(map[string]any{})
	return nil
}

// cmdHook: qilla hook session-start|stop — the other plugin hooks.
func cmdHook(args []string) error {
	if len(args) != 1 {
		emit(map[string]any{})
		return nil
	}
	cfg, err := config.Load(config.DefaultPath())
	in, raw := readHook()
	if err != nil || os.Getenv("QILLA_RUN") != "" || !inScope(cfg, in.Cwd) {
		emit(map[string]any{}) // never inside qilla's own runs, never outside the vault
		return nil
	}
	switch args[0] {
	case "session-start":
		var parts []string
		if out, err := exec.Command(os.Args[0], "status", "--short").Output(); err == nil || len(out) > 0 {
			if t := strings.TrimSpace(string(out)); t != "" && t != "qilla: all green" {
				parts = append(parts, t)
			}
		}
		parts = append(parts, sessionSignals(cfg)...)
		if cfg.Hooks.SessionStart != "" {
			c := exec.Command("sh", "-c", cfg.Hooks.SessionStart)
			c.Dir = in.Cwd // user hooks often key on $PWD being the vault
			c.Stdin = strings.NewReader(string(raw))
			if out, err := c.Output(); err == nil {
				// a user hook may print plain text or Claude's JSON with additionalContext
				var j struct {
					HookSpecificOutput struct {
						AdditionalContext string `json:"additionalContext"`
					} `json:"hookSpecificOutput"`
				}
				if json.Unmarshal(out, &j) == nil && j.HookSpecificOutput.AdditionalContext != "" {
					parts = append(parts, j.HookSpecificOutput.AdditionalContext)
				} else if t := strings.TrimSpace(string(out)); t != "" && !strings.HasPrefix(t, "{") {
					parts = append(parts, t)
				}
			}
		}
		if len(parts) == 0 {
			emit(map[string]any{})
			return nil
		}
		emit(map[string]any{"hookSpecificOutput": map[string]any{"hookEventName": "SessionStart", "additionalContext": strings.Join(parts, "\n")}})
	case "stop":
		if in.StopHookActive || cfg.Hooks.Stop == "" {
			emit(map[string]any{})
			return nil
		}
		c := exec.Command("sh", "-c", cfg.Hooks.Stop)
		c.Dir = in.Cwd
		c.Stdin = strings.NewReader(string(raw))
		out, _ := c.Output()
		if t := strings.TrimSpace(string(out)); t != "" {
			fmt.Println(t)
		} else {
			emit(map[string]any{})
		}
	default:
		emit(map[string]any{})
	}
	return nil
}

// cmdStatusline lives in statusline.go.

// claudeJSONPath is Claude Code's config holding the skill counters; tests point it away.
var claudeJSONPath = plugin.ClaudeJSONPath()

// cmdPlugin: qilla plugin [install|usage] — materialize the Claude Code plugin (hooks,
// skills, sub-agents); `install` also registers it with Claude and sets the user status
// line; `usage` reports which skills of the configured plugins are actually reached for.
func cmdPlugin(args []string) error {
	cfg, err := config.Load(config.DefaultPath())
	if err != nil {
		return err
	}
	if len(args) > 0 && args[0] == "usage" {
		return pluginUsage(cfg, args[1:])
	}
	confDir := filepath.Dir(cfg.Path)
	defs, _ := subagents.Load(cfg.Vault, cfg.Models)
	agents := map[string]string{}
	for name, d := range defs {
		agents[name] = fmt.Sprintf("---\nname: %s\ndescription: %s\ntools: %s\nmodel: %s\n---\n%s\n", name, d.Description, strings.Join(d.Tools, ", "), d.Model, d.Prompt)
	}
	dir, err := plugin.Materialize(confDir, agents)
	if err != nil {
		return err
	}
	fmt.Println("plugin written to", dir)
	if len(args) == 0 || args[0] != "install" {
		fmt.Println("Register it with Claude Code:\n  " + strings.ReplaceAll(plugin.InstallCommands(dir), "\n", "\n  "))
		fmt.Println("User status line (optional): set ~/.claude/settings.json statusLine.command to \"qilla statusline\"")
		return nil
	}
	for _, line := range strings.Split(plugin.InstallCommands(dir), "\n") {
		fmt.Println("$", line)
		c := exec.Command("sh", "-c", line)
		c.Stdout, c.Stderr = os.Stdout, os.Stderr
		if err := c.Run(); err != nil {
			fmt.Fprintln(os.Stderr, "qilla: that step failed; run it by hand:", line)
		}
	}
	// user-level status line → qilla statusline (the only user-level touch, and it is qilla's)
	home, _ := os.UserHomeDir()
	sp := filepath.Join(home, ".claude", "settings.json")
	var settings map[string]any
	if b, err := os.ReadFile(sp); err == nil {
		json.Unmarshal(b, &settings)
	}
	if settings == nil {
		settings = map[string]any{}
	}
	settings["statusLine"] = map[string]any{"type": "command", "command": "qilla statusline", "padding": 0}
	b, _ := json.MarshalIndent(settings, "", "  ")
	if err := os.WriteFile(sp, append(b, '\n'), 0o644); err != nil {
		return err
	}
	fmt.Println("statusLine → qilla statusline in", sp)
	return nil
}

// pluginUsage: qilla plugin usage [--unused] [--json] — every skill under the configured
// plugin dirs with its use count and last use, read from Claude Code's own counters.
func pluginUsage(cfg *config.Config, args []string) error {
	fs := flag.NewFlagSet("plugin usage", flag.ContinueOnError)
	unused := fs.Bool("unused", false, "only the skills never used")
	asJSON := fs.Bool("json", false, "JSON instead of a table")
	if err := fs.Parse(args); err != nil {
		return err
	}
	rows, err := plugin.Usage(cfg.PluginDirs(), claudeJSONPath)
	if err != nil {
		return err
	}
	if *unused {
		var keep []plugin.SkillUse
		for _, r := range rows {
			if r.Uses == 0 {
				keep = append(keep, r)
			}
		}
		rows = keep
	}
	if *asJSON {
		if rows == nil {
			rows = []plugin.SkillUse{}
		}
		b, err := json.Marshal(rows)
		if err != nil {
			return err
		}
		fmt.Println(string(b))
		return nil
	}
	fmt.Printf("n=%d\n", len(rows))
	for _, r := range rows {
		last := "never"
		if r.Uses > 0 {
			last = r.Last
		}
		fmt.Printf("%s\t%d\t%s\n", r.Name, r.Uses, last)
	}
	return nil
}

// sessionSignals are the cheap, in-process lines a session should open with:
// reminders that are due, judge candidates waiting, and other interactive
// sessions already in this vault. Every one is best-effort: a failure is
// silent, never a hook that breaks the session.
func sessionSignals(cfg *config.Config) []string {
	var out []string

	// reminders due — markdown is the source of truth, so just scan it
	loc := time.Local
	if cfg.Timezone != "" {
		if l, err := time.LoadLocation(cfg.Timezone); err == nil {
			loc = l
		}
	}
	if rs, err := remind.Scan(cfg.Vault, loc); err == nil {
		if n := len(remind.DueNow(rs, time.Now().In(loc))); n > 0 {
			out = append(out, fmt.Sprintf("\u23f0 %d reminder(s) due \u2014 `qilla remind due`", n))
		}
	}

	// judge candidates waiting in the queues
	root := cfg.QueueDir
	if d := os.Getenv("QILLA_QUEUE_DIR"); d != "" {
		root = d
	}
	if counts, err := candidates.New(root).Counts(); err == nil && len(counts) > 0 {
		var fams []string
		for _, c := range counts {
			fams = append(fams, fmt.Sprintf("%s %d", c.Family, c.N))
		}
		out = append(out, "\u2696 judge queue: "+strings.Join(fams, ", "))
	}

	// other interactive claude sessions in this vault (this one does not count)
	if cfg.Vault != "" {
		self := procs.AncestorClaude(os.Getpid())
		n := 0
		for _, p := range procs.ClaudeIn(cfg.Vault) {
			if p.PID != self {
				n++
			}
		}
		if n > 0 {
			out = append(out, fmt.Sprintf("\u26a0 %d other interactive session(s) in this vault (`qilla sessions`)", n))
		}
	}
	return out
}
