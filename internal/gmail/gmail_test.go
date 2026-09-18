package gmail

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/oauth2"
	gmailapi "google.golang.org/api/gmail/v1"
	"google.golang.org/api/option"
)

// fakeRT answers every Gmail call with a canned status/body.
type fakeRT struct {
	status int
	body   string
	calls  []string
}

func (f *fakeRT) RoundTrip(r *http.Request) (*http.Response, error) {
	b, _ := io.ReadAll(r.Body)
	f.calls = append(f.calls, r.URL.Path+" "+string(b))
	return &http.Response{
		StatusCode: f.status,
		Body:       io.NopCloser(strings.NewReader(f.body)),
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Request:    r,
	}, nil
}

func svc(t *testing.T, rt http.RoundTripper) *gmailapi.Service {
	t.Helper()
	s, err := gmailapi.NewService(context.Background(),
		option.WithHTTPClient(&http.Client{Transport: rt}), option.WithoutAuthentication())
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func entry(acct, id string, at time.Time) Entry {
	return Entry{Account: acct, ID: id, StagedAt: at.UTC().Format(time.RFC3339)}
}

func TestStageIdempotent(t *testing.T) {
	p := filepath.Join(t.TempDir(), "stage.jsonl")
	now := time.Now()
	if n, err := Stage(p, []Entry{entry("a@x", "1", now), entry("a@x", "2", now)}); err != nil || n != 2 {
		t.Fatalf("first stage: %d %v", n, err)
	}
	// same (account,id) again, plus one new
	if n, err := Stage(p, []Entry{entry("a@x", "1", now), entry("b@x", "1", now)}); err != nil || n != 1 {
		t.Fatalf("second stage: %d %v", n, err)
	}
	es, err := Read(p)
	if err != nil || len(es) != 3 {
		t.Fatalf("read: %d entries %v", len(es), err)
	}
}

func TestReadMissingFile(t *testing.T) {
	es, err := Read(filepath.Join(t.TempDir(), "nope.jsonl"))
	if err != nil || es != nil {
		t.Fatalf("missing stage file must be empty: %v %v", es, err)
	}
}

func TestApplyHappyPath(t *testing.T) {
	dir := t.TempDir()
	stage := filepath.Join(dir, "stage.jsonl")
	now := time.Now()
	if _, err := Stage(stage, []Entry{entry("a@x", "1", now), entry("a@x", "2", now)}); err != nil {
		t.Fatal(err)
	}
	rt := &fakeRT{status: 204, body: ""}
	res, err := Apply(context.Background(), Options{
		Stage: stage, Applied: AppliedPath(stage),
		NewService: func(ctx context.Context, a string) (*gmailapi.Service, error) { return svc(t, rt), nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Applied != 2 || len(res.Failed) != 0 {
		t.Fatalf("want 2 applied, got %+v", res)
	}
	if len(rt.calls) != 1 || !strings.Contains(rt.calls[0], "batchModify") || !strings.Contains(rt.calls[0], "TRASH") {
		t.Fatalf("calls: %v", rt.calls)
	}
	if es, _ := Read(stage); len(es) != 0 {
		t.Fatalf("stage must be empty: %v", es)
	}
	done, _ := Read(AppliedPath(stage))
	if len(done) != 2 || done[0].AppliedAt == "" {
		t.Fatalf("applied ledger: %+v", done)
	}
}

func TestApplyAccountFilterSkips(t *testing.T) {
	stage := filepath.Join(t.TempDir(), "stage.jsonl")
	now := time.Now()
	Stage(stage, []Entry{entry("a@x", "1", now), entry("b@x", "2", now)})
	rt := &fakeRT{status: 204}
	res, err := Apply(context.Background(), Options{
		Stage: stage, Applied: AppliedPath(stage), Account: "a@x",
		NewService: func(ctx context.Context, a string) (*gmailapi.Service, error) { return svc(t, rt), nil },
	})
	if err != nil || res.Applied != 1 || res.Skipped != 1 {
		t.Fatalf("got %+v %v", res, err)
	}
	if es, _ := Read(stage); len(es) != 1 || es[0].Account != "b@x" {
		t.Fatalf("other account must stay staged: %+v", es)
	}
}

func TestApplyFailureKeepsEntries(t *testing.T) {
	stage := filepath.Join(t.TempDir(), "stage.jsonl")
	now := time.Now()
	Stage(stage, []Entry{entry("a@x", "1", now)})
	rt := &fakeRT{status: 500, body: `{"error":{"message":"boom"}}`}
	res, err := Apply(context.Background(), Options{
		Stage: stage, Applied: AppliedPath(stage),
		NewService: func(ctx context.Context, a string) (*gmailapi.Service, error) { return svc(t, rt), nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Applied != 0 || len(res.Failed) != 1 || res.Failed[0].Attempts != 1 {
		t.Fatalf("got %+v", res)
	}
	es, _ := Read(stage)
	if len(es) != 1 || es[0].Attempts != 1 || es[0].Error == "" {
		t.Fatalf("failed entry must stay with attempts++: %+v", es)
	}
	// a second failing run increments again
	Apply(context.Background(), Options{Stage: stage, Applied: AppliedPath(stage),
		NewService: func(ctx context.Context, a string) (*gmailapi.Service, error) { return svc(t, rt), nil }})
	es, _ = Read(stage)
	if len(es) != 1 || es[0].Attempts != 2 {
		t.Fatalf("attempts must accumulate: %+v", es)
	}
}

func TestApplyDryRunTouchesNothing(t *testing.T) {
	stage := filepath.Join(t.TempDir(), "stage.jsonl")
	Stage(stage, []Entry{entry("a@x", "1", time.Now())})
	res, err := Apply(context.Background(), Options{Stage: stage, Applied: AppliedPath(stage), DryRun: true,
		NewService: func(ctx context.Context, a string) (*gmailapi.Service, error) {
			t.Fatal("dry run must not build a client")
			return nil, nil
		}})
	if err != nil || res.Applied != 1 {
		t.Fatalf("got %+v %v", res, err)
	}
	if es, _ := Read(stage); len(es) != 1 {
		t.Fatalf("dry run must leave the stage file: %+v", es)
	}
}

func TestGate(t *testing.T) {
	now := time.Now()
	es := []Entry{entry("a@x", "1", now)}
	if err := Gate("newsletters", now.Add(-time.Hour), true, es); err == nil {
		t.Fatal("an older ok run must be refused")
	}
	if err := Gate("newsletters", now.Add(time.Minute), true, es); err != nil {
		t.Fatalf("a newer ok run must pass: %v", err)
	}
	if err := Gate("newsletters", time.Time{}, false, es); err == nil {
		t.Fatal("no ok run must be refused")
	}
	if err := Gate("newsletters", time.Time{}, true, nil); err != nil {
		t.Fatalf("nothing staged is nothing to refuse: %v", err)
	}
}

func TestTokenSourcePersistsRefresh(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"access_token":"fresh","refresh_token":"r2","token_type":"Bearer","expires_in":3600}`)
	}))
	defer srv.Close()
	cfg := &oauth2.Config{ClientID: "c", ClientSecret: "s", Endpoint: oauth2.Endpoint{TokenURL: srv.URL}}
	old := &oauth2.Token{AccessToken: "stale", RefreshToken: "r1", Expiry: time.Now().Add(-time.Hour)}
	var saved bytes.Buffer
	src := TokenSource(context.Background(), cfg, old, func(tok *oauth2.Token) error {
		b, err := json.Marshal(tok)
		saved.Write(b)
		return err
	})
	got, err := src.Token()
	if err != nil {
		t.Fatal(err)
	}
	if got.AccessToken != "fresh" {
		t.Fatalf("token not refreshed: %+v", got)
	}
	if !strings.Contains(saved.String(), "fresh") {
		t.Fatalf("refreshed token not persisted: %q", saved.String())
	}
	// a second call reuses the live token: nothing new is written
	n := saved.Len()
	if _, err := src.Token(); err != nil {
		t.Fatal(err)
	}
	if saved.Len() != n {
		t.Fatalf("a live token must not be re-saved: %q", saved.String())
	}
}

func TestParseClientSecrets(t *testing.T) {
	oc, err := ParseClientSecrets([]byte(`{"installed":{"client_id":"cid","client_secret":"sec","auth_uri":"https://a","token_uri":"https://t"}}`))
	if err != nil || oc.ClientID != "cid" || oc.Endpoint.TokenURL != "https://t" || oc.Scopes[0] != Scope {
		t.Fatalf("installed block: %+v %v", oc, err)
	}
	if oc, err := ParseClientSecrets([]byte(`{"web":{"client_id":"w"}}`)); err != nil || oc.ClientID != "w" {
		t.Fatalf("web block: %+v %v", oc, err)
	}
	if _, err := ParseClientSecrets([]byte(`{}`)); err == nil {
		t.Fatal("a client JSON without installed/web must fail")
	}
}
