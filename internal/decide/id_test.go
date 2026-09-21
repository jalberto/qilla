package decide

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// tinyModel is a hand-written two-class model: one word feature, one char
// feature. Enough to exercise the plumbing without the real 26 MB exports.
const tinyModel = `{
 "format":1,"task":"tiny","classes":["no","yes"],"threshold":0.9,
 "text":{"lowercase":true,"strip_accents":"unicode"},
 "word":{"ngram":[1,1],"token_pattern":"(?u)\\b\\w\\w+\\b","vocab":{"spam":0},"idf":[1.0]},
 "char":{"ngram":[3,3],"vocab":{"spa":0},"idf":[1.0]},
 "coef":[[4.0,4.0]],"intercept":[-1.0],"sublinear_tf":true,"norm":"l2"}`

func tinyDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "models"), 0o755); err != nil {
		t.Fatal(err)
	}
	// Written uncompressed on purpose: ModelPath must accept .model.json as
	// well as .model.json.gz.
	if err := os.WriteFile(filepath.Join(dir, "models", "tiny.model.json"), []byte(tinyModel), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

// TestNumericID: msgvault ids are integers. A record must accept them and the
// result must echo the id back in the type it arrived in — a gather matches
// its own rows on it.
func TestNumericID(t *testing.T) {
	set, err := LoadSet(tinyDir(t), []string{"tiny"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	const in = `{"id":184223,"subject":"Spam spam","from":"x@y.z","body":"spam"}
{"id":"abc","subject":"hello","from":"x@y.z","body":"nothing"}
{"subject":"no id at all"}`
	var records []Record
	for _, line := range splitLines(in) {
		var r Record
		if err := json.Unmarshal([]byte(line), &r); err != nil {
			t.Fatalf("%s: %v", line, err)
		}
		records = append(records, r)
	}
	if got := records[0].ID.String(); got != "184223" {
		t.Errorf("ID.String() = %q, want 184223", got)
	}
	if got := records[0].ID.Value(); got != int64(184223) {
		t.Errorf("ID.Value() = %#v, want int64(184223)", got)
	}
	res := set.Classify(records)
	if len(res) != 3 {
		t.Fatalf("got %d results", len(res))
	}
	want := []string{
		`{"id":184223,"task":"tiny",`,
		`{"id":"abc","task":"tiny",`,
		`{"id":null,"task":"tiny",`,
	}
	for i, w := range want {
		raw, err := json.Marshal(res[i])
		if err != nil {
			t.Fatal(err)
		}
		if got := string(raw); len(got) < len(w) || got[:len(w)] != w {
			t.Errorf("result %d = %s, want prefix %s", i, got, w)
		}
	}
	if res[0].Label != "yes" || res[1].Label != "no" {
		t.Errorf("labels: %q %q", res[0].Label, res[1].Label)
	}
}

func splitLines(s string) []string {
	var out []string
	start := 0
	for i := 0; i <= len(s); i++ {
		if i == len(s) || s[i] == '\n' {
			if i > start {
				out = append(out, s[start:i])
			}
			start = i + 1
		}
	}
	return out
}

// TestModelCachedPerProcess: loading is the whole cost, so the second
// LoadSet for the same file must hand back the model already parsed.
func TestModelCachedPerProcess(t *testing.T) {
	dir := tinyDir(t)
	a, err := LoadSet(dir, []string{"tiny"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	b, err := LoadSet(dir, []string{"tiny"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if a.Models["tiny"] != b.Models["tiny"] {
		t.Error("second LoadSet re-parsed the model instead of using the cache")
	}
}
