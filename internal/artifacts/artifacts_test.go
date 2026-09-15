package artifacts

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

var now = time.Date(2026, 9, 12, 9, 0, 0, 0, time.UTC)

func store(t *testing.T) *Store {
	s, err := New(filepath.Join(t.TempDir(), "art"), 24*time.Hour, 1)
	if err != nil {
		t.Fatal(err)
	}
	s.Now = func() time.Time { return now }
	return s
}

func html(t *testing.T, body string) string {
	p := filepath.Join(t.TempDir(), "x.html")
	os.WriteFile(p, []byte(body), 0o644)
	return p
}

func TestArtifactAddListServePurge(t *testing.T) {
	s := store(t)
	m, err := s.Add(html(t, "<!doctype html><html><body><h1>chart</h1></body></html>"), "Chart", "brief", 7, 0)
	if err != nil || m.Title != "Chart" || m.Routine != "brief" || m.JobID != 7 {
		t.Fatalf("%v %+v", err, m)
	}
	if _, err := s.Add(html(t, "just text"), "", "", 0, 0); err == nil {
		t.Fatal("non-HTML must be refused")
	}
	l, _ := s.List()
	if len(l) != 1 || s.Path(m.ID) == "" {
		t.Fatalf("list/path: %+v %q", l, s.Path(m.ID))
	}
	if s.Path("../etc/passwd") != "" || s.Path(m.ID+".json") != "" {
		t.Fatal("ids with path characters must not resolve")
	}
	s.Now = func() time.Time { return now.Add(25 * time.Hour) }
	if s.Path(m.ID) != "" {
		t.Fatal("expired artifacts are not served")
	}
	if n := s.Purge(); n != 1 {
		t.Fatalf("purged %d", n)
	}
	if l, _ = s.List(); len(l) != 0 {
		t.Fatal("gone")
	}
}

func TestArtifactSizeCap(t *testing.T) {
	s := store(t)
	big := make([]byte, 2<<20)
	copy(big, []byte("<!doctype html>"))
	p := filepath.Join(t.TempDir(), "big.html")
	os.WriteFile(p, big, 0o644)
	if _, err := s.Add(p, "", "", 0, 0); err == nil {
		t.Fatal("over max_mb must be refused")
	}
}
