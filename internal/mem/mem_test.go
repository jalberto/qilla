package mem

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jalberto/qilla/internal/queue"
)

var now = time.Date(2026, 9, 12, 9, 0, 0, 0, time.UTC)

func db(t *testing.T) *queue.Store {
	t.Helper()
	q, err := queue.Open(filepath.Join(t.TempDir(), "q.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { q.Close() })
	return q
}

func sqliteStore(t *testing.T) *Store {
	q := db(t)
	b, err := NewSQLite(q.DB())
	if err != nil {
		t.Fatal(err)
	}
	b.Now = func() time.Time { return now }
	s, err := New(b, q.DB(), Options{MaxChars: 2000, SaidTTLDays: 90, ExpireUnusedDays: 90, PromoteSupport: 3, PromoteDays: 14})
	if err != nil {
		t.Fatal(err)
	}
	s.Now = func() time.Time { return now }
	return s
}

func TestNotesCRUDAndAggregate(t *testing.T) {
	s := sqliteStore(t)
	ctx := context.Background()
	e1, err := s.Add(ctx, "brief", Heuristic, "cost", "brief cost 0.40 usd with 30 mails")
	if err != nil {
		t.Fatal(err)
	}
	e2, _ := s.Add(ctx, "brief", Heuristic, "cost", "brief cost 0.70 usd with 55 mails")
	if e1.ID != e2.ID || e2.Support != 2 {
		t.Fatalf("same key must aggregate: %+v %+v", e1, e2)
	}
	if e2.NumN != 2 || e2.NumMin != 0.40 || e2.NumMax != 0.70 || e2.NumMean < 0.54 || e2.NumMean > 0.56 {
		t.Fatalf("numeric aggregate: %+v", e2)
	}
	es, _ := s.Search(ctx, "brief", "", "cost mails", 5)
	if len(es) != 1 || es[0].Hits != 0 {
		t.Fatalf("search: %+v", es)
	}
	es, _ = s.Search(ctx, "brief", "", "cost", 5)
	if es[0].Hits != 1 {
		t.Fatalf("hits bump after a search: %+v", es[0])
	}
	if err := s.Forget(ctx, e1.ID); err != nil {
		t.Fatal(err)
	}
	if es, _ = s.Search(ctx, "brief", "", "cost", 5); len(es) != 0 {
		t.Fatal("forgotten")
	}
}

func TestScopeByProjectAndKind(t *testing.T) {
	s := sqliteStore(t)
	ctx := context.Background()
	s.Add(ctx, "brief", Said, "", "flagged invoice 4411 from iberdrola")
	s.Add(ctx, "market", Said, "", "flagged invoice 4411 from the utility in market")
	s.Add(ctx, "brief", Heuristic, "", "invoice routine cost 0.1")
	es, _ := s.Search(ctx, "market", "", "invoice", 10)
	if len(es) != 1 || es[0].Project != "market" {
		t.Fatalf("project filter: %+v", es)
	}
	es, _ = s.Search(ctx, "brief", Said, "invoice", 10)
	if len(es) != 1 || es[0].Kind != Said {
		t.Fatalf("kind filter: %+v", es)
	}
}

func TestSeenFTS(t *testing.T) {
	s := sqliteStore(t)
	ctx := context.Background()
	s.Add(ctx, "brief", Said, "", "Flagged invoice 4411 from Iberdrola to the owner on 2026-09-11.")
	seen, prior, _ := s.Seen(ctx, "brief", "iberdrola invoice 4411 flagged")
	if !seen || prior == nil {
		t.Fatal("must be seen")
	}
	seen, _, _ = s.Seen(ctx, "brief", "garmin watch sold to victor")
	if seen {
		t.Fatal("unrelated must not be seen")
	}
}

func TestTTLAndDemotionPurge(t *testing.T) {
	s := sqliteStore(t)
	ctx := context.Background()
	said, _ := s.Add(ctx, "brief", Said, "", "flagged invoice x")
	heur, _ := s.Add(ctx, "brief", Heuristic, "k", "cost 1 usd")
	note, _ := s.Add(ctx, "brief", Note, "", "some note nobody reads")
	s.Now = func() time.Time { return now.AddDate(0, 0, 91) }
	n, err := s.Purge(ctx)
	if err != nil || n != 2 {
		t.Fatalf("purge said (ttl) + note (unused): n=%d err=%v", n, err)
	}
	if _, err := s.B.Get(ctx, heur.ID); err != nil {
		t.Fatal("heuristics never expire by age")
	}
	for _, id := range []string{said.ID, note.ID} {
		if _, err := s.B.Get(ctx, id); err == nil {
			t.Fatalf("%s should be gone", id)
		}
	}
}

func TestPromoteCandidates(t *testing.T) {
	s := sqliteStore(t)
	ctx := context.Background()
	for i := 0; i < 3; i++ {
		s.Add(ctx, "brief", Heuristic, "cost", "brief cost 0.5 usd")
	}
	s.Add(ctx, "brief", Heuristic, "rare", "seen once")
	if c, _ := s.PromotionCandidates(ctx); len(c) != 0 {
		t.Fatal("too young to promote")
	}
	s.Now = func() time.Time { return now.AddDate(0, 0, 15) }
	c, _ := s.PromotionCandidates(ctx)
	if len(c) != 1 || c[0].Key != "cost" || c[0].Support != 3 {
		t.Fatalf("candidates: %+v", c)
	}
	s.MarkPromoted(ctx, c[0].ID)
	if c, _ = s.PromotionCandidates(ctx); len(c) != 0 {
		t.Fatal("promoted ones drop out")
	}
}

func TestInjectCap(t *testing.T) {
	s := sqliteStore(t)
	ctx := context.Background()
	for i := 0; i < 20; i++ {
		s.Add(ctx, "brief", Heuristic, "k"+strconv.Itoa(i), "brief heuristic number "+strconv.Itoa(i)+" about mails and cost")
	}
	txt, n, _ := s.Inject(ctx, "brief", "brief mails cost", 20, 200)
	if n > 200 || n == 0 || strings.Count(txt, "\n") > 5 {
		t.Fatalf("cap: %d chars\n%s", n, txt)
	}
}

// ---- Engram backend against a fake server ----

type fakeEngram struct {
	mu     sync.Mutex
	obs    map[int]map[string]any
	next   int
	sess   map[string]bool
	judged []string
}

func newFake() *fakeEngram {
	return &fakeEngram{obs: map[int]map[string]any{}, next: 1, sess: map[string]bool{}}
}

func (f *fakeEngram) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"status": "ok"})
	})
	mux.HandleFunc("POST /sessions", func(w http.ResponseWriter, r *http.Request) {
		var in map[string]any
		json.NewDecoder(r.Body).Decode(&in)
		f.mu.Lock()
		f.sess[in["id"].(string)] = true
		f.mu.Unlock()
		json.NewEncoder(w).Encode(in)
	})
	mux.HandleFunc("POST /observations", func(w http.ResponseWriter, r *http.Request) {
		var in map[string]any
		json.NewDecoder(r.Body).Decode(&in)
		f.mu.Lock()
		defer f.mu.Unlock()
		if sid, _ := in["session_id"].(string); !f.sess[sid] {
			http.Error(w, `{"error":"session_id and content are required"}`, 400)
			return
		}
		// topic_key upsert
		if tk, _ := in["topic_key"].(string); tk != "" {
			for id, o := range f.obs {
				if o["topic_key"] == tk && o["project"] == in["project"] {
					o["content"] = in["content"]
					o["revision_count"] = o["revision_count"].(int) + 1
					o["id"] = id
					json.NewEncoder(w).Encode(o)
					return
				}
			}
		}
		id := f.next
		f.next++
		in["id"] = id
		in["revision_count"] = 0
		in["duplicate_count"] = 0
		in["created_at"] = "2026-09-12T09:00:00Z"
		f.obs[id] = in
		json.NewEncoder(w).Encode(in)
	})
	mux.HandleFunc("GET /search", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("project") == "never-seen" {
			http.Error(w, `{"code":"unknown_project","error":"project \"never-seen\" not found"}`, 404)
			return
		}
		q := strings.ToLower(r.URL.Query().Get("q"))
		var out []map[string]any
		f.mu.Lock()
		for _, o := range f.obs {
			if p := r.URL.Query().Get("project"); p != "" && o["project"] != p {
				continue
			}
			if ty := r.URL.Query().Get("type"); ty != "" && o["type"] != ty {
				continue
			}
			for _, w := range strings.Fields(q) {
				if strings.Contains(strings.ToLower(o["content"].(string)), w) {
					out = append(out, o)
					break
				}
			}
		}
		f.mu.Unlock()
		if out == nil {
			out = []map[string]any{}
		}
		json.NewEncoder(w).Encode(out)
	})
	mux.HandleFunc("GET /observations/recent", func(w http.ResponseWriter, r *http.Request) {
		var out []map[string]any
		f.mu.Lock()
		for _, o := range f.obs {
			if p := r.URL.Query().Get("project"); p == "" || o["project"] == p {
				out = append(out, o)
			}
		}
		f.mu.Unlock()
		if out == nil {
			out = []map[string]any{}
		}
		json.NewEncoder(w).Encode(out)
	})
	mux.HandleFunc("GET /observations/{id}", func(w http.ResponseWriter, r *http.Request) {
		id, _ := strconv.Atoi(r.PathValue("id"))
		f.mu.Lock()
		o, ok := f.obs[id]
		f.mu.Unlock()
		if !ok {
			http.Error(w, "nope", 404)
			return
		}
		json.NewEncoder(w).Encode(o)
	})
	mux.HandleFunc("DELETE /observations/{id}", func(w http.ResponseWriter, r *http.Request) {
		id, _ := strconv.Atoi(r.PathValue("id"))
		f.mu.Lock()
		delete(f.obs, id)
		f.mu.Unlock()
		w.Write([]byte(`{}`))
	})
	mux.HandleFunc("GET /conflicts", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"relations": []map[string]any{{"sync_id": "rel-1", "relation": "conflicts_with", "judgment_status": "pending", "source_id": 1, "target_id": 2}}})
	})
	mux.HandleFunc("POST /conflicts/judge", func(w http.ResponseWriter, r *http.Request) {
		var in map[string]any
		json.NewDecoder(r.Body).Decode(&in)
		f.mu.Lock()
		f.judged = append(f.judged, in["judgment_id"].(string)+":"+in["relation"].(string))
		f.mu.Unlock()
		w.Write([]byte(`{"relation":{"judgment_status":"judged"}}`))
	})
	return mux
}

func TestEngramBackend(t *testing.T) {
	f := newFake()
	srv := httptest.NewServer(f.handler())
	defer srv.Close()
	b, err := NewEngram(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := b.Health(ctx); err != nil {
		t.Fatal(err)
	}
	e1, err := b.Save(ctx, "brief", Heuristic, "cost", "brief cost 0.4")
	if err != nil {
		t.Fatal(err)
	}
	e2, _ := b.Save(ctx, "brief", Heuristic, "cost", "brief cost 0.7")
	if e1.ID != e2.ID || e2.Support != 2 || e2.Kind != Heuristic || e2.Key != "cost" {
		t.Fatalf("topic_key upsert via engram: %+v %+v", e1, e2)
	}
	b.Save(ctx, "market", Said, "", "bike sold")
	es, _ := b.Search(ctx, "brief", "", "cost", 5)
	if len(es) != 1 || es[0].Project != "brief" {
		t.Fatalf("scoped search: %+v", es)
	}
	rc, _ := b.Recent(ctx, "brief", 5)
	if len(rc) == 0 || rc[0].Key != "cost" {
		t.Fatalf("recent must trim the kind prefix like search/get: %+v", rc)
	}
	if es, err := b.Search(ctx, "never-seen", "", "anything", 5); err != nil || len(es) != 0 {
		t.Fatalf("unknown project must read as empty, not an error: %v %v", err, es)
	}
	cs, _ := b.Conflicts(ctx, "brief")
	if len(cs) != 1 || cs[0].ID != "rel-1" || cs[0].SourceID != "1" {
		t.Fatalf("conflicts: %+v", cs)
	}
	if err := b.Judge(ctx, "rel-1", "not_conflict", "same claim"); err != nil || f.judged[0] != "rel-1:not_conflict" {
		t.Fatalf("judge: %v %v", err, f.judged)
	}
	if err := b.Delete(ctx, e1.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Get(ctx, e1.ID); err == nil {
		t.Fatal("deleted")
	}
	// store on top of engram: meta works the same
	q := db(t)
	s, _ := New(b, q.DB(), Options{MaxChars: 500, SaidTTLDays: 90, ExpireUnusedDays: 90})
	s.Now = func() time.Time { return now }
	s.Add(ctx, "brief", Heuristic, "k", "value 3")
	s.Add(ctx, "brief", Heuristic, "k", "value 5")
	es, _ = s.Search(ctx, "brief", "", "value", 5)
	if len(es) != 1 || es[0].NumN != 2 || es[0].NumMean != 4 {
		t.Fatalf("meta over engram: %+v", es)
	}
}

func TestEngramDownIsClearError(t *testing.T) {
	b, _ := NewEngram("http://127.0.0.1:1")
	err := b.Health(context.Background())
	if err == nil || !strings.Contains(err.Error(), "qilla-engram.service") {
		t.Fatalf("want actionable error, got %v", err)
	}
}

func TestInjectIncludesShared(t *testing.T) {
	s := sqliteStore(t)
	s.Opt.SharedProject = "shared"
	ctx := context.Background()
	s.Add(ctx, "shared", Heuristic, "reddit", "reddit blocks fetches; use the safereddit mirror")
	s.Add(ctx, "brief", Heuristic, "cost", "brief cost 0.5 with 30 mails")
	s.Add(ctx, "market", Heuristic, "x", "reddit market unrelated")
	txt, _, err := s.Inject(ctx, "brief", "reddit mirror", 5, 2000)
	if err != nil || !strings.Contains(txt, "safereddit") || strings.Contains(txt, "market") {
		t.Fatalf("shared visible, other projects not:\n%s %v", txt, err)
	}
}
