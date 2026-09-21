package star

import (
	"compress/gzip"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
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
    rows = [{"id": "a", "subject": "Spam spam", "from": "x@y.z", "body": "spam"},
            {"id": "b", "subject": "hello", "from": "x@y.z", "body": "nothing here"}]
    return {"out": decide(["tiny", "absent"], rows)}
`)
	e := env(vault)
	e.DecidersDir = state
	res, _, err := Run(context.Background(), script, e)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(res)
	var got struct {
		Out []map[string]any `json:"out"`
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	// "absent" has no model: it is skipped, not an error — one row per record.
	if len(got.Out) != 2 {
		t.Fatalf("got %d results, want 2: %s", len(got.Out), raw)
	}
	if got.Out[0]["id"] != "a" || got.Out[0]["task"] != "tiny" {
		t.Errorf("unexpected first result: %v", got.Out[0])
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
