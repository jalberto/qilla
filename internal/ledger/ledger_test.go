package ledger

import (
	"context"
	"math"
	"path/filepath"
	"testing"
	"time"

	"github.com/jalberto/qilla/internal/claude"
	"github.com/jalberto/qilla/internal/config"
	"github.com/jalberto/qilla/internal/prices"
	"github.com/jalberto/qilla/internal/queue"
	"github.com/jalberto/qilla/internal/worker"
)

var sonnet = map[string]prices.Price{"claude-sonnet-5": {Input: 3, Output: 15, CacheRead: 0.3, CacheWrite: 3.75, CacheWrite1h: 6}}

func TestCostIncludesCacheTiers(t *testing.T) {
	u := claude.Usage{Input: 1_000_000, Output: 100_000, CacheRead: 2_000_000, CacheCreation: 500_000, CacheCreation1h: 200_000}
	c, ok := Cost(sonnet, "claude-sonnet-5", u)
	// 3 + 1.5 + 0.6 + 300k*3.75/1M(1.125) + 200k*6/1M(1.2)
	if !ok || math.Abs(c-7.425) > 1e-9 {
		t.Fatalf("cost %v ok %v", c, ok)
	}
	if _, ok := Cost(sonnet, "unknown-model", u); ok {
		t.Fatal("unknown model must be unpriced")
	}
}

func setup(t *testing.T) (*Store, *config.Config) {
	t.Helper()
	q, err := queue.Open(filepath.Join(t.TempDir(), "q.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { q.Close() })
	s, err := New(q.DB(), sonnet)
	if err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{
		Budget:   config.Budget{Warn: 4, Block: 6},
		Agents:   map[string]config.Agent{"chief": {Budget: config.Budget{Block: 3}}},
		Routines: map[string]config.Routine{"brief": {Agent: "chief", Budget: config.Budget{Block: 1.5}}, "free": {Agent: "chief"}},
	}
	return s, cfg
}

func spend(s *Store, routine, agent string, usd float64) {
	// 1 USD = 333,333.33 input tokens at $3/M; use output at $15/M for exactness: usd/15*1e6
	s.Record(worker.Record{JobID: 1, Routine: routine, Agent: agent, Model: "claude-sonnet-5",
		Usage: claude.Usage{Output: int(usd / 15 * 1e6)}, Started: time.Date(2026, 9, 12, 9, 0, 0, 0, time.UTC)})
}

var now = time.Date(2026, 9, 12, 10, 0, 0, 0, time.UTC)

func TestGateThreeLevels(t *testing.T) {
	s, cfg := setup(t)
	var warned []string
	gate := s.Gate(cfg, func(scope string, _, _ float64) { warned = append(warned, scope) })

	if ok, _, _ := gate(&queue.Job{Routine: "brief", Agent: "chief"}, now); !ok {
		t.Fatal("empty ledger must pass")
	}
	spend(s, "brief", "chief", 1.5)
	ok, why, retry := gate(&queue.Job{Routine: "brief", Agent: "chief"}, now)
	if ok || why == "" || retry.Day() != 13 {
		t.Fatalf("routine cap: ok=%v why=%q retry=%v", ok, why, retry)
	}
	if ok, _, _ := gate(&queue.Job{Routine: "free", Agent: "chief"}, now); !ok {
		t.Fatal("other routine under agent cap must pass")
	}
	spend(s, "free", "chief", 1.6) // agent total 3.1 ≥ 3
	if ok, why, _ := gate(&queue.Job{Routine: "free", Agent: "chief"}, now); ok || why == "" {
		t.Fatalf("agent cap must block: %v %q", ok, why)
	}
	spend(s, "other", "x", 1.0) // global 4.1 ≥ warn 4, < block 6
	if ok, _, _ := gate(&queue.Job{Routine: "other", Agent: "x"}, now); !ok || len(warned) == 0 || warned[len(warned)-1] != "global" {
		t.Fatalf("global warn must pass and warn: %v", warned)
	}
	spend(s, "other", "x", 2.0) // 6.1 ≥ 6
	if ok, why, _ := gate(&queue.Job{Routine: "other", Agent: "x"}, now); ok || why == "" {
		t.Fatalf("global block: %v %q", ok, why)
	}
}

// Script routines never spend; an ask is the owner's conversation, so no budget cap
// — global, routine or agent — may ever gate it.
func TestGateAskAndScriptExemptions(t *testing.T) {
	s, cfg := setup(t)
	cfg.Routines["remind"] = config.Routine{Kind: config.KindScript}
	gate := s.Gate(cfg, nil)
	spend(s, "brief", "chief", 6.5) // global 6.5 ≥ block 6, agent chief 6.5 ≥ 3
	if ok, why, _ := gate(&queue.Job{Routine: "brief", Agent: "chief"}, now); ok {
		t.Fatalf("routine must be blocked: %q", why)
	}
	if ok, why, _ := gate(&queue.Job{Routine: "remind"}, now); !ok {
		t.Fatalf("script routine gated by an LLM cap: %q", why)
	}
	if ok, why, _ := gate(&queue.Job{Routine: "ask", Agent: "chief"}, now); !ok {
		t.Fatalf("ask starved by routine spend: %q", why)
	}
	spend(s, "ask", "chief", 3.0) // the conversation itself is over every cap too
	if ok, why, _ := gate(&queue.Job{Routine: "ask", Agent: "chief"}, now); !ok {
		t.Fatalf("a budget cap must never gate an ask: %q", why)
	}
}

func TestSummaryAndSucceeded(t *testing.T) {
	s, _ := setup(t)
	spend(s, "brief", "chief", 0.3)
	s.Record(worker.Record{JobID: 2, Routine: "brief", Agent: "chief", Model: "claude-sonnet-5", Skipped: true, Started: now})
	s.Record(worker.Record{JobID: 3, Routine: "triage", Agent: "chief", Model: "mystery", Usage: claude.Usage{Input: 5}, Error: "boom", Started: now})
	s.Record(worker.Record{JobID: 4, Routine: "cal", Kind: "script", Started: now})
	lines, err := s.Summary(context.Background(), "2026-09-12", "routine")
	if err != nil || len(lines) != 3 || lines[0].Key != "brief" || lines[0].Runs != 2 || lines[0].Skipped != 1 || lines[0].Unpriced != 0 {
		t.Fatalf("summary: %v %+v", err, lines)
	}
	for _, l := range lines {
		switch l.Key {
		case "triage":
			if l.Failed != 1 || l.Unpriced != 1 {
				t.Fatalf("unknown model is unpriced: %+v", l)
			}
		case "cal":
			if l.Unpriced != 0 {
				t.Fatalf("script routine must not be flagged unpriced: %+v", l)
			}
		}
	}
	ok, _ := s.SucceededToday(context.Background(), "2026-09-12", "triage")
	if ok {
		t.Fatal("failed run must not count as success")
	}
	ok, _ = s.SucceededToday(context.Background(), "2026-09-12", "brief")
	if !ok {
		t.Fatal("brief succeeded")
	}
}
