package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jalberto/qilla/internal/config"
)

// hookCfg is a throwaway vault + state dir, with $HOME pointed at it so the
// qmd-index check reads the temp cache and never the machine's.
func hookCfg(t *testing.T) *config.Config {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("PATH", "") // no qmd on PATH: the session-start signal never spawns anything
	t.Setenv("QILLA_QUEUE_DIR", filepath.Join(home, "queue"))
	vault := filepath.Join(home, "Notes")
	if err := os.MkdirAll(filepath.Join(vault, "Desk", "Journal"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(home, "state"), 0o755); err != nil {
		t.Fatal(err)
	}
	return &config.Config{
		Vault: vault, StateDir: filepath.Join(home, "state"), JournalDir: "Desk/Journal",
		Persona: "Qilla/Persona.md", Rules: "Qilla/Rules.md", Questions: "Desk/Questions.md",
		Hooks: config.Hooks{LearnEvery: config.DefaultLearnEvery},
	}
}

func TestSessionSignalsHarness(t *testing.T) {
	cfg := hookCfg(t)
	home := filepath.Dir(cfg.Vault)
	// a qmd index older than a day
	db := filepath.Join(home, ".cache", "qmd", "notes.sqlite")
	if err := os.MkdirAll(filepath.Dir(db), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(db, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-48 * time.Hour)
	if err := os.Chtimes(db, old, old); err != nil {
		t.Fatal(err)
	}

	got := strings.Join(sessionSignals(cfg), "\n")
	today := time.Now().Format("2006-01-02")
	for _, want := range []string{
		"(qilla catchup --all for the rows).",
		"Today's daily note (" + today + ") does not exist yet.",
		"MISSING harness files:",
		"Qilla/Persona.md",
		".claude/settings.json",
		"qmd index is >1 day old",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("session-start signals missing %q:\n%s", want, got)
		}
	}
	if !strings.Contains(got, "n=0 jots=0") {
		t.Fatalf("the catchup header is the first catchup line:\n%s", got)
	}
	for _, line := range strings.Split(got, "\n") {
		if strings.TrimSpace(line) == "" {
			t.Fatalf("one line per signal, no blanks:\n%q", got)
		}
	}
}

// A vault with today's note, the scaffold in place and a fresh index is quiet
// about all three.
func TestSessionSignalsQuietVault(t *testing.T) {
	cfg := hookCfg(t)
	write := func(rel string) {
		p := cfg.VaultPath(rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("Desk/Journal/" + time.Now().Format("2006-01-02") + ".md")
	write("Qilla/Qilla.md")
	write("Qilla/Persona.md")
	write("Qilla/Rules.md")
	write("Qilla/Research/default.md")
	write("Qilla/Subagents/Subagents.md")
	write(".claude/settings.json")
	// a fresh index
	db := filepath.Join(filepath.Dir(cfg.Vault), ".cache", "qmd", "notes.sqlite")
	os.MkdirAll(filepath.Dir(db), 0o755)
	os.WriteFile(db, []byte("x"), 0o644)

	got := strings.Join(sessionSignals(cfg), "\n")
	for _, no := range []string{"does not exist yet", "MISSING harness files", "qmd index"} {
		if strings.Contains(got, no) {
			t.Fatalf("unexpected %q in a healthy vault:\n%s", no, got)
		}
	}
}

func TestStaleQmdIndexFresh(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	if staleQmdIndex(time.Now()) {
		t.Fatal("no index at all is not a stale index")
	}
	db := filepath.Join(home, ".cache", "qmd", "notes.sqlite3")
	os.MkdirAll(filepath.Dir(db), 0o755)
	os.WriteFile(db, []byte("x"), 0o644)
	if staleQmdIndex(time.Now()) {
		t.Fatal("a just-written index is fresh")
	}
}

// --- stop hook ---

func stopCfg(t *testing.T, every, toolUses int) (*config.Config, hookInput) {
	t.Helper()
	dir := t.TempDir()
	tp := filepath.Join(dir, "transcript.jsonl")
	var b strings.Builder
	for i := 0; i < toolUses; i++ {
		b.WriteString(`{"type":"assistant","content":[{"type":"tool_use","name":"Bash"}]}` + "\n")
	}
	b.WriteString(`{"type":"user","content":"no tool here"}` + "\n")
	if err := os.WriteFile(tp, []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{StateDir: filepath.Join(dir, "state"), Hooks: config.Hooks{LearnEvery: every}}
	return cfg, hookInput{SessionID: "s1", TranscriptPath: tp}
}

func TestLearnDueBelowThreshold(t *testing.T) {
	cfg, in := stopCfg(t, 15, 14)
	if learnDue(cfg, in) {
		t.Fatal("14 tool calls is below the threshold of 15")
	}
	if _, err := os.Stat(learnMarker(cfg, in.SessionID)); err == nil {
		t.Fatal("no checkpoint is written when nothing is due")
	}
}

func TestLearnDueRearmsAfterCheckpoint(t *testing.T) {
	cfg, in := stopCfg(t, 15, 20)
	if !learnDue(cfg, in) {
		t.Fatal("20 tool calls is due")
	}
	b, err := os.ReadFile(learnMarker(cfg, in.SessionID))
	if err != nil || strings.TrimSpace(string(b)) != "20" {
		t.Fatalf("marker holds the counted calls: %q %v", b, err)
	}
	// same transcript, second call: the checkpoint absorbs it
	if learnDue(cfg, in) {
		t.Fatal("a second stop with no new work must not block again")
	}
	// more work → due again
	f, _ := os.OpenFile(in.TranscriptPath, os.O_APPEND|os.O_WRONLY, 0o644)
	for i := 0; i < 15; i++ {
		f.WriteString(`{"type":"tool_use"}` + "\n")
	}
	f.Close()
	if !learnDue(cfg, in) {
		t.Fatal("15 more tool calls re-arm the block")
	}
}

func TestLearnEveryZeroDisables(t *testing.T) {
	cfg, in := stopCfg(t, 0, 100)
	if learnDue(cfg, in) {
		t.Fatal("learn_every = 0 disables the stop hook")
	}
}

func TestLearnDueNeedsSessionAndTranscript(t *testing.T) {
	cfg, in := stopCfg(t, 15, 30)
	noSID := in
	noSID.SessionID = ""
	if learnDue(cfg, noSID) {
		t.Fatal("no session id → no marker, no block")
	}
	gone := in
	gone.TranscriptPath = filepath.Join(t.TempDir(), "missing.jsonl")
	if learnDue(cfg, gone) {
		t.Fatal("an unreadable transcript is never due")
	}
}

func TestInScopeGuard(t *testing.T) {
	cfg := &config.Config{Vault: "/home/x/Notes", Guard: config.Guard{Scope: "vault"}}
	if !inScope(cfg, "/home/x/Notes/Desk") {
		t.Fatal("inside the vault is in scope")
	}
	if inScope(cfg, "/home/x/Projects/other") {
		t.Fatal("outside the vault is out of scope")
	}
	cfg.Guard.Scope = "all"
	if !inScope(cfg, "/home/x/Projects/other") {
		t.Fatal("scope = all is everywhere")
	}
}

// learn_every defaults to 15 and an explicit 0 survives the load.
func TestLearnEveryConfigDefault(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "qilla.toml")
	os.WriteFile(p, []byte("vault = \""+dir+"\"\n"), 0o644)
	cfg, err := config.Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Hooks.LearnEvery != 15 {
		t.Fatalf("default learn_every = 15, got %d", cfg.Hooks.LearnEvery)
	}
	os.WriteFile(p, []byte("vault = \""+dir+"\"\n[hooks]\nlearn_every = 0\n"), 0o644)
	cfg, err = config.Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Hooks.LearnEvery != 0 {
		t.Fatalf("an explicit 0 disables, got %d", cfg.Hooks.LearnEvery)
	}
}
