// Package routine scaffolds a routine bundle: vault files + a TOML block,
// one pattern for every routine so status, budgets and doctor stay coherent.
package routine

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/jalberto/qilla/internal/config"
	"github.com/jalberto/qilla/internal/install"
	"github.com/jalberto/qilla/internal/worker"
)

var nameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)

// Options for a new routine.
type Options struct {
	Name     string
	Kind     string // script | ai-fresh | ai-resumed
	Schedule string // systemd OnCalendar
	Window   string // HH:MM-HH:MM or ""
	MustRun  bool
	Agent    string // ai-* only
	Output   string // vault-relative, may contain {{date}}
	Append   bool
	Star     bool   // scaffold gather.star (the default); false → gather.sh (--sh)
	Summary  string // one line for routine.toml
}

// Files returns the vault files to create: relative path → body.
func Files(o Options) map[string]string {
	dir := filepath.Join(worker.RoutinesDir, o.Name)
	f := map[string]string{
		filepath.Join(dir, "routine.toml"): install.RoutineManifest(o.Name, o.Summary),
	}
	if o.Star {
		f[filepath.Join(dir, "gather.star")] = install.RoutineGatherStar(o.Name)
	} else {
		f[filepath.Join(dir, "gather.sh")] = install.RoutineGatherSh(o.Name)
	}
	if o.Output != "" {
		f[filepath.Join(dir, "template.md")] = install.RoutineTemplateMd(o.Name, o.Kind == config.KindScript)
	}
	if o.Kind != config.KindScript {
		f[filepath.Join(dir, "prompt.md")] = install.RoutinePromptMd(o.Name)
	}
	return f
}

// TOMLBlock returns the [routines.<name>] block to append to qilla.toml.
func TOMLBlock(o Options) string {
	var b strings.Builder
	fmt.Fprintf(&b, "\n[routines.%s]\nkind = %q\n", o.Name, o.Kind)
	if o.Kind != config.KindScript {
		fmt.Fprintf(&b, "agent = %q\n", o.Agent)
	}
	fmt.Fprintf(&b, "schedule = %q\n", o.Schedule)
	if o.Window != "" {
		fmt.Fprintf(&b, "window = %q\n", o.Window)
	}
	fmt.Fprintf(&b, "must_run = %v\n", o.MustRun)
	if o.Kind != config.KindScript {
		b.WriteString("# scope = [\"persona\", \"rules\", \"facts:<domain>\", \"journal:1d\", \"recall:5\"]  # default: the agent's scope\n")
		b.WriteString("# budget = { block = 0.50 }\n")
	}
	if o.Output != "" {
		fmt.Fprintf(&b, "output = %q\nappend = %v\n", o.Output, o.Append)
	}
	return b.String()
}

// Validate checks options before writing anything.
func Validate(cfg *config.Config, o Options) error {
	if !nameRe.MatchString(o.Name) {
		return fmt.Errorf("name must be lowercase [a-z0-9-]")
	}
	if _, exists := cfg.Routines[o.Name]; exists {
		return fmt.Errorf("routine %q already exists", o.Name)
	}
	switch o.Kind {
	case config.KindScript, config.KindFresh, config.KindResumed:
	default:
		return fmt.Errorf("kind must be script | ai-fresh | ai-resumed")
	}
	if o.Schedule == "" {
		return fmt.Errorf("schedule required (systemd OnCalendar, e.g. \"*-*-* 08:30\")")
	}
	if o.Kind != config.KindScript {
		if o.Agent == "" {
			return fmt.Errorf("ai routines need --agent")
		}
		if _, ok := cfg.Agents[o.Agent]; !ok {
			return fmt.Errorf("agent %q not in config", o.Agent)
		}
	}
	return nil
}

// Write creates the files (never overwriting) and appends the TOML block.
func Write(cfg *config.Config, o Options) ([]string, error) {
	if err := Validate(cfg, o); err != nil {
		return nil, err
	}
	var written []string
	for rel, body := range Files(o) {
		p := cfg.VaultPath(rel)
		if _, err := os.Stat(p); err == nil {
			return written, fmt.Errorf("%s exists, refusing to overwrite", rel)
		}
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			return written, err
		}
		mode := os.FileMode(0o644)
		if strings.HasSuffix(rel, ".sh") {
			mode = 0o755
		}
		if err := os.WriteFile(p, []byte(body), mode); err != nil {
			return written, err
		}
		written = append(written, rel)
	}
	f, err := os.OpenFile(cfg.Path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return written, err
	}
	defer f.Close()
	if _, err := f.WriteString(TOMLBlock(o)); err != nil {
		return written, err
	}
	return append(written, cfg.Path), nil
}
