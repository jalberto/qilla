package star

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func write(t *testing.T, dir, name, body string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func env(vault string) Env {
	return Env{Routine: "t", Date: "2026-09-12", Vault: vault,
		Vars: map[string]string{"QILLA_ROUTINE": "t", "QILLA_VAULT": vault}}
}

func TestGatherHelpers(t *testing.T) {
	vault := t.TempDir()
	write(t, vault, "Desk/Journal/2026-09-10.md", "hello\n")
	write(t, vault, "Desk/Journal/2026-09-11.md", "world\n")
	src := `
def gather(ctx):
    notes = glob("Desk/Journal/*.md")
    days = re.findall(r"\d{4}-\d{2}-\d{2}", " ".join(notes))
    r = run(["echo", "hi"])
    print("dbg", len(notes))
    return {
        "routine": ctx.routine,
        "date": ctx.date,
        "vault_env": ctx.env["QILLA_ROUTINE"],
        "notes": notes,
        "days": days,
        "body": read("Desk/Journal/2026-09-10.md").strip(),
        "exists": exists("Desk/Journal/2026-09-10.md"),
        "missing": exists("nope.md"),
        "rc": r["rc"],
        "out": r["out"].strip(),
        "decoded": json.decode('{"a": 1}')["a"],
        "who": env("QILLA_VAULT") != "",
        "n": len(listdir("Desk/Journal")),
    }
`
	p := write(t, t.TempDir(), "gather.star", src)
	res, prints, err := Run(context.Background(), p, env(vault))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(prints, "dbg 2") {
		t.Fatalf("print not captured: %q", prints)
	}
	m, ok := res.(map[string]any)
	if !ok {
		t.Fatalf("want map, got %T", res)
	}
	if m["routine"] != "t" || m["date"] != "2026-09-12" || m["vault_env"] != "t" {
		t.Fatalf("ctx: %+v", m)
	}
	notes := m["notes"].([]any)
	if len(notes) != 2 || notes[0] != "Desk/Journal/2026-09-10.md" {
		t.Fatalf("glob: %v", notes)
	}
	if days := m["days"].([]any); len(days) != 2 || days[1] != "2026-09-11" {
		t.Fatalf("findall: %v", days)
	}
	if m["body"] != "hello" || m["exists"] != true || m["missing"] != false {
		t.Fatalf("files: %+v", m)
	}
	if m["rc"].(int64) != 0 || m["out"] != "hi" {
		t.Fatalf("run: %+v", m)
	}
	if m["decoded"].(int64) != 1 || m["who"] != true || m["n"].(int64) != 2 {
		t.Fatalf("misc: %+v", m)
	}
}

func TestReSubAndGroups(t *testing.T) {
	src := `
def gather(ctx):
    return {
        "sub": re.sub(r"(\w+)@(\w+)", "$2/$1", "alice@example"),
        "groups": re.groups(r"(\d+)-(\d+)", "ref 12-34"),
        "nogroups": re.groups(r"(\d+)x", "none"),
        "split": re.split(r",\s*", "a, b,c"),
        "find": re.find(r"\d+", "abc 42"),
        "nofind": re.find(r"\d+", "abc"),
        "match": re.match(r"^a", "abc"),
    }
`
	p := write(t, t.TempDir(), "gather.star", src)
	res, _, err := Run(context.Background(), p, env(t.TempDir()))
	if err != nil {
		t.Fatal(err)
	}
	m := res.(map[string]any)
	if m["sub"] != "example/alice" {
		t.Fatalf("sub: %v", m["sub"])
	}
	if g := m["groups"].([]any); len(g) != 2 || g[0] != "12" || g[1] != "34" {
		t.Fatalf("groups: %v", g)
	}
	if m["nogroups"] != nil || m["nofind"] != nil {
		t.Fatalf("want None→nil: %+v", m)
	}
	if s := m["split"].([]any); len(s) != 3 || s[2] != "c" {
		t.Fatalf("split: %v", s)
	}
	if m["find"] != "42" || m["match"] != true {
		t.Fatalf("find/match: %+v", m)
	}
}

func TestInvalidRegexp(t *testing.T) {
	p := write(t, t.TempDir(), "gather.star", "def gather(ctx):\n    return {\"x\": re.match(\"(\", \"a\")}\n")
	if _, _, err := Run(context.Background(), p, env(t.TempDir())); err == nil ||
		!strings.Contains(err.Error(), "error parsing regexp") {
		t.Fatalf("want regexp error, got %v", err)
	}
}

func TestNonDictResult(t *testing.T) {
	p := write(t, t.TempDir(), "gather.star", "def gather(ctx):\n    return [1, 2]\n")
	if _, _, err := Run(context.Background(), p, env(t.TempDir())); err == nil ||
		err.Error() != "gather() must return a dict" {
		t.Fatalf("got %v", err)
	}
}

func TestTopLevelResult(t *testing.T) {
	p := write(t, t.TempDir(), "gather.star", "result = {\"a\": 1}\n")
	res, _, err := Run(context.Background(), p, env(t.TempDir()))
	if err != nil {
		t.Fatal(err)
	}
	if res.(map[string]any)["a"].(int64) != 1 {
		t.Fatalf("%+v", res)
	}
}

func TestTimeoutCancels(t *testing.T) {
	p := write(t, t.TempDir(), "gather.star", "def gather(ctx):\n    while True:\n        pass\n")
	e := env(t.TempDir())
	e.Timeout = 200 * time.Millisecond
	start := time.Now()
	if _, _, err := Run(context.Background(), p, e); err == nil {
		t.Fatal("want timeout error")
	}
	if d := time.Since(start); d > 10*time.Second {
		t.Fatalf("cancel took %s", d)
	}
}

func TestSQLiteQuery(t *testing.T) {
	vault := t.TempDir()
	db := filepath.Join(vault, "t.db")
	if err := makeTestDB(db); err != nil {
		t.Fatal(err)
	}
	src := `
def gather(ctx):
    rows = sqlite.query("t.db", "SELECT id, name, score FROM t ORDER BY id")
    one = sqlite.query("t.db", "SELECT name FROM t WHERE id = ?", [2])
    return {"rows": rows, "one": one}
`
	p := write(t, t.TempDir(), "gather.star", src)
	res, _, err := Run(context.Background(), p, env(vault))
	if err != nil {
		t.Fatal(err)
	}
	m := res.(map[string]any)
	rows := m["rows"].([]any)
	if len(rows) != 2 {
		t.Fatalf("rows = %v", rows)
	}
	r0 := rows[0].(map[string]any)
	if r0["id"] != int64(1) || r0["name"] != "alpha" || r0["score"] != 1.5 {
		t.Fatalf("row0 = %#v", r0)
	}
	r1 := rows[1].(map[string]any)
	if r1["name"] != "beta" || r1["score"] != nil {
		t.Fatalf("row1 = %#v", r1)
	}
	if got := m["one"].([]any)[0].(map[string]any)["name"]; got != "beta" {
		t.Fatalf("param query = %v", got)
	}
}

func TestSQLiteRejectsWrites(t *testing.T) {
	vault := t.TempDir()
	if err := makeTestDB(filepath.Join(vault, "t.db")); err != nil {
		t.Fatal(err)
	}
	p := write(t, t.TempDir(), "gather.star", `
def gather(ctx):
    return {"x": sqlite.query("t.db", "UPDATE t SET name = 'x'")}
`)
	_, _, err := Run(context.Background(), p, env(vault))
	if err == nil || !strings.Contains(err.Error(), "read-only") {
		t.Fatalf("want read-only error, got %v", err)
	}
}

func TestSQLiteMissingFile(t *testing.T) {
	vault := t.TempDir()
	p := write(t, t.TempDir(), "gather.star", `
def gather(ctx):
    return {"x": sqlite.query("nope.db", "SELECT 1")}
`)
	_, _, err := Run(context.Background(), p, env(vault))
	if err == nil || !strings.Contains(err.Error(), "sqlite.query") {
		t.Fatalf("want missing-file error, got %v", err)
	}
}

func makeTestDB(path string) error {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return err
	}
	defer db.Close()
	_, err = db.Exec(`CREATE TABLE t (id INTEGER, name TEXT, score REAL);
		INSERT INTO t VALUES (1, 'alpha', 1.5), (2, 'beta', NULL);`)
	return err
}

// [routines.<name>.settings] reaches gather.star as the `settings` global (and
// as ctx.settings): a shareable bundle reads its user-specific values there.
func TestSettingsGlobal(t *testing.T) {
	vault := t.TempDir()
	src := `
def gather(ctx):
    return {
        "accounts": settings["accounts"],
        "limit": settings["limit"],
        "nested": settings["deep"]["k"],
        "via_ctx": ctx.settings["accounts"],
        "keys": sorted(settings.keys()),
    }
`
	p := write(t, vault, "gather.star", src)
	e := env(vault)
	e.Settings = map[string]any{
		"accounts": "ja@example.com",
		"limit":    int64(20),
		"deep":     map[string]any{"k": "v"},
	}
	res, _, err := Run(context.Background(), p, e)
	if err != nil {
		t.Fatal(err)
	}
	m := res.(map[string]any)
	if m["accounts"] != "ja@example.com" || m["via_ctx"] != "ja@example.com" {
		t.Fatalf("accounts: %+v", m)
	}
	if m["limit"] != int64(20) || m["nested"] != "v" {
		t.Fatalf("limit/nested: %+v", m)
	}
	if got := m["keys"].([]any); len(got) != 3 || got[0] != "accounts" {
		t.Fatalf("keys: %v", got)
	}
}

// No settings configured: the global exists and is an empty dict, so a bundle
// can do settings.get(...) without guarding.
func TestSettingsGlobalEmpty(t *testing.T) {
	vault := t.TempDir()
	p := write(t, vault, "gather.star", "def gather(ctx):\n    return {\"n\": len(settings), \"x\": settings.get(\"x\", \"default\")}\n")
	res, _, err := Run(context.Background(), p, env(vault))
	if err != nil {
		t.Fatal(err)
	}
	m := res.(map[string]any)
	if m["n"] != int64(0) || m["x"] != "default" {
		t.Fatalf("empty settings: %+v", m)
	}
}
