package main

import (
	"flag"
	"fmt"

	"github.com/jalberto/qilla/internal/config"
	"github.com/jalberto/qilla/internal/routine"
)

// cmdNew: qilla new routine <name> --kind script|ai-fresh|ai-resumed --schedule "…" [--sh] [--window HH:MM-HH:MM] [--must-run] [--agent chief] [--output path] [--append]
//
// The gather scaffolded is gather.star (built-in Starlark) unless --sh asks for
// a shell one. --star is kept as a no-op alias for the old default-was-sh days.
func cmdNew(args []string) error {
	if len(args) < 2 || args[0] != "routine" {
		return fmt.Errorf("usage: qilla new routine <name> --kind … --schedule … [flags]")
	}
	fs := flag.NewFlagSet("new routine", flag.ContinueOnError)
	o := routine.Options{Name: args[1]}
	fs.StringVar(&o.Summary, "summary", "", "one line describing the routine (routine.toml)")
	fs.StringVar(&o.Kind, "kind", "script", "script | ai-fresh | ai-resumed")
	fs.StringVar(&o.Schedule, "schedule", "", "systemd OnCalendar, e.g. \"*-*-* 08:30\"")
	fs.StringVar(&o.Window, "window", "", "HH:MM-HH:MM in which the routine may run")
	fs.BoolVar(&o.MustRun, "must-run", false, "reconcile guarantees it daily")
	fs.StringVar(&o.Agent, "agent", "", "agent for ai-* kinds")
	fs.StringVar(&o.Output, "output", "", "vault-relative note to render, may use {{date}}")
	fs.BoolVar(&o.Append, "append", false, "append to the output note instead of overwriting")
	var sh, star bool
	fs.BoolVar(&sh, "sh", false, "scaffold gather.sh (any language) instead of the default gather.star")
	fs.BoolVar(&star, "star", false, "no-op: gather.star is the default")
	if err := fs.Parse(args[2:]); err != nil {
		return err
	}
	_ = star
	o.Star = !sh
	cfg, err := config.Load(config.DefaultPath())
	if err != nil {
		return err
	}
	written, err := routine.Write(cfg, o)
	if err != nil {
		return err
	}
	for _, w := range written {
		fmt.Println("wrote", w)
	}
	fmt.Printf("\nNext: edit the files, then `qilla init` (writes the timer) and `qilla doctor`.\n")
	fmt.Printf("Iterate at $0: qilla gather %s\n", o.Name)
	if o.Kind != config.KindScript {
		fmt.Printf("Then one paid proof: qilla run %s\n", o.Name)
	}
	return nil
}
