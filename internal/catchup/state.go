package catchup

import (
	"database/sql"
	"os"
	"time"
)

// State is the per-agent bookmark: when the agent last caught up, and how many
// 📓 Log lines each daily note had at that point. It lives in qilla.db because
// it is telemetry, not knowledge — the vault stays the source of truth.
type State struct{ DB *sql.DB }

const stateSchema = `
CREATE TABLE IF NOT EXISTS catchup(
  agent    TEXT NOT NULL,
  date     TEXT NOT NULL DEFAULT '',   -- '' = the agent's last-run row
  last_run INTEGER NOT NULL DEFAULT 0,
  log_seen INTEGER NOT NULL DEFAULT 0,
  PRIMARY KEY(agent, date));`

// OpenState creates the table if needed.
func OpenState(db *sql.DB) (*State, error) {
	if _, err := db.Exec(stateSchema); err != nil {
		return nil, err
	}
	return &State{DB: db}, nil
}

// Agent is QILLA_AGENT, or "chief".
func Agent() string {
	if a := os.Getenv("QILLA_AGENT"); a != "" {
		return a
	}
	return "chief"
}

// Load returns the agent's last run (zero when never) and its per-date Log counts.
func (s *State) Load(agent string) (time.Time, map[string]int, error) {
	seen := map[string]int{}
	var last time.Time
	rows, err := s.DB.Query(`SELECT date, last_run, log_seen FROM catchup WHERE agent=?`, agent)
	if err != nil {
		return last, seen, err
	}
	defer rows.Close()
	for rows.Next() {
		var date string
		var lastRun, n int64
		if err := rows.Scan(&date, &lastRun, &n); err != nil {
			return last, seen, err
		}
		if date == "" {
			if lastRun > 0 {
				last = time.Unix(lastRun, 0)
			}
			continue
		}
		seen[date] = int(n)
	}
	return last, seen, rows.Err()
}

// Save records the run and the Log counts just printed.
func (s *State) Save(agent string, now time.Time, seen map[string]int) error {
	tx, err := s.DB.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`INSERT INTO catchup(agent,date,last_run) VALUES(?,'',?)
		ON CONFLICT(agent,date) DO UPDATE SET last_run=excluded.last_run`, agent, now.Unix()); err != nil {
		return err
	}
	for date, n := range seen {
		if _, err := tx.Exec(`INSERT INTO catchup(agent,date,log_seen) VALUES(?,?,?)
			ON CONFLICT(agent,date) DO UPDATE SET log_seen=excluded.log_seen`, agent, date, n); err != nil {
			return err
		}
	}
	// keep the table small: only the last few days matter
	if _, err := tx.Exec(`DELETE FROM catchup WHERE agent=? AND date<>'' AND date < ?`,
		agent, now.AddDate(0, 0, -7).Format("2006-01-02")); err != nil {
		return err
	}
	return tx.Commit()
}
