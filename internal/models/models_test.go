package models

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

var now = time.Date(2026, 9, 12, 9, 0, 0, 0, time.UTC)

func cache(t *testing.T, util float64, resets string) string {
	p := filepath.Join(t.TempDir(), "usage-cache.json")
	os.WriteFile(p, []byte(`{"data":{"seven_day":{"utilization":`+ftoa(util)+`,"resets_at":"`+resets+`"}}}`), 0o644)
	return p
}

func ftoa(f float64) string {
	b, _ := json.Marshal(f)
	return string(b)
}

func TestResolveOrderAndDefaults(t *testing.T) {
	var tr Tiers
	tr.Defaults()
	tr.UsageCache = cache(t, 14, "2026-09-17T02:00:00Z")
	if c := tr.Resolve("", "", "", "", now); c.Model != tr.Judgment || c.Tier != "judgment" || c.Degraded {
		t.Fatalf("default: %+v", c)
	}
	if c := tr.Resolve("", "classify", "claude-opus-5", "", now); c.Model != tr.Classify || c.Tier != "classify" {
		t.Fatalf("routine tier beats agent model: %+v", c)
	}
	if c := tr.Resolve("claude-fable-5-1", "classify", "", "", now); c.Model != "claude-fable-5-1" || c.Tier != "" {
		t.Fatalf("explicit routine model wins: %+v", c)
	}
	if c := tr.Resolve("", "", "", "research", now); c.Model != tr.Research {
		t.Fatalf("agent tier: %+v", c)
	}
}

// Coding is a tier of its own: Opus at medium effort unless configured otherwise.
func TestCodingTierAndEffort(t *testing.T) {
	var tr Tiers
	tr.Defaults()
	tr.UsageCache = cache(t, 14, "2026-09-17T02:00:00Z")
	if !Valid("coding") || tr.Model("coding") != "claude-opus-5" || tr.TierEffort("coding") != "medium" {
		t.Fatalf("coding defaults: %q %q", tr.Model("coding"), tr.TierEffort("coding"))
	}
	if c := tr.Resolve("", "coding", "claude-sonnet-5", "", now); c.Model != "claude-opus-5" || c.Effort != "medium" || c.Tier != "coding" {
		t.Fatalf("resolve coding: %+v", c)
	}
	if c := tr.Resolve("claude-haiku-4-5-20251001", "", "", "", now); c.Effort != "" {
		t.Fatalf("explicit model must not carry a tier effort: %+v", c)
	}
	tr2 := Tiers{Coding: "claude-fable-5-1", Effort: map[string]string{"coding": "high"}}
	tr2.Defaults()
	if tr2.Model("coding") != "claude-fable-5-1" || tr2.TierEffort("coding") != "high" {
		t.Fatalf("configured coding tier not honoured: %+v", tr2)
	}
}

func TestDegradeAtUsage(t *testing.T) {
	var tr Tiers
	tr.Defaults()
	tr.UsageCache = cache(t, 85, "2026-09-17T02:00:00Z")
	c := tr.Resolve("claude-fable-5-1", "", "", "", now)
	if c.Model != "claude-opus-5" || !c.Degraded || c.Usage != 85 {
		t.Fatalf("one step down: %+v", c)
	}
	c = tr.Resolve("", "classify", "", "", now) // already bottom
	if c.Model != tr.Classify || c.Degraded {
		t.Fatalf("bottom stays: %+v", c)
	}
	tr.UsageCache = cache(t, 85, "2026-09-10T02:00:00Z") // window already reset
	if c := tr.Resolve("claude-fable-5-1", "", "", "", now); c.Degraded || c.Usage != -1 {
		t.Fatalf("expired cache → no degradation: %+v", c)
	}
	tr.UsageCache = "/nonexistent"
	if c := tr.Resolve("claude-fable-5-1", "", "", "", now); c.Degraded {
		t.Fatal("missing cache → fail safe to the chosen model")
	}
}
