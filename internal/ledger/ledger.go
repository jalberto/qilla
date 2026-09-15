// Package ledger records every run's tokens and cost and enforces the three
// daily budgets (global, routine, agent) at dequeue time.
package ledger

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/jalberto/qilla/internal/claude"
	"github.com/jalberto/qilla/internal/config"
	"github.com/jalberto/qilla/internal/prices"
	"github.com/jalberto/qilla/internal/queue"
	"github.com/jalberto/qilla/internal/worker"
)

// Store is the ledger on the shared database.
type Store struct {
	db     *sql.DB
	Prices map[string]prices.Price
	Now    func() time.Time
}

// New prepares the runs table.
func New(db *sql.DB, p map[string]prices.Price) (*Store, error) {
	if _, err := db.Exec(`
CREATE TABLE IF NOT EXISTS runs(
  id INTEGER PRIMARY KEY, job_id INTEGER, routine TEXT NOT NULL, agent TEXT NOT NULL DEFAULT '',
  kind TEXT NOT NULL DEFAULT '', model TEXT NOT NULL DEFAULT '', ok INTEGER NOT NULL, skipped INTEGER NOT NULL DEFAULT 0,
  input INTEGER NOT NULL DEFAULT 0, output INTEGER NOT NULL DEFAULT 0, cache_read INTEGER NOT NULL DEFAULT 0,
  cache_write INTEGER NOT NULL DEFAULT 0, cache_write_1h INTEGER NOT NULL DEFAULT 0,
  cost REAL NOT NULL DEFAULT 0, priced INTEGER NOT NULL DEFAULT 0, prompt_chars INTEGER NOT NULL DEFAULT 0,
  duration_ms INTEGER NOT NULL DEFAULT 0, error TEXT NOT NULL DEFAULT '', started INTEGER NOT NULL, day TEXT NOT NULL,
  tier TEXT NOT NULL DEFAULT '', degraded INTEGER NOT NULL DEFAULT 0);
CREATE INDEX IF NOT EXISTS runs_day ON runs(day, routine, agent);`); err != nil {
		return nil, err
	}
	// upgrade older stores in place
	db.Exec(`ALTER TABLE runs ADD COLUMN tier TEXT NOT NULL DEFAULT ''`)
	db.Exec(`ALTER TABLE runs ADD COLUMN degraded INTEGER NOT NULL DEFAULT 0`)
	if p == nil {
		p = map[string]prices.Price{}
	}
	return &Store{db: db, Prices: p, Now: time.Now}, nil
}

// Cost prices a usage for a model. priced=false when the model is unknown.
func Cost(p map[string]prices.Price, model string, u claude.Usage) (cost float64, priced bool) {
	pr, ok := p[model]
	if !ok {
		return 0, false
	}
	m := func(tokens int, perM float64) float64 { return float64(tokens) * perM / 1e6 }
	write5m := u.CacheCreation - u.CacheCreation1h
	if write5m < 0 {
		write5m = 0
	}
	return m(u.Input, pr.Input) + m(u.Output, pr.Output) + m(u.CacheRead, pr.CacheRead) +
		m(write5m, pr.CacheWrite) + m(u.CacheCreation1h, pr.CacheWrite1h), true
}

// Record stores a worker record. Use as Worker.OnRun.
func (s *Store) Record(r worker.Record) {
	cost, priced := Cost(s.Prices, r.Model, r.Usage)
	if r.Skipped || r.Model == "" && r.Usage == (claude.Usage{}) {
		cost, priced = 0, true // no model call: nothing to price
	}
	s.db.Exec(`INSERT INTO runs(job_id,routine,agent,kind,model,ok,skipped,input,output,cache_read,cache_write,cache_write_1h,cost,priced,prompt_chars,duration_ms,error,started,day,tier,degraded)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		r.JobID, r.Routine, r.Agent, r.Kind, r.Model, b2i(r.Error == ""), b2i(r.Skipped),
		r.Usage.Input, r.Usage.Output, r.Usage.CacheRead, r.Usage.CacheCreation-r.Usage.CacheCreation1h, r.Usage.CacheCreation1h,
		cost, b2i(priced), r.PromptChars, r.Duration.Milliseconds(), r.Error, r.Started.Unix(), r.Started.Format("2006-01-02"), r.Tier, b2i(r.Degraded))
}

// Spent returns today's cost for a scope: ("", "") global, (routine, ""), ("", agent).
func (s *Store) Spent(ctx context.Context, day, routine, agent string) (float64, error) {
	q := `SELECT COALESCE(SUM(cost),0) FROM runs WHERE day=?`
	args := []any{day}
	if routine != "" {
		q += ` AND routine=?`
		args = append(args, routine)
	}
	if agent != "" {
		q += ` AND agent=?`
		args = append(args, agent)
	}
	var c float64
	err := s.db.QueryRowContext(ctx, q, args...).Scan(&c)
	return c, err
}

// SucceededToday reports whether a routine has a successful, non-skipped-or-skipped run today.
// A digest skip counts: the routine did its job (nothing changed).
func (s *Store) SucceededToday(ctx context.Context, day, routine string) (bool, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM runs WHERE day=? AND routine=? AND ok=1`, day, routine).Scan(&n)
	return n > 0, err
}

// Gate builds the queue gate: blocks when any of the three daily caps is
// reached, requeueing until the next local midnight. Warn levels are
// reported through warn(scope, spent, cap) and do not block.
func (s *Store) Gate(cfg *config.Config, warn func(scope string, spent, cap float64)) queue.Gate {
	return func(j *queue.Job, now time.Time) (bool, string, time.Time) {
		day := now.Format("2006-01-02")
		midnight := time.Date(now.Year(), now.Month(), now.Day()+1, 0, 0, 0, 0, now.Location())
		ctx := context.Background()
		check := func(scope string, b config.Budget, routine, agent string) (bool, string) {
			if b.Block == 0 && b.Warn == 0 {
				return true, ""
			}
			spent, err := s.Spent(ctx, day, routine, agent)
			if err != nil {
				return false, "ledger: " + err.Error()
			}
			if b.Block > 0 && spent >= b.Block {
				return false, fmt.Sprintf("budget: %s blocked (%.2f/%.2f USD today)", scope, spent, b.Block)
			}
			if b.Warn > 0 && spent >= b.Warn && warn != nil {
				warn(scope, spent, b.Warn)
			}
			return true, ""
		}
		r, hasRoutine := cfg.Routines[j.Routine]
		if hasRoutine && r.Kind == config.KindScript {
			// script routines never call a model and never spend — an LLM
			// budget cap blocking them (e.g. remind, notion-poll) is a bug,
			// not a safeguard.
			return true, "", time.Time{}
		}
		if j.Routine == "ask" {
			// budget caps never gate an ask. They exist to protect against
			// scheduled/automated spend — routines and the sub-agents they
			// trigger. A person's own conversation with the agent is never
			// parked by them.
			return true, "", time.Time{}
		}
		// The global cap tracks scheduled routine spend so one runaway
		// routine can't eat the whole day.
		if ok, why := check("global", cfg.Budget, "", ""); !ok {
			return false, why, midnight
		}
		if hasRoutine {
			if ok, why := check("routine "+j.Routine, r.Budget, j.Routine, ""); !ok {
				return false, why, midnight
			}
		}
		if a, ok := cfg.Agents[j.Agent]; ok && j.Agent != "" {
			if ok, why := check("agent "+j.Agent, a.Budget, "", j.Agent); !ok {
				return false, why, midnight
			}
		}
		return true, "", time.Time{}
	}
}

// Line is one row of `qilla cost`.
type Line struct {
	Key      string
	Runs     int
	Skipped  int
	Failed   int
	Cost     float64
	Unpriced int
}

// Summary groups today's (or all-time when day=="") runs by routine or agent.
func (s *Store) Summary(ctx context.Context, day, by string) ([]Line, error) {
	if by != "routine" && by != "agent" {
		return nil, fmt.Errorf("summary by routine|agent")
	}
	q := fmt.Sprintf(`SELECT %s, COUNT(*), SUM(skipped), SUM(1-ok), SUM(cost), SUM(1-priced) FROM runs`, by)
	var args []any
	if day != "" {
		q += ` WHERE day=?`
		args = append(args, day)
	}
	q += fmt.Sprintf(` GROUP BY %s ORDER BY SUM(cost) DESC`, by)
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Line
	for rows.Next() {
		var l Line
		if err := rows.Scan(&l.Key, &l.Runs, &l.Skipped, &l.Failed, &l.Cost, &l.Unpriced); err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}

// Run is one ledger row for the UI.
type Run struct {
	ID       int64   `json:"id"`
	JobID    int64   `json:"job_id"`
	Routine  string  `json:"routine"`
	Agent    string  `json:"agent"`
	Model    string  `json:"model"`
	OK       bool    `json:"ok"`
	Skipped  bool    `json:"skipped"`
	Cost     float64 `json:"cost"`
	Priced   bool    `json:"priced"`
	Tokens   int     `json:"tokens"`
	Duration int64   `json:"duration_ms"`
	Error    string  `json:"error"`
	Started  int64   `json:"started"`
}

// Recent returns the latest runs.
func (s *Store) Recent(ctx context.Context, n int) ([]Run, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id,job_id,routine,agent,model,ok,skipped,cost,priced,input+output+cache_read+cache_write+cache_write_1h,duration_ms,error,started FROM runs ORDER BY id DESC LIMIT ?`, n)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Run
	for rows.Next() {
		var r Run
		var ok, sk, pr int
		if err := rows.Scan(&r.ID, &r.JobID, &r.Routine, &r.Agent, &r.Model, &ok, &sk, &r.Cost, &pr, &r.Tokens, &r.Duration, &r.Error, &r.Started); err != nil {
			return nil, err
		}
		r.OK, r.Skipped, r.Priced = ok == 1, sk == 1, pr == 1
		out = append(out, r)
	}
	return out, rows.Err()
}
