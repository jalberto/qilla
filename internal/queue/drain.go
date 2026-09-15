package queue

import (
	"context"
	"errors"
	"time"
)

// Terminal marks an error that will not go away by retrying (allowlist
// denial, missing routine files). Drain fails the job at once.
type Terminal struct{ Err error }

func (t Terminal) Error() string { return t.Err.Error() }
func (t Terminal) Unwrap() error { return t.Err }

// Deferred marks an error that will clear by itself at Until (a rate-limit
// window). Drain requeues the job for Until without counting an attempt.
type Deferred struct {
	Err   error
	Until time.Time
}

func (d Deferred) Error() string { return d.Err.Error() }
func (d Deferred) Unwrap() error { return d.Err }

// Runner executes one job. Returning an error counts as a failed attempt;
// a Terminal error fails the job without further attempts; a Deferred error
// parks it until its window resets.
type Runner interface {
	Run(ctx context.Context, j *Job) error
}

// Gate decides whether a picked job may run now. ok=false requeues it with
// the reason until retryAt without counting an attempt (budget, window).
type Gate func(j *Job, now time.Time) (ok bool, reason string, retryAt time.Time)

// MaxAttempts tells the drain how many attempts a job's routine allows.
type MaxAttempts func(routine string) int

// Drain runs eligible jobs one at a time until the queue has nothing due.
// It returns how many jobs it ran. Job errors are recorded, not returned.
func Drain(ctx context.Context, s *Store, run Runner, gate Gate, attempts MaxAttempts, now func() time.Time) (int, error) {
	ran := 0
	for {
		if ctx.Err() != nil {
			return ran, ctx.Err()
		}
		j, err := s.Next(ctx, now())
		if err != nil {
			return ran, err
		}
		if j == nil {
			return ran, nil
		}
		if ok, reason, retry := gate(j, now()); !ok {
			if err := s.Requeue(ctx, j.ID, reason, retry); err != nil {
				return ran, err
			}
			continue
		}
		if err := run.Run(ctx, j); err != nil {
			var def Deferred
			if errors.As(err, &def) {
				if err := s.Requeue(ctx, j.ID, "deferred: "+def.Err.Error(), def.Until); err != nil {
					return ran, err
				}
				continue
			}
			max := attempts(j.Routine)
			var term Terminal
			if errors.As(err, &term) {
				max = 1
			}
			if _, ferr := s.Fail(ctx, j.ID, err, max, now()); ferr != nil {
				return ran, ferr
			}
		} else if err := s.Done(ctx, j.ID); err != nil {
			return ran, err
		}
		ran++
	}
}
