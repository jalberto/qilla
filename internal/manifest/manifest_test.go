package manifest

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const sample = `
name = "newsletters"
summary = "Triage newsletters from Gmail into a daily feed note"

[requires]
commands = ["msgvault", "curl"]
secrets = []
settings = ["accounts"]

[[optional]]
name = "karakeep"
settings = ["karakeep_url"]
secrets = ["karakeep"]
commands = []
hint = "Set settings.karakeep_url to save top stories."

[[user_files]]
path = "preferences.md"
required = false
hint = "Interests + services you use."

[[signals]]
name = "newsletter label"
run = ["msgvault", "search", "label:Newsletter", "--account", "{{settings.accounts}}", "--json"]
expect = "json-nonempty"
hint = "No mail labelled Newsletter."
`

func bundle(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	if body != "" {
		if err := os.WriteFile(filepath.Join(dir, File), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func TestLoadParsesEveryBlock(t *testing.T) {
	m, err := Load(bundle(t, sample))
	if err != nil {
		t.Fatal(err)
	}
	if m.Name != "newsletters" || !strings.HasPrefix(m.Summary, "Triage") {
		t.Fatalf("name/summary: %+v", m)
	}
	if got := m.Requires.Commands; len(got) != 2 || got[0] != "msgvault" {
		t.Fatalf("requires.commands: %v", got)
	}
	if len(m.Requires.Settings) != 1 || m.Requires.Settings[0] != "accounts" {
		t.Fatalf("requires.settings: %v", m.Requires.Settings)
	}
	if len(m.Optional) != 1 || m.Optional[0].Name != "karakeep" || m.Optional[0].Secrets[0] != "karakeep" {
		t.Fatalf("optional: %+v", m.Optional)
	}
	if len(m.UserFiles) != 1 || m.UserFiles[0].Path != "preferences.md" || m.UserFiles[0].Required {
		t.Fatalf("user_files: %+v", m.UserFiles)
	}
	if len(m.Signals) != 1 || m.Signals[0].Expect != ExpectJSONNonempty || len(m.Signals[0].Run) != 6 {
		t.Fatalf("signals: %+v", m.Signals)
	}
}

func TestLoadMissingManifestIsNotAnError(t *testing.T) {
	m, err := Load(bundle(t, ""))
	if err != nil || m != nil {
		t.Fatalf("missing manifest: %v %v", m, err)
	}
}

func TestLoadRejectsUnknownExpect(t *testing.T) {
	_, err := Load(bundle(t, "name = \"x\"\n[[signals]]\nname = \"s\"\nrun = [\"true\"]\nexpect = \"maybe\"\n"))
	if err == nil || !strings.Contains(err.Error(), "unknown expect") {
		t.Fatalf("want unknown expect error, got %v", err)
	}
}

// fake is a runner env: present commands/secrets and a scripted signal output.
type fake struct {
	cmds    map[string]bool
	secrets map[string]bool
	out     string
	err     error
	gotArgs []string
}

func (f *fake) env() Env {
	return Env{
		LookPath: func(c string) (string, error) {
			if f.cmds[c] {
				return "/usr/bin/" + c, nil
			}
			return "", errors.New("not found")
		},
		HasSecret: func(n string) bool { return f.secrets[n] },
		Run: func(name string, args ...string) (string, error) {
			f.gotArgs = append([]string{name}, args...)
			return f.out, f.err
		},
	}
}

func find(rs []Result, item string) *Result {
	for i := range rs {
		if rs[i].Item == item {
			return &rs[i]
		}
	}
	return nil
}

func TestCheckHardFailAndOptionalDisabled(t *testing.T) {
	dir := bundle(t, sample)
	m, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	f := &fake{cmds: map[string]bool{"curl": true}, out: `[{"id":1}]`}
	rs := m.Check(f.env(), map[string]any{"accounts": "ja@example.com"})

	if r := find(rs, "command msgvault"); r == nil || r.OK || !r.Hard || r.Status != StatusMissing {
		t.Fatalf("msgvault must fail hard: %+v", r)
	}
	if r := find(rs, "command curl"); r == nil || !r.OK {
		t.Fatalf("curl must pass: %+v", r)
	}
	if r := find(rs, "setting accounts"); r == nil || !r.OK {
		t.Fatalf("setting accounts must pass: %+v", r)
	}
	o := find(rs, "optional karakeep")
	if o == nil || o.Status != StatusDisabled || !o.OK {
		t.Fatalf("karakeep must be disabled but not fail: %+v", o)
	}
	if !strings.Contains(o.Hint, "setting karakeep_url") || !strings.Contains(o.Hint, "secret karakeep") {
		t.Fatalf("hint must name what is missing: %q", o.Hint)
	}
	// an optional user_file that is absent is a note, never a failure
	if r := find(rs, "file preferences.md"); r == nil || !r.OK || r.Status != StatusMissing {
		t.Fatalf("optional file: %+v", r)
	}
	if !Failed(rs) {
		t.Fatal("a missing required command must fail the check")
	}
	if !strings.Contains(FirstFailure(rs), "command msgvault") {
		t.Fatalf("FirstFailure: %q", FirstFailure(rs))
	}
	// the signal ran with {{settings.accounts}} expanded
	want := []string{"msgvault", "search", "label:Newsletter", "--account", "ja@example.com", "--json"}
	if strings.Join(f.gotArgs, " ") != strings.Join(want, " ") {
		t.Fatalf("placeholder expansion: %v", f.gotArgs)
	}
	if r := find(rs, "signal newsletter label"); r == nil || !r.OK {
		t.Fatalf("signal must pass on a nonempty JSON array: %+v", r)
	}
}

func TestCheckAllGreenAndOptionalEnabled(t *testing.T) {
	m, _ := Load(bundle(t, sample))
	if err := os.WriteFile(filepath.Join(m.Dir, "preferences.md"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	f := &fake{cmds: map[string]bool{"curl": true, "msgvault": true}, secrets: map[string]bool{"karakeep": true}, out: `[1]`}
	rs := m.Check(f.env(), map[string]any{"accounts": "a", "karakeep_url": "http://x"})
	if Failed(rs) {
		t.Fatalf("all requirements met, still failed: %s", FirstFailure(rs))
	}
	if o := find(rs, "optional karakeep"); o == nil || o.Status != StatusEnabled {
		t.Fatalf("karakeep must be enabled: %+v", o)
	}
	if r := find(rs, "file preferences.md"); r == nil || r.Status != StatusOK {
		t.Fatalf("present file: %+v", r)
	}
}

func TestRequiredUserFileMissingFails(t *testing.T) {
	m, err := Load(bundle(t, "name = \"x\"\n[[user_files]]\npath = \"accounts.md\"\nrequired = true\nhint = \"list them\"\n"))
	if err != nil {
		t.Fatal(err)
	}
	rs := m.Check(Env{}, nil)
	if !Failed(rs) {
		t.Fatalf("missing required user file must fail: %+v", rs)
	}
	if !strings.Contains(FirstFailure(rs), "list them") {
		t.Fatalf("hint must reach the user: %q", FirstFailure(rs))
	}
}

func TestSignalExpectVariants(t *testing.T) {
	cases := []struct {
		expect string
		out    string
		err    error
		want   bool
	}{
		{ExpectNonempty, "hello\n", nil, true},
		{ExpectNonempty, "   \n", nil, false},
		{ExpectNonempty, "hello", errors.New("rc 1"), false},
		{"", "hello", nil, true}, // empty expect defaults to nonempty
		{ExpectOK, "", nil, true},
		{ExpectOK, "anything", errors.New("rc 2"), false},
		{ExpectJSONNonempty, `[]`, nil, false},
		{ExpectJSONNonempty, `[{"a":1}]`, nil, true},
		{ExpectJSONNonempty, `{}`, nil, false},
		{ExpectJSONNonempty, `{"a":1}`, nil, true},
		{ExpectJSONNonempty, `not json`, nil, false},
	}
	for _, c := range cases {
		f := &fake{out: c.out, err: c.err}
		r := signalResult(f.env(), Signal{Name: "s", Run: []string{"x"}, Expect: c.expect}, nil)
		if r.OK != c.want {
			t.Errorf("expect=%q out=%q err=%v: got OK=%v", c.expect, c.out, c.err, r.OK)
		}
	}
}

func TestSignalUnsetPlaceholderFails(t *testing.T) {
	f := &fake{out: "x"}
	r := signalResult(f.env(), Signal{Name: "s", Run: []string{"x", "{{settings.who}}"}}, map[string]any{})
	if r.OK || !strings.Contains(r.Hint, "who") {
		t.Fatalf("unset placeholder must fail with a clear hint: %+v", r)
	}
	if f.gotArgs != nil {
		t.Fatal("the command must not run with an unexpanded placeholder")
	}
}

func TestExpandNonStringSettings(t *testing.T) {
	got, err := Expand([]string{"--limit", "{{settings.n}}", "--on", "{{settings.flag}}"},
		map[string]any{"n": int64(7), "flag": true})
	if err != nil {
		t.Fatal(err)
	}
	if got[1] != "7" || got[3] != "true" {
		t.Fatalf("scalar expansion: %v", got)
	}
	if _, err := Expand([]string{"{{env.HOME}}"}, nil); err == nil {
		t.Fatal("only settings placeholders are allowed")
	}
}
