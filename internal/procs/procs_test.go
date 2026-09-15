package procs

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func TestIsInteractiveClaude(t *testing.T) {
	cases := []struct {
		name string
		comm string
		argv []string
		want bool
	}{
		{"comm claude", "claude\n", []string{"/usr/bin/claude"}, true},
		{"argv base claude", "node", []string{"/opt/node/claude", "chat"}, true},
		{"print one-shot", "claude", []string{"claude", "-p", "hi"}, false},
		{"print long flag", "claude", []string{"claude", "--print", "hi"}, false},
		{"not claude", "node", []string{"node", "server.js"}, false},
		{"empty", "", nil, false},
		{"resume is interactive", "claude", []string{"claude", "--resume", "abc"}, true},
	}
	for _, c := range cases {
		if got := IsInteractiveClaude(c.comm, c.argv); got != c.want {
			t.Errorf("%s: got %v want %v", c.name, got, c.want)
		}
	}
}

// fakeProc writes a minimal /proc/<pid> with comm and PPid.
func fakeProc(t *testing.T, root string, pid, ppid int, comm string) {
	t.Helper()
	dir := filepath.Join(root, fmt.Sprint(pid))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "comm"), []byte(comm+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	status := fmt.Sprintf("Name:\t%s\nPid:\t%d\nPPid:\t%d\n", comm, pid, ppid)
	if err := os.WriteFile(filepath.Join(dir, "status"), []byte(status), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestAncestorClaude(t *testing.T) {
	root := t.TempDir()
	old := Root
	Root = root
	t.Cleanup(func() { Root = old })

	// 1 (init) <- 100 (zsh) <- 200 (claude) <- 300 (bash) <- 400 (qilla)
	fakeProc(t, root, 100, 1, "zsh")
	fakeProc(t, root, 200, 100, "claude")
	fakeProc(t, root, 300, 200, "bash")
	fakeProc(t, root, 400, 300, "qilla")

	if got := AncestorClaude(400); got != 200 {
		t.Fatalf("AncestorClaude(400) = %d, want 200", got)
	}
	if got := AncestorClaude(200); got != 200 {
		t.Fatalf("self = %d, want 200", got)
	}
	if got := AncestorClaude(100); got != 0 {
		t.Fatalf("no claude ancestor = %d, want 0", got)
	}
	if got := PPid(300); got != 200 {
		t.Fatalf("PPid(300) = %d", got)
	}
}
