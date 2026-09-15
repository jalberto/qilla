package doctor

// The stack watch: the systemd
// user units qilla depends on, the artifacts that must stay fresh, and the
// queue's own failed or parked jobs.

import (
	"context"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/jalberto/qilla/internal/config"
	"github.com/jalberto/qilla/internal/queue"
)

// JobProblem is a queue job the watch reports: failed today, or parked
// (queued/running) so the work it owns has silently stopped.
type JobProblem struct {
	ID      int64  `json:"id"`
	Routine string `json:"routine"`
	State   string `json:"state"`
	Reason  string `json:"reason"`
	Parked  bool   `json:"parked"`
}

// Health returns the [health] rows on their own — one per watched unit, one
// per freshness rule, plus the queue row. `qilla doctor` appends them to the
// full report; the statusline and the session-start hook ask for these alone,
// which is why they are not folded into Run's body.
func Health(cfg *config.Config, env Env) []Check {
	var out []Check
	now := time.Now()
	if env.Now != nil {
		now = env.Now()
	}
	if env.UnitState != nil {
		for _, u := range cfg.Health.WatchedServices {
			u = strings.TrimSpace(u)
			if u == "" {
				continue
			}
			state := env.UnitState(u)
			switch state {
			case unitFailed:
				out = append(out, Check{"unit " + u, false, true, "failed — `qilla doctor fix` restarts it"})
			case unitNotFound:
				out = append(out, Check{"unit " + u, false, true, "not-found — unit missing, cannot restart"})
			case unitActive:
				out = append(out, Check{"unit " + u, true, true, "active"})
			case unitInactive:
				// oneshot services and timers idle between runs: not failed, not a problem.
				out = append(out, Check{"unit " + u, true, true, "inactive (not failed)"})
			default:
				out = append(out, Check{"unit " + u, true, true, state})
			}
		}
	}
	for _, entry := range cfg.Health.Freshness {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		r, err := parseFreshRule(entry)
		if err != nil {
			out = append(out, Check{"fresh " + entry, false, false, err.Error()})
			continue
		}
		ok, info := r.eval(cfg, env, now)
		out = append(out, Check{"fresh " + r.Label, ok, false, info})
	}
	if env.Jobs != nil {
		probs, err := env.Jobs(cfg)
		switch {
		case err != nil:
			out = append(out, Check{"jobs", false, false, "queue unreadable: " + firstLine(err.Error())})
		case len(probs) == 0:
			out = append(out, Check{"jobs", true, false, "no failed or parked jobs"})
		default:
			var failed, parked []string
			for _, p := range probs {
				line := fmt.Sprintf("#%d %s", p.ID, p.Routine)
				if p.Reason != "" {
					line += " — " + p.Reason
				}
				if p.Parked {
					parked = append(parked, line)
				} else {
					failed = append(failed, line)
				}
			}
			var parts []string
			if len(failed) > 0 {
				parts = append(parts, fmt.Sprintf("%d failed: %s", len(failed), strings.Join(failed, "; ")))
			}
			if len(parked) > 0 {
				parts = append(parts, fmt.Sprintf("%d parked: %s", len(parked), strings.Join(parked, "; ")))
			}
			out = append(out, Check{"jobs", false, false, strings.Join(parts, " · ")})
		}
	}
	return out
}

// systemd states the watch distinguishes.
const (
	unitActive   = "active"
	unitInactive = "inactive"
	unitFailed   = "failed"
	unitNotFound = "not-found"
)

// freshRule is one `watched freshness` entry.
type freshRule struct {
	Label  string        // the key as written: a vault-relative path, or git@<remote>
	Path   string        // vault-relative path (file rules)
	Remote string        // git rules
	By     string        // daily rules: HH:MM the file must have been touched by
	Max    time.Duration // age rules (Nh/Nd) and git rules
}

// parseFreshRule reads "path=daily@HH:MM", "path=Nh", "path=Nd" or "git@REMOTE=Nd".
func parseFreshRule(entry string) (freshRule, error) {
	key, spec, ok := strings.Cut(entry, "=")
	key, spec = strings.TrimSpace(key), strings.TrimSpace(spec)
	if !ok || key == "" || spec == "" {
		return freshRule{}, fmt.Errorf("bad freshness entry %q: want path=daily@HH:MM, path=Nh, path=Nd or git@remote=Nd", entry)
	}
	r := freshRule{Label: key}
	if remote, isGit := strings.CutPrefix(key, "git@"); isGit {
		d, err := parseAge(spec)
		if err != nil || remote == "" {
			return freshRule{}, fmt.Errorf("bad git freshness entry %q: want git@remote=Nd", entry)
		}
		r.Remote, r.Max = remote, d
		return r, nil
	}
	r.Path = key
	if by, isDaily := strings.CutPrefix(spec, "daily@"); isDaily {
		if _, err := time.Parse("15:04", by); err != nil {
			return freshRule{}, fmt.Errorf("bad freshness entry %q: daily@HH:MM expected", entry)
		}
		r.By = by
		return r, nil
	}
	d, err := parseAge(spec)
	if err != nil {
		return freshRule{}, fmt.Errorf("bad freshness entry %q: %v", entry, err)
	}
	r.Max = d
	return r, nil
}

// parseAge reads "3h" or "7d".
func parseAge(spec string) (time.Duration, error) {
	if len(spec) < 2 {
		return 0, fmt.Errorf("age %q: want Nh or Nd", spec)
	}
	n, err := strconv.Atoi(spec[:len(spec)-1])
	if err != nil || n <= 0 {
		return 0, fmt.Errorf("age %q: want Nh or Nd", spec)
	}
	switch spec[len(spec)-1] {
	case 'h':
		return time.Duration(n) * time.Hour, nil
	case 'd':
		return time.Duration(n) * 24 * time.Hour, nil
	}
	return 0, fmt.Errorf("age %q: want Nh or Nd", spec)
}

// eval reports whether the rule's artifact is fresh at now, with the line the
// report shows either way.
func (r freshRule) eval(cfg *config.Config, env Env, now time.Time) (bool, string) {
	if r.Remote != "" {
		return r.evalGit(cfg, env, now)
	}
	fi, err := env.Stat(cfg.VaultPath(r.Path))
	if err != nil {
		return false, "missing"
	}
	mt := fi.ModTime()
	stamp := mt.Format("2006-01-02 15:04")
	if r.By != "" {
		if mt.Format("2006-01-02") == now.Format("2006-01-02") {
			return true, "touched today " + mt.Format("15:04")
		}
		if now.Format("15:04") > r.By {
			return false, fmt.Sprintf("last touched %s, expected today by %s", stamp, r.By)
		}
		return true, fmt.Sprintf("last touched %s, due today by %s", stamp, r.By)
	}
	if now.Sub(mt) > r.Max {
		return false, fmt.Sprintf("mtime %s older than %s", stamp, age(r.Max))
	}
	return true, fmt.Sprintf("mtime %s (max %s)", stamp, age(r.Max))
}

// evalGit flags commits sitting unpushed to a remote for longer than the rule allows.
func (r freshRule) evalGit(cfg *config.Config, env Env, now time.Time) (bool, string) {
	if env.Run == nil {
		return true, "not probed"
	}
	rng := r.Remote + "/main..HEAD"
	out, err := env.Run("git", "-C", cfg.Vault, "log", rng, "--format=%ct")
	if err != nil {
		return true, "not probed: " + firstLine(strings.TrimSpace(err.Error()))
	}
	lines := strings.Fields(strings.TrimSpace(out))
	if len(lines) == 0 {
		return true, "nothing unpushed"
	}
	oldest, err := strconv.ParseInt(lines[len(lines)-1], 10, 64)
	if err != nil {
		return true, "not probed"
	}
	d := now.Sub(time.Unix(oldest, 0))
	if d <= r.Max {
		return true, fmt.Sprintf("%d commits unpushed, oldest %dd old (max %s)", len(lines), int(d.Hours()/24), age(r.Max))
	}
	return false, fmt.Sprintf("%d commits unpushed, oldest %dd old (max %s)", len(lines), int(d.Hours()/24), age(r.Max))
}

// age renders a duration the way the rules write it (7d, 6h).
func age(d time.Duration) string {
	if d%(24*time.Hour) == 0 {
		return strconv.Itoa(int(d.Hours()/24)) + "d"
	}
	return strconv.Itoa(int(d.Hours())) + "h"
}

// Fix restarts every failed watched unit, re-checks it and returns one report
// line per unit; ok is false when anything is still failed.
func Fix(cfg *config.Config, env Env) (lines []string, ok bool) {
	if env.UnitState == nil {
		return []string{"units not probed"}, true
	}
	ok = true
	var broken []string
	for _, u := range cfg.Health.WatchedServices {
		u = strings.TrimSpace(u)
		if u == "" {
			continue
		}
		if s := env.UnitState(u); s == unitFailed || s == unitNotFound {
			broken = append(broken, u)
		}
	}
	if len(broken) == 0 {
		return []string{"all green"}, true
	}
	for _, u := range broken {
		if env.UnitState(u) == unitNotFound {
			lines = append(lines, u+": not-found — unit missing, cannot restart")
			ok = false
			continue
		}
		if env.Restart == nil {
			lines = append(lines, u+": failed — restart not available")
			ok = false
			continue
		}
		if err := env.Restart(u); err != nil {
			lines = append(lines, u+": restart failed — "+firstLine(err.Error()))
			ok = false
			continue
		}
		if env.Sleep != nil {
			env.Sleep(2 * time.Second) // systemd needs a moment before is-failed is meaningful
		}
		if env.UnitState(u) != unitFailed {
			lines = append(lines, u+": recovered by restart")
			continue
		}
		msg := u + ": still failed after restart"
		if env.Run != nil {
			if out, err := env.Run("systemctl", "--user", "show", "-p", "ExecMainStatus", "--value", u); err == nil {
				if s := strings.TrimSpace(out); s != "" {
					msg += " — exit " + s
				}
			}
		}
		lines = append(lines, msg)
		ok = false
	}
	return lines, ok
}

// unitState asks systemd for a watched unit's state. `is-failed` prints
// "failed" for a failed unit and exits 4 when the unit does not exist at all —
// a renamed unit must surface, not vanish from the watch.
func unitState(unit string) string {
	out, err := exec.Command("systemctl", "--user", "is-failed", unit).Output()
	state := strings.TrimSpace(string(out))
	if state == unitFailed {
		return unitFailed
	}
	if ee, isExit := err.(*exec.ExitError); isExit && ee.ExitCode() == 4 {
		return unitNotFound
	}
	if state == "" {
		return unitInactive
	}
	return state
}

// queueProblems reads the queue for jobs failed today or parked, mirroring how
// `qilla status` classifies them.
func queueProblems(cfg *config.Config) ([]JobProblem, error) {
	q, err := queue.Open(cfg.DBPath())
	if err != nil {
		return nil, err
	}
	defer q.Close()
	jobs, err := q.List(context.Background(), 200)
	if err != nil {
		return nil, err
	}
	day := time.Now().Format("2006-01-02")
	var out []JobProblem
	for _, j := range jobs {
		switch {
		case j.State == queue.Queued && strings.HasPrefix(j.Error, "deferred"):
			// waiting on the usage window on purpose
		case j.State == queue.Queued || j.State == queue.Running:
			out = append(out, JobProblem{j.ID, j.Routine, j.State, j.State, true})
		case j.State == queue.Suspended && strings.Contains(j.Error, "dropped"):
			// dropped on purpose from the page: not a failure to chase
		case (j.State == queue.Failed || j.State == queue.Suspended) && j.Updated.Format("2006-01-02") == day:
			out = append(out, JobProblem{j.ID, j.Routine, j.State, firstLine(j.Error), false})
		}
	}
	return out, nil
}
