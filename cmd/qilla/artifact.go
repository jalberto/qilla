package main

import (
	"flag"
	"fmt"
	"time"

	"github.com/jalberto/qilla/internal/artifacts"
	"github.com/jalberto/qilla/internal/config"
)

// cmdArtifact: qilla artifact add <file.html> [--title t] [--routine r] [--ttl 30d] | list | rm <id>
func cmdArtifact(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: qilla artifact add <file.html> [--title t] [--ttl 720h] | list | rm <id>")
	}
	cfg, err := config.Load(config.DefaultPath())
	if err != nil {
		return err
	}
	st, err := artifacts.New(cfg.ArtifactsDir(), time.Duration(cfg.Artifacts.TTLDays)*24*time.Hour, cfg.Artifacts.MaxMB)
	if err != nil {
		return err
	}
	switch args[0] {
	case "add":
		fs := flag.NewFlagSet("artifact add", flag.ContinueOnError)
		title := fs.String("title", "", "title shown on the page")
		routine := fs.String("routine", "", "origin (default QILLA_ROUTINE/agent)")
		ttl := fs.Duration("ttl", 0, "lifetime, e.g. 168h (default [artifacts].ttl_days)")
		var file string
		rest := args[1:]
		for len(rest) > 0 {
			if rest[0][0] == '-' {
				if err := fs.Parse(rest); err != nil {
					return err
				}
				rest = fs.Args()
				continue
			}
			file, rest = rest[0], rest[1:]
		}
		if file == "" {
			return fmt.Errorf("usage: qilla artifact add <file.html> [--title t]")
		}
		m, err := st.Add(file, *title, *routine, 0, *ttl)
		if err != nil {
			return err
		}
		fmt.Printf("%s  http://%s/artifacts/%s  (%s)\n", m.ID, cfg.Web.Listen, m.ID, m.Title)
	case "list":
		l, err := st.List()
		if err != nil {
			return err
		}
		for _, m := range l {
			fmt.Printf("%s  %-32s %s  %s\n", m.ID, m.Title, m.Routine, m.Created.Format("Mon 15:04"))
		}
	case "rm":
		if len(args) != 2 {
			return fmt.Errorf("usage: qilla artifact rm <id>")
		}
		return st.Remove(args[1])
	default:
		return fmt.Errorf("unknown artifact command %q", args[0])
	}
	return nil
}
