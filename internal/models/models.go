// Package models resolves "which model for this run": tiers name a job
// (classify, extract, research, synthesis, judgment, coding), config maps
// tiers to model ids (and optionally an effort), and weekly subscription usage
// degrades every tier one step down the ladder when the window is nearly spent.
package models

import (
	"encoding/json"
	"os"
	"time"
)

// Tiers is the [models] table.
type Tiers struct {
	Classify   string            `toml:"classify"`
	Extract    string            `toml:"extract"`
	Research   string            `toml:"research"`
	Synthesis  string            `toml:"synthesis"`
	Judgment   string            `toml:"judgment"`
	Coding     string            `toml:"coding"`      // writing/changing code: Opus unless told otherwise
	Effort     map[string]string `toml:"effort"`      // per-tier effort (low|medium|high); unset = Claude Code's default
	Ladder     []string          `toml:"ladder"`      // most → least capable; degradation steps down
	DegradeAt  int               `toml:"degrade_at"`  // seven-day utilization % that triggers one step down
	UsageCache string            `toml:"usage_cache"` // Claude Code's usage cache; empty = default
}

// Defaults fills unset fields.
func (t *Tiers) Defaults() {
	def := func(p *string, v string) {
		if *p == "" {
			*p = v
		}
	}
	def(&t.Classify, "claude-haiku-4-5-20251001")
	def(&t.Extract, "claude-haiku-4-5-20251001")
	def(&t.Research, "claude-sonnet-5")
	def(&t.Synthesis, "claude-sonnet-5")
	def(&t.Judgment, "claude-sonnet-5")
	def(&t.Coding, "claude-opus-5")
	if t.Effort == nil {
		t.Effort = map[string]string{}
	}
	if t.Effort["coding"] == "" {
		t.Effort["coding"] = "medium" // coding runs on Opus at medium effort unless set explicitly
	}
	if len(t.Ladder) == 0 {
		t.Ladder = []string{"claude-fable-5-1", "claude-opus-5", "claude-sonnet-5", "claude-haiku-4-5-20251001"}
	}
	if t.DegradeAt == 0 {
		t.DegradeAt = 80
	}
	if t.UsageCache == "" {
		home, _ := os.UserHomeDir()
		t.UsageCache = home + "/.claude/usage-cache.json"
	}
}

// Known tier names.
var Names = []string{"classify", "extract", "research", "synthesis", "judgment", "coding"}

// Model returns the model id for a tier ("" for unknown).
func (t Tiers) Model(tier string) string {
	switch tier {
	case "classify":
		return t.Classify
	case "extract":
		return t.Extract
	case "research":
		return t.Research
	case "synthesis":
		return t.Synthesis
	case "judgment":
		return t.Judgment
	case "coding":
		return t.Coding
	}
	return ""
}

// TierEffort returns the configured effort for a tier ("" = leave it to Claude Code).
func (t Tiers) TierEffort(tier string) string { return t.Effort[tier] }

// Valid reports whether tier is a known name.
func Valid(tier string) bool {
	for _, n := range Names {
		if n == tier {
			return true
		}
	}
	return false
}

// Usage is the seven-day utilization Claude Code caches, or -1 when unknown/expired.
func Usage(path string, now time.Time) int {
	b, err := os.ReadFile(path)
	if err != nil {
		return -1
	}
	var c struct {
		Data struct {
			SevenDay struct {
				Utilization float64 `json:"utilization"`
				ResetsAt    string  `json:"resets_at"`
			} `json:"seven_day"`
		} `json:"data"`
	}
	if json.Unmarshal(b, &c) != nil {
		return -1
	}
	if r, err := time.Parse(time.RFC3339Nano, c.Data.SevenDay.ResetsAt); err == nil && r.Before(now) {
		return -1 // window reset since the cache was written
	}
	return int(c.Data.SevenDay.Utilization)
}

// Degrade returns the next model down the ladder (same model when already at
// the bottom or not on the ladder).
func (t Tiers) Degrade(model string) string {
	for i, m := range t.Ladder {
		if m == model && i+1 < len(t.Ladder) {
			return t.Ladder[i+1]
		}
	}
	return model
}

// Choice is a resolved model for a run.
type Choice struct {
	Model    string
	Tier     string // "" when an explicit model was given
	Effort   string // from the tier's effort, "" when none is configured
	Degraded bool
	Usage    int
}

// Resolve picks the model: explicit routine model > routine tier > agent model
// > agent tier > judgment tier; then degrades when usage ≥ DegradeAt.
func (t Tiers) Resolve(routineModel, routineTier, agentModel, agentTier string, now time.Time) Choice {
	c := Choice{Usage: Usage(t.UsageCache, now)}
	switch {
	case routineModel != "":
		c.Model = routineModel
	case routineTier != "":
		c.Model, c.Tier = t.Model(routineTier), routineTier
	case agentModel != "":
		c.Model = agentModel
	case agentTier != "":
		c.Model, c.Tier = t.Model(agentTier), agentTier
	default:
		c.Model, c.Tier = t.Judgment, "judgment"
	}
	c.Effort = t.TierEffort(c.Tier)
	if c.Usage >= t.DegradeAt && t.DegradeAt > 0 {
		if d := t.Degrade(c.Model); d != c.Model {
			c.Model, c.Degraded = d, true
		}
	}
	return c
}

// FiveHour returns the five-hour window utilization (%) and its reset time
// from Claude Code's usage cache; util -1 when unknown.
func FiveHour(path string) (util int, resetsAt time.Time) {
	b, err := os.ReadFile(path)
	if err != nil {
		return -1, time.Time{}
	}
	var c struct {
		Data struct {
			FiveHour struct {
				Utilization float64 `json:"utilization"`
				ResetsAt    string  `json:"resets_at"`
			} `json:"five_hour"`
		} `json:"data"`
	}
	if json.Unmarshal(b, &c) != nil {
		return -1, time.Time{}
	}
	r, _ := time.Parse(time.RFC3339Nano, c.Data.FiveHour.ResetsAt)
	return int(c.Data.FiveHour.Utilization), r
}
