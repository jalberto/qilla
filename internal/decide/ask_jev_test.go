package decide

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// jevServer serves one canned System One reply and records what it was sent.
func jevServer(t *testing.T, answer map[string]any, seen *map[string]any, hdr *http.Header) *httptest.Server {
	t.Helper()
	return fake(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != JevPath {
			t.Errorf("path = %q, want %q", r.URL.Path, JevPath)
		}
		raw, _ := io.ReadAll(r.Body)
		if seen != nil {
			var body map[string]any
			if err := json.Unmarshal(raw, &body); err != nil {
				t.Errorf("request body: %v", err)
			}
			*seen = body
		}
		if hdr != nil {
			*hdr = r.Header.Clone()
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"model":   "jev-2026-09-15",
			"answers": map[string]any{"q": answer},
			"usage":   map[string]any{"input_tokens": 120, "output_tokens": 12},
		})
	})
}

func jevReq(srv *httptest.Server, req AskRequest) AskRequest {
	req.Route, req.Public, req.JevEnabled = "jev", true, true
	req.JevURL, req.JevKey = srv.URL, "sekrit"
	req.JevDailyMax = -1 // the cap has its own test
	return req
}

func TestAskJevChoice(t *testing.T) {
	var seen map[string]any
	var hdr http.Header
	srv := jevServer(t, map[string]any{
		"type": "choice", "choice": "archive", "confidence": 0.93,
		"probabilities": map[string]any{"archive": 0.8, "respond": 0.2},
	}, &seen, &hdr)
	res := mustAsk(t, jevReq(srv, AskRequest{
		Kind: "choice", Options: []string{"respond", "archive"},
		Question: "what now?", Text: "hello", Floor: 0.5,
	}))

	if got := hdr.Get("Authorization"); got != "Bearer sekrit" {
		t.Fatalf("Authorization = %q, want the bearer key", got)
	}
	if seen["state"] != "hello" || seen["model"] != DefaultJevModel {
		t.Fatalf("state/model = %v/%v", seen["state"], seen["model"])
	}
	q, ok := seen["questions"].(map[string]any)["q"].(map[string]any)
	if !ok {
		t.Fatalf("questions = %v, want a q", seen["questions"])
	}
	if q["type"] != "choice" || q["instructions"] != "what now?" {
		t.Fatalf("question = %v", q)
	}
	criteria, ok := q["criteria"].(map[string]any)
	if !ok || len(criteria) != 2 {
		t.Fatalf("criteria = %v, want the two options", q["criteria"])
	}
	for _, o := range []string{"respond", "archive"} {
		if v, ok := criteria[o]; !ok || v != nil {
			t.Fatalf("criteria[%s] = %v (present %v), want an undescribed label", o, v, ok)
		}
	}
	if _, ok := criteria[Unknown]; ok {
		t.Fatal("unknown is the local escape and must not be sent as a criterion")
	}

	if res.Label != "archive" || res.Route != "jev" || res.Model != "jev-2026-09-15" {
		t.Fatalf("got %+v, want archive over jev", res)
	}
	if math.Abs(res.Conf-0.93) > 1e-9 {
		t.Fatalf("conf = %v, want the API's confidence", res.Conf)
	}
	if len(res.Dist) != 2 || math.Abs(res.Dist["archive"]-0.8) > 1e-9 {
		t.Fatalf("dist = %v", res.Dist)
	}
	if res.Tokens != 120 {
		t.Fatalf("tokens = %d, want the API's input_tokens", res.Tokens)
	}
	if res.Error != "" {
		t.Fatalf("error = %q, want none", res.Error)
	}
}

func TestAskJevNoul(t *testing.T) {
	var seen map[string]any
	srv := jevServer(t, map[string]any{"type": "noul", "noul": 0.98}, &seen, nil)
	res := mustAsk(t, jevReq(srv, AskRequest{Kind: "noul", Question: "spam?", Text: "buy now"}))

	q := seen["questions"].(map[string]any)["q"].(map[string]any)
	if q["type"] != "noul" || q["instructions"] != "spam?" {
		t.Fatalf("question = %v", q)
	}
	if _, ok := q["criteria"]; ok {
		t.Fatalf("noul with no described outcomes must not send criteria: %v", q)
	}
	if res.Label != "yes" || math.Abs(res.Conf-0.98) > 1e-9 {
		t.Fatalf("got %+v, want yes at 0.98", res)
	}
	if math.Abs(res.Dist["yes"]-0.98) > 1e-4 || math.Abs(res.Dist["no"]-0.02) > 1e-4 {
		t.Fatalf("dist = %v, want yes/no", res.Dist)
	}
}

func TestAskJevScore(t *testing.T) {
	var seen map[string]any
	srv := jevServer(t, map[string]any{
		"type": "score", "score": 1.7, "confidence": 0.9,
		"legend":        map[string]any{"0": "can wait", "1": "this week", "2": "today"},
		"probabilities": map[string]any{"0": 0.1, "1": 0.1, "2": 0.8},
	}, &seen, nil)
	res := mustAsk(t, jevReq(srv, AskRequest{
		Kind: "score", Options: []string{"can wait", "this week", "today"},
		Question: "how urgent?", Text: "the roof is on fire", Floor: 0.5,
	}))

	q := seen["questions"].(map[string]any)["q"].(map[string]any)
	criteria, ok := q["criteria"].([]any)
	if q["type"] != "score" || !ok || len(criteria) != 3 || criteria[0] != "can wait" {
		t.Fatalf("question = %v, want the ordered rubric", q)
	}
	if res.Kind != "score" || res.Value == nil || *res.Value != 2 {
		t.Fatalf("got %+v, want the expected score rounded to 2", res)
	}
	if math.Abs(res.Conf-0.9) > 1e-9 {
		t.Fatalf("conf = %v", res.Conf)
	}
}

// Without options a score is asked over the local route's 0-100 scale, so the
// value means the same thing on both routes.
func TestAskJevScoreDefaultRubric(t *testing.T) {
	var seen map[string]any
	srv := jevServer(t, map[string]any{"type": "score", "score": 42.4, "confidence": 0.9}, &seen, nil)
	res := mustAsk(t, jevReq(srv, AskRequest{Kind: "score", Text: "x", Floor: 0.5}))

	criteria := seen["questions"].(map[string]any)["q"].(map[string]any)["criteria"].([]any)
	if len(criteria) != 101 || criteria[0] != "0" || criteria[100] != "100" {
		t.Fatalf("criteria = %d levels (%v … %v), want 0-100", len(criteria), criteria[0], criteria[len(criteria)-1])
	}
	if res.Value == nil || *res.Value != 42 {
		t.Fatalf("value = %v, want 42", res.Value)
	}
}

// A 429 is retried once, honouring Retry-After; a second one gives up.
func TestAskJevRateLimited(t *testing.T) {
	calls := 0
	srv := fake(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Header().Set("Retry-After", "0")
		w.WriteHeader(http.StatusTooManyRequests)
	})
	res := mustAsk(t, jevReq(srv, AskRequest{Kind: "noul", Text: "x"}))
	if res.Label != Unknown || res.Error != "jev rate limited" {
		t.Fatalf("got %+v, want unknown / jev rate limited", res)
	}
	if calls != 2 {
		t.Fatalf("calls = %d, want the one retry", calls)
	}
}

func TestAskJevHTTPErrorHidesBody(t *testing.T) {
	srv := fake(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		fmt.Fprint(w, `{"detail":"bad key sekrit"}`)
	})
	res := mustAsk(t, jevReq(srv, AskRequest{Kind: "noul", Text: "x"}))
	if res.Error != "jev http 401" {
		t.Fatalf("error = %q, want the bare status", res.Error)
	}
	if strings.Contains(res.Error, "sekrit") {
		t.Fatal("the error must never carry the body")
	}
}

func TestAskJevDailyCap(t *testing.T) {
	dir := t.TempDir()
	today := time.Now().UTC().Format("2006-01-02T15:04:05Z")
	var lines []string
	for i := 0; i < 2; i++ {
		lines = append(lines, fmt.Sprintf(`{"ts":%q,"route":"jev","label":"yes"}`, today))
	}
	// Neither of these counts: a local route today, a jev call yesterday.
	lines = append(lines,
		fmt.Sprintf(`{"ts":%q,"route":"local","label":"yes"}`, today),
		`{"ts":"2020-01-01T00:00:00Z","route":"jev","label":"yes"}`)
	if err := os.WriteFile(filepath.Join(dir, TraceFile), []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if n := CountRouteToday(dir, "jev"); n != 2 {
		t.Fatalf("CountRouteToday = %d, want 2", n)
	}

	srv := jevServer(t, map[string]any{"type": "noul", "noul": 0.98}, nil, nil)
	req := jevReq(srv, AskRequest{Kind: "noul", Text: "x"})
	req.JevDailyMax, req.TraceDir = 2, dir
	res := mustAsk(t, req)
	if res.Label != Unknown || res.Error != "jev daily cap" {
		t.Fatalf("got %+v, want unknown / jev daily cap", res)
	}

	req.JevDailyMax = 3 // under the cap the call goes out
	if res := mustAsk(t, req); res.Error != "" || res.Label != "yes" {
		t.Fatalf("got %+v, want the answer under the cap", res)
	}
}

func TestAskJevRefusesNonPublic(t *testing.T) {
	srv := jevServer(t, map[string]any{"type": "noul", "noul": 0.98}, nil, nil)
	req := jevReq(srv, AskRequest{Kind: "noul", Text: "x"})
	req.Public = false
	if _, err := Ask(context.Background(), req); err == nil ||
		!strings.Contains(err.Error(), "jev route requires public input") {
		t.Fatalf("err = %v, want the public-input refusal", err)
	}
}

// A jev failure is never a silent fall back to the local route.
func TestAskJevNeverFallsBack(t *testing.T) {
	srv := fake(t, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusInternalServerError) })
	req := jevReq(srv, AskRequest{Kind: "choice", Options: []string{"a", "b"}, Text: "x"})
	req.URL = "http://127.0.0.1:1" // a local backend that would answer nothing anyway
	res := mustAsk(t, req)
	if res.Route != "jev" || res.Label != Unknown || res.Error != "jev http 500" {
		t.Fatalf("got %+v, want unknown over jev", res)
	}
}

// The trace records the billed input tokens.
func TestAskJevTraceTokens(t *testing.T) {
	dir := t.TempDir()
	srv := jevServer(t, map[string]any{"type": "noul", "noul": 0.98}, nil, nil)
	if _, err := AskAndTrace(context.Background(), dir, jevReq(srv, AskRequest{Kind: "noul", Text: "hello"})); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, TraceFile))
	if err != nil {
		t.Fatal(err)
	}
	var line map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(string(raw))), &line); err != nil {
		t.Fatal(err)
	}
	if line["tokens"] != float64(120) {
		t.Fatalf("tokens = %v, want 120", line["tokens"])
	}
	if strings.Contains(string(raw), "hello") {
		t.Fatal("the trace must never carry the text")
	}
}
