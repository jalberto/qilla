package queue

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func open(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "q.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

var now = time.Date(2026, 9, 12, 8, 0, 0, 0, time.UTC)

func TestNextOrderPriorityThenDue(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	s.Enqueue(ctx, Job{Routine: "low-late", Priority: 1, Due: now.Add(-2 * time.Hour)})
	s.Enqueue(ctx, Job{Routine: "high-early", Priority: 5, Due: now.Add(-1 * time.Hour)})
	s.Enqueue(ctx, Job{Routine: "high-late", Priority: 5, Due: now.Add(-3 * time.Hour)})
	s.Enqueue(ctx, Job{Routine: "future", Priority: 9, Due: now.Add(time.Hour)})
	want := []string{"high-late", "high-early", "low-late"}
	for _, w := range want {
		j, err := s.Next(ctx, now)
		if err != nil || j == nil || j.Routine != w {
			t.Fatalf("want %s got %+v err %v", w, j, err)
		}
		s.Done(ctx, j.ID)
	}
	if j, _ := s.Next(ctx, now); j != nil {
		t.Fatalf("future job must not be picked: %+v", j)
	}
}

func TestEnqueueDedupesQueuedRoutine(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	id1, dup1, _ := s.Enqueue(ctx, Job{Routine: "brief", Due: now})
	id2, dup2, _ := s.Enqueue(ctx, Job{Routine: "brief", Due: now})
	if dup1 || !dup2 || id1 != id2 {
		t.Fatalf("second enqueue must dedupe: %d %v / %d %v", id1, dup1, id2, dup2)
	}
	// an ask (with Text) is never deduped
	_, dup3, _ := s.Enqueue(ctx, Job{Routine: "ask", Text: "hola", Due: now})
	_, dup4, _ := s.Enqueue(ctx, Job{Routine: "ask", Text: "hola", Due: now})
	if dup3 || dup4 {
		t.Fatal("asks must not dedupe")
	}
}

func TestFailBackoffThenFailed(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	id, _, _ := s.Enqueue(ctx, Job{Routine: "brief", Due: now})
	j, _ := s.Next(ctx, now)
	if j.State != Running {
		t.Fatalf("Next must mark running, got %s", j.State)
	}
	st, _ := s.Fail(ctx, id, errors.New("boom"), 3, now)
	if st != Queued {
		t.Fatalf("first failure requeues, got %s", st)
	}
	if j, _ = s.Next(ctx, now); j != nil {
		t.Fatal("backoff must hide the job for 1 min")
	}
	j, _ = s.Next(ctx, now.Add(61*time.Second))
	if j == nil || j.Attempts != 1 {
		t.Fatalf("after backoff the job is back, got %+v", j)
	}
	s.Fail(ctx, id, errors.New("boom"), 3, now)
	j, _ = s.Next(ctx, now.Add(6*time.Minute))
	st, _ = s.Fail(ctx, id, errors.New("boom"), 3, now)
	if st != Failed {
		t.Fatalf("third failure is terminal, got %s", st)
	}
	g, _ := s.Get(ctx, id)
	if g.Error != "boom" || g.Attempts != 3 {
		t.Fatalf("error and attempts recorded: %+v", g)
	}
}

func TestNotAfterExpires(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	id, _, _ := s.Enqueue(ctx, Job{Routine: "brief", Due: now.Add(-time.Hour), NotAfter: now.Add(-time.Minute)})
	if j, _ := s.Next(ctx, now); j != nil {
		t.Fatal("expired job must not run")
	}
	g, _ := s.Get(ctx, id)
	if g.State != Expired {
		t.Fatalf("want expired got %s", g.State)
	}
}

func TestListToday(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	s.Enqueue(ctx, Job{Routine: "a", Due: now})
	s.Enqueue(ctx, Job{Routine: "b", Due: now})
	js, err := s.List(ctx, 10)
	if err != nil || len(js) != 2 {
		t.Fatalf("list: %v %d", err, len(js))
	}
}
