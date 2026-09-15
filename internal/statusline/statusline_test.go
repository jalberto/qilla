package statusline

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParse(t *testing.T) {
	in := Parse(strings.NewReader(`{"model":{"display_name":"Opus"},"context_window":{"used_percentage":42.7},"rate_limits":{"seven_day":{"used_percentage":71.2}},"workspace":{"current_dir":"/v"}}`))
	if in.Model.DisplayName != "Opus" || in.Workspace.CurrentDir != "/v" {
		t.Fatalf("model/dir: %+v", in)
	}
	if got := Pct(in.ContextWindow.UsedPercentage); got != 42 {
		t.Fatalf("ctx %d", got)
	}
	if got := Pct(in.RateLimits.SevenDay.UsedPercentage); got != 71 {
		t.Fatalf("usage %d", got)
	}
	if in := Parse(strings.NewReader("not json")); in.Model.DisplayName != "?" {
		t.Fatalf("fallback model %q", in.Model.DisplayName)
	}
	if Pct(0) != -1 {
		t.Fatal("absent percentage should be -1")
	}
}

func TestRender(t *testing.T) {
	plain := Render(View{Model: "Opus", Usage: -1, Context: -1})
	if plain != "🌙 \033[1;35mqilla\033[0m · Opus" {
		t.Fatalf("plain bar: %q", plain)
	}
	full := Render(View{Qilla: true, Who: "brief", Model: "Opus", Usage: 71, Context: 90, Services: 2, Reminders: 1, Questions: 3, Jots: 4})
	for _, want := range []string{
		"\033[45;30m ◆ qilla \033[0m \033[1;35mbrief\033[0m · Opus",
		" · \033[1;33m⚡71%\033[0m", // yellow from 70
		" · \033[1;31m◔90%\033[0m", // red from 85
		" · \033[1;31m🚨2\033[0m",
		" · \033[1;31m🔔1\033[0m",
		" · ❓3",
		" · 📋4",
	} {
		if !strings.Contains(full, want) {
			t.Fatalf("bar %q missing %q", full, want)
		}
	}
	if s := Render(View{Model: "m", Usage: 81, Context: 10}); !strings.Contains(s, "\033[1;31m⚡81%") || !strings.Contains(s, " · ◔10%") {
		t.Fatalf("thresholds: %q", s)
	}
	if s := Render(View{Model: "m", Usage: -1, Context: -1, Services: 0, Reminders: 0, Questions: 0, Jots: 0}); strings.ContainsAny(s, "🚨🔔❓📋⚡◔") {
		t.Fatalf("zero counts should be omitted: %q", s)
	}
}

func TestCounts(t *testing.T) {
	dir := t.TempDir()
	q := filepath.Join(dir, "Questions.md")
	os.WriteFile(q, []byte("# Questions\n- [ ] one\n- [x] done\n  - [ ] indented\n- not a task\n"), 0o644)
	if n := CountOpenTasks(q); n != 2 {
		t.Fatalf("open tasks %d", n)
	}
	if n := CountOpenTasks(filepath.Join(dir, "missing.md")); n != 0 {
		t.Fatalf("missing file %d", n)
	}

	j := filepath.Join(dir, "2026-09-12.md")
	os.WriteFile(j, []byte("## 📥 Inbox\n- jot one\n- jot two\nprose\n\n## 📓 Log\n- 10:00 not a jot\n"), 0o644)
	if n := CountInboxJots(j); n != 2 {
		t.Fatalf("jots %d", n)
	}
	if n := CountInboxJots(filepath.Join(dir, "none.md")); n != 0 {
		t.Fatalf("missing journal %d", n)
	}
}

func TestCount(t *testing.T) {
	for in, want := range map[string]int{"3\n": 3, " 7 ": 7, "2\nextra\n": 2, "": 0, "nope": 0, "-1": 0} {
		if got := Count(in); got != want {
			t.Fatalf("Count(%q) = %d, want %d", in, got, want)
		}
	}
}
