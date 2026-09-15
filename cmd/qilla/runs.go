package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"flag"
	"fmt"
	"strings"
	"time"

	"github.com/jalberto/qilla/internal/config"
	"github.com/jalberto/qilla/internal/ledger"
	"github.com/jalberto/qilla/internal/queue"
)

const runsUsage = `usage: qilla runs <command>

  last <routine>              start time (RFC3339, local) of its newest ok run
                              — exit 1 with "no run" when it never ran
  log [--routine r] [--days n] [--json]
                              recent runs: time, routine, state, cost, duration, error`

// cmdRuns: qilla runs last|log — the run history in the ledger, the watermark
// helpers read to decide whether anything is new since the last paid run.
func cmdRuns(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("%s", runsUsage)
	}
	cfg, err := config.Load(config.DefaultPath())
	if err != nil {
		return err
	}
	q, err := queue.Open(cfg.DBPath())
	if err != nil {
		return err
	}
	defer q.Close()
	if _, err := ledger.New(q.DB(), nil); err != nil { // ensures the runs table exists
		return err
	}
	ctx := context.Background()
	switch args[0] {
	case "last":
		if len(args) != 2 {
			return fmt.Errorf("usage: qilla runs last <routine>")
		}
		t, ok, err := lastOK(ctx, q.DB(), args[1])
		if err != nil {
			return err
		}
		if !ok {
			return fmt.Errorf("no run")
		}
		fmt.Println(t.Local().Format(time.RFC3339))
	case "log":
		fs := flag.NewFlagSet("runs log", flag.ContinueOnError)
		routine := fs.String("routine", "", "only this routine")
		days := fs.Int("days", 7, "how far back to look")
		asJSON := fs.Bool("json", false, "JSON array instead of the table")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		rows, err := runLog(ctx, q.DB(), *routine, *days, 200)
		if err != nil {
			return err
		}
		if *asJSON {
			out := make([]runJSON, 0, len(rows))
			for _, r := range rows {
				out = append(out, runJSON{Routine: r.Routine, Agent: r.Agent, State: runState(r), OK: r.OK,
					Skipped: r.Skipped, Cost: r.Cost, DurationMS: r.Duration, Error: r.Error,
					Started: time.Unix(r.Started, 0).Local().Format(time.RFC3339),
					Day:     time.Unix(r.Started, 0).Local().Format("2006-01-02")})
			}
			b, err := json.Marshal(out)
			if err != nil {
				return err
			}
			fmt.Println(string(b))
			return nil
		}
		// TSV: time, routine, state, USD, duration, first line of the error
		fmt.Printf("n=%d\n", len(rows))
		for _, r := range rows {
			fmt.Printf("%s\t%s\t%s\t%.3f\t%s\t%s\n",
				time.Unix(r.Started, 0).Local().Format("2006-01-02 15:04"), r.Routine, runState(r), r.Cost, dur(r.Duration), head(r.Error))
		}
	default:
		return fmt.Errorf("unknown runs command %q\n%s", args[0], runsUsage)
	}
	return nil
}

// runJSON is one ledger row as `qilla runs log --json` prints it: the shape a
// gather can consume without parsing the table.
type runJSON struct {
	Routine    string  `json:"routine"`
	Agent      string  `json:"agent,omitempty"`
	State      string  `json:"state"` // ok | skipped | failed
	OK         bool    `json:"ok"`
	Skipped    bool    `json:"skipped"`
	Cost       float64 `json:"cost"`
	DurationMS int64   `json:"duration_ms"`
	Error      string  `json:"error,omitempty"`
	Started    string  `json:"started"`
	Day        string  `json:"day"`
}

// lastOK is the start of the newest successful, non-skipped run of a routine.
func lastOK(ctx context.Context, db *sql.DB, routine string) (time.Time, bool, error) {
	var started sql.NullInt64
	err := db.QueryRowContext(ctx, `SELECT MAX(started) FROM runs WHERE routine=? AND ok=1 AND skipped=0`, routine).Scan(&started)
	if err != nil {
		return time.Time{}, false, err
	}
	if !started.Valid {
		return time.Time{}, false, nil
	}
	return time.Unix(started.Int64, 0), true, nil
}

// runLog returns the newest runs of the last days, newest first.
func runLog(ctx context.Context, db *sql.DB, routine string, days, limit int) ([]ledger.Run, error) {
	if days <= 0 {
		days = 7
	}
	since := time.Now().AddDate(0, 0, -days).Unix()
	q := `SELECT routine,agent,ok,skipped,cost,duration_ms,error,started FROM runs WHERE started>=?`
	args := []any{since}
	if routine != "" {
		q += ` AND routine=?`
		args = append(args, routine)
	}
	q += ` ORDER BY started DESC LIMIT ?`
	args = append(args, limit)
	rows, err := db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ledger.Run
	for rows.Next() {
		var r ledger.Run
		var ok, sk int
		if err := rows.Scan(&r.Routine, &r.Agent, &ok, &sk, &r.Cost, &r.Duration, &r.Error, &r.Started); err != nil {
			return nil, err
		}
		r.OK, r.Skipped = ok == 1, sk == 1
		out = append(out, r)
	}
	return out, rows.Err()
}

func runState(r ledger.Run) string {
	switch {
	case r.Skipped:
		return "skipped"
	case r.OK:
		return "ok"
	}
	return "failed"
}

func dur(ms int64) string {
	d := time.Duration(ms) * time.Millisecond
	if d < time.Minute {
		return fmt.Sprintf("%.1fs", d.Seconds())
	}
	return fmt.Sprintf("%dm%02ds", int(d.Minutes()), int(d.Seconds())%60)
}

// head is the first line of an error, short enough to keep the table one row
// per run.
func head(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if r := []rune(s); len(r) > 60 {
		s = string(r[:60])
	}
	return s
}
