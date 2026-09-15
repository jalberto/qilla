package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/jalberto/qilla/internal/config"
)

// cmdBrowser: qilla browser [--agent a] <agent-browser args…>
// Headless browsing for routines and sub-agents with a persistent profile per
// agent (logins survive runs). When a page needs a human — login, captcha,
// blocked automation — `qilla browser handoff <url>` opens the same profile in
// the headed browser from [browser].headed and waits for it to close.
func cmdBrowser(args []string) error {
	cfg, err := config.Load(config.DefaultPath())
	if err != nil {
		return err
	}
	agent := os.Getenv("QILLA_AGENT")
	if agent == "" {
		agent = "chief"
	}
	if len(args) >= 2 && args[0] == "--agent" {
		agent, args = args[1], args[2:]
	}
	profile := filepath.Join(cfg.StateDir, "browser", agent)
	if err := os.MkdirAll(profile, 0o700); err != nil {
		return err
	}
	if len(args) == 0 {
		return fmt.Errorf("usage: qilla browser [--agent a] <agent-browser command…> | handoff <url>")
	}
	if args[0] == "handoff" {
		return handoff(cfg, profile, args[1:])
	}
	// Rung 1 of the ladder: a plain content read (open/read a URL) tries
	// obscura first — no window, no session, ~seconds. Anything obscura
	// can't do (interactive commands, or it errors/returns nothing) falls
	// through to agent-browser headless below.
	if (args[0] == "open" || args[0] == "read") && len(args) >= 2 {
		if out, ok := tryObscura(args[1]); ok {
			fmt.Print(out)
			return nil
		}
	}
	bin := cfg.Browser.Headless
	if bin == "" {
		bin = "agent-browser"
	}
	path, err := exec.LookPath(bin)
	if err != nil {
		return fmt.Errorf("%s not on PATH — install it (npm i -g agent-browser) or set [browser].headless", bin)
	}
	env := append(os.Environ(), "AGENT_BROWSER_PROFILE="+profile, "QILLA_BROWSER_PROFILE="+profile)
	argv := append([]string{path}, args...)
	return syscall.Exec(path, argv, env)
}

// tryObscura attempts a fast, windowless content fetch. Returns ok=false on
// any failure (not installed, non-zero exit, empty output) so the caller can
// fall back to agent-browser without the model needing to notice or retry.
func tryObscura(url string) (string, bool) {
	if _, err := exec.LookPath("obscura"); err != nil {
		return "", false
	}
	// "--" forces obscura's arg parser to treat url as positional even if it
	// starts with "-"/"--" (argument-injection guard: url may be attacker-
	// controlled page content, and obscura's --eval runs arbitrary JS).
	out, err := exec.Command("obscura", "--stealth", "fetch", "--dump", "text", "--", url).Output()
	if err != nil {
		return "", false
	}
	text := strings.TrimSpace(string(out))
	if text == "" {
		return "", false
	}
	return text + "\n", true
}

// handoff opens the url in the headed browser on the agent's profile and blocks
// until it exits, so a routine can resume with the cookies the human left.
func handoff(cfg *config.Config, profile string, args []string) error {
	if cfg.Browser.Headed == "" {
		return errors.New("no headed browser configured ([browser].headed) — this page needs a human; the run stops here")
	}
	if os.Getenv("DISPLAY") == "" && os.Getenv("WAYLAND_DISPLAY") == "" {
		return errors.New("no display: a headed browser cannot open here — this page needs a human on a desktop")
	}
	url := ""
	if len(args) > 0 {
		url = args[0]
	}
	cmdline := strings.ReplaceAll(strings.ReplaceAll(cfg.Browser.Headed, "%u", url), "%p", profile)
	fmt.Fprintf(os.Stderr, "\033[45;30m ◆ qilla \033[0m handoff → %s\n", cmdline)
	c := exec.Command("sh", "-c", cmdline)
	c.Stdout, c.Stderr = os.Stderr, os.Stderr
	return c.Run()
}
