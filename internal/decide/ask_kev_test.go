package decide

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// statusServer answers every request with one status and counts the calls.
func statusServer(t *testing.T, status int, calls *int) string {
	t.Helper()
	return fake(t, func(w http.ResponseWriter, _ *http.Request) {
		*calls++
		w.WriteHeader(status)
	}).URL
}

func noulYes() map[string]any { return map[string]any{"type": "noul", "noul": 0.97} }

func TestAskLocalGoesToKev(t *testing.T) {
	var seen map[string]any
	var hdr http.Header
	kev := jevServer(t, noulYes(), &seen, &hdr)
	res := mustAsk(t, AskRequest{Kind: "noul", Text: "x", Route: "local", KevURL: kev.URL})
	if res.Route != "local" || res.Endpoint != EndpointKev || res.Label != "yes" {
		t.Fatalf("got %+v, want local/kev/yes", res)
	}
	if seen["model"] != DefaultKevModel {
		t.Fatalf("model = %v, want %s", seen["model"], DefaultKevModel)
	}
	if hdr.Get("Authorization") != "" {
		t.Fatal("kev without a key must send no bearer")
	}
	mustAsk(t, AskRequest{Kind: "noul", Text: "x", KevURL: kev.URL, KevKey: "k", KevModel: "kev-9"})
	if hdr.Get("Authorization") != "Bearer k" || seen["model"] != "kev-9" {
		t.Fatalf("auth %q model %v", hdr.Get("Authorization"), seen["model"])
	}
}

func TestAskNoKevURLFallsBackToLemonade(t *testing.T) {
	srv := fake(t, func(w http.ResponseWriter, _ *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"message": map[string]any{"content": "yes"}}}})
	})
	res := mustAsk(t, AskRequest{Kind: "noul", Text: "x", URL: srv.URL, Floor: 0.5})
	if res.Endpoint != EndpointLemonade || res.Route != "local" || res.Label != "yes" {
		t.Fatalf("got %+v, want local/lemonade/yes", res)
	}
}

func TestResolveRoute(t *testing.T) {
	cases := []struct {
		req  AskRequest
		want string
	}{
		{AskRequest{}, "local"},
		{AskRequest{Route: "auto", Public: true}, "local"}, // jev disabled
		{AskRequest{Route: "auto", Public: true, JevEnabled: true}, "jev"},
		{AskRequest{Public: true, JevEnabled: true}, "jev"},
		{AskRequest{Route: "auto", JevEnabled: true}, "local"}, // not public
		{AskRequest{Route: "auto", Public: true, JevEnabled: true, DefaultRoute: "local"}, "local"},
		{AskRequest{Route: "local", Public: true, JevEnabled: true}, "local"},
		{AskRequest{Route: "jev", Public: true}, "jev"},
	}
	for i, c := range cases {
		if got := c.req.ResolveRoute(); got != c.want {
			t.Errorf("case %d: %+v → %s, want %s", i, c.req, got, c.want)
		}
	}
	if err := (AskRequest{Kind: "noul", Route: "auto"}).Validate(); err != nil {
		t.Fatalf("auto must validate: %v", err)
	}
	if err := (AskRequest{Kind: "noul", Route: "jev"}).Validate(); err == nil {
		t.Fatal("explicit jev without public must stay an error")
	}
}

func TestAskAutoPublicGoesJev(t *testing.T) {
	jev := jevServer(t, noulYes(), nil, nil)
	kevCalls := 0
	req := jevReq(jev, AskRequest{Kind: "noul", Text: "x"})
	req.Route = "auto"
	req.KevURL = statusServer(t, 500, &kevCalls)
	res := mustAsk(t, req)
	if res.Route != "jev" || res.Endpoint != EndpointJev || res.Fallback != "" || kevCalls != 0 {
		t.Fatalf("got %+v kev calls %d, want jev only", res, kevCalls)
	}
}

func TestAskAutoNonPublicStaysLocal(t *testing.T) {
	jevCalls := 0
	kev := jevServer(t, noulYes(), nil, nil)
	req := AskRequest{Kind: "noul", Text: "private", JevEnabled: true, JevKey: "s",
		JevURL: statusServer(t, 200, &jevCalls), KevURL: kev.URL}
	res := mustAsk(t, req)
	if res.Endpoint != EndpointKev || jevCalls != 0 {
		t.Fatalf("got %+v jev calls %d, want kev and no jev", res, jevCalls)
	}
}

func TestAskAutoJevFailureFallsBackToKev(t *testing.T) {
	for _, status := range []int{500, 503} {
		dir := t.TempDir()
		jevCalls := 0
		kev := jevServer(t, noulYes(), nil, nil)
		req := AskRequest{Kind: "noul", Text: "x", Public: true, JevEnabled: true, JevKey: "s",
			JevDailyMax: -1, JevURL: statusServer(t, status, &jevCalls), KevURL: kev.URL, Caller: "t"}
		res, err := AskAndTrace(context.Background(), dir, req)
		if err != nil {
			t.Fatal(err)
		}
		if res.Route != "local" || res.Endpoint != EndpointKev || res.Label != "yes" || jevCalls != 1 {
			t.Fatalf("status %d: got %+v jev calls %d", status, res, jevCalls)
		}
		raw, _ := os.ReadFile(filepath.Join(dir, TraceFile))
		var line map[string]any
		if err := json.Unmarshal([]byte(strings.TrimSpace(string(raw))), &line); err != nil {
			t.Fatal(err)
		}
		if line["endpoint"] != "kev" || line["route"] != "local" || !strings.HasPrefix(line["fallback"].(string), "jev http 5") {
			t.Fatalf("trace = %v", line)
		}
	}
}

func TestAskAutoJevUnreachableFallsBack(t *testing.T) {
	kev := jevServer(t, noulYes(), nil, nil)
	res := mustAsk(t, AskRequest{Kind: "noul", Text: "x", Public: true, JevEnabled: true, JevKey: "s",
		JevDailyMax: -1, JevURL: "http://127.0.0.1:1", KevURL: kev.URL})
	if res.Endpoint != EndpointKev || res.Fallback != "jev unreachable" {
		t.Fatalf("got %+v", res)
	}
}

func TestAskAutoJevDailyCapFallsBack(t *testing.T) {
	dir := t.TempDir()
	jevCalls := 0
	kev := jevServer(t, noulYes(), nil, nil)
	req := AskRequest{Kind: "noul", Text: "x", Public: true, JevEnabled: true, JevKey: "s",
		JevDailyMax: 1, JevURL: statusServer(t, 200, &jevCalls), KevURL: kev.URL}
	AppendTrace(dir, TraceRecord(TraceInput{Kind: "choice", Route: "jev", Label: "yes"}), "")
	res, err := AskAndTrace(context.Background(), dir, req)
	if err != nil {
		t.Fatal(err)
	}
	if res.Endpoint != EndpointKev || res.Fallback != "jev daily cap" || jevCalls != 0 {
		t.Fatalf("got %+v jev calls %d", res, jevCalls)
	}
}

func TestAskJevNoFallbackWhenExplicitOr4xx(t *testing.T) {
	kevCalls := 0
	kevURL := statusServer(t, 200, &kevCalls)
	jevCalls := 0
	explicit := AskRequest{Kind: "noul", Text: "x", Route: "jev", Public: true, JevEnabled: true, JevKey: "s",
		JevDailyMax: -1, JevURL: statusServer(t, 503, &jevCalls), KevURL: kevURL}
	if res := mustAsk(t, explicit); res.Endpoint != EndpointJev || res.Error != "jev http 503" {
		t.Fatalf("explicit jev: %+v", res)
	}
	auth := explicit
	auth.Route = "auto"
	auth.JevURL = statusServer(t, 401, &jevCalls)
	if res := mustAsk(t, auth); res.Endpoint != EndpointJev || res.Error != "jev http 401" {
		t.Fatalf("auto 401: %+v", res)
	}
	if kevCalls != 0 {
		t.Fatalf("kev called %d times", kevCalls)
	}
}

func TestAskKevErrorsNameKev(t *testing.T) {
	calls := 0
	res := mustAsk(t, AskRequest{Kind: "noul", Text: "x", KevURL: statusServer(t, 502, &calls)})
	if res.Error != "kev http 502" || res.Label != Unknown {
		t.Fatalf("got %+v", res)
	}
}
