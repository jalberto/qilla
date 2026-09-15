package main

import (
	"fmt"
	"time"

	"github.com/jalberto/qilla/internal/config"
	"github.com/jalberto/qilla/internal/models"
)

// cmdModel: qilla model [tier] — resolved model per tier, with usage-aware degradation.
func cmdModel(args []string) error {
	cfg, err := config.Load(config.DefaultPath())
	if err != nil {
		return err
	}
	now := time.Now()
	u := models.Usage(cfg.Models.UsageCache, now)
	if len(args) == 1 {
		if !models.Valid(args[0]) {
			return fmt.Errorf("tier must be one of %v", models.Names)
		}
		c := cfg.Models.Resolve("", args[0], "", "", now)
		fmt.Println(c.Model)
		return nil
	}
	if u >= 0 {
		fmt.Printf("usage_7d=%d degrade_at=%d\n", u, cfg.Models.DegradeAt)
	} else {
		fmt.Printf("usage_7d=unknown degrade_at=%d\n", cfg.Models.DegradeAt)
	}
	for _, t := range models.Names {
		c := cfg.Models.Resolve("", t, "", "", now)
		fmt.Printf("%s=%s", t, c.Model)
		if c.Degraded {
			fmt.Printf(" degraded_from=%s", cfg.Models.Model(t))
		}
		fmt.Println()
	}
	return nil
}
