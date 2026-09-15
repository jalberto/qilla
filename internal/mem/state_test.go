package mem

import (
	"context"
	"strings"
	"testing"
)

// A routine's state entry is what it wrote about its own last run: it must lead
// the injected block whatever the query ranks.
func TestStateInjectedFirst(t *testing.T) {
	s := sqliteStore(t)
	ctx := context.Background()
	if _, err := s.Add(ctx, "market", Heuristic, "pricing", "lowball offers under 60% get declined"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Add(ctx, "market", State, "market/last", "judged offers 41,42; draft pending owner on the camera"); err != nil {
		t.Fatal(err)
	}
	txt, n, err := s.Inject(ctx, "market", "lowball offers pricing", 5, 2000)
	if err != nil {
		t.Fatal(err)
	}
	first := strings.SplitN(strings.TrimSpace(txt), "\n", 2)[0]
	if !strings.HasPrefix(first, "- [state]") || n == 0 {
		t.Fatalf("state must lead the memory block, got:\n%s", txt)
	}
	if !strings.Contains(txt, "lowball offers") {
		t.Fatalf("ranked entries must still follow the state line:\n%s", txt)
	}
}

// State is last-write-wins per key: no numeric aggregation, one line per key.
func TestStateLastWriteWins(t *testing.T) {
	s := sqliteStore(t)
	ctx := context.Background()
	s.Add(ctx, "market", State, "market/last", "3 offers judged")
	e, err := s.Add(ctx, "market", State, "market/last", "7 offers judged")
	if err != nil {
		t.Fatal(err)
	}
	if e.NumN != 0 {
		t.Fatalf("state must not aggregate numbers: %+v", e)
	}
	es, err := s.State(ctx, "market", 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(es) != 1 || !strings.Contains(es[0].Text, "7 offers") {
		t.Fatalf("one line per key, newest wins: %+v", es)
	}
}

// The regression that made memory write-only: the prompt's first line is a
// sentence, backends match terms, so Inject returned an empty block and no
// entry ever registered a hit.
func TestInjectSurvivesSentenceQueries(t *testing.T) {
	s := sqliteStore(t)
	ctx := context.Background()
	if _, err := s.Add(ctx, "market", Heuristic, "shipping", "buyers who ask for shipping outside the app are scams"); err != nil {
		t.Fatal(err)
	}
	txt, _, err := s.Inject(ctx, "market",
		"Judge today's Market inbox and propose replies for the offers that arrived overnight", 5, 2000)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(txt, "outside the app are scams") {
		t.Fatalf("a sentence query must still surface the routine's own memory, got %q", txt)
	}
	es, err := s.B.Recent(ctx, "market", 5)
	if err != nil {
		t.Fatal(err)
	}
	e, err := s.decorate(ctx, es[0])
	if err != nil {
		t.Fatal(err)
	}
	if e.Hits == 0 {
		t.Fatal("an injected entry must register a hit")
	}
}
