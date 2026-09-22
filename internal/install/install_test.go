package install

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jalberto/qilla/internal/config"
)

func TestTemplateIsTheExampleAndLoads(t *testing.T) {
	ex, err := os.ReadFile("../../qilla.example.toml")
	if err != nil {
		t.Fatal(err)
	}
	if string(ex) != Template {
		t.Fatal("internal/install/qilla.toml must be byte-identical to qilla.example.toml (cp it)")
	}
	p := filepath.Join(t.TempDir(), "qilla.toml")
	os.WriteFile(p, []byte(Template), 0o644)
	if _, err := config.Load(p); err != nil {
		t.Fatal(err)
	}
}

func TestUnitsPerRoutine(t *testing.T) {
	cfg := &config.Config{Web: config.Web{Listen: "127.0.0.1:7433"}, StateDir: "/h/.local/state/qilla", Memory: config.Memory{Backend: "engram", EngramURL: "unix:///run/user/1000/qilla-engram.sock"}, Routines: map[string]config.Routine{
		"brief": {Kind: "ai-fresh", Schedule: "*-*-* 08:30; *-*-* 13:00"}, "versions": {Kind: "script", Schedule: "Mon *-*-* 09:00"}, "research": {Kind: "ai-fresh", Schedule: "manual"}}}
	u := Units(cfg, Paths{ConfigDir: "/h/.config/qilla", UnitDir: "/h/.config/systemd/user", Qilla: "/h/.local/bin/qilla", Mise: "/h/.local/bin/mise"})
	want := []string{"qilla.socket", "qilla.service", "qilla-engram.service", "qilla-reconcile.timer", "qilla-reconcile.service", "qilla-brief.timer", "qilla-brief.service", "qilla-versions.timer", "qilla-versions.service"}
	if len(u) != len(want) {
		t.Fatalf("units: %d (manual routines get no timer)", len(u))
	}
	if _, has := u["qilla-research.timer"]; has {
		t.Fatal("manual schedule must not produce a timer")
	}
	for _, w := range want {
		if _, ok := u[w]; !ok {
			t.Fatalf("missing %s", w)
		}
	}
	if !strings.Contains(u["qilla-brief.timer"], "OnCalendar=*-*-* 08:30\nOnCalendar=*-*-* 13:00\n") || !strings.Contains(u["qilla-brief.timer"], "Persistent=true") {
		t.Fatalf("timer: %s", u["qilla-brief.timer"])
	}
	if !strings.Contains(u["qilla-brief.service"], "MISE_GLOBAL_CONFIG_FILE=/h/.config/qilla/mise.toml") || !strings.Contains(u["qilla-brief.service"], "MISE_AUTO_INSTALL=false") {
		t.Fatalf("units must isolate mise from the user's global config:\n%s", u["qilla-brief.service"])
	}
	if !strings.Contains(u["qilla-brief.service"], "mise -C /h/.config/qilla exec -- /h/.local/bin/qilla enqueue brief") {
		t.Fatalf("service must run through mise: %s", u["qilla-brief.service"])
	}
	if !strings.Contains(u["qilla.socket"], "ListenStream=%t/qilla.sock") || !strings.Contains(u["qilla.socket"], "ListenStream=127.0.0.1:7433") {
		t.Fatalf("socket: %s", u["qilla.socket"])
	}
	if !strings.Contains(u["qilla-engram.service"], "ENGRAM_SOCKET=%t/qilla-engram.sock") || !strings.Contains(u["qilla-engram.service"], "exec -- engram serve") {
		t.Fatalf("engram unit: %s", u["qilla-engram.service"])
	}
	if !strings.Contains(EnableCommands(u, "/h/.config/qilla"), "qilla-engram.service") {
		t.Fatal("enable line must include the sidecar")
	}
	for _, want := range []string{"ProtectHome=tmpfs", "ProtectSystem=full", "BindPaths=%t", "NoNewPrivileges=true", "BindPaths=-/h/.local/state/qilla", "BindReadOnlyPaths=-/h/.local/bin", ".gitconfig", "ReadWritePaths=-/h/.local/state/qilla", "/h/.config/qilla "} {
		if !strings.Contains(u["qilla.service"], want) {
			t.Fatalf("hardening missing %q:\n%s", want, u["qilla.service"])
		}
	}
	if !strings.Contains(u["qilla.service"], "RestrictAddressFamilies=AF_UNIX AF_INET AF_INET6 AF_NETLINK") {
		t.Fatalf("bwrap needs AF_NETLINK for loopback:\n%s", u["qilla.service"])
	}
	if strings.Contains(u["qilla.service"], ".agent-browser") {
		t.Fatalf("no agent-browser bind unless it is the headless browser:\n%s", u["qilla.service"])
	}
	cfg.Browser.Headless = "agent-browser"
	if ab := Units(cfg, Paths{ConfigDir: "/h/.config/qilla", Qilla: "/q"})["qilla.service"]; !strings.Contains(ab, "BindPaths=-"+filepath.Join(homeDir(t), ".agent-browser")+"\n") {
		t.Fatalf("agent-browser state dir must be bound rw:\n%s", ab)
	}
	cfg.Browser.Headless = ""
	cfg.Sandbox.BindRO = []string{"/h/.demo"}
	cfg.Sandbox.BindRW = []string{"/h/.cache/demo"}
	u2 := Units(cfg, Paths{ConfigDir: "/h/.config/qilla", Qilla: "/q"})
	if !strings.Contains(u2["qilla.service"], "BindReadOnlyPaths=-/h/.demo") || !strings.Contains(u2["qilla.service"], "BindPaths=-/h/.cache/demo") {
		t.Fatalf("extra binds from config:\n%s", u2["qilla.service"])
	}
	plain := Units(cfg, Paths{Qilla: "/usr/local/bin/qilla"})
	if !strings.Contains(plain["qilla.service"], "ExecStart=/usr/local/bin/qilla serve") {
		t.Fatalf("without mise run qilla directly: %s", plain["qilla.service"])
	}
	if tn := TimerNames(u); len(tn) != 3 || tn[0] != "qilla-brief.timer" {
		t.Fatalf("timers: %v", tn)
	}
}

func TestWriteFileRespectsExisting(t *testing.T) {
	p := filepath.Join(t.TempDir(), "x.toml")
	if w, _ := WriteFile(p, "a", false); !w {
		t.Fatal("first write")
	}
	if w, _ := WriteFile(p, "b", false); w {
		t.Fatal("must not overwrite without force")
	}
	if w, _ := WriteFile(p, "b", true); !w {
		t.Fatal("force overwrites")
	}
	b, _ := os.ReadFile(p)
	if string(b) != "b" {
		t.Fatal(string(b))
	}
}

func TestCredentialsLines(t *testing.T) {
	dir := t.TempDir()
	if got := Credentials(dir); !strings.Contains(got, "none yet") {
		t.Fatal(got)
	}
	os.MkdirAll(CredsDir(dir), 0o700)
	os.WriteFile(filepath.Join(CredsDir(dir), "gcal.cred"), []byte("x"), 0o600)
	os.WriteFile(filepath.Join(CredsDir(dir), "market.cred"), []byte("x"), 0o600)
	got := Credentials(dir)
	if !strings.Contains(got, "LoadCredentialEncrypted=gcal:"+filepath.Join(CredsDir(dir), "gcal.cred")) || strings.Count(got, "LoadCredentialEncrypted=") != 2 {
		t.Fatal(got)
	}
	cfg := &config.Config{Web: config.Web{Listen: "127.0.0.1:1"}, Memory: config.Memory{Backend: "sqlite"}}
	u := Units(cfg, Paths{ConfigDir: dir, Qilla: "/q"})
	if !strings.Contains(u["qilla.service"], "LoadCredentialEncrypted=market:") {
		t.Fatal("unit must load credentials")
	}
}

func TestUnitsWorkerRole(t *testing.T) {
	host, err := os.Hostname()
	if err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{
		Web:      config.Web{Listen: "127.0.0.1:7433"},
		StateDir: "/h/.local/state/qilla",
		Memory:   config.Memory{Backend: "engram", EngramURL: "unix:///run/user/1000/qilla-engram.sock"},
		Host:     config.Host{Roles: map[string]string{host: "worker", "midori": "brain"}},
		Routines: map[string]config.Routine{
			"brief":       {Kind: "ai-fresh", Schedule: "*-*-* 08:30"},
			"qmd-refresh": {Kind: "script", Schedule: "*-*-* *:15"},
		},
	}
	u := Units(cfg, Paths{ConfigDir: "/h/.config/qilla", UnitDir: "/h/.config/systemd/user", Qilla: "/h/.local/bin/qilla", Mise: "/h/.local/bin/mise"})
	want := []string{"qilla-qmd-refresh.timer", "qilla-qmd-refresh.service"}
	if len(u) != len(want) {
		t.Fatalf("worker units: got %v, want only %v", u, want)
	}
	for _, w := range want {
		if _, ok := u[w]; !ok {
			t.Fatalf("worker missing %s", w)
		}
	}
	// no qmd-refresh routine configured => no units at all
	cfg.Routines = map[string]config.Routine{"brief": {Kind: "ai-fresh", Schedule: "*-*-* 08:30"}}
	if u := Units(cfg, Paths{ConfigDir: "/h/.config/qilla", Qilla: "/q"}); len(u) != 0 {
		t.Fatalf("worker without qmd-refresh routine must get no units: %v", u)
	}
}

func homeDir(t *testing.T) string {
	t.Helper()
	h, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	return h
}
