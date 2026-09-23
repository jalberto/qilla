// Package engine answers "what is the engine doing?": which routines are
// running, when each last ran, and where the engine's vault checkout stands.
// On the engine (or when the engine is this host) the answer is read from the
// local state DB and git; an agent host asks over ssh by running
// `qilla engine status --json` on the engine.
package engine

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/jalberto/qilla/internal/config"
	"github.com/jalberto/qilla/internal/ledger"
	"github.com/jalberto/qilla/internal/queue"
)

// Running is one routine the engine's queue has marked running.
type Running struct {
	Routine string    `json:"routine"`
	Since   time.Time `json:"since"`
}

// Last is a routine's newest ledger row.
type Last struct {
	At        time.Time `json:"at"`
	OK        bool      `json:"ok"`
	DurationS float64   `json:"duration_s"`
}

// Vault is the engine's checkout state. Empty fields mean git failed.
type Vault struct {
	Head         string `json:"head"`
	Dirty        int    `json:"dirty"`
	LastCommitAt string `json:"last_commit_at"`
}

// Status is the whole answer.
type Status struct {
	Host    string          `json:"host"`
	Role    string          `json:"role"`
	Now     time.Time       `json:"now"`
	Running []Running       `json:"running"`
	Last    map[string]Last `json:"last"`
	Vault   Vault           `json:"vault"`
}

// staleRunning is how old a queue row in state "running" may be before it is
// treated as a crashed leftover rather than a live run.
const staleRunning = 12 * time.Hour

// gitTimeout bounds each git call; a slow or broken repo yields empty fields.
const gitTimeout = 3 * time.Second

// sshTimeout bounds the whole remote call (ConnectTimeout is 5 s of it).
var sshTimeout = 15 * time.Second

// UnreachableError is returned when the engine cannot be asked.
type UnreachableError struct {
	Host   string
	Detail string // first stderr line, or the underlying error
}

func (e *UnreachableError) Error() string {
	if e.Detail == "" {
		return "engine " + e.Host + " unreachable"
	}
	return "engine " + e.Host + " unreachable: " + e.Detail
}

// ShortHostname is os.Hostname() with any domain suffix stripped.
func ShortHostname() string {
	name, _ := os.Hostname()
	if i := strings.IndexByte(name, '.'); i >= 0 {
		name = name[:i]
	}
	return name
}

// IsLocal reports whether status is read on this host rather than over ssh:
// this host is the engine, no engine host is set, or it names this host.
func IsLocal(cfg *config.Config, self string) bool {
	h := cfg.Engine.Host
	if i := strings.IndexByte(h, '@'); i >= 0 { // user@host
		h = h[i+1:]
	}
	if i := strings.IndexByte(h, '.'); i >= 0 {
		h = h[:i]
	}
	return cfg.IsEngine() || h == "" || h == self
}

// Get returns the engine's status: locally when IsLocal, else over ssh.
func Get(ctx context.Context, cfg *config.Config) (*Status, error) {
	if IsLocal(cfg, ShortHostname()) {
		return ReadLocal(ctx, cfg)
	}
	return Remote(ctx, cfg.Engine.Host)
}

// ReadLocal opens this host's state DB and reads status from it.
func ReadLocal(ctx context.Context, cfg *config.Config) (*Status, error) {
	if err := os.MkdirAll(cfg.StateDir, 0o755); err != nil {
		return nil, err
	}
	q, err := queue.Open(cfg.DBPath())
	if err != nil {
		return nil, err
	}
	defer q.Close()
	if _, err := ledger.New(q.DB(), nil); err != nil { // ensures the runs table
		return nil, err
	}
	return Local(ctx, cfg, q.DB(), time.Now())
}

// Local builds the status from a state DB (jobs + runs tables) and the vault.
func Local(ctx context.Context, cfg *config.Config, db *sql.DB, now time.Time) (*Status, error) {
	st := &Status{Host: ShortHostname(), Role: cfg.Role(), Now: now, Running: []Running{}, Last: map[string]Last{}}
	rows, err := db.QueryContext(ctx, `SELECT routine, updated FROM jobs WHERE state=? AND updated>=? ORDER BY updated`,
		queue.Running, now.Add(-staleRunning).Unix())
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var r string
		var since int64
		if err := rows.Scan(&r, &since); err != nil {
			rows.Close()
			return nil, err
		}
		st.Running = append(st.Running, Running{Routine: r, Since: time.Unix(since, 0)})
	}
	rows.Close()
	rows, err = db.QueryContext(ctx, `SELECT routine, started, ok, duration_ms FROM runs WHERE id IN (SELECT MAX(id) FROM runs GROUP BY routine)`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var r string
		var started, ms int64
		var ok int
		if err := rows.Scan(&r, &started, &ok, &ms); err != nil {
			return nil, err
		}
		st.Last[r] = Last{At: time.Unix(started, 0), OK: ok == 1, DurationS: float64(ms) / 1000}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	st.Vault = VaultState(ctx, cfg.Vault)
	return st, nil
}

// VaultState reads head, dirty count and last commit time of a git checkout.
// Each git call has its own timeout; a failure leaves that field empty.
func VaultState(ctx context.Context, dir string) Vault {
	var v Vault
	if dir == "" {
		return v
	}
	v.Head = git(ctx, dir, "rev-parse", "--short", "HEAD")
	if out, ok := gitRaw(ctx, dir, "status", "--porcelain"); ok {
		for _, l := range strings.Split(out, "\n") {
			if strings.TrimSpace(l) != "" {
				v.Dirty++
			}
		}
	}
	v.LastCommitAt = git(ctx, dir, "log", "-1", "--format=%cI")
	return v
}

func git(ctx context.Context, dir string, args ...string) string {
	out, _ := gitRaw(ctx, dir, args...)
	return strings.TrimSpace(out)
}

func gitRaw(ctx context.Context, dir string, args ...string) (string, bool) {
	ctx, cancel := context.WithTimeout(ctx, gitTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...).Output()
	if err != nil {
		return "", false
	}
	return string(out), true
}

// Remote runs `qilla engine status --json` on host over ssh (BatchMode: never
// prompts) and decodes it. Any failure is an *UnreachableError.
func Remote(ctx context.Context, host string) (*Status, error) {
	ctx, cancel := context.WithTimeout(ctx, sshTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "ssh", "-o", "BatchMode=yes", "-o", "ConnectTimeout=5", host, "qilla", "engine", "status", "--json")
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		detail := firstLine(stderr.String())
		if detail == "" {
			detail = err.Error()
		}
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			detail = "timed out"
		}
		return nil, &UnreachableError{Host: host, Detail: detail}
	}
	var st Status
	if err := json.Unmarshal(stdout.Bytes(), &st); err != nil {
		return nil, &UnreachableError{Host: host, Detail: "bad status JSON: " + err.Error()}
	}
	return &st, nil
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	return strings.TrimSpace(s)
}

// RecentWindow is how recently an ok run on the engine makes an agent's
// `qilla run` stop: routine schedules are systemd OnCalendar strings, which
// qilla does not parse, so one fixed window covers them (the densest
// scheduled routines run a few times a day).
const RecentWindow = 6 * time.Hour

// Decision is what `qilla run` does after asking the engine.
type Decision struct {
	Stop bool   // print Msg on stdout and exit 0 without running
	Msg  string // the stop line
	Warn string // one stderr line, then run (engine unreachable)
}

// Decide is the agent pre-check. force skips both stops (running and ran
// recently); an unreachable engine never blocks. manual routines (schedule
// "manual") have no interval, so only the running stop applies to them.
func Decide(routine string, st *Status, err error, force, manual bool, now time.Time) Decision {
	if err != nil || st == nil {
		msg := "engine status unavailable"
		if err != nil {
			msg = err.Error()
		}
		return Decision{Warn: msg + "; running here"}
	}
	if force {
		return Decision{}
	}
	for _, r := range st.Running {
		if r.Routine == routine {
			return Decision{Stop: true, Msg: fmt.Sprintf("%s running on %s since %s", routine, st.Host, clock(r.Since, now))}
		}
	}
	if l, ok := st.Last[routine]; ok && l.OK && !manual && now.Sub(l.At) < RecentWindow {
		return Decision{Stop: true, Msg: fmt.Sprintf("%s ran on %s at %s; --force to run anyway", routine, st.Host, clock(l.At, now))}
	}
	return Decision{}
}

// clock renders a time as HH:MM when it is today, else YYYY-MM-DD HH:MM.
func clock(t, now time.Time) string {
	t = t.In(now.Location())
	if t.Format("2006-01-02") == now.Format("2006-01-02") {
		return t.Format("15:04")
	}
	return t.Format("2006-01-02 15:04")
}

// Parity compares the engine's vault with this checkout. inSync is true when
// heads match and neither side is dirty; line is "vault in sync (<head>)" or
// the one-line warning.
func Parity(engineHost string, eng, here Vault) (line string, inSync bool) {
	if sameHead(eng.Head, here.Head) && eng.Dirty == 0 && here.Dirty == 0 {
		return "vault in sync (" + eng.Head + ")", true
	}
	return fmt.Sprintf("vault out of sync: %s %s dirty %d · here %s dirty %d",
		engineHost, orUnknown(eng.Head), eng.Dirty, orUnknown(here.Head), here.Dirty), false
}

func orUnknown(s string) string {
	if s == "" {
		return "?"
	}
	return s
}

// sameHead compares two abbreviated hashes, which git may abbreviate to
// different lengths on different clones.
func sameHead(a, b string) bool {
	if a == "" || b == "" {
		return false
	}
	return strings.HasPrefix(a, b) || strings.HasPrefix(b, a)
}
