package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/jalberto/qilla/internal/config"
	"github.com/jalberto/qilla/internal/install"
	"github.com/jalberto/qilla/internal/manifest"
	"github.com/jalberto/qilla/internal/worker"
)

// routineManifest loads <vault>/Qilla/Routines/<name>/routine.toml.
// (nil, nil) = legacy bundle without a manifest.
func routineManifest(cfg *config.Config, name string) (*manifest.Manifest, error) {
	return manifest.Load(filepath.Join(cfg.Vault, worker.RoutinesDir, name))
}

// manifestEnv probes the real machine, with secrets read from the config dir.
func manifestEnv(cfg *config.Config) manifest.Env {
	names := map[string]bool{}
	for _, n := range install.SecretNames(filepath.Dir(cfg.Path)) {
		names[n] = true
	}
	return manifest.DefaultEnv(func(n string) bool { return names[n] })
}

// checkRoutine evaluates one routine's manifest. ok=false means the bundle has
// no manifest (legacy): nothing to check, and that is not a failure.
func checkRoutine(cfg *config.Config, name string, env manifest.Env) (rs []manifest.Result, hasManifest bool, err error) {
	m, err := routineManifest(cfg, name)
	if err != nil || m == nil {
		return nil, false, err
	}
	if m.Name == "" {
		m.Name = name
	}
	return m.Check(env, cfg.Routines[name].Settings), true, nil
}

// cmdRoutine: qilla routine check <name> | qilla routine check --all [--json]
func cmdRoutine(args []string) error {
	if len(args) == 0 || args[0] != "check" {
		return fmt.Errorf("usage: qilla routine check <name> | qilla routine check --all [--json]")
	}
	fs := flag.NewFlagSet("routine check", flag.ContinueOnError)
	all := fs.Bool("all", false, "check every configured routine")
	asJSON := fs.Bool("json", false, "machine-readable report")
	// flags may come before or after the routine name
	var flags, positional []string
	for _, a := range args[1:] {
		if strings.HasPrefix(a, "-") {
			flags = append(flags, a)
		} else {
			positional = append(positional, a)
		}
	}
	if err := fs.Parse(flags); err != nil {
		return err
	}
	cfg, err := config.Load(config.DefaultPath())
	if err != nil {
		return err
	}
	names := positional
	if *all {
		for n := range cfg.Routines {
			names = append(names, n)
		}
		sort.Strings(names)
	}
	if len(names) == 0 {
		return fmt.Errorf("usage: qilla routine check <name> | qilla routine check --all [--json]")
	}
	env := manifestEnv(cfg)

	type report struct {
		Routine  string            `json:"routine"`
		Manifest bool              `json:"manifest"`
		OK       bool              `json:"ok"`
		Results  []manifest.Result `json:"results"`
	}
	var reports []report
	bad := false
	for _, n := range names {
		rs, has, err := checkRoutine(cfg, n, env)
		if err != nil {
			return err
		}
		failed := has && manifest.Failed(rs)
		bad = bad || failed
		reports = append(reports, report{Routine: n, Manifest: has, OK: !failed, Results: rs})
	}
	if *asJSON {
		b, err := json.MarshalIndent(reports, "", "  ")
		if err != nil {
			return err
		}
		os.Stdout.Write(append(b, '\n'))
	} else {
		for i, r := range reports {
			if i > 0 {
				fmt.Println()
			}
			fmt.Print(formatCheck(r.Routine, r.Manifest, r.Results))
		}
	}
	if bad {
		os.Exit(1)
	}
	return nil
}

// formatCheck renders the requirement · status · hint table.
func formatCheck(name string, hasManifest bool, rs []manifest.Result) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s\n", name)
	if !hasManifest {
		fmt.Fprintf(&b, "  no manifest (%s/%s/%s) — nothing to check\n", worker.RoutinesDir, name, manifest.File)
		return b.String()
	}
	w := 0
	for _, r := range rs {
		if len(r.Item) > w {
			w = len(r.Item)
		}
	}
	for _, r := range rs {
		mark := "ok  "
		if !r.OK {
			mark = "FAIL"
		} else if r.Status == manifest.StatusDisabled {
			mark = "note"
		}
		line := fmt.Sprintf("  %s  %-*s  %s", mark, w, r.Item, r.Status)
		if r.Hint != "" {
			line += "  · " + r.Hint
		}
		b.WriteString(strings.TrimRight(line, " ") + "\n")
	}
	if manifest.Failed(rs) {
		b.WriteString("  → not ready: fix the FAIL rows before enabling it\n")
	}
	return b.String()
}

// checkBlocks reports the message to refuse a routine with, "" when it is fine.
func checkBlocks(cfg *config.Config, name string, env manifest.Env) string {
	rs, has, err := checkRoutine(cfg, name, env)
	if err != nil || !has || !manifest.Failed(rs) {
		return ""
	}
	return fmt.Sprintf("routine %s is not ready: %s (see `qilla routine check %s`)", name, manifest.FirstFailure(rs), name)
}
