package main

import (
	"context"
	"fmt"

	"github.com/jalberto/qilla/internal/config"
	"github.com/jalberto/qilla/internal/queue"
	"github.com/jalberto/qilla/internal/worker"
)

// cmdGather: qilla gather <routine> [--dry] — run ONLY the gather step of a
// bundle and print its JSON. The cheap way to develop a gather.sh or
// gather.star: no model is called, nothing is recorded, no note is rendered.
// --dry (like QILLA_DRY_RUN=1) suppresses a gather.star's declared write and
// non-GET http calls and reports them as "_dry_actions".
func cmdGather(args []string) error {
	dry := false
	var rest []string
	for _, a := range args {
		if a == "--dry" || a == "-dry" {
			dry = true
			continue
		}
		rest = append(rest, a)
	}
	args = rest
	if len(args) != 1 {
		return fmt.Errorf("usage: qilla gather <routine> [--dry]")
	}
	cfg, err := config.Load(config.DefaultPath())
	if err != nil {
		return err
	}
	s, err := queue.Open(cfg.DBPath())
	if err != nil {
		return err
	}
	defer s.Close()
	w, err := worker.New(config.NewHolder(cfg), s.DB())
	if err != nil {
		return err
	}
	out, ok, err := w.GatherDry(context.Background(), args[0], dry)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("routine %s has no gather.sh or gather.star", args[0])
	}
	fmt.Print(out)
	if len(out) > 0 && out[len(out)-1] != '\n' {
		fmt.Println()
	}
	return nil
}
