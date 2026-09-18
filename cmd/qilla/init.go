package main

import (
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"github.com/jalberto/qilla/internal/config"
	"github.com/jalberto/qilla/internal/install"
)

// cmdInit: qilla init [--force] [--print]
func cmdInit(args []string) error {
	fs := flag.NewFlagSet("init", flag.ContinueOnError)
	force := fs.Bool("force", false, "overwrite an existing qilla.toml and mise.toml")
	print := fs.Bool("print", false, "print the config template and exit")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *print {
		fmt.Print(install.Template)
		return nil
	}
	cfgPath := config.DefaultPath()
	dir := filepath.Dir(cfgPath)
	wrote, err := install.WriteFile(cfgPath, install.Template, *force)
	if err != nil {
		return err
	}
	report(wrote, cfgPath)
	pre, err := config.Load(cfgPath)
	if err != nil {
		return err
	}
	wrote, err = install.WriteFile(filepath.Join(dir, "mise.toml"), fmt.Sprintf(install.MiseTomlTemplate, pre.Memory.EngramVersion), *force)
	if err != nil {
		return err
	}
	report(wrote, filepath.Join(dir, "mise.toml"))

	if _, err := install.WriteFile(filepath.Join(dir, "statusline.sh"), install.StatusLine, true); err != nil {
		return err
	}
	os.Chmod(filepath.Join(dir, "statusline.sh"), 0o755)
	cfg, err := config.Load(cfgPath)
	if err != nil {
		return err
	}
	for _, w := range install.Ensure(cfg) {
		fmt.Println("wrote default", w, "— yours to edit")
	}
	self, _ := os.Executable()
	mise, _ := exec.LookPath("mise")
	home, _ := os.UserHomeDir()
	p := install.Paths{ConfigDir: dir, UnitDir: filepath.Join(home, ".config", "systemd", "user"), Qilla: self, Mise: mise}
	units := install.Units(cfg, p)
	// every routine's manifest is checked before its timer is written: a
	// shareable bundle with a missing requirement must not start silently.
	units = checkRoutinesForInit(cfg, units, *force)
	// prune units of routines that no longer exist in the config
	if es, err := os.ReadDir(p.UnitDir); err == nil {
		for _, e := range es {
			n := e.Name()
			if strings.HasPrefix(n, "qilla-") && (strings.HasSuffix(n, ".timer") || strings.HasSuffix(n, ".service")) {
				if _, keep := units[n]; !keep {
					os.Remove(filepath.Join(p.UnitDir, n))
					fmt.Println("removed stale", n, "(disable it: systemctl --user disable", n+")")
				}
			}
		}
	}
	for name, body := range units {
		// units always follow the config: rewrite them every init
		if _, err := install.WriteFile(filepath.Join(p.UnitDir, name), body, true); err != nil {
			return err
		}
	}
	fmt.Printf("wrote %d systemd user units to %s\n\n", len(units), p.UnitDir)
	fmt.Println("Next:")
	fmt.Println(indent(install.EnableCommands(units, dir)))
	fmt.Println("Persona and rules live in the vault:", cfg.VaultPath(cfg.Persona), "·", cfg.VaultPath(cfg.Rules))
	return nil
}

// checkRoutinesForInit warns for every configured routine whose manifest check
// fails and drops its timer/service from units, unless force says otherwise.
func checkRoutinesForInit(cfg *config.Config, units map[string]string, force bool) map[string]string {
	env := manifestEnv(cfg)
	names := make([]string, 0, len(cfg.Routines))
	for n := range cfg.Routines {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		msg := checkBlocks(cfg, n, env)
		if msg == "" {
			continue
		}
		fmt.Fprintln(os.Stderr, "qilla:", msg)
		if force {
			fmt.Fprintf(os.Stderr, "qilla: writing the timer for %s anyway (--force)\n", n)
			continue
		}
		delete(units, "qilla-"+n+".timer")
		delete(units, "qilla-"+n+".service")
		fmt.Fprintf(os.Stderr, "qilla: no timer written for %s (--force to write it anyway)\n", n)
	}
	return units
}

func report(wrote bool, path string) {
	if wrote {
		fmt.Println("wrote", path)
	} else {
		fmt.Println("kept ", path, "(use --force to overwrite)")
	}
}

func indent(s string) string {
	out := ""
	for _, l := range splitLines(s) {
		out += "  " + l + "\n"
	}
	return out
}

func splitLines(s string) []string {
	var out []string
	cur := ""
	for _, r := range s {
		if r == '\n' {
			if cur != "" {
				out = append(out, cur)
			}
			cur = ""
			continue
		}
		cur += string(r)
	}
	if cur != "" {
		out = append(out, cur)
	}
	return out
}
