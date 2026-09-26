package decide

import (
	"context"
	"encoding/json"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// tok is one candidate in a position's top_logprobs.
type tok struct {
	s string
	p float64 // probability; stored as its log
}

// lemonade builds a fake Lemonade reply: one entry per generated position.
func lemonade(content string, positions [][]tok) map[string]any {
	var entries []any
	for _, pos := range positions {
		var top []any
		for _, c := range pos {
			top = append(top, map[string]any{"token": c.s, "logprob": math.Log(c.p)})
		}
		entries = append(entries, map[string]any{
			"token": pos[0].s, "logprob": math.Log(pos[0].p), "top_logprobs": top,
		})
	}
	return map[string]any{"choices": []any{map[string]any{
		"message":  map[string]any{"content": content},
		"logprobs": map[string]any{"content": entries},
	}}}
}

// fake serves one canned body and records the requests it saw.
func fake(t *testing.T, handler http.HandlerFunc) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return srv
}

func serveBody(t *testing.T, body map[string]any, seen *[]map[string]any) *httptest.Server {
	return fake(t, func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var req map[string]any
		json.Unmarshal(raw, &req)
		if seen != nil {
			*seen = append(*seen, req)
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(body)
	})
}

func mustAsk(t *testing.T, req AskRequest) AskResult {
	t.Helper()
	res, err := Ask(context.Background(), req)
	if err != nil {
		t.Fatalf("Ask: %v", err)
	}
	return res
}

func TestAskLogprobs(t *testing.T) {
	var seen []map[string]any
	srv := serveBody(t, lemonade("archive", [][]tok{{
		{"arch", 0.9}, {"resp", 0.06}, {"un", 0.04},
	}}), &seen)
	res := mustAsk(t, AskRequest{
		Kind: "choice", Options: []string{"respond", "archive"},
		Question: "what now?", Text: "hello", URL: srv.URL,
	})
	if res.Label != "archive" {
		t.Fatalf("label = %q, want archive", res.Label)
	}
	if math.Abs(res.Conf-0.9) > 1e-4 {
		t.Fatalf("conf = %v, want 0.9", res.Conf)
	}
	if len(res.Dist) != 3 || res.Dist["unknown"] == 0 {
		t.Fatalf("dist = %v, want all three options", res.Dist)
	}
	sum := 0.0
	for _, p := range res.Dist {
		sum += p
	}
	if math.Abs(sum-1) > 1e-3 {
		t.Fatalf("dist does not sum to 1: %v", res.Dist)
	}
	// The request must carry the determinism and no-thinking settings, and
	// the prefilled assistant turn.
	req := seen[0]
	if req["temperature"] != float64(0) || req["logprobs"] != true || req["top_logprobs"] != float64(10) {
		t.Fatalf("payload missing determinism/logprob settings: %v", req)
	}
	if req["reasoning"] != false {
		t.Fatalf("reasoning must be false: %v", req["reasoning"])
	}
	msgs := req["messages"].([]any)
	if len(msgs) != 2 || msgs[1].(map[string]any)["content"] != "Answer:" {
		t.Fatalf("want a prefilled assistant turn, got %v", msgs)
	}
}

func TestAskSkipsThinkingAndWhitespace(t *testing.T) {
	// Position 0 is a preamble word, position 1 is pure whitespace (its
	// candidates would match "yes" but the position emitted nothing), the
	// answer is at position 2.
	srv := serveBody(t, lemonade("First, yes", [][]tok{
		{{"First", 0.8}, {"Hmm", 0.2}},
		{{" ", 0.99}, {"ye", 0.01}},
		{{"ye", 0.95}, {"no", 0.03}, {"un", 0.02}},
	}), nil)
	res := mustAsk(t, AskRequest{Kind: "noul", Text: "ok?", URL: srv.URL})
	if res.Label != "yes" || math.Abs(res.Conf-0.95) > 1e-4 {
		t.Fatalf("got %+v, want yes @0.95", res)
	}
}

func TestAskFloorGivesUnknown(t *testing.T) {
	srv := serveBody(t, lemonade("yes", [][]tok{{{"ye", 0.6}, {"no", 0.4}}}), nil)
	res := mustAsk(t, AskRequest{Kind: "noul", Text: "ok?", URL: srv.URL, Floor: 0.85})
	if res.Label != Unknown {
		t.Fatalf("label = %q, want unknown below the floor", res.Label)
	}
	if math.Abs(res.Conf-0.6) > 1e-4 {
		t.Fatalf("conf must survive the escape, got %v", res.Conf)
	}
}

func TestAskRetriesWithoutPrefill(t *testing.T) {
	var seen []map[string]any
	body := lemonade("yes", [][]tok{{{"ye", 0.99}, {"no", 0.01}}})
	srv := fake(t, func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var req map[string]any
		json.Unmarshal(raw, &req)
		seen = append(seen, req)
		if len(req["messages"].([]any)) > 1 {
			http.Error(w, "assistant turn must be last", http.StatusBadRequest)
			return
		}
		json.NewEncoder(w).Encode(body)
	})
	res := mustAsk(t, AskRequest{Kind: "noul", Text: "ok?", URL: srv.URL})
	if res.Label != "yes" {
		t.Fatalf("got %+v, want yes after the retry", res)
	}
	if len(seen) != 2 {
		t.Fatalf("want 2 requests (prefill, then without), got %d", len(seen))
	}
	if len(seen[1]["messages"].([]any)) != 1 {
		t.Fatalf("the retry must drop the prefilled turn: %v", seen[1]["messages"])
	}
}

func TestAskTextFallbackLastWord(t *testing.T) {
	// No logprobs at all: the answer is read from the text, last word wins.
	body := map[string]any{"choices": []any{map[string]any{
		"message": map[string]any{"content": "Answer: the user is asking for a call, so respond"},
	}}}
	srv := serveBody(t, body, nil)
	res := mustAsk(t, AskRequest{
		Kind: "choice", Options: []string{"respond", "archive"}, Text: "x",
		URL: srv.URL, Floor: 0.5,
	})
	if res.Label != "respond" {
		t.Fatalf("label = %q, want respond", res.Label)
	}
	if res.DistFrom != "text" {
		t.Fatalf("dist_from = %q, want text", res.DistFrom)
	}
	if math.Abs(res.Conf-0.6) > 1e-4 {
		t.Fatalf("conf = %v, want the text confidence 0.6", res.Conf)
	}
}

func TestAskScore(t *testing.T) {
	srv := serveBody(t, lemonade("72", [][]tok{{{"7", 0.92}, {"8", 0.05}}}), nil)
	res := mustAsk(t, AskRequest{Kind: "score", Text: "how urgent?", URL: srv.URL, Floor: 0.85})
	if res.Value == nil || *res.Value != 72 {
		t.Fatalf("value = %v, want 72", res.Value)
	}
	if res.Dist != nil {
		t.Fatalf("a score carries no dist, got %v", res.Dist)
	}
	raw, _ := json.Marshal(res)
	if !strings.Contains(string(raw), `"value":72`) || strings.Contains(string(raw), `"label"`) {
		t.Fatalf("score JSON = %s", raw)
	}

	// Same answer, higher floor: undecided, and the JSON says so with null.
	low := serveBody(t, lemonade("72", [][]tok{{{"7", 0.5}, {"8", 0.5}}}), nil)
	res = mustAsk(t, AskRequest{Kind: "score", Text: "how urgent?", URL: low.URL, Floor: 0.85})
	if res.Value != nil {
		t.Fatalf("value = %v, want nil below the floor", *res.Value)
	}
	raw, _ = json.Marshal(res)
	if !strings.Contains(string(raw), `"value":null`) {
		t.Fatalf("score JSON = %s, want a null value", raw)
	}
}

func TestAskServerDown(t *testing.T) {
	srv := serveBody(t, nil, nil)
	url := srv.URL
	srv.Close()
	res := mustAsk(t, AskRequest{Kind: "noul", Text: "ok?", URL: url})
	if res.Label != Unknown || res.Conf != 0 {
		t.Fatalf("got %+v, want unknown @0", res)
	}
	if res.Error != "lemonade unreachable" {
		t.Fatalf("error = %q, want lemonade unreachable", res.Error)
	}
}

func TestAskHTTPErrorSurfaced(t *testing.T) {
	srv := fake(t, func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "model not found: no-such-model", http.StatusNotFound)
	})
	res := mustAsk(t, AskRequest{Kind: "noul", Text: "ok?", URL: srv.URL})
	if res.Label != Unknown {
		t.Fatalf("label = %q, want unknown", res.Label)
	}
	if !strings.HasPrefix(res.Error, "http 404: ") || !strings.Contains(res.Error, "no-such-model") {
		t.Fatalf("error = %q, want the 404 body", res.Error)
	}
}

func TestAskJevNeedsPublic(t *testing.T) {
	_, err := Ask(context.Background(), AskRequest{Kind: "noul", Text: "x", Route: "jev"})
	if err == nil || !strings.Contains(err.Error(), "jev route requires public input") {
		t.Fatalf("err = %v, want the public-input refusal", err)
	}
}

func TestAskJevDisabled(t *testing.T) {
	res := mustAsk(t, AskRequest{Kind: "noul", Text: "x", Route: "jev", Public: true})
	if res.Label != Unknown || res.Error != "jev disabled" {
		t.Fatalf("got %+v, want unknown / jev disabled", res)
	}
	if res.Model != "jev" || res.Route != "jev" {
		t.Fatalf("route/model = %s/%s, want jev/jev", res.Route, res.Model)
	}
}

func TestAskBadKind(t *testing.T) {
	if _, err := Ask(context.Background(), AskRequest{Kind: "guess", Text: "x"}); err == nil {
		t.Fatal("want an error for an unknown kind")
	}
	if _, err := Ask(context.Background(), AskRequest{Kind: "choice", Text: "x"}); err == nil {
		t.Fatal("want an error for a choice with no options")
	}
}

func TestTraceHasNoRawText(t *testing.T) {
	dir := t.TempDir()
	srv := serveBody(t, lemonade("yes", [][]tok{{{"ye", 0.99}, {"no", 0.01}}}), nil)
	secret := "the contract redline is attached, Marta"
	res, err := AskAndTrace(context.Background(), dir, AskRequest{
		Kind: "noul", Text: secret, Question: "reply?", URL: srv.URL, Caller: "test",
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Label != "yes" {
		t.Fatalf("label = %q", res.Label)
	}
	raw, err := os.ReadFile(filepath.Join(dir, TraceFile))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "Marta") || strings.Contains(string(raw), "redline") {
		t.Fatalf("the trace must never carry the text: %s", raw)
	}
	var line map[string]any
	if err := json.Unmarshal(raw, &line); err != nil {
		t.Fatal(err)
	}
	if line["text_sha1"] != SHA1Hex(secret) {
		t.Fatalf("text_sha1 = %v", line["text_sha1"])
	}
	if line["question_sha1"] != SHA1Hex("reply?") {
		t.Fatalf("question_sha1 = %v", line["question_sha1"])
	}
	if line["text_len"].(float64) != float64(len([]rune(secret))) {
		t.Fatalf("text_len = %v", line["text_len"])
	}
	for _, k := range []string{"ts", "kind", "options", "label", "conf", "dist", "route", "model", "floor", "policy_version", "ms", "caller"} {
		if _, ok := line[k]; !ok {
			t.Fatalf("trace line is missing %q: %s", k, raw)
		}
	}
	if line["policy_version"] != DefaultPolicyVersion {
		t.Fatalf("policy_version = %v", line["policy_version"])
	}
	// A decided outcome leaves no escape.
	if _, err := os.Stat(filepath.Join(dir, AskEscapesFile)); !os.IsNotExist(err) {
		t.Fatalf("a decided ask must not write an escape (%v)", err)
	}
}

func TestEscapeOnlyOnUnknown(t *testing.T) {
	dir := t.TempDir()
	long := strings.Repeat("x", 800)
	srv := serveBody(t, lemonade("yes", [][]tok{{{"ye", 0.5}, {"no", 0.5}}}), nil)
	if _, err := AskAndTrace(context.Background(), dir, AskRequest{
		Kind: "noul", Text: long, URL: srv.URL, Floor: 0.85,
	}); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, AskEscapesFile))
	if err != nil {
		t.Fatalf("an unknown must write an escape: %v", err)
	}
	var esc map[string]any
	if err := json.Unmarshal(raw, &esc); err != nil {
		t.Fatal(err)
	}
	if esc["label"] != Unknown {
		t.Fatalf("escape label = %v", esc["label"])
	}
	if got := esc["text"].(string); len(got) != EscapeTextChars {
		t.Fatalf("escape text is %d chars, want %d", len(got), EscapeTextChars)
	}
	lines, err := TraceTail(dir, 10)
	if err != nil || len(lines) != 1 {
		t.Fatalf("TraceTail = %v, %v", lines, err)
	}
}

func TestWithUnknown(t *testing.T) {
	got := WithUnknown([]string{"a", "unknown", "b"})
	if strings.Join(got, ",") != "a,b,unknown" {
		t.Fatalf("WithUnknown = %v", got)
	}
	if strings.Join(WithUnknown(nil), ",") != Unknown {
		t.Fatalf("unknown must always be in the set")
	}
}
