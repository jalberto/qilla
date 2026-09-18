package main

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// gmailEnv points cmdGmail at a throwaway config with its own stage file.
func gmailEnv(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	stage := filepath.Join(dir, "gmail-stage.jsonl")
	cfg := filepath.Join(dir, "qilla.toml")
	body := "queue_dir = \"" + filepath.Join(dir, "queue") + "\"\nstate_dir = \"" + dir + "\"\n" +
		"[gmail]\nstage_file = \"" + stage + "\"\n"
	if err := os.WriteFile(cfg, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("QILLA_CONFIG", cfg)
	return stage
}

// gmailOut runs one gmail command and returns stdout plus the error.
func gmailOut(t *testing.T, args ...string) (string, error) {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	old := os.Stdout
	os.Stdout = w
	cmdErr := cmdGmail(args)
	os.Stdout = old
	w.Close()
	b, _ := io.ReadAll(r)
	return string(b), cmdErr
}

func TestGmailStagedEmpty(t *testing.T) {
	gmailEnv(t)
	for _, args := range [][]string{{"staged"}, {"staged", "--ids"}} {
		out, err := gmailOut(t, args...)
		if err != nil || out != "" {
			t.Fatalf("%v: want silent exit 0, got %q %v", args, out, err)
		}
	}
	out, err := gmailOut(t, "staged", "--json")
	if err != nil || strings.TrimSpace(out) != "[]" {
		t.Fatalf("--json on an empty ledger: %q %v", out, err)
	}
}

func TestGmailStageThenStagedIDs(t *testing.T) {
	gmailEnv(t)
	if _, err := gmailOut(t, "stage", "--account", "a@x", "--id", "m1", "--msgvault-id", "7"); err != nil {
		t.Fatal(err)
	}
	// duplicate (account,id) is a no-op
	out, err := gmailOut(t, "stage", "--account", "a@x", "--id", "m1", "--msgvault-id", "7")
	if err != nil || !strings.Contains(out, `"staged":0`) {
		t.Fatalf("duplicate stage: %q %v", out, err)
	}
	if _, err := gmailOut(t, "stage", "--account", "b@x", "--id", "m2"); err != nil {
		t.Fatal(err)
	}
	out, err = gmailOut(t, "staged", "--ids")
	if err != nil || strings.TrimSpace(out) != "7 m2" {
		t.Fatalf("--ids: %q %v", out, err)
	}
	out, err = gmailOut(t, "staged", "--ids", "--account", "b@x")
	if err != nil || strings.TrimSpace(out) != "m2" {
		t.Fatalf("--ids --account: %q %v", out, err)
	}
}

func TestGmailStageStdin(t *testing.T) {
	gmailEnv(t)
	r, w, _ := os.Pipe()
	go func() {
		io.WriteString(w, "{\"account\":\"a@x\",\"id\":\"m1\"}\n{\"account\":\"a@x\",\"id\":\"m2\"}\n")
		w.Close()
	}()
	old := os.Stdin
	os.Stdin = r
	defer func() { os.Stdin = old }()
	out, err := gmailOut(t, "stage", "--stdin")
	if err != nil || !strings.Contains(out, `"staged":2`) {
		t.Fatalf("--stdin: %q %v", out, err)
	}
}

func TestGmailApplyOnlyIfRunOKRefusesWithExit2(t *testing.T) {
	gmailEnv(t)
	if _, err := gmailOut(t, "stage", "--account", "a@x", "--id", "m1"); err != nil {
		t.Fatal(err)
	}
	// the routine never ran ok in this throwaway ledger
	_, err := gmailOut(t, "apply", "--only-if-run-ok", "newsletters")
	if err == nil {
		t.Fatal("apply must refuse when the reading run is older than what is staged")
	}
	if exitCode(err) != 2 {
		t.Fatalf("want exit 2, got %d (%v)", exitCode(err), err)
	}
}

func TestGmailSecretName(t *testing.T) {
	if got := secretName("ja@vizlegal.com"); got != "gmail-ja-vizlegal-com" {
		t.Fatalf("secretName: %q", got)
	}
}
