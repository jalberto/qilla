package star

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// page_kind() answers from the rules alone: no backend, no trace.
func TestPageKindBuiltin(t *testing.T) {
	vault := t.TempDir()
	script := write(t, vault, "g.star", `
def gather(ctx):
    blocked = page_kind("Sorry, you have been blocked\nCloudflare Ray ID: 8f2a")
    empty = page_kind("")
    return {"blocked": blocked, "empty": empty}
`)
	res, _, err := Run(context.Background(), script, env(vault))
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(res)
	var got struct {
		Blocked map[string]any `json:"blocked"`
		Empty   map[string]any `json:"empty"`
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	if got.Blocked["kind"] != "blocked" || got.Blocked["via"] != "rules" {
		t.Fatalf("blocked = %#v", got.Blocked)
	}
	if c, ok := got.Blocked["conf"].(float64); !ok || c < 0.9 {
		t.Fatalf("conf = %#v, want ≥ 0.9", got.Blocked["conf"])
	}
	if got.Empty["kind"] != "empty" {
		t.Fatalf("empty = %#v", got.Empty)
	}
}

// fetch() returns the --json dict shape; with no tools on PATH every rung is
// skipped and the script still gets a well-formed answer.
func TestFetchBuiltinShape(t *testing.T) {
	vault := t.TempDir()
	t.Setenv("PATH", t.TempDir()) // no defuddle/curl/obscura/agent-browser
	script := write(t, vault, "g.star", `
def gather(ctx):
    return {"r": fetch("https://example.invalid", max_rung=4)}
`)
	res, _, err := Run(context.Background(), script, env(vault))
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(res)
	var got struct {
		R map[string]any `json:"r"`
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"url", "rung", "pos", "kind", "conf", "via", "chars", "order", "route", "tried", "text", "needs_human"} {
		if _, ok := got.R[k]; !ok {
			t.Fatalf("missing key %q in %s", k, raw)
		}
	}
	if got.R["url"] != "https://example.invalid" {
		t.Fatalf("url = %v", got.R["url"])
	}
	tried, _ := got.R["tried"].([]any)
	if len(tried) != 4 {
		t.Fatalf("tried = %v, want four skipped rungs", got.R["tried"])
	}
	first, _ := tried[2].(map[string]any) // defuddle; 0-1 are the lookups
	if first["kind"] != "skipped" || !strings.Contains(first["note"].(string), "PATH") {
		t.Fatalf("tried[0] = %#v, want a skipped rung with a PATH note", first)
	}
}

// fetch() in a routine never opens a headed window unless the routine's own
// [fetch] headed overrides it.
func TestFetchBuiltinHeadedNeverByDefault(t *testing.T) {
	vault := t.TempDir()
	t.Setenv("PATH", t.TempDir())
	script := write(t, vault, "g.star", `
def gather(ctx):
    return {"r": fetch("https://example.invalid")}
`)
	for _, tc := range []struct{ headed, want string }{{"", "never"}, {"always", "agent-browser not on PATH"}} {
		e := env(vault)
		e.Fetch.Headed = tc.headed
		res, _, err := Run(context.Background(), script, e)
		if err != nil {
			t.Fatal(err)
		}
		raw, _ := json.Marshal(res)
		var got struct {
			R struct {
				NeedsHuman bool             `json:"needs_human"`
				Tried      []map[string]any `json:"tried"`
			} `json:"r"`
		}
		json.Unmarshal(raw, &got)
		last := got.R.Tried[len(got.R.Tried)-1]
		if last["rung"] != "headed" || !strings.Contains(last["note"].(string), tc.want) {
			t.Fatalf("headed=%q: last try %v, want note containing %q", tc.headed, last, tc.want)
		}
	}
}
