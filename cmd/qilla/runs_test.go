package main

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/jalberto/qilla/internal/ledger"
	"github.com/jalberto/qilla/internal/queue"
)

// runsDB is a temp ledger with a handful of rows.
func runsDB(t *testing.T, now time.Time) *sql.DB {
	t.Helper()
	q, err := queue.Open(filepath.Join(t.TempDir(), "q.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { q.Close() })
	if _, err := ledger.New(q.DB(), nil); err != nil {
		t.Fatal(err)
	}
	ins := func(routine string, ok, skipped int, ago time.Duration, cost float64, ms int64, errText string) {
		started := now.Add(-ago)
		if _, err := q.DB().Exec(`INSERT INTO runs(routine,agent,ok,skipped,cost,duration_ms,error,started,day) VALUES(?,?,?,?,?,?,?,?,?)`,
			routine, "chief", ok, skipped, cost, ms, errText, started.Unix(), started.Format("2006-01-02")); err != nil {
			t.Fatal(err)
		}
	}
	ins("brief", 1, 0, 48*time.Hour, 0.20, 30_000, "")
	ins("brief", 1, 0, 2*time.Hour, 0.41, 92_400, "")
	ins("brief", 1, 1, 1*time.Hour, 0, 900, "")            // skipped: never the watermark
	ins("brief", 0, 0, 30*time.Minute, 0, 1100, "boom\nx") // failed: never the watermark
	ins("market", 1, 0, 40*24*time.Hour, 0.05, 5000, "")
	return q.DB()
}

func TestLastOK(t *testing.T) {
	now := time.Now()
	db := runsDB(t, now)
	got, ok, err := lastOK(context.Background(), db, "brief")
	if err != nil || !ok {
		t.Fatalf("lastOK: ok=%v err=%v", ok, err)
	}
	if want := now.Add(-2 * time.Hour).Unix(); got.Unix() != want {
		t.Fatalf("lastOK = %d, want %d (newest ok non-skipped run)", got.Unix(), want)
	}
	if _, ok, err := lastOK(context.Background(), db, "never-ran"); err != nil || ok {
		t.Fatalf("unknown routine: ok=%v err=%v", ok, err)
	}
}

func TestRunLogFilters(t *testing.T) {
	now := time.Now()
	db := runsDB(t, now)
	rows, err := runLog(context.Background(), db, "", 7, 200)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 4 { // the 40-day-old market run is out of the window
		t.Fatalf("got %d rows, want 4: %+v", len(rows), rows)
	}
	if rows[0].Started < rows[1].Started {
		t.Fatal("rows must be newest first")
	}
	if s := runState(rows[0]); s != "failed" {
		t.Fatalf("newest run state %q, want failed", s)
	}
	if s := runState(rows[1]); s != "skipped" {
		t.Fatalf("state %q, want skipped", s)
	}
	if h := head(rows[0].Error); h != "boom" {
		t.Fatalf("error head %q, want boom", h)
	}
	rows, err = runLog(context.Background(), db, "market", 60, 200)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Routine != "market" {
		t.Fatalf("routine filter: %+v", rows)
	}
}

func TestDurFormat(t *testing.T) {
	for _, c := range []struct{ ms, want any }{{int64(1100), "1.1s"}, {int64(92_400), "1m32s"}} {
		if got := dur(c.ms.(int64)); got != c.want {
			t.Fatalf("dur(%v) = %s, want %v", c.ms, got, c.want)
		}
	}
}
