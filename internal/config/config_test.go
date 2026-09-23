package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func write(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "qilla.toml")
	os.WriteFile(p, []byte(body), 0o644)
	return p
}

func TestExampleLoads(t *testing.T) {
	c, err := Load("../../qilla.example.toml")
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Routines) != 9 || c.Routines["brief"].Agent != "chief" || c.Routines["brief"].MaxAttempts != 3 {
		t.Fatalf("example not parsed as expected: %+v", c.Routines["brief"])
	}
	if !strings.HasPrefix(c.Vault, "/") {
		t.Fatalf("vault not expanded: %s", c.Vault)
	}
}

func TestValidateRejects(t *testing.T) {
	cases := map[string]string{
		"kind":    "[routines.x]\nkind='tui'\nschedule='daily'\n",
		"sched":   "[routines.x]\nkind='script'\n",
		"agent":   "[routines.x]\nkind='ai-fresh'\nschedule='daily'\nagent='nobody'\n",
		"scope":   "[agents.chief]\n[routines.x]\nkind='ai-fresh'\nschedule='daily'\nscope=['brain']\n",
		"window":  "[routines.x]\nkind='script'\nschedule='daily'\nwindow='8-9'\n",
		"unknown": "vaultt='x'\n",
		"name":    "[routines.Brief]\nkind='script'\nschedule='daily'\n",
	}
	for name, body := range cases {
		if _, err := Load(write(t, body)); err == nil {
			t.Errorf("%s: expected error", name)
		}
	}
}

func TestDefaults(t *testing.T) {
	c, err := Load(write(t, ""))
	if err != nil {
		t.Fatal(err)
	}
	if c.Web.Listen != "127.0.0.1:7433" || c.Claude != "claude" || !strings.Contains(c.RecallCmd, "qmd") {
		t.Fatalf("defaults missing: %+v", c)
	}
}

func TestRoleFor(t *testing.T) {
	roles := map[string]string{"midori": "engine", "akane": "agent"}
	cases := []struct {
		name     string
		hostname string
		roles    map[string]string
		want     string
	}{
		{"empty table defaults to engine", "akane", nil, RoleEngine},
		{"known agent", "akane", roles, RoleAgent},
		{"known engine", "midori", roles, RoleEngine},
		{"unknown host with non-empty table is agent", "unknown-host", roles, RoleAgent},
		{"domain suffix stripped", "akane.example.com", roles, RoleAgent},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := roleFor(tc.hostname, tc.roles); got != tc.want {
				t.Errorf("roleFor(%q, %v) = %q, want %q", tc.hostname, tc.roles, got, tc.want)
			}
		})
	}
}
