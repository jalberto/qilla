package main

import (
	"encoding/json"
	"errors"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jalberto/qilla/internal/decide"
)

// askEnv writes a minimal qilla.toml pointing [deciders] at a fake Lemonade
// and returns the state dir the trace lands under.
func askEnv(t *testing.T, lemonadeURL string) string {
	t.Helper()
	dir := t.TempDir()
	cfg := filepath.Join(dir, "qilla.toml")
	body := "vault = \"" + dir + "\"\nstate_dir = \"" + dir + "\"\n" +
		"[deciders]\nlemonade_url = \"" + lemonadeURL + "\"\nask_model = \"fake-decider\"\nconf_floor = 0.85\n"
	if err := os.WriteFile(cfg, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("QILLA_CONFIG", cfg)
	return filepath.Join(dir, "deciders")
}

// runAsk runs `qilla decide ask …` and returns stdout and the error.
func runAsk(t *testing.T, stdin string, args ...string) (string, error) {
	t.Helper()
	if stdin != "" {
		in, err := os.CreateTemp(t.TempDir(), "stdin")
		if err != nil {
			t.Fatal(err)
		}
		in.WriteString(stdin)
		in.Seek(0, 0)
		oldIn := os.Stdin
		os.Stdin = in
		defer func() { os.Stdin = oldIn; in.Close() }()
	}
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	old := os.Stdout
	os.Stdout = w
	cmdErr := cmdDecide(append([]string{"ask"}, args...))
	os.Stdout = old
	w.Close()
	b, _ := io.ReadAll(r)
	return string(b), cmdErr
}

func fakeLemonade(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{
			"message": map[string]any{"content": "respond"},
			"logprobs": map[string]any{"content": []any{map[string]any{
				"token": "resp", "logprob": math.Log(0.97),
				"top_logprobs": []any{
					map[string]any{"token": "resp", "logprob": math.Log(0.97)},
					map[string]any{"token": "arch", "logprob": math.Log(0.02)},
					map[string]any{"token": "un", "logprob": math.Log(0.01)},
				},
			}}},
		}}})
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestDecideAskText(t *testing.T) {
	srv := fakeLemonade(t)
	state := askEnv(t, srv.URL)
	out, err := runAsk(t, "", "--choice", "respond,archive", "--question", "what now?",
		"--text", "can we meet?", "--caller", "test")
	if err != nil {
		t.Fatalf("decide ask: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("stdout %q: %v", out, err)
	}
	if got["label"] != "respond" || got["kind"] != "choice" {
		t.Fatalf("got %v", got)
	}
	if got["model"] != "fake-decider" {
		t.Fatalf("model = %v, want the configured one", got["model"])
	}
	raw, err := os.ReadFile(filepath.Join(state, decide.TraceFile))
	if err != nil {
		t.Fatalf("no trace: %v", err)
	}
	if strings.Contains(string(raw), "can we meet") {
		t.Fatalf("the trace must not carry the text: %s", raw)
	}
	// …and `--show-trace` reads it back.
	shown, err := runAsk(t, "", "--show-trace", "--tail", "5")
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(shown) != strings.TrimSpace(string(raw)) {
		t.Fatalf("--show-trace = %q, want the trace line", shown)
	}
}

func TestDecideAskStdinBatch(t *testing.T) {
	srv := fakeLemonade(t)
	askEnv(t, srv.URL)
	out, err := runAsk(t, "{\"id\":1,\"text\":\"first\"}\n\n{\"id\":\"b\",\"text\":\"second\"}\nnot json\n",
		"--choice", "respond,archive", "--stdin")
	if err != nil {
		t.Fatalf("decide ask --stdin: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) != 2 {
		t.Fatalf("want one result per parseable row, got %q", out)
	}
	var first, second map[string]any
	json.Unmarshal([]byte(lines[0]), &first)
	json.Unmarshal([]byte(lines[1]), &second)
	if first["id"] != float64(1) || second["id"] != "b" {
		t.Fatalf("ids not carried through: %v %v", first["id"], second["id"])
	}
	if first["label"] != "respond" || second["label"] != "respond" {
		t.Fatalf("labels: %v %v", first["label"], second["label"])
	}
}

func TestDecideAskServerDownExitsZero(t *testing.T) {
	srv := fakeLemonade(t)
	url := srv.URL
	srv.Close()
	askEnv(t, url)
	out, err := runAsk(t, "", "--noul", "--text", "is this a newsletter?")
	if err != nil {
		t.Fatalf("a dead backend must not be an error, got %v", err)
	}
	var got map[string]any
	json.Unmarshal([]byte(out), &got)
	if got["label"] != "unknown" || got["error"] != "lemonade unreachable" {
		t.Fatalf("got %v, want unknown / lemonade unreachable", got)
	}
}

func TestDecideAskUsageErrors(t *testing.T) {
	for _, c := range []struct {
		name string
		args []string
	}{
		{"no kind", []string{"--text", "x"}},
		{"both text and stdin", []string{"--choice", "a,b", "--text", "x", "--stdin"}},
		{"neither", []string{"--choice", "a,b"}},
		{"jev without public", []string{"--noul", "--text", "x", "--route", "jev"}},
		{"bad floor", []string{"--noul", "--text", "x", "--floor", "high"}},
		{"unknown flag", []string{"--noul", "--text", "x", "--wat"}},
	} {
		if _, err := parseAskArgs(c.args); !errors.Is(err, errUsage) {
			t.Fatalf("%s: err = %v, want a usage error (exit 2)", c.name, err)
		}
	}
	if _, err := parseAskArgs([]string{"--noul", "--text", "x"}); err != nil {
		t.Fatalf("a good call must parse: %v", err)
	}
}
