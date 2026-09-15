package serve

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestProjectSlug(t *testing.T) {
	if got := projectSlug("/home/alice/Notes"); got != "-home-alice-Notes" {
		t.Fatalf("slug = %q", got)
	}
	if got := projectSlug("/home/x/my.project"); got != "-home-x-my-project" {
		t.Fatalf("slug = %q", got)
	}
}

func TestParseTurn(t *testing.T) {
	cases := []struct {
		name string
		line string
		ok   bool
		role string
		text string
		tool int
	}{
		{"user string", `{"type":"user","uuid":"u1","message":{"role":"user","content":"hola"}}`, true, "user", "hola", 0},
		{"user tool_result only", `{"type":"user","uuid":"u2","message":{"role":"user","content":[{"type":"tool_result","content":"x"}]}}`, false, "", "", 0},
		{"user injected context", `{"type":"user","uuid":"u3","message":{"role":"user","content":"<system-reminder>x</system-reminder>"}}`, false, "", "", 0},
		{"user injected then typed", `{"type":"user","uuid":"u4","message":{"role":"user","content":"<system-reminder>ctx</system-reminder>\nreal question"}}`, true, "user", "real question", 0},
		{"user starting with <", `{"type":"user","uuid":"u5","message":{"role":"user","content":"<div> is what I pasted"}}`, true, "user", "<div> is what I pasted", 0},
		{"user slash-command plumbing", `{"type":"user","uuid":"u6","message":{"role":"user","content":"<command-name>/x</command-name><command-message>x</command-message>"}}`, false, "", "", 0},
		{"assistant text", `{"type":"assistant","uuid":"a1","message":{"role":"assistant","model":"m","content":[{"type":"text","text":"hi"}]}}`, true, "assistant", "hi", 0},
		{"assistant tools only", `{"type":"assistant","uuid":"a2","message":{"role":"assistant","content":[{"type":"tool_use","name":"Bash"},{"type":"tool_use","name":"Read"}]}}`, true, "assistant", "", 2},
		{"assistant thinking only", `{"type":"assistant","uuid":"a4","message":{"role":"assistant","content":[{"type":"thinking","thinking":"hmm"}]}}`, true, "assistant", "", 0},
		{"assistant text+tool", `{"type":"assistant","uuid":"a3","message":{"role":"assistant","content":[{"type":"text","text":"doing"},{"type":"tool_use"}]}}`, true, "assistant", "doing", 1},
		{"system event", `{"type":"cost-state","uuid":"s1"}`, false, "", "", 0},
		{"garbage", `not json`, false, "", "", 0},
	}
	for _, c := range cases {
		got, ok := parseTurn([]byte(c.line))
		if ok != c.ok {
			t.Fatalf("%s: ok=%v want %v", c.name, ok, c.ok)
		}
		if ok && (got.Role != c.role || got.Text != c.text || got.Tools != c.tool) {
			t.Fatalf("%s: got %+v", c.name, got)
		}
	}
}

func TestReadTurnsCollapsesToolLoopsAndKeepsLastN(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "s.jsonl")
	lines := "" +
		`{"type":"user","uuid":"u1","message":{"role":"user","content":"q1"}}` + "\n" +
		`{"type":"assistant","uuid":"a1","message":{"role":"assistant","content":[{"type":"tool_use"}]}}` + "\n" +
		`{"type":"user","uuid":"u2","message":{"role":"user","content":[{"type":"tool_result"}]}}` + "\n" +
		`{"type":"assistant","uuid":"a2","message":{"role":"assistant","content":[{"type":"tool_use"},{"type":"tool_use"}]}}` + "\n" +
		`{"type":"assistant","uuid":"a3","message":{"role":"assistant","content":[{"type":"text","text":"answer"}]}}` + "\n" +
		`{"type":"user","uuid":"u3","message":{"role":"user","content":"q2"}}` + "\n"
	if err := os.WriteFile(p, []byte(lines), 0o600); err != nil {
		t.Fatal(err)
	}
	turns, err := readTurns(p, 10)
	if err != nil {
		t.Fatal(err)
	}
	// q1 · [3 tool calls collapsed] · answer · q2
	if len(turns) != 4 {
		t.Fatalf("turns = %d: %+v", len(turns), turns)
	}
	if turns[1].Role != "assistant" || turns[1].Text != "" || turns[1].Tools != 3 {
		t.Fatalf("tool loop not collapsed: %+v", turns[1])
	}
	last, err := readTurns(p, 2)
	if err != nil || len(last) != 2 || last[0].Text != "answer" || last[1].Text != "q2" {
		t.Fatalf("last 2 = %+v (%v)", last, err)
	}
}

func TestParseTurnKeepsThinkingAndCallSummaries(t *testing.T) {
	line := `{"type":"assistant","uuid":"a9","message":{"role":"assistant","content":[
	  {"type":"thinking","thinking":"  plan first  "},
	  {"type":"tool_use","name":"Bash","input":{"command":"ls -la /tmp","description":"list"}},
	  {"type":"tool_use","name":"Read","input":{"file_path":"/etc/hosts"}},
	  {"type":"text","text":"done"}]}}`
	tr, ok := parseTurn([]byte(line))
	if !ok || tr.Text != "done" || tr.Thinking != "plan first" || tr.Tools != 2 {
		t.Fatalf("%+v ok=%v", tr, ok)
	}
	if len(tr.Calls) != 2 || tr.Calls[0].Name != "Bash" || tr.Calls[0].Input != "ls -la /tmp" || tr.Calls[1].Input != "/etc/hosts" {
		t.Fatalf("calls: %+v", tr.Calls)
	}
	if got := clip(strings.Repeat("x", 200), 160); len(got) != 160+len("…") {
		t.Fatalf("clip: %d", len(got))
	}
}

func TestOpenSubagents(t *testing.T) {
	dir := t.TempDir()
	sid := "sess-1"
	p := filepath.Join(dir, sid+".jsonl")
	lines := []string{
		`{"type":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","id":"t1","name":"Agent","input":{}},{"type":"tool_use","id":"t2","name":"Task","input":{}}]}}`,
		`{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1"}]}}`,
	}
	if err := os.WriteFile(p, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if n := openSubagents(p, sid); n != 1 {
		t.Fatalf("one Agent call still open, got %d", n)
	}
	sub := filepath.Join(dir, sid, "subagents")
	os.MkdirAll(sub, 0o755)
	live := filepath.Join(sub, "t2.jsonl")
	os.WriteFile(live, []byte("{}\n"), 0o644)
	if n := openSubagents(p, sid); n != 1 {
		t.Fatalf("open call + its live file must not double count, got %d", n)
	}
	// stale file, and no open tool_use left: nothing is running
	old := time.Now().Add(-2 * time.Minute)
	os.Chtimes(live, old, old)
	done := p + ".done"
	os.WriteFile(done, []byte(lines[0]+"\n"+lines[1]+"\n"+
		`{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"t2"}]}}`+"\n"), 0o644)
	if n := openSubagents(done, sid); n != 0 {
		t.Fatalf("stale file and no open call = 0, got %d", n)
	}
}

func TestAttachedFalseForUnknownSession(t *testing.T) {
	if attached("") || attached("no-such-session-id-0000") {
		t.Fatal("attached should be false")
	}
}
