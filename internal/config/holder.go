package config

import (
	"os"
	"sync"
	"time"
)

// Holder is the live config: one copy shared by the supervisor and the worker
// so a reload is visible to both. The supervisor is long-lived (socket
// activated, it only exits when idle), so an edit to qilla.toml would
// otherwise stay invisible until a restart — a routine added while it runs
// used to fail every job with `routine "x" not in config`.
//
// Get returns the pointer to an immutable snapshot: callers that must not see
// a mid-flight change (a running job) read it once and keep it.
type Holder struct {
	mu   sync.RWMutex
	cfg  *Config
	mtim time.Time // stat of the file as of the last reload attempt
	size int64
	err  string // last reload failure, to log it once rather than every check
}

// NewHolder wraps an already-loaded config. The stat baseline is taken from
// the file it came from, so the first check after startup only fires on a real
// edit.
func NewHolder(c *Config) *Holder {
	h := &Holder{cfg: c}
	if fi, err := os.Stat(c.Path); err == nil {
		h.mtim, h.size = fi.ModTime(), fi.Size()
	}
	return h
}

// Get is the current config.
func (h *Holder) Get() *Config {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.cfg
}

// Set swaps the config in (tests, and any caller that loaded it itself).
func (h *Holder) Set(c *Config) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.cfg = c
}

// Reload re-reads the file when its mtime or size changed since the last
// attempt (size too: two writes within the same second are otherwise
// indistinguishable). A failure keeps the previous good config — a broken
// edit must never take the supervisor down or lose queued work — and is
// logged once, not on every check. Returns true when the config changed.
func (h *Holder) Reload(logf func(string, ...any)) bool {
	return h.reload(logf, false)
}

// Force reloads whatever the stat says, for the explicit SIGHUP trigger.
func (h *Holder) Force(logf func(string, ...any)) bool {
	return h.reload(logf, true)
}

func (h *Holder) reload(logf func(string, ...any), force bool) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	path := h.cfg.Path
	if path == "" {
		return false // a config built in code, not read from a file: nothing to watch
	}
	fi, err := os.Stat(path)
	if err != nil {
		h.logOnce(logf, err.Error())
		return false
	}
	if !force && fi.ModTime().Equal(h.mtim) && fi.Size() == h.size {
		return false
	}
	h.mtim, h.size = fi.ModTime(), fi.Size()
	c, err := Load(path)
	if err != nil {
		h.logOnce(logf, err.Error())
		return false
	}
	h.cfg, h.err = c, ""
	if logf != nil {
		logf("qilla: config reloaded (%s)", path)
	}
	return true
}

// logOnce reports a reload failure only when it is new: a config left broken
// would otherwise fill the journal on every check.
func (h *Holder) logOnce(logf func(string, ...any), msg string) {
	if h.err == msg {
		return
	}
	h.err = msg
	if logf != nil {
		logf("qilla: config reload failed, keeping previous: %s", msg)
	}
}
