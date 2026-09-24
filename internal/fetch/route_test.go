package fetch

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jalberto/qilla/internal/decide"
)

// hostTransport sends every request to the test server, whatever its host,
// so a HEAD probe or a fake hister URL never leaves the process.
type hostTransport struct{ srv *httptest.Server }

func (h hostTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	u, _ := url.Parse(h.srv.URL)
	r2 := r.Clone(r.Context())
	r2.Header.Set("X-Orig-Host", r.URL.Host)
	r2.URL.Scheme, r2.URL.Host = u.Scheme, u.Host
	return http.DefaultTransport.RoundTrip(r2)
}

// fakeNet is hister + karakeep + ladder behind one handler, keyed by the
// original host. It records every request as "METHOD host path".
type fakeNet struct {
	mu   sync.Mutex
	reqs []string
	h    map[string]http.HandlerFunc // host → handler
}

func newNet(t *testing.T, h map[string]http.HandlerFunc) (*fakeNet, *http.Client) {
	t.Helper()
	f := &fakeNet{h: h}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host := r.Header.Get("X-Orig-Host")
		f.mu.Lock()
		f.reqs = append(f.reqs, r.Method+" "+host+" "+r.URL.Path)
		f.mu.Unlock()
		if fn, ok := f.h[host]; ok {
			fn(w, r)
			return
		}
		http.Error(w, "no", http.StatusNotFound)
	}))
	t.Cleanup(srv.Close)
	return f, &http.Client{Transport: hostTransport{srv}}
}

func (f *fakeNet) has(sub string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, r := range f.reqs {
		if strings.Contains(r, sub) {
			return true
		}
	}
	return false
}

func TestHisterHitShortCircuits(t *testing.T) {
	page := "https://www.reddit.com/r/x/comments/1/t/"
	f, client := newNet(t, map[string]http.HandlerFunc{
		"hister.test": func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("Origin") == "" {
				http.Error(w, "origin", 500)
				return
			}
			var q map[string]any
			_ = json.Unmarshal([]byte(r.URL.Query().Get("query")), &q)
			doc := map[string]any{"url": page + "?utm_source=x", "text": ""}
			if q["include_html"] == true {
				doc["html"] = "<html><body><p>" + articleText + "</p><script>x()</script></body></html>"
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"documents": []any{
				map[string]any{"url": "https://other.test/", "text": "nope"}, doc}})
		},
	})
	var calls []string
	res, err := Fetch(context.Background(), "https://reddit.com/r/x/comments/1/t", Options{
		HTTP: client, HisterURL: "http://hister.test", Look: allPresent,
		Run: fakeRunner(t, map[string]string{"defuddle": articleText}, &calls),
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Rung != RungHister || res.Pos != 1 || !res.OK() || strings.Contains(res.Text, "x()") {
		t.Fatalf("got %+v", res)
	}
	if len(calls) != 0 || !f.has("GET hister.test /search") {
		t.Fatalf("calls = %v reqs = %v", calls, f.reqs)
	}
}

func TestLedgerStartRungAndRecord(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "fetch", "ledger.json")
	l := Ledger{"example.com": {Rung: RungObscura, OKCount: 2}}
	if err := l.Save(path); err != nil {
		t.Fatal(err)
	}
	var calls []string
	asked := false
	res, err := Fetch(context.Background(), "https://www.sub.example.com/a", Options{
		LedgerPath: path, Look: allPresent,
		Ask: func(context.Context, decide.AskRequest) (decide.AskResult, error) {
			asked = true
			return decide.AskResult{Label: decide.Unknown}, nil
		},
		Run: fakeRunner(t, map[string]string{"defuddle": articleText, "obscura": articleText}, &calls),
		Now: func() time.Time { return time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC) },
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Route != RouteLedger || res.Rung != RungObscura || res.Pos != 3 || asked {
		t.Fatalf("got %+v asked=%v", res, asked)
	}
	if strings.Join(res.Order, ",") != "hister,karakeep-lookup,obscura,karakeep-crawl,headless,headed" {
		t.Fatalf("order = %v", res.Order)
	}
	for _, c := range calls {
		if strings.HasPrefix(c, "defuddle") {
			t.Fatalf("ran a rung before the ledger's: %v", calls)
		}
	}
	got, _ := LoadLedger(path)
	if e := got["example.com"]; e.OKCount != 3 || e.LastOK != "2026-09-24T12:00:00Z" || e.Kind != decide.PageContent {
		t.Fatalf("ledger = %+v", got)
	}
}

func TestLedgerIgnoredWithRouteDefault(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ledger.json")
	_ = Ledger{"example.com": {Rung: RungObscura, OKCount: 1}}.Save(path)
	res, _ := Fetch(context.Background(), "https://example.com/a", Options{
		LedgerPath: path, Route: RouteDefault, Look: allPresent,
		Run: fakeRunner(t, map[string]string{"defuddle": articleText}, nil),
	})
	if res.Route != RouteDefault || res.Rung != RungDefuddle {
		t.Fatalf("got %+v", res)
	}
	if got, _ := LoadLedger(path); got["example.com"].OKCount != 1 {
		t.Fatalf("ledger written with route=default: %+v", got)
	}
}

func TestAskAnswerHonoured(t *testing.T) {
	_, client := newNet(t, map[string]http.HandlerFunc{
		"news.test": func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Server", "cloudflare")
			w.WriteHeader(403)
		},
	})
	var req decide.AskRequest
	var calls []string
	res, err := Fetch(context.Background(), "https://news.test/a", Options{
		HTTP: client, Look: allPresent, LedgerPath: filepath.Join(t.TempDir(), "l.json"),
		Ask: func(_ context.Context, r decide.AskRequest) (decide.AskResult, error) {
			if r.Caller == "fetch-route" {
				req = r
				return decide.AskResult{Kind: "choice", Label: RungObscura, Conf: 0.9}, nil
			}
			return decide.AskResult{Label: decide.Unknown}, nil
		},
		Run: fakeRunner(t, map[string]string{"obscura": articleText, "defuddle": articleText}, &calls),
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Route != RouteAsk || res.Rung != RungObscura || res.Pos != 3 {
		t.Fatalf("got %+v", res)
	}
	if req.Kind != "choice" || len(req.Options) != 8 || !strings.Contains(req.Text, "HEAD 403") || !strings.Contains(req.Text, "cloudflare") {
		t.Fatalf("request = %+v", req)
	}
	if len(calls) != 1 || !strings.HasPrefix(calls[0], "obscura") {
		t.Fatalf("calls = %v", calls)
	}
	if res.Order[2] != RungObscura || len(res.Order) != MaxRung {
		t.Fatalf("order = %v", res.Order)
	}
}

func TestAskUnknownFallsBack(t *testing.T) {
	_, client := newNet(t, nil)
	res, _ := Fetch(context.Background(), "https://news.test/a", Options{
		HTTP: client, Look: allPresent,
		Ask: func(context.Context, decide.AskRequest) (decide.AskResult, error) {
			return decide.AskResult{Label: decide.Unknown}, nil
		},
		Run: fakeRunner(t, map[string]string{"defuddle": articleText}, nil),
	})
	if res.Route != RouteDefault || res.Rung != RungDefuddle {
		t.Fatalf("got %+v", res)
	}
}

func TestMirrorOnlyForReddit(t *testing.T) {
	var calls []string
	run := fakeRunner(t, map[string]string{"safereddit.com": articleText}, &calls)
	res, _ := Fetch(context.Background(), "https://old.reddit.com/r/x/1", Options{
		Look: allPresent, Run: run, Route: RouteDefault,
	})
	if res.Rung != RungMirror || !strings.Contains(strings.Join(calls, "|"), "https://safereddit.com/r/x/1") {
		t.Fatalf("got %+v calls %v", res, calls)
	}
	res, _ = Fetch(context.Background(), "https://example.com/x", Options{
		Look: allPresent, MaxRung: 6, Route: RouteDefault,
		Run: fakeRunner(t, map[string]string{"defuddle": blockedText, "curl": blockedText}, nil),
	})
	for _, tr := range res.Tried {
		if tr.Rung == RungMirror && (tr.Kind != "skipped" || tr.Note == "") {
			t.Fatalf("mirror ran for example.com: %+v", tr)
		}
	}
}

// karakeep fake: POST /bookmarks answers with an existing or new bookmark.
func karakeepFake(t *testing.T, exists, archived bool) (*fakeNet, *http.Client) {
	t.Helper()
	polls := 0
	return newNet(t, map[string]http.HandlerFunc{
		"kk.test": func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("Authorization") != "Bearer k" {
				http.Error(w, "auth", 401)
				return
			}
			switch {
			case r.Method == "GET" && r.URL.Path == "/api/v1/bookmarks/search":
				_ = json.NewEncoder(w).Encode(map[string]any{"bookmarks": []any{}})
			case r.Method == "POST" && r.URL.Path == "/api/v1/bookmarks":
				b, _ := io.ReadAll(r.Body)
				if !strings.Contains(string(b), `"type":"link"`) {
					http.Error(w, "bad", 400)
					return
				}
				code := 201
				if exists {
					code = 200
				}
				w.WriteHeader(code)
				_ = json.NewEncoder(w).Encode(map[string]any{"id": "b1", "archived": archived, "alreadyExists": exists})
			case r.Method == "GET" && r.URL.Path == "/api/v1/bookmarks/b1":
				polls++
				status := "pending"
				if polls > 1 {
					status = "success"
				}
				html := "<p>" + articleText + "</p>"
				_ = json.NewEncoder(w).Encode(map[string]any{"id": "b1", "content": map[string]any{"crawlStatus": status, "htmlContent": html}})
			default:
				w.WriteHeader(200)
				_, _ = w.Write([]byte("{}"))
			}
		},
	})
}

func crawlOnly(t *testing.T, client *http.Client) Result {
	t.Helper()
	res, err := Fetch(context.Background(), "https://paywall.test/a", Options{
		HTTP: client, KarakeepURL: "http://kk.test", KarakeepKey: "k", Route: RouteDefault,
		Look:         func(string) (string, error) { return "", errNotFaked }, // no local tools
		KarakeepPoll: time.Millisecond, KarakeepWait: time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func TestKarakeepCrawlHidesOnlyWhatItCreated(t *testing.T) {
	f, client := karakeepFake(t, false, false)
	res := crawlOnly(t, client)
	if res.Rung != RungKarakeepCrawl || !res.OK() {
		t.Fatalf("got %+v", res)
	}
	if !f.has("PATCH kk.test /api/v1/bookmarks/b1") || !f.has("POST kk.test /api/v1/bookmarks/b1/tags") {
		t.Fatalf("created bookmark not archived+tagged: %v", f.reqs)
	}

	f, client = karakeepFake(t, true, false)
	res = crawlOnly(t, client)
	if res.Rung != RungKarakeepCrawl || !res.OK() {
		t.Fatalf("got %+v", res)
	}
	if f.has("PATCH") || f.has("/tags") {
		t.Fatalf("JA's own bookmark was touched: %v", f.reqs)
	}
}

func TestKarakeepLookupHit(t *testing.T) {
	_, client := newNet(t, map[string]http.HandlerFunc{
		"kk.test": func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/api/v1/bookmarks/search" {
				if !strings.HasPrefix(r.URL.Query().Get("q"), "url:") {
					http.Error(w, "q", 400)
					return
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"bookmarks": []any{
					map[string]any{"id": "b9", "content": map[string]any{"url": "https://www.saved.test/a/"}}}})
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"id": "b9", "content": map[string]any{"crawlStatus": "success", "htmlContent": articleText}})
		},
	})
	res, _ := Fetch(context.Background(), "https://saved.test/a", Options{
		HTTP: client, KarakeepURL: "http://kk.test", KarakeepKey: "k", Look: allPresent,
		Run: fakeRunner(t, nil, nil),
	})
	if res.Rung != RungKarakeepLookup || res.Pos != 2 || !res.OK() {
		t.Fatalf("got %+v", res)
	}
}

func TestMaxRungMapsToChosenOrder(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ledger.json")
	_ = Ledger{"example.com": {Rung: RungObscura, OKCount: 1}}.Save(path)
	var calls []string
	res, _ := Fetch(context.Background(), "https://example.com/a", Options{
		LedgerPath: path, MaxRung: 4, Look: allPresent, ChallengeWait: noWait(t),
		Run: fakeRunner(t, map[string]string{"obscura": blockedText}, &calls),
	})
	// order: hister, karakeep-lookup, obscura, karakeep-crawl | headless...
	if len(res.Tried) != 4 || res.Tried[3].Rung != RungKarakeepCrawl {
		t.Fatalf("tried = %+v", res.Tried)
	}
	if strings.Contains(strings.Join(calls, "|"), "agent-browser") {
		t.Fatalf("climbed past --max-rung 4: %v", calls)
	}
}

func TestDomainAndNormalize(t *testing.T) {
	for in, want := range map[string]string{
		"https://www.reddit.com/r/x": "reddit.com", "https://old.reddit.com/x": "reddit.com",
		"https://news.bbc.co.uk/a": "bbc.co.uk", "https://redd.it/abc": "redd.it",
	} {
		if got := Domain(in); got != want {
			t.Errorf("Domain(%q) = %q, want %q", in, got, want)
		}
	}
	if NormalizeURL("https://www.x.test/a/?utm_source=n&id=2#c") != NormalizeURL("https://x.test/a?id=2") {
		t.Fatal("normalize mismatch")
	}
}

func TestHisterVariants(t *testing.T) {
	got := strings.Join(histerVariants("https://reddit.com/r/x"), " ")
	for _, w := range []string{"https://reddit.com/r/x", "https://reddit.com/r/x/", "https://www.reddit.com/r/x", "https://www.reddit.com/r/x/"} {
		if !strings.Contains(got+" ", w+" ") {
			t.Fatalf("variants %q missing %q", got, w)
		}
	}
}
