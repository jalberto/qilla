package reconcile

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/jalberto/qilla/internal/config"
	"github.com/jalberto/qilla/internal/ledger"
	"github.com/jalberto/qilla/internal/queue"
	"github.com/jalberto/qilla/internal/worker"
)

func setup(t *testing.T) (*config.Config, *queue.Store, *ledger.Store) {
	t.Helper()
	q, err := queue.Open(filepath.Join(t.TempDir(), "q.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { q.Close() })
	l, _ := ledger.New(q.DB(), nil)
	cfg := &config.Config{Routines: map[string]config.Routine{
		"brief":   {Kind: "ai-fresh", Agent: "chief", MustRun: true, Window: "08:00-13:00"},
		"retro":   {Kind: "ai-resumed", Agent: "chief", MustRun: true, Window: "17:00-23:00"},
		"anytime": {Kind: "script", MustRun: true},
		"opt":     {Kind: "script", MustRun: false},
	}}
	return cfg, q, l
}

func local(h int) time.Time { return time.Date(2026, 9, 12, h, 30, 0, 0, time.Local) }

func states(out []Outcome) map[string]string {
	m := map[string]string{}
	for _, o := range out {
		m[o.Routine] = o.State
	}
	return m
}

func TestInsideWindowEnqueues(t *testing.T) {
	cfg, q, l := setup(t)
	out, enq, err := Run(context.Background(), cfg, q, l, local(9))
	if err != nil || !enq {
		t.Fatal(err, enq)
	}
	s := states(out)
	if s["brief"] != "enqueued" || s["retro"] != "waiting" || s["anytime"] != "enqueued" || len(out) != 3 {
		t.Fatalf("%v", s)
	}
	// second pass: already queued, nothing new
	out, enq, _ = Run(context.Background(), cfg, q, l, local(9))
	if enq || states(out)["brief"] != "queued" {
		t.Fatalf("second pass must not duplicate: %v", states(out))
	}
}

func TestAfterWindowEnqueuesLateOnce(t *testing.T) {
	cfg, q, l := setup(t)
	out, _, _ := Run(context.Background(), cfg, q, l, local(14))
	if states(out)["brief"] != "enqueued-late" {
		t.Fatalf("%v", states(out))
	}
	// the late job ran and failed → no second late attempt today
	j, _ := q.Next(context.Background(), local(14))
	for j != nil && j.Routine != "brief" {
		q.Done(context.Background(), j.ID)
		j, _ = q.Next(context.Background(), local(14))
	}
	q.Fail(context.Background(), j.ID, errTest, 1, local(14))
	out, enq, _ := Run(context.Background(), cfg, q, l, local(15))
	if enq && states(out)["brief"] != "pending" {
		t.Fatalf("late must be attempted once: %v", states(out))
	}
	if states(out)["brief"] != "pending" {
		t.Fatalf("%v", states(out))
	}
}

func TestDoneIsDone(t *testing.T) {
	cfg, q, l := setup(t)
	l.Record(worker.Record{Routine: "brief", Started: local(9)})
	l.Record(worker.Record{Routine: "anytime", Skipped: true, Started: local(9)})
	out, enq, _ := Run(context.Background(), cfg, q, l, local(10))
	s := states(out)
	if s["brief"] != "done" || s["anytime"] != "done" || s["retro"] != "waiting" || enq {
		t.Fatalf("%v enq=%v", s, enq)
	}
}

type e string

func (e e) Error() string { return string(e) }

var errTest = e("boom")

func TestImminentTimerIsLeftAlone(t *testing.T) {
	cfg, q, l := setup(t)
	next := func(r string) time.Time {
		if r == "brief" {
			return local(9).Add(20 * time.Minute)
		}
		return time.Time{}
	}
	out, _, _ := RunWith(context.Background(), cfg, q, l, local(9), next)
	s := states(out)
	if s["brief"] != "scheduled" || s["anytime"] != "enqueued" {
		t.Fatalf("%v", s)
	}
}
