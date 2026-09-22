// Package install writes what `qilla init` puts on a machine: the config
// template, the runtime mise.toml, and the systemd user units.
package install

import (
	_ "embed"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/jalberto/qilla/internal/config"
)

//go:embed qilla.toml
var Template string

//go:embed files/statusline.sh
var StatusLine string

//go:embed files/persona.md
var DefaultPersona string

//go:embed files/rules.md
var DefaultRules string

// MiseTomlTemplate pins the runtime tools next to qilla.toml (%s = engram version). Installed with
// `mise install -C ~/.config/qilla`; units run through `mise exec` there.
const MiseTomlTemplate = `# qilla runtime tools — install with:
#   mise install -C ~/.config/qilla && mise exec -C ~/.config/qilla -- npm i -g knap @tobilu/qmd
# (npm packages go through node's own npm: mise's npm backend prompts interactively as of 2026.9)
[tools]
node = "24"
"aqua:anthropics/claude-code" = "latest"
"ubi:Gentleman-Programming/engram" = "%s"   # working-memory sidecar (memory.backend = "engram")
"github:rtk-ai/rtk" = "0.49.0"   # token-saving shell proxy; the claude PreToolUse hook (rtk hook claude) needs it on the unit PATH

# user-space helpers the routines call — inside the unit mise shims resolve only
# against this file (MISE_GLOBAL_CONFIG_FILE), so a helper installed through the
# user's own global mise config dies with "No version is set for shim: <name>".
# "latest" resolves offline to the already-installed version (mise shares installs
# across configs), so pinning here does not force a re-install.
"github:wesm/msgvault" = "latest"     # brief: mail digest
"npm:agent-browser" = "latest"        # market: browser automation
"github:asciimoo/hister" = "latest"   # recall: browsing history
`

// Paths of everything init writes.
type Paths struct {
	ConfigDir string // ~/.config/qilla
	UnitDir   string // ~/.config/systemd/user
	Qilla     string // absolute path of this binary
	Mise      string // absolute path of mise, or "" to run qilla directly
}

// Units renders every systemd unit for cfg. Keys are file names.
// A worker host only gets the qilla-qmd-refresh timer/service (when that
// routine is configured): no socket, no supervisor, no engram sidecar, no
// reconcile, no other routine timers.
func Units(cfg *config.Config, p Paths) map[string]string {
	if !cfg.IsBrain() {
		return workerUnits(cfg, p)
	}
	return brainUnits(cfg, p)
}

func workerUnits(cfg *config.Config, p Paths) map[string]string {
	u := map[string]string{}
	r, ok := cfg.Routines["qmd-refresh"]
	if !ok || strings.EqualFold(strings.TrimSpace(r.Schedule), "manual") {
		return u
	}
	env := "Environment=PATH=%h/.local/share/mise/shims:%h/.local/bin:/usr/local/bin:/usr/bin:/bin\n" +
		"Environment=QILLA_CONFIG=" + filepath.Join(p.ConfigDir, "qilla.toml") + "\n" +
		"Environment=MISE_GLOBAL_CONFIG_FILE=" + filepath.Join(p.ConfigDir, "mise.toml") + " MISE_IGNORED_CONFIG_PATHS=%h/.config/mise/config.toml\n" +
		"Environment=MISE_AUTO_INSTALL=false MISE_YES=1 MISE_QUIET=1"
	exec := func(args string) string {
		if p.Mise != "" {
			return fmt.Sprintf("%s -C %s exec -- %s %s", p.Mise, p.ConfigDir, p.Qilla, args)
		}
		return p.Qilla + " " + args
	}
	var cal strings.Builder
	for _, part := range strings.Split(r.Schedule, ";") {
		if part = strings.TrimSpace(part); part != "" {
			fmt.Fprintf(&cal, "OnCalendar=%s\n", part)
		}
	}
	u["qilla-qmd-refresh.timer"] = fmt.Sprintf(`[Unit]
Description=qilla routine qmd-refresh (%s)

[Timer]
%sPersistent=true
RandomizedDelaySec=1min
AccuracySec=1min

[Install]
WantedBy=timers.target
`, r.Kind, cal.String())
	u["qilla-qmd-refresh.service"] = fmt.Sprintf(`[Unit]
Description=qilla enqueue qmd-refresh

[Service]
Type=oneshot
%s
ExecStart=%s
`, env, exec("enqueue qmd-refresh"))
	return u
}

func brainUnits(cfg *config.Config, p Paths) map[string]string {
	exec := func(args string) string {
		if p.Mise != "" {
			return fmt.Sprintf("%s -C %s exec -- %s %s", p.Mise, p.ConfigDir, p.Qilla, args)
		}
		return p.Qilla + " " + args
	}
	// the unit sees only the runtime mise.toml (never the user's global config) and never auto-installs
	env := "Environment=PATH=%h/.local/share/mise/shims:%h/.local/bin:/usr/local/bin:/usr/bin:/bin\n" +
		"Environment=QILLA_CONFIG=" + filepath.Join(p.ConfigDir, "qilla.toml") + "\n" +
		"Environment=MISE_GLOBAL_CONFIG_FILE=" + filepath.Join(p.ConfigDir, "mise.toml") + " MISE_IGNORED_CONFIG_PATHS=%h/.config/mise/config.toml\n" +
		"Environment=MISE_AUTO_INSTALL=false MISE_YES=1 MISE_QUIET=1"
	u := map[string]string{}
	u["qilla.socket"] = fmt.Sprintf(`[Unit]
Description=qilla — supervisor activation (poke + web)

[Socket]
ListenStream=%%t/qilla.sock
ListenStream=%s
NoDelay=true

[Install]
WantedBy=sockets.target
`, cfg.Web.Listen)
	u["qilla.service"] = fmt.Sprintf(`[Unit]
Description=qilla — supervisor: drains the queue, serves the page, exits when idle
Requires=qilla.socket
After=qilla.socket

[Service]
Type=simple
%s
ExecStart=%s
# finish the running job on stop, never kill claude mid-turn
KillSignal=SIGTERM
TimeoutStopSec=35min
Restart=on-failure
RestartSec=30s

# ── sandbox: every claude -p and gather.sh runs inside this unit ──
# The home directory is an empty tmpfs; only what a routine needs is bound in.
%s
# ── secrets: qilla secret set <name> → decrypted into $CREDENTIALS_DIRECTORY for gather.sh only ──
%s`, env, exec("serve"), Hardening(cfg, p), Credentials(p.ConfigDir))
	if cfg.Memory.Backend == "engram" {
		sock := "%t/qilla-engram.sock"
		if strings.HasPrefix(cfg.Memory.EngramURL, "unix://") && !strings.Contains(cfg.Memory.EngramURL, "/qilla-engram.sock") {
			sock = strings.TrimPrefix(cfg.Memory.EngramURL, "unix://")
		}
		engramExec := "engram serve"
		if p.Mise != "" {
			engramExec = fmt.Sprintf("%s -C %s exec -- engram serve", p.Mise, p.ConfigDir)
		}
		u["qilla-engram.service"] = fmt.Sprintf(`[Unit]
Description=qilla — working-memory sidecar (engram)

[Service]
Type=simple
%s
Environment=ENGRAM_DATA_DIR=%s/engram
Environment=ENGRAM_SOCKET=%s
ExecStart=%s
Restart=on-failure
RestartSec=5s

[Install]
WantedBy=default.target
`, env, cfg.StateDir, sock, engramExec)
	}
	u["qilla-reconcile.timer"] = `[Unit]
Description=qilla — prove every must_run routine succeeded today

[Timer]
OnCalendar=*-*-* *:07,37:00
OnBootSec=2min
Persistent=true
AccuracySec=1min

[Install]
WantedBy=timers.target
`
	u["qilla-reconcile.service"] = fmt.Sprintf(`[Unit]
Description=qilla reconcile

[Service]
Type=oneshot
%s
ExecStart=%s
`, env, exec("reconcile"))
	names := make([]string, 0, len(cfg.Routines))
	for n := range cfg.Routines {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		r := cfg.Routines[n]
		if strings.EqualFold(strings.TrimSpace(r.Schedule), "manual") {
			continue // ask/enqueue-only: no timer, no oneshot service
		}
		var cal strings.Builder
		for _, part := range strings.Split(r.Schedule, ";") { // "a; b" → one OnCalendar= per part
			if part = strings.TrimSpace(part); part != "" {
				fmt.Fprintf(&cal, "OnCalendar=%s\n", part)
			}
		}
		u["qilla-"+n+".timer"] = fmt.Sprintf(`[Unit]
Description=qilla routine %s (%s)

[Timer]
%sPersistent=true
RandomizedDelaySec=1min
AccuracySec=1min

[Install]
WantedBy=timers.target
`, n, r.Kind, cal.String())
		u["qilla-"+n+".service"] = fmt.Sprintf(`[Unit]
Description=qilla enqueue %s

[Service]
Type=oneshot
%s
ExecStart=%s
`, n, env, exec("enqueue "+n))
	}
	return u
}

// TimerNames lists the timers init enables.
func TimerNames(units map[string]string) []string {
	var t []string
	for n := range units {
		if strings.HasSuffix(n, ".timer") {
			t = append(t, n)
		}
	}
	sort.Strings(t)
	return t
}

// WriteFile writes only when the target is missing or force is set.
// Returns whether it wrote.
func WriteFile(path, body string, force bool) (bool, error) {
	if !force {
		if _, err := os.Stat(path); err == nil {
			return false, nil
		}
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return false, err
	}
	return true, os.WriteFile(path, []byte(body), 0o644)
}

// EnableCommands is what the user runs after init.
func EnableCommands(units map[string]string, configDir string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "mise install -C %s && mise exec -C %s -- npm i -g knap @tobilu/qmd\n", configDir, configDir)
	b.WriteString("systemctl --user daemon-reload\n")
	extra := ""
	if _, ok := units["qilla-engram.service"]; ok {
		extra = " qilla-engram.service"
	}
	b.WriteString("systemctl --user enable --now qilla.socket" + extra + " " + strings.Join(TimerNames(units), " ") + "\n")
	b.WriteString("loginctl enable-linger $USER\n")
	b.WriteString("qilla doctor\n")
	return b.String()
}

// Hardening returns the systemd sandbox block for qilla.service: home is a
// tmpfs with only the vault, qilla state/config, Claude's own files and the
// toolchain bound in; nothing else in $HOME exists for a routine.
func Hardening(cfg *config.Config, p Paths) string {
	home, _ := os.UserHomeDir()
	rw := []string{cfg.Vault, cfg.StateDir, p.ConfigDir,
		filepath.Join(home, ".claude"), filepath.Join(home, ".claude.json"),
		filepath.Join(home, ".cache"), filepath.Join(home, ".npm"),
		filepath.Join(home, ".local", "state", "mise")} // mise writes trusted-configs here even for `exec`
	// agent-browser keeps its state dir (session state + the Chrome for Testing binaries it downloads)
	// in ~/.agent-browser; without the bind it finds no Chrome and falls back to whatever browser is on
	// PATH (e.g. the brave-browser Flatpak wrapper, whose nested bwrap cannot run inside the unit)
	if f := strings.Fields(cfg.Browser.Headless); len(f) > 0 && filepath.Base(f[0]) == "agent-browser" {
		rw = append(rw, filepath.Join(home, ".agent-browser"))
	}
	rw = append(rw, cfg.Sandbox.BindRW...)
	ro := []string{filepath.Join(home, ".local", "share", "mise"), filepath.Join(home, ".local", "bin"), filepath.Join(home, ".config", "mise"),
		filepath.Join(home, ".gitconfig"), filepath.Join(home, ".config", "git")} // git identity: the model commits in the vault
	if p.Qilla != "" {
		ro = append(ro, filepath.Dir(p.Qilla))
	}
	ro = append(ro, cfg.Sandbox.BindRO...)
	var b strings.Builder
	// full, not strict: strict makes /run read-only too and connecting to unix sockets (engram, the
	// systemd user bus) then fails with EROFS; a ReadWritePaths=%t bind does not survive the user namespace
	b.WriteString("ProtectSystem=full\nProtectHome=tmpfs\nPrivateTmp=true\nNoNewPrivileges=true\n")
	// user+mnt+net+pid stay: Claude's sandbox-runtime runs bwrap --unshare-user --unshare-net --unshare-pid;
	// denying pid makes unshare() EPERM and every Bash call in a run dies with bwrap's "kernel does not allow
	// non-privileged user namespaces" (which is not what is happening)
	// AF_NETLINK: that same bwrap opens a NETLINK_ROUTE socket to bring up loopback in the new netns;
	// without it every Bash call in a run dies with "bwrap: loopback: Failed to create NETLINK_ROUTE socket"
	b.WriteString("RestrictAddressFamilies=AF_UNIX AF_INET AF_INET6 AF_NETLINK\nRestrictNamespaces=~cgroup ipc uts\n")
	// no ProtectKernelTunables/ProtectKernelLogs/ProtectHostname: they overmount /proc paths, so the kernel
	// refuses a fresh proc mount in an unprivileged userns and Claude's bwrap dies with "Can't mount proc on /proc"
	b.WriteString("CapabilityBoundingSet=\nLockPersonality=true\nProtectKernelModules=true\nProtectClock=true\nProtectControlGroups=true\nProtectProc=invisible\nPrivateDevices=true\nRestrictRealtime=true\nRestrictSUIDSGID=true\nRemoveIPC=true\nSystemCallArchitectures=native\nUMask=0077\n")
	for _, d := range uniq(rw) {
		fmt.Fprintf(&b, "BindPaths=-%s\n", d)
	}
	for _, d := range uniq(ro) {
		fmt.Fprintf(&b, "BindReadOnlyPaths=-%s\n", d)
	}
	// ProtectSystem=strict makes / read-only — including $XDG_RUNTIME_DIR, and connecting to a
	// unix socket (engram, systemd's private bus) needs write access to it.
	// user-service sandboxing replaces $XDG_RUNTIME_DIR with a private one: bind the real one back
	// so the engram socket, the poke socket and the systemd user bus stay reachable
	b.WriteString("BindPaths=%t\n")
	// every rw bind is also listed as writable (belt for ProtectSystem)
	b.WriteString("ReadWritePaths=")
	for _, d := range uniq(rw) {
		fmt.Fprintf(&b, "-%s ", d)
	}
	b.WriteString("\n")
	return b.String()
}

func uniq(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range in {
		if s != "" && !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

// CredsDir is where `qilla secret set` stores encrypted credentials.
func CredsDir(configDir string) string { return filepath.Join(configDir, "creds") }

// Credentials renders one LoadCredentialEncrypted= line per *.cred file.
func Credentials(configDir string) string {
	names := SecretNames(configDir)
	if len(names) == 0 {
		return "# (none yet)\n"
	}
	var b strings.Builder
	for _, n := range names {
		fmt.Fprintf(&b, "LoadCredentialEncrypted=%s:%s\n", n, filepath.Join(CredsDir(configDir), n+".cred"))
	}
	return b.String()
}

// SecretNames lists configured credential names, sorted.
func SecretNames(configDir string) []string {
	es, err := os.ReadDir(CredsDir(configDir))
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range es {
		if strings.HasSuffix(e.Name(), ".cred") {
			out = append(out, strings.TrimSuffix(e.Name(), ".cred"))
		}
	}
	sort.Strings(out)
	return out
}

// StatusLineCommand is the settings `statusLine.command`: qilla's wrapper, told which base
// status line to wrap (the user's own, so nothing they rely on disappears).
func StatusLineCommand(configDir, base string) string {
	cmd := filepath.Join(configDir, "statusline.sh")
	if base != "" {
		return fmt.Sprintf("QILLA_STATUSLINE_BASE=%q %s", base, cmd)
	}
	return cmd
}
