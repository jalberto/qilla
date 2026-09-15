package star

import (
	"context"
	"strings"
	"testing"
)

func TestMemModule(t *testing.T) {
	vault := t.TempDir()
	p := write(t, vault, "gather.star", `
def gather(ctx):
    hits = mem.search("offer 41", n=3)
    st = mem.state()
    return {"hit": hits[0]["text"], "state_key": st[0]["key"], "state_routine": st[0]["project"]}
`)
	e := env(vault)
	e.MemSearch = func(q string, n int) ([]MemEntry, error) {
		return []MemEntry{{Project: "t", Kind: "said", Text: "already judged offer 41"}}, nil
	}
	e.MemState = func(routine string) ([]MemEntry, error) {
		return []MemEntry{{Project: routine, Kind: "state", Key: routine + "/last", Text: "3 offers judged"}}, nil
	}
	res, _, err := Run(context.Background(), p, e)
	if err != nil {
		t.Fatal(err)
	}
	m := res.(map[string]any)
	if m["hit"] != "already judged offer 41" || m["state_key"] != "t/last" || m["state_routine"] != "t" {
		t.Fatalf("mem module: %+v", m)
	}
}

// Without a memory store the call fails loudly instead of pretending the
// routine has never seen anything.
func TestMemModuleUnavailable(t *testing.T) {
	vault := t.TempDir()
	p := write(t, vault, "gather.star", "def gather(ctx):\n    return {\"x\": mem.state()}\n")
	_, _, err := Run(context.Background(), p, env(vault))
	if err == nil || !strings.Contains(err.Error(), "not available") {
		t.Fatalf("expected a clear error, got %v", err)
	}
}
