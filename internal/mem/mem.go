// Package mem is qilla's working memory: what qilla needs and the user never
// reads. Human-meaningful facts live in the vault; this holds heuristics,
// "already said" markers, routing hints and TTL notes. The store is a
// pluggable backend (Engram sidecar or own FTS5 table) plus a small qilla-side
// table for what the backend lacks: hits, numeric aggregates, TTL, promotion.
package mem

import (
	"context"
	"database/sql"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Kinds.
const (
	Heuristic = "heuristic"
	Said      = "said"
	Route     = "route"
	Note      = "note"
	// State is a routine's own last outcome: last-write-wins per key, never
	// aggregated, and always injected first for its project.
	State = "state"
)

// Entry is one working-memory note as qilla sees it.
type Entry struct {
	ID       string    `json:"id"` // backend id
	Project  string    `json:"project"`
	Kind     string    `json:"kind"`
	Key      string    `json:"key"`
	Text     string    `json:"text"`
	Support  int       `json:"support"`
	Created  time.Time `json:"created"`
	Updated  time.Time `json:"updated"`
	Hits     int       `json:"hits"`
	LastUsed time.Time `json:"last_used"`
	NumMin   float64   `json:"num_min,omitempty"`
	NumMax   float64   `json:"num_max,omitempty"`
	NumMean  float64   `json:"num_mean,omitempty"`
	NumN     int       `json:"num_n,omitempty"`
	Score    float64   `json:"score"`
}

// Backend stores and searches entries. Project scopes everything.
type Backend interface {
	Save(ctx context.Context, project, kind, key, text string) (Entry, error)
	Search(ctx context.Context, project, kind, query string, n int) ([]Entry, error)
	Get(ctx context.Context, id string) (Entry, error)
	Delete(ctx context.Context, id string) error
	Recent(ctx context.Context, project string, n int) ([]Entry, error)
	Conflicts(ctx context.Context, project string) ([]Conflict, error)
	Judge(ctx context.Context, relationID, relation, reason string) error
	Health(ctx context.Context) error
	Name() string
}

// Conflict is a pending relation between two entries.
type Conflict struct {
	ID       string `json:"id"`
	SourceID string `json:"source_id"`
	TargetID string `json:"target_id"`
	Relation string `json:"relation"`
	Status   string `json:"status"`
}

// Options are the tunables from qilla.toml [memory].
type Options struct {
	SharedProject    string // notes visible to every project ("" = none)
	MaxChars         int
	SaidTTLDays      int
	ExpireUnusedDays int
	PromoteSupport   int
	PromoteDays      int
}

// Store combines a backend with the qilla-side meta table.
type Store struct {
	B   Backend
	db  *sql.DB
	Opt Options
	Now func() time.Time
}

// New prepares the meta table.
func New(b Backend, db *sql.DB, opt Options) (*Store, error) {
	if _, err := db.Exec(`
CREATE TABLE IF NOT EXISTS mem_meta(
  id TEXT PRIMARY KEY, project TEXT NOT NULL, kind TEXT NOT NULL, key TEXT NOT NULL,
  hits INTEGER NOT NULL DEFAULT 0, last_used INTEGER NOT NULL DEFAULT 0,
  num_n INTEGER NOT NULL DEFAULT 0, num_min REAL, num_max REAL, num_sum REAL NOT NULL DEFAULT 0,
  ttl_until INTEGER NOT NULL DEFAULT 0, promoted INTEGER NOT NULL DEFAULT 0, created INTEGER NOT NULL);
CREATE INDEX IF NOT EXISTS mem_meta_proj ON mem_meta(project, kind);`); err != nil {
		return nil, err
	}
	return &Store{B: b, db: db, Opt: opt, Now: time.Now}, nil
}

var numRe = regexp.MustCompile(`(-?\d+(?:\.\d+)?)`)

// Add saves a note. Same project+key aggregates in the backend (upsert) and
// in meta (support, numeric min/max/mean from the first number in text).
func (s *Store) Add(ctx context.Context, project, kind, key, text string) (Entry, error) {
	if kind == "" {
		kind = Note
	}
	if !validKind(kind) {
		return Entry{}, fmt.Errorf("kind must be heuristic | said | route | note")
	}
	if strings.TrimSpace(text) == "" {
		return Entry{}, fmt.Errorf("empty text")
	}
	e, err := s.B.Save(ctx, project, kind, key, text)
	if err != nil {
		return Entry{}, err
	}
	now := s.Now().Unix()
	var ttl int64
	if kind == Said && s.Opt.SaidTTLDays > 0 {
		ttl = s.Now().AddDate(0, 0, s.Opt.SaidTTLDays).Unix()
	}
	num, hasNum := firstNumber(text)
	if kind == State {
		hasNum = false // state is last-write-wins, never aggregated
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO mem_meta(id,project,kind,key,created,ttl_until,num_n,num_min,num_max,num_sum)
		VALUES(?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(id) DO UPDATE SET ttl_until=excluded.ttl_until,
		  num_n = mem_meta.num_n + excluded.num_n,
		  num_min = CASE WHEN excluded.num_n=0 THEN mem_meta.num_min WHEN mem_meta.num_min IS NULL THEN excluded.num_min ELSE MIN(mem_meta.num_min, excluded.num_min) END,
		  num_max = CASE WHEN excluded.num_n=0 THEN mem_meta.num_max WHEN mem_meta.num_max IS NULL THEN excluded.num_max ELSE MAX(mem_meta.num_max, excluded.num_max) END,
		  num_sum = mem_meta.num_sum + excluded.num_sum`,
		e.ID, project, kind, key, now, ttl, b2i(hasNum), nullIf(num, !hasNum), nullIf(num, !hasNum), numOr0(num, hasNum))
	if err != nil {
		return Entry{}, err
	}
	return s.decorate(ctx, e)
}

// Search returns ranked entries for a project (and kind when set), expired
// ones excluded, hits bumped on what is returned.
func (s *Store) Search(ctx context.Context, project, kind, query string, n int) ([]Entry, error) {
	es, err := s.B.Search(ctx, project, kind, query, n*2)
	if err != nil {
		return nil, err
	}
	// Backends match terms, not sentences: a whole prompt line finds nothing.
	// Retry with the distinctive words before giving up.
	if len(es) == 0 {
		if kt := keyTerms(query); kt != "" && kt != query {
			if es, err = s.B.Search(ctx, project, kind, kt, n*2); err != nil {
				return nil, err
			}
		}
	}
	now := s.Now().Unix()
	var out []Entry
	for _, e := range es {
		e, err = s.decorate(ctx, e)
		if err != nil {
			return nil, err
		}
		if expired(e, now, s.Opt.ExpireUnusedDays) {
			continue
		}
		out = append(out, e)
		if len(out) == n {
			break
		}
	}
	s.bumpHits(ctx, out)
	return out, nil
}

// bumpHits records that these entries were actually shown to a model.
func (s *Store) bumpHits(ctx context.Context, es []Entry) {
	now := s.Now().Unix()
	for _, e := range es {
		res, err := s.db.ExecContext(ctx, `UPDATE mem_meta SET hits=hits+1, last_used=? WHERE id=?`, now, e.ID)
		if err != nil {
			continue
		}
		// an entry written by another qilla instance (or before mem_meta
		// existed) has no meta row: create one so hits are not lost.
		if n, _ := res.RowsAffected(); n == 0 {
			s.db.ExecContext(ctx, `INSERT OR IGNORE INTO mem_meta(id,project,kind,key,hits,last_used,created) VALUES(?,?,?,?,1,?,?)`,
				e.ID, e.Project, e.Kind, e.Key, now, now)
		}
	}
}

// Seen reports whether something equivalent was already recorded for the
// project (kind said): lexical match on the text's key terms.
func (s *Store) Seen(ctx context.Context, project, text string) (bool, *Entry, error) {
	q := keyTerms(text)
	if q == "" {
		return false, nil, nil
	}
	es, err := s.Search(ctx, project, Said, q, 1)
	if err != nil || len(es) == 0 {
		return false, nil, err
	}
	return true, &es[0], nil
}

// State returns the project's state entries, newest per key first. They are
// what a routine wrote about its own last run, so they are returned by
// recency, never by rank.
func (s *Store) State(ctx context.Context, project string, n int) ([]Entry, error) {
	if project == "" {
		return nil, nil
	}
	es, err := s.B.Recent(ctx, project, 50)
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	var out []Entry
	for _, e := range es {
		if e.Kind != State || seen[e.Key] {
			continue
		}
		seen[e.Key] = true
		e, err = s.decorate(ctx, e)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
		if n > 0 && len(out) == n {
			break
		}
	}
	s.bumpHits(ctx, out)
	return out, nil
}

// Inject returns the memory block for a prompt: the project's state first
// (always, whatever it ranks), then top-n for the query from the project plus
// the shared project, capped at maxChars, one line per entry.
func (s *Store) Inject(ctx context.Context, project, query string, n, maxChars int) (string, int, error) {
	state, err := s.State(ctx, project, 3)
	if err != nil {
		return "", 0, err
	}
	es, err := s.Search(ctx, project, "", query, n)
	if err != nil {
		return "", 0, err
	}
	if len(es) == 0 {
		// nothing matched the query: the routine's own recent notes still beat
		// an empty memory block, which is what made memory write-only.
		if rec, rerr := s.B.Recent(ctx, project, n); rerr == nil {
			for _, e := range rec {
				if e, derr := s.decorate(ctx, e); derr == nil {
					es = append(es, e)
				}
			}
			s.bumpHits(ctx, es)
		}
	}
	if s.Opt.SharedProject != "" && s.Opt.SharedProject != project {
		shared, err := s.Search(ctx, s.Opt.SharedProject, "", query, n)
		if err != nil {
			return "", 0, err
		}
		es = append(es, shared...)
		sort.SliceStable(es, func(i, j int) bool { return es[i].Score > es[j].Score })
		if len(es) > n {
			es = es[:n]
		}
	}
	// state first, deduped against the ranked results
	inState := map[string]bool{}
	for _, e := range state {
		inState[e.ID] = true
	}
	ranked := es[:0:0]
	for _, e := range es {
		if !inState[e.ID] {
			ranked = append(ranked, e)
		}
	}
	es = append(state, ranked...)
	var b strings.Builder
	for _, e := range es {
		line := fmt.Sprintf("- [%s] %s", e.Kind, strings.TrimSpace(e.Text))
		if e.NumN > 1 {
			line += fmt.Sprintf(" (n=%d min=%.3g max=%.3g mean=%.3g)", e.NumN, e.NumMin, e.NumMax, e.NumMean)
		}
		if b.Len()+len(line)+1 > maxChars {
			break
		}
		b.WriteString(line)
		b.WriteByte('\n')
	}
	return b.String(), b.Len(), nil
}

// Forget deletes an entry everywhere.
func (s *Store) Forget(ctx context.Context, id string) error {
	if err := s.B.Delete(ctx, id); err != nil {
		return err
	}
	_, err := s.db.ExecContext(ctx, `DELETE FROM mem_meta WHERE id=?`, id)
	return err
}

// Purge removes entries past their TTL or unused for ExpireUnusedDays
// (measured from last use, or creation when never used). Returns the count.
func (s *Store) Purge(ctx context.Context) (int, error) {
	now := s.Now().Unix()
	cut := s.Now().AddDate(0, 0, -s.Opt.ExpireUnusedDays).Unix()
	rows, err := s.db.QueryContext(ctx, `SELECT id FROM mem_meta WHERE promoted=0 AND ((ttl_until>0 AND ttl_until<?) OR (MAX(last_used, created) < ? AND kind!=?))`, now, cut, Heuristic)
	if err != nil {
		return 0, err
	}
	var ids []string
	for rows.Next() {
		var id string
		rows.Scan(&id)
		ids = append(ids, id)
	}
	rows.Close()
	for _, id := range ids {
		if err := s.Forget(ctx, id); err != nil {
			return len(ids), err
		}
	}
	return len(ids), nil
}

// PromotionCandidates lists heuristics with enough support and age to be
// proposed as vault facts.
func (s *Store) PromotionCandidates(ctx context.Context) ([]Entry, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id FROM mem_meta WHERE promoted=0 AND kind=? AND created<=?`,
		Heuristic, s.Now().AddDate(0, 0, -s.Opt.PromoteDays).Unix())
	if err != nil {
		return nil, err
	}
	var ids []string
	for rows.Next() {
		var id string
		rows.Scan(&id)
		ids = append(ids, id)
	}
	rows.Close()
	var out []Entry
	for _, id := range ids {
		e, err := s.B.Get(ctx, id)
		if err != nil {
			continue
		}
		e, _ = s.decorate(ctx, e)
		if e.Support >= s.Opt.PromoteSupport {
			out = append(out, e)
		}
	}
	return out, nil
}

// MarkPromoted records that the owner accepted the entry into the vault.
func (s *Store) MarkPromoted(ctx context.Context, id string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE mem_meta SET promoted=1 WHERE id=?`, id)
	return err
}

// Stats for doctor and the status page.
type Stats struct {
	Entries    int    `json:"entries"`
	Expiring   int    `json:"expiring_7d"`
	Promotable int    `json:"promotable"`
	Backend    string `json:"backend"`
}

// Stats counts what the meta table knows.
func (s *Store) Stats(ctx context.Context) (Stats, error) {
	var st Stats
	st.Backend = s.B.Name()
	s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM mem_meta`).Scan(&st.Entries)
	s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM mem_meta WHERE ttl_until>0 AND ttl_until<?`, s.Now().AddDate(0, 0, 7).Unix()).Scan(&st.Expiring)
	pc, _ := s.PromotionCandidates(ctx)
	st.Promotable = len(pc)
	return st, nil
}

func (s *Store) decorate(ctx context.Context, e Entry) (Entry, error) {
	var lastUsed int64
	var nmin, nmax sql.NullFloat64
	var nsum float64
	err := s.db.QueryRowContext(ctx, `SELECT hits,last_used,num_n,num_min,num_max,num_sum FROM mem_meta WHERE id=?`, e.ID).
		Scan(&e.Hits, &lastUsed, &e.NumN, &nmin, &nmax, &nsum)
	if err != nil && err != sql.ErrNoRows {
		return e, err
	}
	if lastUsed > 0 {
		e.LastUsed = time.Unix(lastUsed, 0)
	}
	if e.NumN > 0 {
		e.NumMin, e.NumMax, e.NumMean = nmin.Float64, nmax.Float64, nsum/float64(e.NumN)
	}
	return e, nil
}

func expired(e Entry, now int64, unusedDays int) bool {
	if unusedDays <= 0 || e.Kind == Heuristic {
		return false
	}
	ref := e.Created
	if !e.LastUsed.IsZero() {
		ref = e.LastUsed
	}
	return !ref.IsZero() && now-ref.Unix() > int64(unusedDays)*86400
}

func validKind(k string) bool {
	return k == Heuristic || k == Said || k == Route || k == Note || k == State
}

func firstNumber(text string) (float64, bool) {
	m := numRe.FindString(text)
	if m == "" {
		return 0, false
	}
	f, err := strconv.ParseFloat(m, 64)
	return f, err == nil
}

// keyTerms keeps the distinctive tokens of a text for an FTS query.
func keyTerms(text string) string {
	var out []string
	for _, w := range strings.Fields(strings.ToLower(text)) {
		w = strings.Trim(w, ".,;:!?\"'()[]{}")
		if len(w) < 4 || stop[w] {
			continue
		}
		out = append(out, w)
		if len(out) == 6 {
			break
		}
	}
	return strings.Join(out, " ")
}

var stop = map[string]bool{"that": true, "this": true, "with": true, "from": true, "have": true, "were": true, "been": true, "will": true, "your": true, "about": true, "there": true, "their": true, "which": true, "would": true, "should": true, "today": true, "yesterday": true}

func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}

func nullIf(v float64, null bool) any {
	if null {
		return nil
	}
	return v
}

func numOr0(v float64, has bool) float64 {
	if has {
		return v
	}
	return 0
}
