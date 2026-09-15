package main

import (
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// queueEnv points cmdQueue at a throwaway config and queue dir.
func queueEnv(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	cfg := filepath.Join(dir, "qilla.toml")
	if err := os.WriteFile(cfg, []byte("queue_dir = \""+filepath.Join(dir, "queue")+"\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("QILLA_CONFIG", cfg)
	t.Setenv("QILLA_QUEUE_DIR", "")
}

// capture runs one queue command and returns its stdout.
func capture(t *testing.T, args ...string) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	old := os.Stdout
	os.Stdout = w
	cmdErr := cmdQueue(args)
	os.Stdout = old
	w.Close()
	b, _ := io.ReadAll(r)
	if cmdErr != nil {
		t.Fatalf("queue %v: %v", args, cmdErr)
	}
	return string(b)
}

func TestCmdQueueAddListDrop(t *testing.T) {
	queueEnv(t)
	id := strings.TrimSpace(capture(t, "add", "time-promises", "--text", "call Ana next week",
		"--source", "meeting", "--people", "Ana, Bob", "--date-hint", "next week"))
	if !strings.HasPrefix(id, "c_") {
		t.Fatalf("add printed %q", id)
	}
	line := strings.TrimSpace(capture(t, "list"))
	for _, want := range []string{`"id":"` + id + `"`, `"source":"meeting"`, `"text":"call Ana next week"`,
		`"people":["Ana","Bob"]`, `"date_hint":"next week"`} {
		if !strings.Contains(line, want) {
			t.Errorf("list line %s missing %s", line, want)
		}
	}
	if got := strings.TrimSpace(capture(t, "count", "facts")); got != "" {
		t.Errorf("count of an empty family printed %q", got)
	}
	if got := strings.TrimSpace(capture(t, "drop", id)); got != "dropped=1" {
		t.Errorf("drop printed %q", got)
	}
	if got := capture(t, "list"); got != "" {
		t.Errorf("list after drop printed %q", got)
	}
}

// TestCmdQueueCountExits checks the scripts' contract: something queued ⇒ exit 1.
// count exits the process, so it runs in a re-exec of this test binary.
func TestCmdQueueCountExits(t *testing.T) {
	if os.Getenv("QILLA_QUEUE_COUNT_CHILD") == "1" {
		queueEnv(t)
		capture(t, "add", "facts", "--text", "Alice moved to Lisbon")
		cmdQueue([]string{"count"}) // exits 1
		return
	}
	cmd := exec.Command(os.Args[0], "-test.run=TestCmdQueueCountExits", "-test.v=false")
	cmd.Env = append(os.Environ(), "QILLA_QUEUE_COUNT_CHILD=1")
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("count with a queued candidate exited 0: %s", out)
	}
	if ee, ok := err.(*exec.ExitError); !ok || ee.ExitCode() != 1 {
		t.Fatalf("exit = %v, want 1: %s", err, out)
	}
	if !strings.Contains(string(out), "facts=1") {
		t.Errorf("count printed %s, want a \"facts=1\" line", out)
	}
}

func TestCmdQueueBadInput(t *testing.T) {
	queueEnv(t)
	for _, args := range [][]string{
		{}, {"nope"}, {"add", "--text", "x"}, {"add", "facts"}, {"list", "Bad"}, {"drop"},
	} {
		if err := cmdQueue(args); err == nil {
			t.Errorf("queue %v was accepted", args)
		}
	}
}
