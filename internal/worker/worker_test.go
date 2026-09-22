package worker

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jalberto/qilla/internal/config"
	"github.com/jalberto/qilla/internal/mem"
	"github.com/jalberto/qilla/internal/queue"
)

// fake claude: logs argv + stdin, prints an envelope. Env FAKE_DENY adds a denial.
const fakeClaude = `#!/bin/sh
printf '%s\n' "$*" >> "$FAKE_LOG"
cat >> "$FAKE_LOG.stdin"
den=""
[ -n "$FAKE_DENY" ] && den=',"permission_denials":[{"tool_name":"'"$FAKE_DENY"'"}]'
res="${FAKE_RESULT-answer 42}"
printf '{"type":"result","is_error":false,"result":"%s","session_id":"sess-1","usage":{"input_tokens":10,"output_tokens":3}%s}\n' "$res" "$den"
`

type env struct {
	cfg  *config.Config
	w    *Worker
	log  string
	recs []Record
}

func setup(t *testing.T, routine config.Routine, gather string) *env {
	t.Helper()
	root := t.TempDir()
	vault := filepath.Join(root, "vault")
	bin := filepath.Join(root, "bin")
	os.MkdirAll(bin, 0o755)
	os.WriteFile(filepath.Join(bin, "claude"), []byte(fakeClaude), 0o755)
	dir := filepath.Join(vault, RoutinesDir, "r")
	os.MkdirAll(dir, 0o755)
	os.MkdirAll(filepath.Join(vault, "Qilla"), 0o755)
	os.WriteFile(filepath.Join(vault, "Qilla", "Persona.md"), []byte("persona"), 0o644)
	os.WriteFile(filepath.Join(dir, "prompt.md"), []byte("do it"), 0o644)
	if gather != "" {
		os.WriteFile(filepath.Join(dir, "gather.sh"), []byte(gather), 0o755)
	}
	log := filepath.Join(root, "claude.log")
	t.Setenv("FAKE_LOG", log)
	t.Setenv("CLAUDECODE", "1") // must be scrubbed
	os.MkdirAll(filepath.Join(vault, "Qilla", "Plugin"), 0o755)
	cfg := &config.Config{Path: filepath.Join(root, "cfg", "qilla.toml"), Vault: vault, Plugins: config.Plugins{Dirs: []string{"Qilla/Plugin"}}, StateDir: filepath.Join(root, "state"), Persona: "Qilla/Persona.md", Rules: "Qilla/Rules.md",
		FactsDir: "Qilla/Facts", Claude: filepath.Join(bin, "claude"),
		Agents:   map[string]config.Agent{"chief": {Model: "claude-sonnet-5", AllowedTools: []string{"Read", "Grep"}, Scope: []string{"persona"}}},
		Routines: map[string]config.Routine{"r": routine}}
	s, err := queue.Open(filepath.Join(root, "q.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	w, err := New(cfg, s.DB())
	if err != nil {
		t.Fatal(err)
	}
	e := &env{cfg: cfg, w: w, log: log}
	w.OnRun = func(r Record) { e.recs = append(e.recs, r) }
	w.Now = func() time.Time { return time.Date(2026, 9, 12, 9, 0, 0, 0, time.UTC) }
	return e
}

func TestRememberFromResult(t *testing.T) {
	e := setup(t, config.Routine{Kind: config.KindFresh, Agent: "chief"}, "")
	q, _ := queue.Open(filepath.Join(t.TempDir(), "m.db"))
	defer q.Close()
	b, _ := mem.NewSQLite(q.DB())
	e.w.Mem, _ = mem.New(b, q.DB(), mem.Options{MaxChars: 500})
	t.Setenv("FAKE_RESULT", `{\"summary\":\"ok\",\"remember\":[{\"kind\":\"heuristic\",\"key\":\"cost\",\"text\":\"brief cost 0.4\"},{\"kind\":\"said\",\"text\":\"flagged invoice 4411\"}]}`)
	if err := e.w.Run(context.Background(), &queue.Job{ID: 1, Routine: "r"}); err != nil {
		t.Fatal(err)
	}
	if e.recs[0].Remembered != 2 {
		t.Fatalf("remembered: %+v", e.recs[0])
	}
	es, _ := e.w.Mem.Search(context.Background(), "r", "", "invoice", 5)
	if len(es) != 1 || es[0].Kind != "said" {
		t.Fatalf("stored under the routine's project: %+v", es)
	}
}

func TestSandboxSettingsPassed(t *testing.T) {
	e := setup(t, config.Routine{Kind: config.KindFresh, Agent: "chief", AllowedDomains: []string{"api.example.com"}}, "")
	e.cfg.Sandbox.AllowedDomains = []string{"wttr.in"}
	if err := e.w.Run(context.Background(), &queue.Job{ID: 1, Routine: "r"}); err != nil {
		t.Fatal(err)
	}
	a := e.argv()
	if !strings.Contains(a, "--settings ") {
		t.Fatalf("settings file must be passed: %s", a)
	}
	b, err := os.ReadFile(filepath.Join(e.cfg.StateDir, "settings", "r.json"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"enabled": true`, `"allowUnsandboxedCommands": false`, "api.example.com", "wttr.in", `"denyRead"`, "/creds", `"statusLine"`, "statusline.sh"} {
		if !strings.Contains(string(b), want) {
			t.Fatalf("settings missing %q:\n%s", want, b)
		}
	}
	e.cfg.Sandbox.Disabled = true
	os.Remove(e.log)
	e.w.Run(context.Background(), &queue.Job{ID: 2, Routine: "r"})
	if strings.Contains(e.argv(), "--settings") {
		t.Fatal("disabled sandbox → no settings flag")
	}
}

func TestSecretsReachGatherNotClaude(t *testing.T) {
	e := setup(t, config.Routine{Kind: config.KindFresh, Agent: "chief"}, "#!/bin/sh\nprintf '{\"secrets\":\"%s\"}' \"${QILLA_SECRETS_DIR:-none}\"\n")
	t.Setenv("CREDENTIALS_DIRECTORY", "/run/creds/qilla")
	// fake claude dumps its env to FAKE_LOG.env
	os.WriteFile(e.cfg.Claude, []byte(fakeClaude+"env > \"$FAKE_LOG.env\"\n"), 0o755)
	if err := e.w.Run(context.Background(), &queue.Job{ID: 1, Routine: "r"}); err != nil {
		t.Fatal(err)
	}
	stdin, _ := os.ReadFile(e.log + ".stdin")
	if !strings.Contains(string(stdin), `"secrets":"/run/creds/qilla"`) {
		t.Fatalf("gather must see QILLA_SECRETS_DIR: %s", stdin)
	}
	envb, _ := os.ReadFile(e.log + ".env")
	if strings.Contains(string(envb), "CREDENTIALS_DIRECTORY") || strings.Contains(string(envb), "QILLA_SECRETS_DIR") {
		t.Fatal("claude must never see the credentials directory")
	}
	if !strings.Contains(string(envb), "QILLA_RUN=1") || !strings.Contains(string(envb), "QILLA_ROUTINE=r") {
		t.Fatalf("claude must carry the qilla session markers:\n%s", envb)
	}
	sb, _ := os.ReadFile(filepath.Join(e.cfg.StateDir, "settings", "r.json"))
	if !strings.Contains(string(sb), "/run/creds/qilla") {
		t.Fatalf("credentials dir must be denied inside the bwrap sandbox:\n%s", sb)
	}
}

func TestSettingsWriteFailureFailsRun(t *testing.T) {
	e := setup(t, config.Routine{Kind: config.KindFresh, Agent: "chief"}, "")
	os.MkdirAll(e.cfg.StateDir, 0o755)
	os.WriteFile(filepath.Join(e.cfg.StateDir, "settings"), []byte("not a dir"), 0o644) // blocks MkdirAll
	err := e.w.Run(context.Background(), &queue.Job{ID: 1, Routine: "r"})
	if err == nil || !strings.Contains(err.Error(), "sandbox settings") {
		t.Fatalf("must not run unsandboxed on I/O error: %v", err)
	}
	if _, err := os.Stat(e.log); err == nil {
		t.Fatal("claude must not have been invoked")
	}
}

func (e *env) argv() string {
	b, _ := os.ReadFile(e.log)
	return string(b)
}

func TestFreshCallsClaudeWithAgentDefaults(t *testing.T) {
	e := setup(t, config.Routine{Kind: config.KindFresh, Agent: "chief"}, "")
	if err := e.w.Run(context.Background(), &queue.Job{ID: 1, Routine: "r"}); err != nil {
		t.Fatal(err)
	}
	a := e.argv()
	for _, want := range []string{"-p", "--output-format json", "--model claude-sonnet-5", "--allowedTools Read,Grep"} {
		if !strings.Contains(a, want) {
			t.Errorf("argv missing %q: %s", want, a)
		}
	}
	if strings.Contains(a, "--resume") {
		t.Error("fresh must not resume")
	}
	if !strings.Contains(a, "--agents ") || !strings.Contains(a, "researcher") {
		t.Errorf("sub-agent definitions must travel with the spawn: %s", a[:200])
	}
	// the vault plugin dir is merged into the installed qilla plugin, never passed per spawn
	if strings.Contains(a, "--plugin-dir") {
		t.Errorf("--plugin-dir must be gone (one merged plugin): %s", a)
	}
	if !strings.Contains(a, "--allowedTools Read,Grep,Skill") {
		t.Errorf("Skill must be allowed when plugins are configured: %s", a)
	}
	stdin, _ := os.ReadFile(e.log + ".stdin")
	if strings.Contains(string(stdin), "persona") || !strings.Contains(string(stdin), "do it") {
		t.Errorf("user prompt carries the task, not the persona: %s", stdin)
	}
	if !strings.Contains(a, "--append-system-prompt-file") {
		t.Errorf("persona + rules must travel as the system prompt: %s", a)
	}
	if len(e.recs) != 1 || e.recs[0].Usage.Input != 10 || e.recs[0].Model != "claude-sonnet-5" {
		t.Fatalf("record: %+v", e.recs)
	}
}

func TestResumedStoresAndReusesSession(t *testing.T) {
	e := setup(t, config.Routine{Kind: config.KindResumed, Agent: "chief"}, "")
	e.w.Run(context.Background(), &queue.Job{ID: 1, Routine: "r"})
	e.w.Run(context.Background(), &queue.Job{ID: 2, Routine: "r"})
	lines := strings.Split(strings.TrimSpace(e.argv()), "\n")
	if strings.Contains(lines[0], "--resume") || !strings.Contains(lines[1], "--resume sess-1") {
		t.Fatalf("first fresh, second resumed:\n%s", e.argv())
	}
}

func TestDeniedToolWithResultIsAWarning(t *testing.T) {
	e := setup(t, config.Routine{Kind: config.KindFresh, Agent: "chief"}, "")
	t.Setenv("FAKE_DENY", "Bash")
	if err := e.w.Run(context.Background(), &queue.Job{ID: 1, Routine: "r"}); err != nil {
		t.Fatalf("a denial after a usable result keeps the work: %v", err)
	}
	if e.recs[0].Denied[0] != "Bash" || len(e.recs[0].Warnings) == 0 || e.recs[0].Error != "" {
		t.Fatalf("record must carry the denial as a warning: %+v", e.recs[0])
	}
}

func TestDeniedToolWithoutResultIsTerminal(t *testing.T) {
	e := setup(t, config.Routine{Kind: config.KindFresh, Agent: "chief"}, "")
	t.Setenv("FAKE_DENY", "Bash")
	t.Setenv("FAKE_RESULT", "")
	err := e.w.Run(context.Background(), &queue.Job{ID: 1, Routine: "r"})
	var term queue.Terminal
	if err == nil || !errors.As(err, &term) {
		t.Fatalf("denial with no result must be terminal: %v", err)
	}
}

func TestDigestSkipsUnchangedInput(t *testing.T) {
	e := setup(t, config.Routine{Kind: config.KindFresh, Agent: "chief"}, "#!/bin/sh\necho '{\"mail\":3}'\n")
	e.w.Run(context.Background(), &queue.Job{ID: 1, Routine: "r"})
	e.w.Run(context.Background(), &queue.Job{ID: 2, Routine: "r"})
	if n := len(strings.Split(strings.TrimSpace(e.argv()), "\n")); n != 1 {
		t.Fatalf("claude must run once, ran %d", n)
	}
	if !e.recs[1].Skipped || e.recs[0].Digest == "" {
		t.Fatalf("second run skipped by digest: %+v", e.recs)
	}
}

func TestScriptKindNeverCallsClaude(t *testing.T) {
	e := setup(t, config.Routine{Kind: config.KindScript}, "#!/bin/sh\necho \"{\\\"date\\\":\\\"$QILLA_DATE\\\"}\"\n")
	if err := e.w.Run(context.Background(), &queue.Job{ID: 1, Routine: "r"}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(e.log); err == nil {
		t.Fatal("script kind must not invoke claude")
	}
	if !strings.Contains(e.recs[0].Result, "2026-09-12") {
		t.Fatalf("gather env QILLA_DATE missing: %+v", e.recs[0])
	}
	if _, err := os.Stat(filepath.Join(e.cfg.StateDir, "runs", "1.json")); err != nil {
		t.Fatal("run record must be saved")
	}
}

func TestScriptWithoutGatherFails(t *testing.T) {
	e := setup(t, config.Routine{Kind: config.KindScript}, "")
	if err := e.w.Run(context.Background(), &queue.Job{ID: 1, Routine: "r"}); err == nil {
		t.Fatal("script kind without gather.sh must fail")
	}
}

// A bundle with only gather.star gathers through the in-process Starlark
// runtime; with both files, gather.sh wins (doctor warns about the loser).
func TestGatherStar(t *testing.T) {
	e := setup(t, config.Routine{Kind: config.KindScript}, "")
	dir := filepath.Join(e.cfg.Vault, RoutinesDir, "r")
	os.WriteFile(filepath.Join(dir, "gather.star"),
		[]byte("def gather(ctx):\n    return {\"who\": ctx.routine, \"date\": ctx.date}\n"), 0o644)

	out, ok, err := e.w.Gather(context.Background(), "r")
	if err != nil || !ok {
		t.Fatalf("gather.star: %v (ok=%v)", err, ok)
	}
	if out != `{"date":"2026-09-12","who":"r"}` {
		t.Fatalf("got %s", out)
	}

	os.WriteFile(filepath.Join(dir, "gather.sh"), []byte("printf '{\"from\":\"sh\"}'\n"), 0o755)
	out, _, err = e.w.Gather(context.Background(), "r")
	if err != nil || out != `{"from":"sh"}` {
		t.Fatalf("gather.sh must win: %q %v", out, err)
	}
}

func TestGatherStarFailureIsAnError(t *testing.T) {
	e := setup(t, config.Routine{Kind: config.KindScript}, "")
	os.WriteFile(filepath.Join(e.cfg.Vault, RoutinesDir, "r", "gather.star"),
		[]byte("def gather(ctx):\n    print(\"halfway\")\n    fail(\"nope\")\n"), 0o644)
	_, ok, err := e.w.Gather(context.Background(), "r")
	if !ok || err == nil {
		t.Fatalf("want a failure, got ok=%v err=%v", ok, err)
	}
	if !strings.Contains(err.Error(), "nope") || !strings.Contains(err.Error(), "halfway") {
		t.Fatalf("error must carry the failure and the prints: %v", err)
	}
}

func TestJSONOrString(t *testing.T) {
	obj := map[string]any{"a": "b"}
	cases := []struct {
		name string
		in   string
		want any
	}{
		{"bare object", `{"a":"b"}`, obj},
		{"fenced object", "```\n{\"a\":\"b\"}\n```", obj},
		{"fenced json tag", "```json\n{\"a\":\"b\"}\n```", obj},
		{"fenced array", "```json\n[1,2]\n```", []any{1.0, 2.0}},
		{"fenced with blanks around", "\n\n```json\n{\"a\":\"b\"}\n```\n\n", obj},
		{"prose then json", "Here it is: {\"a\":\"b\"}", "Here it is: {\"a\":\"b\"}"},
		{"json then prose", "{\"a\":\"b\"} and that's it", "{\"a\":\"b\"} and that's it"},
		{"invalid json", "{not json}", "{not json}"},
		{"fenced invalid json", "```json\n{nope}\n```", "```json\n{nope}\n```"},
		{"plain text", "hello", "hello"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := jsonOrString(c.in)
			if !reflect.DeepEqual(got, c.want) {
				t.Fatalf("jsonOrString(%q) = %#v, want %#v", c.in, got, c.want)
			}
		})
	}
}

// [routines.<name>.settings] reaches gather.sh as $QILLA_SETTINGS (JSON) and
// the template as data.settings; with no settings the variable is absent.
func TestGatherSeesSettingsEnv(t *testing.T) {
	e := setup(t, config.Routine{Kind: config.KindScript, Settings: map[string]any{"accounts": "ja@example.com"}},
		"#!/bin/sh\nprintf '{\"settings\":%s}' \"${QILLA_SETTINGS:-null}\"\n")
	out, ok, err := e.w.Gather(context.Background(), "r")
	if err != nil || !ok {
		t.Fatalf("gather: %v (ok=%v)", err, ok)
	}
	if !strings.Contains(out, `"accounts":"ja@example.com"`) {
		t.Fatalf("gather must see QILLA_SETTINGS: %s", out)
	}
	if got := e.w.GatherEnv("r")["QILLA_SETTINGS"]; got != `{"accounts":"ja@example.com"}` {
		t.Fatalf("QILLA_SETTINGS: %q", got)
	}
}

func TestGatherEnvNoSettings(t *testing.T) {
	e := setup(t, config.Routine{Kind: config.KindScript}, "#!/bin/sh\necho '{}'\n")
	if _, ok := e.w.GatherEnv("r")["QILLA_SETTINGS"]; ok {
		t.Fatal("no settings configured: QILLA_SETTINGS must be absent")
	}
}

// capture is a Renderer that keeps the last envelope.
type capture struct{ data Data }

func (c *capture) Render(_ context.Context, _ string, _ config.Routine, d Data) error {
	c.data = d
	return nil
}

// The template envelope carries settings alongside gathered/result.
func TestTemplateDataCarriesSettings(t *testing.T) {
	e := setup(t, config.Routine{Kind: config.KindScript, Output: "out.md",
		Settings: map[string]any{"accounts": "ja@example.com"}}, "#!/bin/sh\necho '{\"a\":1}'\n")
	c := &capture{}
	e.w.Render = c
	if err := e.w.Run(context.Background(), &queue.Job{Routine: "r"}); err != nil {
		t.Fatal(err)
	}
	if c.data.Settings["accounts"] != "ja@example.com" {
		t.Fatalf("template settings: %+v", c.data.Settings)
	}
}
