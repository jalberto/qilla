package queue

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

type fakeRunner struct {
	fail map[string]bool
	ran  []string
}

func (f *fakeRunner) Run(_ context.Context, j *Job) error {
	f.ran = append(f.ran, j.Routine)
	if f.fail[j.Routine] {
		return errors.New("nope")
	}
	return nil
}

func allow(*Job, time.Time) (bool, string, time.Time) { return true, "", time.Time{} }
func three(string) int                                { return 3 }

func TestDrainRunsInOrderAndRecords(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	s.Enqueue(ctx, Job{Routine: "a", Priority: 1, Due: now})
	s.Enqueue(ctx, Job{Routine: "b", Priority: 5, Due: now})
	s.Enqueue(ctx, Job{Routine: "bad", Priority: 9, Due: now})
	r := &fakeRunner{fail: map[string]bool{"bad": true}}
	n, err := Drain(ctx, s, r, allow, three, func() time.Time { return now })
	if err != nil || n != 3 {
		t.Fatalf("ran %d err %v", n, err)
	}
	if len(r.ran) != 3 || r.ran[0] != "bad" || r.ran[1] != "b" || r.ran[2] != "a" {
		t.Fatalf("order: %v", r.ran)
	}
	js, _ := s.List(ctx, 10)
	for _, j := range js {
		switch j.Routine {
		case "bad":
			if j.State != Queued || j.Attempts != 1 {
				t.Fatalf("bad should be requeued with backoff: %+v", j)
			}
		default:
			if j.State != Done {
				t.Fatalf("%s should be done: %+v", j.Routine, j)
			}
		}
	}
}

func TestDrainGateRequeuesWithoutAttempt(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	id, _, _ := s.Enqueue(ctx, Job{Routine: "brief", Due: now})
	block := func(*Job, time.Time) (bool, string, time.Time) {
		return false, "budget: routine brief blocked (1.60/1.50)", now.Add(time.Hour)
	}
	r := &fakeRunner{}
	n, _ := Drain(ctx, s, r, block, three, func() time.Time { return now })
	j, _ := s.Get(ctx, id)
	if n != 0 || len(r.ran) != 0 || j.State != Queued || j.Attempts != 0 || j.Error == "" || !j.Due.After(now) {
		t.Fatalf("gate must requeue with reason and later due: n=%d %+v", n, j)
	}
}

type terminalRunner struct{ n int }

func (r *terminalRunner) Run(_ context.Context, j *Job) error {
	r.n++
	return Terminal{Err: errors.New("tools denied by allowlist: Bash")}
}

func TestDrainTerminalErrorFailsWithoutRetry(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	id, _, _ := s.Enqueue(ctx, Job{Routine: "brief", Due: now})
	r := &terminalRunner{}
	Drain(ctx, s, r, allow, three, func() time.Time { return now })
	j, _ := s.Get(ctx, id)
	if j.State != Failed || j.Attempts != 1 || r.n != 1 {
		t.Fatalf("terminal error must fail at once: %+v runs=%d", j, r.n)
	}
}

type deferRunner struct{ n int }

func (r *deferRunner) Run(_ context.Context, j *Job) error {
	r.n++
	return Deferred{Err: errors.New("session limit"), Until: now.Add(90 * time.Minute)}
}

func TestDrainDeferredParksWithoutAttempt(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	id, _, _ := s.Enqueue(ctx, Job{Routine: "brief", Due: now})
	r := &deferRunner{}
	Drain(ctx, s, r, allow, three, func() time.Time { return now })
	j, _ := s.Get(ctx, id)
	if j.State != Queued || j.Attempts != 0 || !j.Due.After(now.Add(80*time.Minute)) || !strings.Contains(j.Error, "deferred") {
		t.Fatalf("deferred must park until the window resets: %+v", j)
	}
	if r.n != 1 {
		t.Fatalf("ran %d", r.n)
	}
}
