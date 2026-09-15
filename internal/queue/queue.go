// Package queue is qilla's job queue: one SQLite table, priority then due
// order, backoff on failure, dedupe of routine jobs already waiting.
package queue

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	_ "modernc.org/sqlite"
)

// Job states.
const (
	Queued    = "queued"
	Running   = "running"
	Done      = "done"
	Failed    = "failed"
	Suspended = "suspended"
	Expired   = "expired"
)

// Backoff between attempts, indexed by attempts already made.
var Backoff = []time.Duration{time.Minute, 5 * time.Minute, 15 * time.Minute}

// Job is one queued execution of a routine or an ask.
type Job struct {
	ID       int64
	Routine  string
	Agent    string
	Priority int
	Due      time.Time
	NotAfter time.Time // zero = never expires
	Attempts int
	State    string
	Digest   string
	Error    string
	Text     string // an ask's text; routines leave it empty
	Late     bool   // enqueued by reconcile after the window closed
	Created  time.Time
	Updated  time.Time
}

// Store is the queue on disk.
type Store struct{ db *sql.DB }

const schema = `
CREATE TABLE IF NOT EXISTS jobs (
  id        INTEGER PRIMARY KEY,
  routine   TEXT NOT NULL,
  agent     TEXT NOT NULL DEFAULT '',
  priority  INTEGER NOT NULL DEFAULT 0,
  due       INTEGER NOT NULL,
  not_after INTEGER NOT NULL DEFAULT 0,
  attempts  INTEGER NOT NULL DEFAULT 0,
  state     TEXT NOT NULL DEFAULT 'queued',
  digest    TEXT NOT NULL DEFAULT '',
  error     TEXT NOT NULL DEFAULT '',
  text      TEXT NOT NULL DEFAULT '',
  late      INTEGER NOT NULL DEFAULT 0,
  created   INTEGER NOT NULL,
  updated   INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS jobs_pick ON jobs(state, priority DESC, due ASC);`

// Open creates or opens the queue database.
func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path+"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("queue schema: %w", err)
	}
	return &Store{db: db}, nil
}

// Close releases the database.
func (s *Store) Close() error { return s.db.Close() }

// Enqueue adds a job. A routine job (empty Text) already queued for the same
// routine is returned instead of duplicated (deduped=true).
func (s *Store) Enqueue(ctx context.Context, j Job) (id int64, deduped bool, err error) {
	if j.Routine == "" {
		return 0, false, errors.New("enqueue: routine required")
	}
	if j.Text == "" {
		err = s.db.QueryRowContext(ctx, `SELECT id FROM jobs WHERE routine=? AND text='' AND state=?`, j.Routine, Queued).Scan(&id)
		if err == nil {
			return id, true, nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return 0, false, err
		}
	}
	now := time.Now()
	if j.Due.IsZero() {
		j.Due = now
	}
	res, err := s.db.ExecContext(ctx, `INSERT INTO jobs(routine,agent,priority,due,not_after,state,text,late,created,updated)
		VALUES(?,?,?,?,?,?,?,?,?,?)`,
		j.Routine, j.Agent, j.Priority, j.Due.Unix(), unixOrZero(j.NotAfter), Queued, j.Text, b2i(j.Late), now.Unix(), now.Unix())
	if err != nil {
		return 0, false, err
	}
	id, err = res.LastInsertId()
	return id, false, err
}

// Next expires overdue jobs, then returns the best queued job due by now,
// marking it running. nil when nothing is eligible.
func (s *Store) Next(ctx context.Context, now time.Time) (*Job, error) {
	if _, err := s.db.ExecContext(ctx, `UPDATE jobs SET state=?, updated=? WHERE state=? AND not_after>0 AND not_after<?`,
		Expired, now.Unix(), Queued, now.Unix()); err != nil {
		return nil, err
	}
	row := s.db.QueryRowContext(ctx, `SELECT `+cols+` FROM jobs WHERE state=? AND due<=? ORDER BY priority DESC, due ASC LIMIT 1`, Queued, now.Unix())
	j, err := scan(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE jobs SET state=?, updated=? WHERE id=?`, Running, now.Unix(), j.ID); err != nil {
		return nil, err
	}
	j.State = Running
	return j, nil
}

// Done marks a job finished.
func (s *Store) Done(ctx context.Context, id int64) error {
	return s.setState(ctx, id, Done, "")
}

// Fail records a failure. With attempts left the job is requeued with backoff
// (state Queued); otherwise it is Failed. Returns the resulting state.
func (s *Store) Fail(ctx context.Context, id int64, cause error, maxAttempts int, now time.Time) (string, error) {
	j, err := s.Get(ctx, id)
	if err != nil {
		return "", err
	}
	attempts := j.Attempts + 1
	state, due := Failed, j.Due
	if attempts < maxAttempts {
		state = Queued
		due = now.Add(Backoff[min(attempts-1, len(Backoff)-1)])
	}
	_, err = s.db.ExecContext(ctx, `UPDATE jobs SET state=?, attempts=?, error=?, due=?, updated=? WHERE id=?`,
		state, attempts, cause.Error(), due.Unix(), now.Unix(), id)
	return state, err
}

// Requeue puts a blocked job back with a reason (budget, window) without
// counting an attempt.
func (s *Store) Requeue(ctx context.Context, id int64, reason string, due time.Time) error {
	_, err := s.db.ExecContext(ctx, `UPDATE jobs SET state=?, error=?, due=?, updated=? WHERE id=?`, Queued, reason, due.Unix(), time.Now().Unix(), id)
	return err
}

// Suspend parks a job (and by convention its routine) until resumed.
func (s *Store) Suspend(ctx context.Context, id int64, reason string) error {
	return s.setState(ctx, id, Suspended, reason)
}

// Get returns one job.
func (s *Store) Get(ctx context.Context, id int64) (*Job, error) {
	return scan(s.db.QueryRowContext(ctx, `SELECT `+cols+` FROM jobs WHERE id=?`, id))
}

// List returns the most recent jobs.
func (s *Store) List(ctx context.Context, limit int) ([]Job, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+cols+` FROM jobs ORDER BY id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Job
	for rows.Next() {
		j, err := scan(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *j)
	}
	return out, rows.Err()
}

// Pending counts queued and running jobs.
func (s *Store) Pending(ctx context.Context) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM jobs WHERE state IN (?,?)`, Queued, Running).Scan(&n)
	return n, err
}

func (s *Store) setState(ctx context.Context, id int64, state, msg string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE jobs SET state=?, error=?, updated=? WHERE id=?`, state, msg, time.Now().Unix(), id)
	return err
}

const cols = `id,routine,agent,priority,due,not_after,attempts,state,digest,error,text,late,created,updated`

type scanner interface{ Scan(...any) error }

func scan(r scanner) (*Job, error) {
	var j Job
	var due, notAfter, created, updated int64
	var late int
	err := r.Scan(&j.ID, &j.Routine, &j.Agent, &j.Priority, &due, &notAfter, &j.Attempts, &j.State, &j.Digest, &j.Error, &j.Text, &late, &created, &updated)
	if err != nil {
		return nil, err
	}
	j.Due = time.Unix(due, 0)
	if notAfter > 0 {
		j.NotAfter = time.Unix(notAfter, 0)
	}
	j.Late = late == 1
	j.Created, j.Updated = time.Unix(created, 0), time.Unix(updated, 0)
	return &j, nil
}

func unixOrZero(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.Unix()
}

func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}

// DB exposes the shared handle so sibling packages (sessions, ledger) keep
// their tables in the same file.
func (s *Store) DB() *sql.DB { return s.db }
