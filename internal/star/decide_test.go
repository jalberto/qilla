package star

import (
	"compress/gzip"
	"context"
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jalberto/qilla/internal/decide"
)

// A tiny two-class model written by hand: one word feature, one char feature,
// so the builtin is exercised end to end without the real (tens of MB) models.
const tinyModel = `{
 "format":1,"task":"tiny","classes":["no","yes"],"threshold":0.9,
 "text":{"lowercase":true,"strip_accents":"unicode"},
 "word":{"ngram":[1,1],"token_pattern":"(?u)\\b\\w\\w+\\b","vocab":{"spam":0},"idf":[1.0]},
 "char":{"ngram":[3,3],"vocab":{"spa":0},"idf":[1.0]},
 "coef":[[4.0,4.0]],"intercept":[-1.0],"sublinear_tf":true,"norm":"l2"}`

func TestDecideBuiltin(t *testing.T) {
	vault := t.TempDir()
	state := t.TempDir()
	if err := os.MkdirAll(filepath.Join(state, "models"), 0o755); err != nil {
		t.Fatal(err)
	}
	f, err := os.Create(filepath.Join(state, "models", "tiny.model.json.gz"))
	if err != nil {
		t.Fatal(err)
	}
	zw := gzip.NewWriter(f)
	if _, err := zw.Write([]byte(tinyModel)); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	f.Close()
	script := write(t, vault, "g.star", `
def gather(ctx):
    rows = [{"id": 184223, "subject": "Spam spam", "from": "x@y.z", "body": "spam"},
            {"id": "b", "subject": "hello", "from": "x@y.z", "body": "nothing here"}]
    first = decide(["tiny", "absent"], rows)
    # called twice on purpose: the second call must reuse the loaded model
    second = decide(["tiny"], rows)
    return {"out": first, "again": second}
`)
	e := env(vault)
	e.DecidersDir = state
	res, _, err := Run(context.Background(), script, e)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(res)
	var got struct {
		Out   []map[string]any `json:"out"`
		Again []map[string]any `json:"again"`
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	// "absent" has no model: it is skipped, not an error — one row per record.
	if len(got.Out) != 2 {
		t.Fatalf("got %d results, want 2: %s", len(got.Out), raw)
	}
	// msgvault ids are integers: the id comes back a number, not "184223".
	if got.Out[0]["id"] != float64(184223) || got.Out[0]["task"] != "tiny" {
		t.Errorf("unexpected first result: %#v", got.Out[0])
	}
	if got.Out[1]["id"] != "b" {
		t.Errorf("a string id must stay a string: %#v", got.Out[1])
	}
	if len(got.Again) != 2 {
		t.Errorf("second decide() call returned %d results", len(got.Again))
	}
	if got.Out[0]["label"] != "yes" {
		t.Errorf("row a should be yes, got %v", got.Out[0])
	}
	if got.Out[1]["label"] != "no" || got.Out[1]["unknown"] != true {
		t.Errorf("row b should be a no under the threshold, got %v", got.Out[1])
	}
}

func TestDecideNeedsDir(t *testing.T) {
	vault := t.TempDir()
	script := write(t, vault, "g.star", "def gather(ctx):\n    return {\"o\": decide([\"t\"], [])}\n")
	if _, _, err := Run(context.Background(), script, env(vault)); err == nil {
		t.Fatal("want an error without a deciders directory")
	}
}

// TestAskBuiltin: ask() round-trips through a fake Lemonade, and the script
// sees the same dict the CLI prints — plus a trace line under DecidersDir.
func TestAskBuiltin(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{
			"message": map[string]any{"content": "archive"},
			"logprobs": map[string]any{"content": []any{map[string]any{
				"token": "arch", "logprob": math.Log(0.96),
				"top_logprobs": []any{
					map[string]any{"token": "arch", "logprob": math.Log(0.96)},
					map[string]any{"token": "resp", "logprob": math.Log(0.03)},
					map[string]any{"token": "un", "logprob": math.Log(0.01)},
				},
			}}},
		}}})
	}))
	defer srv.Close()

	vault := t.TempDir()
	state := t.TempDir()
	script := write(t, vault, "g.star", `
def gather(ctx):
    return {"out": ask("choice", "a newsletter about GPUs",
                       options=["respond", "archive"],
                       question="what should the owner do?",
                       caller="unit")}
`)
	e := env(vault)
	e.DecidersDir = state
	e.AskConfig = func(req *decide.AskRequest) { req.URL = srv.URL; req.Model = "fake-decider" }
	res, _, err := Run(context.Background(), script, e)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(res)
	var got struct {
		Out map[string]any `json:"out"`
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	if got.Out["label"] != "archive" || got.Out["kind"] != "choice" {
		t.Fatalf("ask() = %v", got.Out)
	}
	if got.Out["model"] != "fake-decider" || got.Out["route"] != "local" {
		t.Fatalf("ask() = %v", got.Out)
	}
	if c, _ := got.Out["conf"].(float64); math.Abs(c-0.96) > 1e-3 {
		t.Fatalf("conf = %v, want 0.96", got.Out["conf"])
	}
	dist, ok := got.Out["dist"].(map[string]any)
	if !ok || len(dist) != 3 {
		t.Fatalf("dist = %v", got.Out["dist"])
	}
	line, err := os.ReadFile(filepath.Join(state, decide.TraceFile))
	if err != nil {
		t.Fatalf("ask() must trace: %v", err)
	}
	if strings.Contains(string(line), "newsletter about GPUs") {
		t.Fatalf("the trace must not carry the text: %s", line)
	}
	if !strings.Contains(string(line), `"caller":"unit"`) {
		t.Fatalf("trace caller: %s", line)
	}
}

// An unreachable backend is an unknown answer, never a script error.
func TestAskBuiltinBackendDown(t *testing.T) {
	vault := t.TempDir()
	script := write(t, vault, "g.star", `
def gather(ctx):
    return {"out": ask("noul", "is this a newsletter?")}
`)
	e := env(vault)
	e.DecidersDir = t.TempDir()
	e.AskConfig = func(req *decide.AskRequest) { req.URL = "http://127.0.0.1:1"; req.Timeout = time.Second }
	res, _, err := Run(context.Background(), script, e)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(res)
	var got struct {
		Out map[string]any `json:"out"`
	}
	json.Unmarshal(raw, &got)
	if got.Out["label"] != "unknown" || got.Out["error"] != "lemonade unreachable" {
		t.Fatalf("ask() = %v, want unknown / lemonade unreachable", got.Out)
	}
}
