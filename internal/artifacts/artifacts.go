// Package artifacts stores self-contained HTML produced by runs and chats and
// serves it under qilla's page: charts, tables, diagrams — ephemeral, TTL'd,
// the vault keeps anything durable.
package artifacts

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Meta is the sidecar next to each HTML file.
type Meta struct {
	ID      string    `json:"id"`
	Title   string    `json:"title"`
	Routine string    `json:"routine,omitempty"`
	JobID   int64     `json:"job_id,omitempty"`
	Created time.Time `json:"created"`
	Expires time.Time `json:"expires"`
	Bytes   int64     `json:"bytes"`
}

// Store is a directory of <id>.html + <id>.json.
type Store struct {
	Dir   string
	TTL   time.Duration
	MaxMB int64
	Now   func() time.Time
}

// New prepares the directory.
func New(dir string, ttl time.Duration, maxMB int64) (*Store, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	if ttl <= 0 {
		ttl = 30 * 24 * time.Hour
	}
	if maxMB <= 0 {
		maxMB = 5
	}
	return &Store{Dir: dir, TTL: ttl, MaxMB: maxMB, Now: time.Now}, nil
}

// Add copies an HTML file into the store.
func (s *Store) Add(src, title, routine string, jobID int64, ttl time.Duration) (Meta, error) {
	b, err := os.ReadFile(src)
	if err != nil {
		return Meta{}, err
	}
	if int64(len(b)) > s.MaxMB<<20 {
		return Meta{}, fmt.Errorf("artifact %s is %d MB, max %d", filepath.Base(src), len(b)>>20, s.MaxMB)
	}
	if !strings.Contains(strings.ToLower(string(b[:min(len(b), 2048)])), "<html") && !strings.Contains(strings.ToLower(string(b[:min(len(b), 2048)])), "<!doctype") {
		return Meta{}, errors.New("not an HTML document (needs <!doctype html> or <html>)")
	}
	if ttl <= 0 {
		ttl = s.TTL
	}
	if title == "" {
		title = strings.TrimSuffix(filepath.Base(src), filepath.Ext(src))
	}
	rb := make([]byte, 6)
	rand.Read(rb)
	m := Meta{ID: s.Now().Format("20060102-150405") + "-" + hex.EncodeToString(rb), Title: title, Routine: routine, JobID: jobID,
		Created: s.Now(), Expires: s.Now().Add(ttl), Bytes: int64(len(b))}
	if err := os.WriteFile(filepath.Join(s.Dir, m.ID+".html"), b, 0o644); err != nil {
		return Meta{}, err
	}
	mb, _ := json.Marshal(m)
	return m, os.WriteFile(filepath.Join(s.Dir, m.ID+".json"), mb, 0o644)
}

// List returns live artifacts, newest first.
func (s *Store) List() ([]Meta, error) {
	es, err := os.ReadDir(s.Dir)
	if err != nil {
		return nil, err
	}
	var out []Meta
	now := s.Now()
	for _, e := range es {
		if !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(s.Dir, e.Name()))
		if err != nil {
			continue
		}
		var m Meta
		if json.Unmarshal(b, &m) == nil && m.Expires.After(now) {
			out = append(out, m)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Created.After(out[j].Created) })
	return out, nil
}

// Path returns the HTML path for a live id ("" when unknown or expired).
func (s *Store) Path(id string) string {
	if strings.ContainsAny(id, "/\\.") {
		return ""
	}
	b, err := os.ReadFile(filepath.Join(s.Dir, id+".json"))
	if err != nil {
		return ""
	}
	var m Meta
	if json.Unmarshal(b, &m) != nil || !m.Expires.After(s.Now()) {
		return ""
	}
	return filepath.Join(s.Dir, id+".html")
}

// Remove deletes one artifact.
func (s *Store) Remove(id string) error {
	if strings.ContainsAny(id, "/\\.") {
		return errors.New("bad id")
	}
	os.Remove(filepath.Join(s.Dir, id+".json"))
	return os.Remove(filepath.Join(s.Dir, id+".html"))
}

// Purge deletes expired artifacts; returns the count.
func (s *Store) Purge() int {
	es, _ := os.ReadDir(s.Dir)
	n := 0
	for _, e := range es {
		if !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(s.Dir, e.Name()))
		var m Meta
		if err == nil && json.Unmarshal(b, &m) == nil && !m.Expires.After(s.Now()) {
			s.Remove(m.ID)
			n++
		}
	}
	return n
}

// CSP is the policy served with every artifact: inline styles/scripts only, no network.
const CSP = "default-src 'none'; style-src 'unsafe-inline'; img-src data: blob:; script-src 'unsafe-inline'; font-src data:; connect-src 'none'; form-action 'none'; base-uri 'none'"
