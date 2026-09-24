package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jalberto/qilla/internal/fetch"
)

// hermeticFetchConfig points qilla at a temp config: its own state dir (so
// the ledger is not JA's) and dead hister/ladder URLs (so no rung reaches
// a real service).
func hermeticFetchConfig(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	cfg := filepath.Join(dir, "qilla.toml")
	body := "state_dir = \"" + dir + "/state\"\n[fetch]\nhister_url = \"http://127.0.0.1:1\"\nladder_url = \"http://127.0.0.1:1\"\n"
	if err := os.WriteFile(cfg, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("QILLA_CONFIG", cfg)
	return filepath.Join(dir, "state")
}

func TestCmdFetchLedgerForget(t *testing.T) {
	state := hermeticFetchConfig(t)
	path := filepath.Join(state, "fetch", "ledger.json")
	if err := (fetch.Ledger{"example.invalid": {Rung: "curl", OKCount: 1}}).Save(path); err != nil {
		t.Fatal(err)
	}
	if err := cmdFetch([]string{"--ledger"}); err != nil {
		t.Fatal(err)
	}
	if err := cmdFetch([]string{"--forget", "example.invalid"}); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(path); strings.Contains(string(b), "example.invalid") {
		t.Fatalf("still in ledger: %s", b)
	}
	if err := cmdFetch([]string{"--forget", "www.example.invalid"}); err == nil {
		t.Fatal("forgetting twice should fail")
	}
}

func TestCmdFetchUsage(t *testing.T) {
	for _, args := range [][]string{{}, {"--max-rung"}, {"--max-rung", "99", "u"}, {"--nope", "u"}} {
		if err := cmdFetch(args); !errors.Is(err, errUsage) {
			t.Fatalf("cmdFetch(%v) = %v, want a usage error", args, err)
		}
	}
}

// With nothing on PATH every rung is skipped: no content, so exit 3's error.
func TestCmdFetchExitThree(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	hermeticFetchConfig(t)
	err := cmdFetch([]string{"https://example.invalid", "--max-rung", "2", "--json"})
	var blocked errBlocked
	if !errors.As(err, &blocked) {
		t.Fatalf("err = %v, want errBlocked", err)
	}
	if blocked.kind != "empty" {
		t.Fatalf("kind = %q, want empty", blocked.kind)
	}
}
