// Package candidates is the judge queues: one JSONL file per family under a
// single root, one line per candidate a flow detected but did not judge.
// Flows append (cheap, no judgement); the judge routines read the family file,
// act, and drop the ids they handled.
package candidates

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

// Candidate is one queued line. Field order is the on-disk key order.
type Candidate struct {
	ID       string   `json:"id"`
	TS       string   `json:"ts"`
	Source   string   `json:"source"`
	Text     string   `json:"text"`
	People   []string `json:"people"`
	DateHint string   `json:"date_hint"`
}

// Item is a queued line: the parsed candidate plus the bytes as written, so
// listing preserves fields other producers (e.g. the Notion poller) add.
type Item struct {
	Candidate
	Raw string
}

// Count is one family's depth.
type Count struct {
	Family string
	N      int
}

var familyRe = regexp.MustCompile(`^[a-z][a-z0-9-]*$`)

// ValidFamily reports whether name is a usable queue family (and file name).
func ValidFamily(name string) bool { return familyRe.MatchString(name) }

// Store is a queue root holding <family>.jsonl files.
type Store struct{ Root string }

func New(root string) *Store { return &Store{Root: root} }

// Path is the file backing a family.
func (s *Store) Path(family string) string {
	return filepath.Join(s.Root, family+".jsonl")
}

// NewID mints c_YYYYMMDD_HHMMSS_<hex>, the id format the rest of qilla's
// queues use.
func NewID(now time.Time) string {
	var b [4]byte
	rand.Read(b[:])
	return fmt.Sprintf("c_%s_%s", now.Format("20060102_150405"), hex.EncodeToString(b[:]))
}

// Families lists the families that have a file, in name order.
func (s *Store) Families() ([]string, error) {
	paths, err := filepath.Glob(filepath.Join(s.Root, "*.jsonl"))
	if err != nil {
		return nil, err
	}
	var out []string
	for _, p := range paths {
		out = append(out, strings.TrimSuffix(filepath.Base(p), ".jsonl"))
	}
	sort.Strings(out)
	return out, nil
}

// List returns a family's pending items (empty when the file is absent).
func (s *Store) List(family string) ([]Item, error) {
	b, err := os.ReadFile(s.Path(family))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []Item
	for _, line := range strings.Split(string(b), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		it := Item{Raw: line}
		json.Unmarshal([]byte(line), &it.Candidate)
		out = append(out, it)
	}
	return out, nil
}

// Count is a family's pending depth.
func (s *Store) Count(family string) (int, error) {
	items, err := s.List(family)
	return len(items), err
}

// Counts is every non-empty family's depth, in name order.
func (s *Store) Counts() ([]Count, error) {
	fams, err := s.Families()
	if err != nil {
		return nil, err
	}
	var out []Count
	for _, f := range fams {
		n, err := s.Count(f)
		if err != nil {
			return nil, err
		}
		if n > 0 {
			out = append(out, Count{Family: f, N: n})
		}
	}
	return out, nil
}

// Add appends a candidate and returns its id. Exact-text duplicates are not
// appended: the same mention harvested twice (today's and yesterday's inbox)
// must not become two candidates; dup is true and the id is the existing one.
func (s *Store) Add(family string, c Candidate, now time.Time) (id string, dup bool, err error) {
	if !ValidFamily(family) {
		return "", false, fmt.Errorf("bad family %q: want ^[a-z][a-z0-9-]*$", family)
	}
	if strings.TrimSpace(c.Text) == "" {
		return "", false, errors.New("--text is required")
	}
	items, err := s.List(family)
	if err != nil {
		return "", false, err
	}
	for _, it := range items {
		if it.Text == c.Text {
			return it.ID, true, nil
		}
	}
	c.ID, c.TS = NewID(now), now.Format(time.RFC3339)
	if c.People == nil {
		c.People = []string{}
	}
	if err := os.MkdirAll(s.Root, 0o755); err != nil {
		return "", false, err
	}
	line, err := json.Marshal(c)
	if err != nil {
		return "", false, err
	}
	f, err := os.OpenFile(s.Path(family), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return "", false, err
	}
	defer f.Close()
	if _, err := f.Write(append(line, '\n')); err != nil { // one write: concurrent emitters never interleave
		return "", false, err
	}
	return c.ID, false, nil
}

// Drop removes ids from one family and returns how many lines went.
func (s *Store) Drop(family string, ids []string) (int, error) {
	items, err := s.List(family)
	if err != nil || len(items) == 0 {
		return 0, err
	}
	gone := map[string]bool{}
	for _, id := range ids {
		gone[id] = true
	}
	var keep []string
	for _, it := range items {
		if gone[it.ID] {
			continue
		}
		keep = append(keep, it.Raw)
	}
	n := len(items) - len(keep)
	if n == 0 {
		return 0, nil
	}
	body := ""
	if len(keep) > 0 {
		body = strings.Join(keep, "\n") + "\n"
	}
	return n, replace(s.Path(family), body)
}

// DropAll removes ids wherever they are queued.
func (s *Store) DropAll(ids []string) (int, error) {
	fams, err := s.Families()
	if err != nil {
		return 0, err
	}
	total := 0
	for _, f := range fams {
		n, err := s.Drop(f, ids)
		if err != nil {
			return total, err
		}
		total += n
	}
	return total, nil
}

func replace(path, body string) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(body), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
