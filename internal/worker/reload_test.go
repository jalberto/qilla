package worker

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jalberto/qilla/internal/config"
	"github.com/jalberto/qilla/internal/queue"
)

// reloadEnv writes a real qilla.toml and a worker over it. The routine
// vault-sync is a script routine whose gather.sh is already in place, so the
// only thing that can make the job fail is the config not knowing it.
func reloadEnv(t *testing.T, toml string) (*Worker, *config.Holder, string) {
	t.Helper()
	root := t.TempDir()
	vault := filepath.Join(root, "vault")
	dir := filepath.Join(vault, RoutinesDir, "vault-sync")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(dir, "gather.sh"), []byte(`echo '{"ok":true}'`), 0o755)
	path := filepath.Join(root, "qilla.toml")
	write := func(body string) {
		if err := os.WriteFile(path, []byte("vault = \""+vault+"\"\nstate_dir = \""+filepath.Join(root, "state")+"\"\n"+body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(toml)
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	hold := config.NewHolder(cfg)
	s, err := queue.Open(filepath.Join(root, "q.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	w, err := New(hold, s.DB())
	if err != nil {
		t.Fatal(err)
	}
	return w, hold, path
}

// rewrite replaces the config file and makes sure its mtime differs from the
// one the holder saw (same-second writes are caught by the size check, but the
// bodies here also differ in length).
func rewrite(t *testing.T, path, body string) {
	t.Helper()
	old, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	head := string(old)
	if i := strings.Index(head, "[routines"); i >= 0 {
		head = head[:i]
	}
	if err := os.WriteFile(path, []byte(head+body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestReloadPicksUpNewRoutine(t *testing.T) {
	w, hold, path := reloadEnv(t, "")
	err := w.Run(context.Background(), &queue.Job{ID: 1, Routine: "vault-sync"})
	if err == nil || !strings.Contains(err.Error(), "not in config") {
		t.Fatalf("want a not-in-config failure before the edit, got %v", err)
	}
	rewrite(t, path, "[routines.vault-sync]\nkind = \"script\"\nschedule = \"manual\"\n")
	if !hold.Reload(nil) {
		t.Fatal("reload did not pick up the edit")
	}
	if err := w.Run(context.Background(), &queue.Job{ID: 2, Routine: "vault-sync"}); err != nil {
		t.Fatalf("job after the reload: %v", err)
	}
}

func TestReloadKeepsPreviousConfigOnError(t *testing.T) {
	w, hold, path := reloadEnv(t, "[routines.vault-sync]\nkind = \"script\"\nschedule = \"manual\"\n")
	rewrite(t, path, "[routines.vault-sync\nkind = oops")
	var logged int
	logf := func(string, ...any) { logged++ }
	if hold.Reload(logf) {
		t.Fatal("a broken config must not be swapped in")
	}
	if _, ok := hold.Get().Routines["vault-sync"]; !ok {
		t.Fatal("the previous good config was lost")
	}
	if err := w.Run(context.Background(), &queue.Job{ID: 1, Routine: "vault-sync"}); err != nil {
		t.Fatalf("a broken config edit must not fail the job: %v", err)
	}
	if logged != 1 {
		t.Fatalf("want the failure logged once, got %d lines", logged)
	}
	// the same failure on a later check is silent (no new mtime, nothing new to say)
	if hold.Reload(logf); logged != 1 {
		t.Fatalf("the same failure was logged again: %d lines", logged)
	}
}
