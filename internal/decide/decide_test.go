package decide

import (
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// goldenTasks are the tasks whose exported model + golden file the agreement
// test uses when they are present on this machine. Models are user state,
// never checked in, so the test skips the ones that are missing.
var goldenTasks = []string{"newsletter", "trash", "category"}

func statePath() string {
	if d := os.Getenv("QILLA_DECIDERS_DIR"); d != "" {
		return d
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".local", "state", "qilla", "deciders")
}

type goldenFile struct {
	Task    string   `json:"task"`
	Classes []string `json:"classes"`
	Rows    []struct {
		Text  string    `json:"text"`
		Proba []float64 `json:"proba"`
	} `json:"rows"`
}

// TestGoldenAgreesWithSklearn: the Go inference must reproduce sklearn's
// probabilities to 1e-6 on the exported sample. This is the contract that
// lets training stay in Python.
func TestGoldenAgreesWithSklearn(t *testing.T) {
	for _, task := range goldenTasks {
		t.Run(task, func(t *testing.T) { goldenAgrees(t, task) })
	}
}

func goldenAgrees(t *testing.T, goldenTask string) {
	t.Helper()
	dir := statePath()
	modelPath := ModelPath(dir, goldenTask)
	goldenPath := filepath.Join(dir, "models", goldenTask+".golden.json")
	if _, err := os.Stat(modelPath); err != nil {
		t.Skipf("no exported model at %s (run scripts/export_model.py in qilla-deciders)", modelPath)
	}
	raw, err := os.ReadFile(goldenPath)
	if err != nil {
		t.Skipf("no golden file: %v", err)
	}
	var g goldenFile
	if err := json.Unmarshal(raw, &g); err != nil {
		t.Fatal(err)
	}
	m, err := LoadModel(modelPath)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(m.Classes, g.Classes) {
		t.Fatalf("classes %v != golden %v", m.Classes, g.Classes)
	}
	texts := make([]string, len(g.Rows))
	for i, r := range g.Rows {
		texts[i] = r.Text
	}
	preds := m.Predict(texts)
	worst := 0.0
	for i, r := range g.Rows {
		for c, want := range r.Proba {
			got := preds[i].Dist[m.Classes[c]]
			if d := math.Abs(got - want); d > worst {
				worst = d
			}
		}
	}
	t.Logf("%s: %d rows, max |Go - sklearn| = %.3e", goldenTask, len(g.Rows), worst)
	if worst >= 1e-6 {
		t.Errorf("max abs diff %.3e >= 1e-6", worst)
	}
}

// --- unit tests against values computed once with sklearn 1.8 --------------
// Produced in the `deciders` toolbox with TfidfVectorizer(...).build_analyzer()
// and strip_accents_unicode(s.lower()); pasted here verbatim.

func TestStripAccents(t *testing.T) {
	cases := []struct{ in, want string }{
		{"Café", "cafe"},
		{"naïve ﬁle", "naive file"},
		{"ASCII only", "ascii only"},
		{"Ångström", "angstrom"},
		{"日本語", "日本語"},
		{"İstanbul", "istanbul"},
	}
	for _, c := range cases {
		if got := Preprocess(c.in); got != c.want {
			t.Errorf("Preprocess(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestCharWBNGrams(t *testing.T) {
	m := &Model{}
	m.Char.NGram = []int{3, 5}
	cases := []struct {
		in   string
		want []string
	}{
		{"hi there", []string{" hi", "hi ", " hi ", " th", "the", "her", "ere", "re ", " the", "ther", "here", "ere ", " ther", "there", "here "}},
		{"a", []string{" a "}},
		{"ab", []string{" ab", "ab ", " ab "}},
		{"hola qué tal", []string{" ho", "hol", "ola", "la ", " hol", "hola", "ola ", " hola", "hola ", " qu", "qué", "ué ", " qué", "qué ", " qué ", " ta", "tal", "al ", " tal", "tal ", " tal "}},
		{"  x  yy  ", []string{" x ", " yy", "yy ", " yy "}},
	}
	for _, c := range cases {
		if got := m.CharWBNGrams(c.in); !reflect.DeepEqual(got, c.want) {
			t.Errorf("CharWBNGrams(%q) =\n %q\nwant\n %q", c.in, got, c.want)
		}
	}
	// The same input through the full preprocessor loses the accent, exactly
	// as sklearn's analyzer does.
	got := m.CharWBNGrams(Preprocess("hola qué tal"))
	want := []string{" ho", "hol", "ola", "la ", " hol", "hola", "ola ", " hola", "hola ", " qu", "que", "ue ", " que", "que ", " que ", " ta", "tal", "al ", " tal", "tal ", " tal "}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("preprocessed char_wb =\n %q\nwant\n %q", got, want)
	}
}

func TestWordNGrams(t *testing.T) {
	m := &Model{}
	m.Word.NGram = []int{1, 2}
	var err error
	if m.tokenRe, err = compileTokenPattern(sklearnTokenPattern); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		in   string
		want []string
	}{
		{"Hola, ¿Qué tal? José_1 a", []string{"hola", "que", "tal", "jose_1", "hola que", "que tal", "tal jose_1"}},
		{"CAFÉ ﬁle straße", []string{"cafe", "file", "straße", "cafe file", "file straße"}},
	}
	for _, c := range cases {
		if got := m.WordNGrams(Preprocess(c.in)); !reflect.DeepEqual(got, c.want) {
			t.Errorf("WordNGrams(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestPredictDeterministic: float addition is not associative, so the sparse
// row must be summed in a fixed order — otherwise the same input gives
// different last digits between runs and a gather's digest churns.
func TestPredictDeterministic(t *testing.T) {
	dir := statePath()
	path := ModelPath(dir, goldenTasks[0])
	if _, err := os.Stat(path); err != nil {
		t.Skipf("no exported model at %s", path)
	}
	m, err := LoadModel(path)
	if err != nil {
		t.Fatal(err)
	}
	texts := []string{
		"Weekly digest\nNewsletter Bot <n@e.test>\nunsubscribe unsubscribe digest digest digest",
		"Reunión\nJosé <j@e.test>\n¿puedes confirmar?",
	}
	first := m.Predict(texts)
	for i := 0; i < 20; i++ {
		if got := m.Predict(texts); !reflect.DeepEqual(got, first) {
			t.Fatalf("run %d differs: %+v vs %+v", i, got, first)
		}
	}
}
