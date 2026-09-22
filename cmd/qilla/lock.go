package main

import (
	"fmt"

	"github.com/jalberto/qilla/internal/config"
	"github.com/jalberto/qilla/internal/tasks"
)

// cmdLock: qilla lock <vault path> — record this host's soft lock on a shared
// note in Qilla/Handoff/locks.md (see host-roles-worker.md §3).
func cmdLock(args []string) error {
	if len(args) != 1 || args[0] == "" {
		return fmt.Errorf("usage: qilla lock <vault path>")
	}
	cfg, err := config.Load(config.DefaultPath())
	if err != nil {
		return err
	}
	if err := tasks.LockPath(cfg.Vault, args[0], shortHostname(), taskNow(cfg)); err != nil {
		return err
	}
	fmt.Printf("locked %s for %s\n", args[0], shortHostname())
	return nil
}

// cmdUnlock: qilla unlock <vault path> — remove this host's own lock line, if any.
func cmdUnlock(args []string) error {
	if len(args) != 1 || args[0] == "" {
		return fmt.Errorf("usage: qilla unlock <vault path>")
	}
	cfg, err := config.Load(config.DefaultPath())
	if err != nil {
		return err
	}
	if err := tasks.UnlockPath(cfg.Vault, args[0], shortHostname()); err != nil {
		return err
	}
	fmt.Printf("unlocked %s\n", args[0])
	return nil
}
