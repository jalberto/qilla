package serve

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jalberto/qilla/internal/config"
	"github.com/jalberto/qilla/internal/ledger"
	"github.com/jalberto/qilla/internal/queue"
	"github.com/jalberto/qilla/internal/worker"
)

type fakeW struct{ n atomic.Int32 }

func (f *fakeW) Run(_ context.Context, j *queue.Job) error { f.n.Add(1); return nil }

func newServer(t *testing.T) (*Server, Listeners, *fakeW) {
	t.Helper()
	dir := t.TempDir()
	cfg := &config.Config{StateDir: dir, Web: config.Web{Listen: "127.0.0.1:0", IdleExit: "300ms"},
		Agents: map[string]config.Agent{"chief": {}}, Routines: map[string]config.Routine{"brief": {Kind: "ai-fresh", Agent: "chief", MustRun: true}}}
	q, err := queue.Open(filepath.Join(dir, "q.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { q.Close() })
	w, _ := worker.New(cfg, q.DB())
	l, _ := ledger.New(q.DB(), nil)
	s := New(cfg, q, w, l)
	s.Log = func(string, ...any) {}
	ls, err := Listen(cfg, filepath.Join(dir, "p.sock"))
	if err != nil {
		t.Fatal(err)
	}
	return s, ls, &fakeW{}
}

func TestIdleExitWhenQueueEmpty(t *testing.T) {
	s, ls, _ := newServer(t)
	done := make(chan error, 1)
	go func() { done <- s.Run(context.Background(), ls) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("serve must exit when idle")
	}
}

func TestPokeDrainsAndAPIWorks(t *testing.T) {
	s, ls, fw := newServer(t)
	s.IdleExit = 5 * time.Second
	q := s.Q
	q.Enqueue(context.Background(), queue.Job{Routine: "brief", Agent: "chief"})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.runWith(ctx, ls, fw)
	time.Sleep(200 * time.Millisecond)

	base := "http://" + ls.HTTP.Addr().String()
	res, err := http.Get(base + "/api/status")
	if err != nil {
		t.Fatal(err)
	}
	var st map[string]any
	json.NewDecoder(res.Body).Decode(&st)
	if _, ok := st["must_run"].(map[string]any)["brief"]; !ok {
		t.Fatalf("status must list must_run routines: %v", st)
	}
	res, err = http.Post(base+"/api/ask", "application/json", strings.NewReader(`{"text":"hola"}`))
	if err != nil || res.StatusCode != 200 {
		t.Fatalf("ask: %v %v", err, res.StatusCode)
	}
	// poke over the unix socket
	c, err := net.Dial("unix", ls.Poke.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	c.Write([]byte("poke\n"))
	c.Close()
	time.Sleep(300 * time.Millisecond)
	js, _ := q.List(context.Background(), 10)
	for _, j := range js {
		if j.State != queue.Done {
			t.Fatalf("all jobs drained after poke/ask: %+v", js)
		}
	}
	if fw.n.Load() < 2 {
		t.Fatalf("fake runner ran %d", fw.n.Load())
	}
}

func TestNonLoopbackWithoutPasswordRefused(t *testing.T) {
	cfg := &config.Config{StateDir: t.TempDir(), Web: config.Web{Listen: "0.0.0.0:0"}}
	if _, err := Listen(cfg, filepath.Join(t.TempDir(), "p.sock")); err == nil || !strings.Contains(err.Error(), "passwd") {
		t.Fatalf("must refuse: %v", err)
	}
	cfg.Web.PasswordHash = "$2a$10$x"
	ls, err := Listen(cfg, filepath.Join(t.TempDir(), "p.sock"))
	if err != nil {
		t.Fatal(err)
	}
	ls.HTTP.Close()
	ls.Poke.Close()
}

func TestSecondServeDoesNotStealSocket(t *testing.T) {
	cfg := &config.Config{StateDir: t.TempDir(), Web: config.Web{Listen: "127.0.0.1:0"}}
	sock := filepath.Join(t.TempDir(), "p.sock")
	first, err := Listen(cfg, sock)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Poke.Close()
	defer first.HTTP.Close()
	go func() {
		for {
			c, err := first.Poke.Accept()
			if err != nil {
				return
			}
			c.Close()
		}
	}()
	if _, err := Listen(cfg, sock); err == nil {
		t.Fatal("second Listen must refuse while the first answers")
	}
	if _, err := net.Dial("unix", sock); err != nil {
		t.Fatalf("first socket must still be live: %v", err)
	}
}

func TestArtifactServedWithCSP(t *testing.T) {
	s, ls, _ := newServer(t)
	s.IdleExit = 5 * time.Second
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.Run(ctx, ls)
	time.Sleep(200 * time.Millisecond)
	st := s.artStore()
	p := filepath.Join(t.TempDir(), "a.html")
	os.WriteFile(p, []byte("<!doctype html><html><body>hi</body></html>"), 0o644)
	m, err := st.Add(p, "A", "brief", 1, 0)
	if err != nil {
		t.Fatal(err)
	}
	res, err := http.Get("http://" + ls.HTTP.Addr().String() + "/artifacts/" + m.ID)
	if err != nil || res.StatusCode != 200 {
		t.Fatalf("%v %v", err, res.StatusCode)
	}
	if csp := res.Header.Get("Content-Security-Policy"); !strings.Contains(csp, "default-src 'none'") || !strings.Contains(csp, "connect-src 'none'") {
		t.Fatalf("csp: %q", csp)
	}
	if res, _ := http.Get("http://" + ls.HTTP.Addr().String() + "/artifacts/../etc/passwd"); res.StatusCode == 200 {
		t.Fatal("path characters must not resolve")
	}
}
