package star

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jalberto/qilla/internal/manifest"
)

// capEnv is env() plus the declared capabilities.
func capEnv(vault string, c *manifest.Capabilities) Env {
	e := env(vault)
	e.Caps = c
	return e
}

// hostOf is the host[:port] a bundle declares for an httptest server.
func hostOf(t *testing.T, raw string) string {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	return u.Host
}

func runSrc(t *testing.T, e Env, src string) (any, []map[string]any, error) {
	t.Helper()
	p := write(t, t.TempDir(), "gather.star", src)
	res, actions, prints, err := RunActions(context.Background(), p, e)
	if prints != "" {
		t.Log(prints)
	}
	return res, actions, err
}

// ── A2 · http ────────────────────────────────────────────────────────────

func TestHTTPDeclaredHost(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"method":"` + r.Method + `","got":` + quote(string(body)) + `,"auth":"` + r.Header.Get("Authorization") + `"}`))
	}))
	defer srv.Close()
	vault := t.TempDir()
	e := capEnv(vault, &manifest.Capabilities{HTTP: &manifest.HTTPCap{
		Hosts: []string{hostOf(t, srv.URL)}, Methods: []string{"GET", "POST"}}})

	res, _, err := runSrc(t, e, `
def gather(ctx):
    g = http("GET", "`+srv.URL+`/x")
    p = http("POST", "`+srv.URL+`/y", headers={"Authorization": "Bearer zzz"}, json={"a": 1})
    return {"status": g["status"], "ctype": g["headers"]["content-type"],
            "method": p["json"]["method"], "body": p["json"]["got"], "auth": p["json"]["auth"]}
`)
	if err != nil {
		t.Fatal(err)
	}
	m := res.(map[string]any)
	if m["status"] != int64(200) {
		t.Errorf("status = %v", m["status"])
	}
	if !strings.Contains(m["ctype"].(string), "json") {
		t.Errorf("headers not lowercased/parsed: %v", m["ctype"])
	}
	if m["method"] != "POST" || m["body"] != `{"a":1}` {
		t.Errorf("post = %v / %v", m["method"], m["body"])
	}
	if m["auth"] != "Bearer zzz" {
		t.Errorf("headers= not sent: %v", m["auth"])
	}
}

func quote(s string) string {
	return `"` + strings.ReplaceAll(strings.ReplaceAll(s, `\`, `\\`), `"`, `\"`) + `"`
}

func TestHTTPUndeclaredHostAndMethod(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer srv.Close()
	host := hostOf(t, srv.URL)
	e := capEnv(t.TempDir(), &manifest.Capabilities{HTTP: &manifest.HTTPCap{Hosts: []string{host}}})

	_, _, err := runSrc(t, e, `
def gather(ctx):
    return {"x": http("GET", "http://example.invalid/x")["status"]}
`)
	if err == nil || !strings.Contains(err.Error(), "capability http: host example.invalid not declared in routine.toml") {
		t.Fatalf("host error = %v", err)
	}
	_, _, err = runSrc(t, e, `
def gather(ctx):
    return {"x": http("POST", "`+srv.URL+`/x")["status"]}
`)
	if err == nil || !strings.Contains(err.Error(), "capability http: method POST not declared") {
		t.Fatalf("method error = %v", err)
	}
}

func TestHTTPErrorHidesHeaders(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	host := hostOf(t, srv.URL)
	srv.Close() // nothing listening any more: the transport fails
	e := capEnv(t.TempDir(), &manifest.Capabilities{HTTP: &manifest.HTTPCap{Hosts: []string{host}}})
	_, _, err := runSrc(t, e, `
def gather(ctx):
    return {"x": http("GET", "http://`+host+`/x", headers={"Authorization": "Bearer SUPERSECRET"})["status"]}
`)
	if err == nil {
		t.Fatal("want an error")
	}
	if strings.Contains(err.Error(), "SUPERSECRET") {
		t.Fatalf("error leaks a header value: %v", err)
	}
}

func TestHTTPRedirectOffAllowlist(t *testing.T) {
	off := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("nope")) }))
	defer off.Close()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, off.URL+"/elsewhere", http.StatusFound)
	}))
	defer srv.Close()
	e := capEnv(t.TempDir(), &manifest.Capabilities{HTTP: &manifest.HTTPCap{Hosts: []string{hostOf(t, srv.URL)}}})
	_, _, err := runSrc(t, e, `
def gather(ctx):
    return {"x": http("GET", "`+srv.URL+`/x")["status"]}
`)
	if err == nil {
		t.Fatal("redirect off the allowlist must fail")
	}
}

func TestHTTPSecret(t *testing.T) {
	vault := t.TempDir()
	secrets := t.TempDir()
	os.WriteFile(filepath.Join(secrets, "notion"), []byte("tok\n"), 0o600)
	e := capEnv(vault, &manifest.Capabilities{HTTP: &manifest.HTTPCap{Hosts: []string{"example.com"}}})
	e.SecretsDir = secrets
	res, _, err := runSrc(t, e, `
def gather(ctx):
    return {"s": secret("notion")}
`)
	if err != nil {
		t.Fatal(err)
	}
	if res.(map[string]any)["s"] != "tok" {
		t.Errorf("secret = %v", res)
	}
	_, _, err = runSrc(t, e, `
def gather(ctx):
    return {"s": secret("missing")}
`)
	if err == nil || !strings.Contains(err.Error(), "secret missing not available (list it in requires.secrets)") {
		t.Fatalf("missing secret error = %v", err)
	}
}

// ── A3 · write ───────────────────────────────────────────────────────────

func writeCaps() *manifest.Capabilities {
	return &manifest.Capabilities{Write: &manifest.WriteCap{Paths: []string{"Library/Newsletters/", ".obsidian/snippets/today.css"}}}
}

func TestWriteInsidePrefix(t *testing.T) {
	vault := t.TempDir()
	e := capEnv(vault, writeCaps())
	res, _, err := runSrc(t, e, `
def gather(ctx):
    a = write("Library/Newsletters/x.md", "hello\n")
    b = write("Library/Newsletters/x.md", "hello\n")
    c = write(".obsidian/snippets/today.css", "body{}")
    return {"a": a, "b": b, "c": c}
`)
	if err != nil {
		t.Fatal(err)
	}
	m := res.(map[string]any)
	if m["a"] != true || m["b"] != false || m["c"] != true {
		t.Fatalf("write results = %v", m)
	}
	got, err := os.ReadFile(filepath.Join(vault, "Library/Newsletters/x.md"))
	if err != nil || string(got) != "hello\n" {
		t.Fatalf("file = %q %v", got, err)
	}
	// identical content must not touch the file: no leftover temp files either
	ents, _ := os.ReadDir(filepath.Join(vault, "Library/Newsletters"))
	if len(ents) != 1 {
		t.Fatalf("temp files left behind: %d entries", len(ents))
	}
}

func TestWriteOutsidePrefix(t *testing.T) {
	vault := t.TempDir()
	e := capEnv(vault, writeCaps())
	for _, p := range []string{"Desk/Todo.md", "Library/Newsletters/../../etc/x", "/etc/passwd", ".obsidian/snippets/other.css"} {
		_, _, err := runSrc(t, e, "def gather(ctx):\n    return {\"x\": write("+quote(p)+", \"boom\")}\n")
		if err == nil || !strings.Contains(err.Error(), "capability write:") {
			t.Fatalf("write(%q) error = %v", p, err)
		}
	}
}

func TestWriteNotDeclared(t *testing.T) {
	_, _, err := runSrc(t, capEnv(t.TempDir(), &manifest.Capabilities{}), `
def gather(ctx):
    return {"x": write("a.md", "b")}
`)
	if err == nil || !strings.Contains(err.Error(), "undefined: write") {
		t.Fatalf("error = %v", err)
	}
}

// ── A4 · dry run ─────────────────────────────────────────────────────────

func TestDryRunRecordsActions(t *testing.T) {
	hits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()
	vault := t.TempDir()
	caps := writeCaps()
	caps.HTTP = &manifest.HTTPCap{Hosts: []string{hostOf(t, srv.URL)}, Methods: []string{"GET", "POST"}}
	e := capEnv(vault, caps)
	e.DryRun = true

	res, actions, err := runSrc(t, e, `
def gather(ctx):
    g = http("GET", "`+srv.URL+`/read")
    p = http("POST", "`+srv.URL+`/act", json={"a": 1})
    w = write("Library/Newsletters/x.md", "hello")
    return {"get": g["status"], "post": p["status"], "dry": p["dry"], "wrote": w,
            "ctx_dry": ctx.dry_run, "n": len(ctx.actions), "global_dry": dry_run}
`)
	if err != nil {
		t.Fatal(err)
	}
	m := res.(map[string]any)
	if m["get"] != int64(200) || hits != 1 {
		t.Errorf("GET must still run: %v hits=%d", m["get"], hits)
	}
	if m["post"] != int64(0) || m["dry"] != true || m["wrote"] != false {
		t.Errorf("mutations not suppressed: %v", m)
	}
	if m["ctx_dry"] != true || m["global_dry"] != true || m["n"] != int64(2) {
		t.Errorf("ctx = %v", m)
	}
	if _, err := os.Stat(filepath.Join(vault, "Library/Newsletters/x.md")); err == nil {
		t.Error("dry run wrote the file")
	}
	want := []map[string]any{
		{"kind": "http", "target": "POST " + srv.URL + "/act"},
		{"kind": "write", "target": "Library/Newsletters/x.md"},
	}
	if len(actions) != 2 || actions[0]["target"] != want[0]["target"] || actions[1]["target"] != want[1]["target"] ||
		actions[0]["kind"] != "http" || actions[1]["kind"] != "write" {
		t.Errorf("actions = %v, want %v", actions, want)
	}
}

func TestDryRunOffHasNoActions(t *testing.T) {
	vault := t.TempDir()
	_, actions, err := runSrc(t, capEnv(vault, writeCaps()), `
def gather(ctx):
    write("Library/Newsletters/x.md", "hello")
    return {"n": len(ctx.actions)}
`)
	if err != nil {
		t.Fatal(err)
	}
	if len(actions) != 0 {
		t.Errorf("actions = %v", actions)
	}
}

// ── A5 · frozen set ──────────────────────────────────────────────────────

func TestFrozenWithoutCapabilities(t *testing.T) {
	vault := t.TempDir()
	for _, name := range []string{"http", "write", "secret"} {
		_, _, err := runSrc(t, env(vault), "def gather(ctx):\n    return {\"x\": "+name+"}\n")
		if err == nil || !strings.Contains(err.Error(), "undefined: "+name) {
			t.Fatalf("%s must not exist without [capabilities]: %v", name, err)
		}
	}
	// dry_run is always there
	res, _, err := runSrc(t, env(vault), "def gather(ctx):\n    return {\"d\": dry_run, \"c\": ctx.dry_run, \"a\": len(ctx.actions)}\n")
	if err != nil {
		t.Fatal(err)
	}
	m := res.(map[string]any)
	if m["d"] != false || m["c"] != false || m["a"] != int64(0) {
		t.Errorf("dry_run globals = %v", m)
	}
}

func TestFrozenExecCapability(t *testing.T) {
	vault := t.TempDir()
	e := capEnv(vault, &manifest.Capabilities{Exec: []string{"echo"}})
	res, _, err := runSrc(t, e, `
def gather(ctx):
    return {"rc": run(["echo", "hi"])["rc"]}
`)
	if err != nil {
		t.Fatal(err)
	}
	if res.(map[string]any)["rc"] != int64(0) {
		t.Errorf("declared exec must run: %v", res)
	}
	_, _, err = runSrc(t, e, `
def gather(ctx):
    return {"rc": run(["/bin/true"])["rc"]}
`)
	if err == nil || !strings.Contains(err.Error(), "capability exec: true not declared") {
		t.Fatalf("error = %v", err)
	}
	// no exec declared ⇒ run() is unrestricted, as before
	if _, _, err := runSrc(t, env(vault), `
def gather(ctx):
    return {"rc": run(["echo", "hi"])["rc"]}
`); err != nil {
		t.Fatal(err)
	}
}
