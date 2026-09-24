// Package serve is the supervisor: socket-activated, drains the queue one
// job at a time under a file lock, serves the API + SSE + page, exits when
// idle. With nothing to do there is no qilla process.
package serve

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/jalberto/qilla/internal/auth"
	"github.com/jalberto/qilla/internal/config"
	"github.com/jalberto/qilla/internal/ledger"
	"github.com/jalberto/qilla/internal/models"
	"github.com/jalberto/qilla/internal/queue"
	"github.com/jalberto/qilla/internal/worker"
)

//go:embed ui/index.html ui/icon.svg ui/manifest.json
var ui embed.FS

// Server wires queue, worker and ledger behind HTTP and a poke socket.
type Server struct {
	// Hold is the live config, shared with the worker: qilla.toml is re-read
	// before each drain, so nothing here may cache a copy of it.
	Hold     *config.Holder
	Q        *queue.Store
	W        *worker.Worker
	L        *ledger.Store
	IdleExit time.Duration
	Now      func() time.Time
	Log      func(string, ...any)
	Auth     *auth.Guard

	hub    *hub
	wake   chan struct{}
	act    chan struct{} // any activity resets the idle timer
	stop   chan struct{} // SIGTERM: finish the running job, then exit
	once   sync.Once
	runner queue.Runner // defaults to W
}

// Listeners are the two sockets: the poke socket (unix) and HTTP (tcp).
type Listeners struct {
	Poke net.Listener
	HTTP net.Listener
}

// Cfg is the config as of right now.
func (s *Server) Cfg() *config.Config { return s.Hold.Get() }

// New builds a server with defaults.
func New(hold *config.Holder, q *queue.Store, w *worker.Worker, l *ledger.Store) *Server {
	cfg := hold.Get()
	idle, err := time.ParseDuration(cfg.Web.IdleExit)
	if err != nil || idle <= 0 {
		idle = 10 * time.Minute
	}
	s := &Server{Hold: hold, Q: q, W: w, L: l, IdleExit: idle, Now: time.Now,
		Log: func(f string, a ...any) { fmt.Fprintf(os.Stderr, f+"\n", a...) },
		hub: newHub(), wake: make(chan struct{}, 1), act: make(chan struct{}, 1), stop: make(chan struct{})}
	s.Auth, err = auth.NewPersistent(cfg.Web.PasswordHash, !auth.IsLoopback(cfg.Web.Listen), filepath.Join(cfg.StateDir, "web-session.key"))
	if err != nil {
		s.Auth = auth.New(cfg.Web.PasswordHash, !auth.IsLoopback(cfg.Web.Listen)) // degrade rather than fail serve
	}
	prev := w.OnRun
	w.OnRun = func(r worker.Record) {
		if prev != nil {
			prev(r)
		}
		s.hub.publish("run", runMsg(r), r)
	}
	return s
}

// Listen returns systemd-activated listeners when LISTEN_FDS is set (unix
// first, tcp second in the .socket unit), else opens both itself.
func Listen(cfg *config.Config, sock string) (Listeners, error) {
	var ls Listeners
	if n, _ := strconv.Atoi(os.Getenv("LISTEN_FDS")); n > 0 && os.Getenv("LISTEN_PID") == strconv.Itoa(os.Getpid()) {
		for fd := 3; fd < 3+n; fd++ {
			f := os.NewFile(uintptr(fd), "listen-"+strconv.Itoa(fd))
			l, err := net.FileListener(f)
			if err != nil {
				return ls, fmt.Errorf("activation fd %d: %w", fd, err)
			}
			if l.Addr().Network() == "unix" {
				ls.Poke = l
			} else {
				ls.HTTP = l
			}
		}
		if ls.Poke == nil || ls.HTTP == nil {
			return ls, errors.New("socket activation needs one unix and one tcp ListenStream")
		}
		return ls, nil
	}
	if cfg.Web.PasswordHash == "" && !auth.IsLoopback(cfg.Web.Listen) {
		return ls, fmt.Errorf("web.listen %s is not loopback and no password is set — run `qilla passwd` first", cfg.Web.Listen)
	}
	if c, err := net.DialTimeout("unix", sock, 300*time.Millisecond); err == nil {
		c.Close()
		return ls, fmt.Errorf("another qilla serve is listening on %s", sock)
	}
	os.Remove(sock) // stale socket from a dead process
	var err error
	if ls.Poke, err = net.Listen("unix", sock); err != nil {
		return ls, err
	}
	if ls.HTTP, err = net.Listen("tcp", cfg.Web.Listen); err != nil {
		ls.Poke.Close()
		return ls, err
	}
	return ls, nil
}

// Run serves until idle, SIGTERM (via Stop) or ctx cancel. Returns nil on a
// clean idle exit.
func (s *Server) Run(ctx context.Context, ls Listeners) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	lockPath := filepath.Join(s.Cfg().StateDir, "serve.lock")
	unlock, err := tryLock(lockPath)
	if err != nil {
		return fmt.Errorf("another qilla serve holds %s", lockPath)
	}
	defer unlock()

	go s.acceptPokes(ctx, ls.Poke)
	go s.tailTranscripts(ctx)
	srv := &http.Server{Handler: s.Auth.Wrap(s.routes()), ReadHeaderTimeout: 10 * time.Second}
	go srv.Serve(ls.HTTP)
	defer func() {
		// SSE streams would keep Shutdown waiting forever (the browser tab is open):
		// tell them to return, then give everything else a few seconds.
		s.hub.close()
		sctx, scancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer scancel()
		srv.Shutdown(sctx)
	}()

	s.hub.publish("info", "serve started", nil)
	s.kick()
	idle := time.NewTimer(s.IdleExit)
	tick := time.NewTicker(time.Minute)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-s.stop:
			s.hub.publish("info", "serve stopping", nil)
			return nil
		case <-s.wake:
			s.drain(ctx)
			resetTimer(idle, s.IdleExit)
		case <-tick.C: // due jobs whose timers poked while we were away
			s.drain(ctx)
		case <-s.act:
			resetTimer(idle, s.IdleExit)
		case <-idle.C:
			if n, _ := s.Q.Pending(ctx); n == 0 {
				s.hub.publish("info", "idle, exiting", nil)
				return nil
			}
			resetTimer(idle, s.IdleExit)
		}
	}
}

// Stop asks Run to finish the current job and return (SIGTERM handler).
func (s *Server) Stop() { s.once.Do(func() { close(s.stop) }) }

func (s *Server) kick() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

func (s *Server) touch() {
	select {
	case s.act <- struct{}{}:
	default:
	}
}

func (s *Server) drain(ctx context.Context) {
	// a qilla.toml edit is picked up here, right before the jobs run: a routine
	// added while the supervisor is up must not fail as "not in config".
	s.Hold.Reload(s.Log)
	gate := s.L.Gate(s.Cfg(), func(scope string, spent, cap float64) {
		s.hub.publish("warn", fmt.Sprintf("budget warn: %s at %.2f (warn %.2f)", scope, spent, cap), nil)
	})
	attempts := func(r string) int {
		if rt, ok := s.Cfg().Routines[r]; ok && rt.MaxAttempts > 0 {
			return rt.MaxAttempts
		}
		return 3
	}
	var r queue.Runner = s.W
	if s.runner != nil {
		r = s.runner
	}
	r = announcing{r, s}
	n, err := queue.Drain(ctx, s.Q, r, s.gateWithEvents(gate), attempts, s.Now)
	if err != nil && !errors.Is(err, context.Canceled) {
		s.Log("drain: %v", err)
		s.hub.publish("warn", "drain: "+err.Error(), nil)
	}
	if n > 0 {
		s.touch()
	}
}

func (s *Server) gateWithEvents(g queue.Gate) queue.Gate {
	return func(j *queue.Job, now time.Time) (bool, string, time.Time) {
		// a spent five-hour window would only burn attempts: park until it resets
		if u, at := models.FiveHour(s.Cfg().Models.UsageCache); u >= 98 && at.After(now) {
			why := fmt.Sprintf("usage: five-hour window at %d%%, resumes %s", u, at.Local().Format("15:04"))
			s.hub.publish("job", fmt.Sprintf("#%d %s waits: %s", j.ID, j.Routine, why), nil)
			return false, why, at.Add(2 * time.Minute)
		}
		ok, why, retry := g(j, now)
		if !ok {
			s.hub.publish("job", fmt.Sprintf("#%d %s waits: %s", j.ID, j.Routine, why), map[string]any{"id": j.ID, "state": "waiting", "reason": why, "retry": retry.Unix()})
		}
		return ok, why, retry
	}
}

func (s *Server) acceptPokes(ctx context.Context, l net.Listener) {
	for {
		c, err := l.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			continue
		}
		c.SetReadDeadline(time.Now().Add(time.Second))
		buf := make([]byte, 16)
		c.Read(buf)
		c.Close()
		s.kick()
	}
}

func tryLock(path string) (func(), error) {
	os.MkdirAll(filepath.Dir(path), 0o755)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		return nil, err
	}
	return func() { syscall.Flock(int(f.Fd()), syscall.LOCK_UN); f.Close() }, nil
}

func resetTimer(t *time.Timer, d time.Duration) {
	if !t.Stop() {
		select {
		case <-t.C:
		default:
		}
	}
	t.Reset(d)
}

func runMsg(r worker.Record) string {
	switch {
	case r.Skipped:
		return fmt.Sprintf("%s: unchanged, skipped", r.Routine)
	case r.Error != "":
		return fmt.Sprintf("%s: failed — %s", r.Routine, r.Error)
	case len(r.Artifacts) > 0:
		return fmt.Sprintf("%s: ok (%s) · %d artifact(s)", r.Routine, r.Duration.Round(time.Second), len(r.Artifacts))
	default:
		return fmt.Sprintf("%s: ok (%s)", r.Routine, r.Duration.Round(time.Second))
	}
}

// runWith is Run with a substitute runner (tests).
func (s *Server) runWith(ctx context.Context, ls Listeners, r queue.Runner) error {
	s.runner = r
	return s.Run(ctx, ls)
}

// announcing publishes a job-start event before running it.
type announcing struct {
	queue.Runner
	s *Server
}

func (a announcing) Run(ctx context.Context, j *queue.Job) error {
	a.s.hub.publish("job", fmt.Sprintf("#%d %s running", j.ID, j.Routine), map[string]any{"id": j.ID, "routine": j.Routine, "state": "running"})
	err := a.Runner.Run(ctx, j)
	state := "done"
	if err != nil {
		state = "failed"
	}
	a.s.hub.publish("job", fmt.Sprintf("#%d %s %s", j.ID, j.Routine, state), map[string]any{"id": j.ID, "routine": j.Routine, "state": state})
	return err
}
