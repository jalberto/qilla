// Package reconcile proves every must_run routine succeeded today, or
// enqueues it: inside its window normally, after the window once and late.
package reconcile

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jalberto/qilla/internal/config"
	"github.com/jalberto/qilla/internal/ledger"
	"github.com/jalberto/qilla/internal/queue"
)

// Outcome per must_run routine.
type Outcome struct {
	Routine string
	State   string // done | queued | enqueued | enqueued-late | waiting | pending
	Note    string
}

// NextFire tells reconcile when a routine's timer fires next (zero = unknown).
type NextFire func(routine string) time.Time

// Grace: a slot this close is left to its timer instead of enqueueing now.
const Grace = 90 * time.Minute

// Run checks every must_run routine once. Returns one outcome per routine,
// sorted by name, and whether anything was enqueued (caller pokes serve).
func Run(ctx context.Context, cfg *config.Config, q *queue.Store, l *ledger.Store, now time.Time) ([]Outcome, bool, error) {
	return RunWith(ctx, cfg, q, l, now, nil)
}

// RunWith is Run with timer knowledge: inside the window, a routine whose
// timer fires within Grace is reported "scheduled" and not enqueued.
func RunWith(ctx context.Context, cfg *config.Config, q *queue.Store, l *ledger.Store, now time.Time, next NextFire) ([]Outcome, bool, error) {
	day := now.Format("2006-01-02")
	var out []Outcome
	enqueued := false
	for name, r := range cfg.Routines {
		if !r.MustRun {
			continue
		}
		o := Outcome{Routine: name}
		ok, err := l.SucceededToday(ctx, day, name)
		if err != nil {
			return nil, false, err
		}
		switch {
		case ok:
			o.State = "done"
		default:
			pending, err := hasPending(ctx, q, name)
			if err != nil {
				return nil, false, err
			}
			if pending {
				o.State = "queued"
				break
			}
			pos := position(r.Window, now)
			switch pos {
			case before:
				o.State = "waiting"
				o.Note = "window opens at " + strings.Split(r.Window, "-")[0]
			case inside:
				if next != nil {
					if nf := next(name); !nf.IsZero() && nf.After(now) && nf.Sub(now) <= Grace {
						o.State, o.Note = "scheduled", "timer fires "+nf.Format("15:04")
						break
					}
				}
				if _, _, err := q.Enqueue(ctx, queue.Job{Routine: name, Agent: r.Agent, Due: now}); err != nil {
					return nil, false, err
				}
				o.State, enqueued = "enqueued", true
			case after:
				if lateToday, err := hadLateJob(ctx, q, name, day); err != nil {
					return nil, false, err
				} else if lateToday {
					o.State = "pending"
					o.Note = "late job already attempted today"
					break
				}
				if _, _, err := q.Enqueue(ctx, queue.Job{Routine: name, Agent: r.Agent, Due: now, Late: true, Priority: 1}); err != nil {
					return nil, false, err
				}
				o.State, o.Note, enqueued = "enqueued-late", "window "+r.Window+" missed", true
			}
		}
		out = append(out, o)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Routine < out[j].Routine })
	return out, enqueued, nil
}

type where int

const (
	inside where = iota
	before
	after
)

// position of now relative to a "HH:MM-HH:MM" window; empty window = inside.
func position(window string, now time.Time) where {
	if window == "" {
		return inside
	}
	a, b, _ := strings.Cut(window, "-")
	start, end := at(now, a), at(now, b)
	switch {
	case now.Before(start):
		return before
	case !now.Before(end):
		return after
	default:
		return inside
	}
}

func at(day time.Time, hhmm string) time.Time {
	h, m, _ := strings.Cut(hhmm, ":")
	hh, _ := strconv.Atoi(h)
	mm, _ := strconv.Atoi(m)
	return time.Date(day.Year(), day.Month(), day.Day(), hh, mm, 0, 0, day.Location())
}

func hasPending(ctx context.Context, q *queue.Store, routine string) (bool, error) {
	var n int
	err := q.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM jobs WHERE routine=? AND state IN (?,?)`, routine, queue.Queued, queue.Running).Scan(&n)
	return n > 0, err
}

func hadLateJob(ctx context.Context, q *queue.Store, routine, day string) (bool, error) {
	start := at(mustDay(day), "00:00").Unix()
	var n int
	err := q.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM jobs WHERE routine=? AND late=1 AND created>=?`, routine, start).Scan(&n)
	return n > 0, err
}

func mustDay(day string) time.Time {
	t, _ := time.ParseInLocation("2006-01-02", day, time.Local)
	return t
}

// Format renders outcomes for the terminal.
func Format(out []Outcome) string {
	var b strings.Builder
	for _, o := range out {
		mark := map[string]string{"done": "✓", "queued": "…", "enqueued": "→", "enqueued-late": "⚠", "waiting": "·", "pending": "✗", "scheduled": "⏲"}[o.State]
		fmt.Fprintf(&b, "%s %-20s %-14s %s\n", mark, o.Routine, o.State, o.Note)
	}
	return b.String()
}
