// Package procs reads live process state from /proc. The kernel is the source
// of truth for "which Claude sessions are running": no registry file, so
// nothing can go stale.
package procs

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Root is the procfs mount point. Tests point it at a fake tree.
var Root = "/proc"

// Proc is one process as seen from /proc.
type Proc struct {
	PID     int
	Comm    string
	Cmdline []string
	CWD     string
	Started time.Time
}

// pids lists the numeric entries under Root, ascending.
func pids() []int {
	entries, err := os.ReadDir(Root)
	if err != nil {
		return nil
	}
	var out []int
	for _, e := range entries {
		if n, err := strconv.Atoi(e.Name()); err == nil {
			out = append(out, n)
		}
	}
	return out
}

func read(pid int, name string) string {
	b, err := os.ReadFile(filepath.Join(Root, strconv.Itoa(pid), name))
	if err != nil {
		return ""
	}
	return string(b)
}

func args(pid int) []string {
	raw := strings.TrimRight(read(pid, "cmdline"), "\x00")
	if raw == "" {
		return nil
	}
	return strings.Split(raw, "\x00")
}

// IsInteractiveClaude reports whether a process with this comm and cmdline is
// an interactive Claude Code session: the binary is `claude` (by comm or by the
// base name of argv[0]) and it is not a -p/--print one-shot.
func IsInteractiveClaude(comm string, argv []string) bool {
	name := strings.TrimSpace(comm)
	if name != "claude" {
		name = ""
		if len(argv) > 0 {
			name = filepath.Base(argv[0])
		}
		if name != "claude" {
			return false
		}
	}
	for _, a := range argv {
		if a == "-p" || a == "--print" {
			return false
		}
	}
	return true
}

// PPid reads the parent pid from /proc/<pid>/status.
func PPid(pid int) int {
	for _, line := range strings.Split(read(pid, "status"), "\n") {
		if rest, ok := strings.CutPrefix(line, "PPid:"); ok {
			if n, err := strconv.Atoi(strings.TrimSpace(rest)); err == nil {
				return n
			}
		}
	}
	return 0
}

// comm returns the process name without its trailing newline.
func comm(pid int) string { return strings.TrimSpace(read(pid, "comm")) }

// AncestorClaude walks up the process tree from pid and returns the first
// ancestor (or pid itself) that is a claude process, or 0 if there is none.
func AncestorClaude(pid int) int {
	for p := pid; p > 1; {
		if comm(p) == "claude" {
			return p
		}
		next := PPid(p)
		if next <= 0 || next == p {
			break
		}
		p = next
	}
	return 0
}

// bootTime reads btime from /proc/stat (seconds since the epoch).
func bootTime() time.Time {
	b, err := os.ReadFile(filepath.Join(Root, "stat"))
	if err != nil {
		return time.Time{}
	}
	for _, line := range strings.Split(string(b), "\n") {
		if rest, ok := strings.CutPrefix(line, "btime "); ok {
			if n, err := strconv.ParseInt(strings.TrimSpace(rest), 10, 64); err == nil {
				return time.Unix(n, 0)
			}
		}
	}
	return time.Time{}
}

// startTime returns when the process started: btime + starttime from
// /proc/<pid>/stat, falling back to the mtime of /proc/<pid>.
func startTime(pid int, boot time.Time) time.Time {
	stat := read(pid, "stat")
	// field 22 (1-based) is starttime, in clock ticks since boot; the comm
	// field may contain spaces, so parse after the last ')'.
	if i := strings.LastIndex(stat, ")"); i >= 0 && !boot.IsZero() {
		fields := strings.Fields(stat[i+1:])
		// fields[0] is state, so starttime is index 19 after it.
		if len(fields) > 19 {
			if ticks, err := strconv.ParseInt(fields[19], 10, 64); err == nil {
				return boot.Add(time.Duration(ticks) * time.Second / clockTick)
			}
		}
	}
	if fi, err := os.Stat(filepath.Join(Root, strconv.Itoa(pid))); err == nil {
		return fi.ModTime()
	}
	return time.Time{}
}

// clockTick is USER_HZ, 100 on every Linux architecture Go supports.
const clockTick = 100

// ClaudeIn returns the interactive claude processes whose cwd is inside root
// (a vault directory), ascending by pid.
func ClaudeIn(dir string) []Proc {
	boot := bootTime()
	var out []Proc
	for _, pid := range pids() {
		argv := args(pid)
		c := comm(pid)
		if !IsInteractiveClaude(c, argv) {
			continue
		}
		cwd, err := os.Readlink(filepath.Join(Root, strconv.Itoa(pid), "cwd"))
		if err != nil || !strings.HasPrefix(cwd, dir) {
			continue
		}
		out = append(out, Proc{PID: pid, Comm: c, Cmdline: argv, CWD: cwd, Started: startTime(pid, boot)})
	}
	return out
}

// Attached reports whether an interactive claude holds this session right now
// (qilla chat in a terminal, or Remote Control on it): a process whose cmdline
// has "--resume <sid>" and is not a -p/--print one-shot.
func Attached(sid string) bool {
	if sid == "" {
		return false
	}
	for _, pid := range pids() {
		argv := args(pid)
		if !IsInteractiveClaude(comm(pid), argv) {
			continue
		}
		for i, a := range argv {
			if a == "--resume" && i+1 < len(argv) && argv[i+1] == sid {
				return true
			}
		}
	}
	return false
}
