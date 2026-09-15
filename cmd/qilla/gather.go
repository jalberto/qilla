package main

import (
	"context"
	"fmt"

	"github.com/jalberto/qilla/internal/config"
	"github.com/jalberto/qilla/internal/queue"
	"github.com/jalberto/qilla/internal/worker"
)

// cmdGather: qilla gather <routine> — run ONLY the gather step of a bundle and
// print its JSON. The cheap way to develop a gather.sh or gather.star: no
// model is called, nothing is recorded, no note is rendered.
func cmdGather(args []string) error {
	if len(args) != 1 {
		return fmt.Errorf("usage: qilla gather <routine>")
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
	w, err := worker.New(cfg, s.DB())
	if err != nil {
		return err
	}
	out, ok, err := w.Gather(context.Background(), args[0])
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
