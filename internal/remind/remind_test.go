package remind

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

var madrid = func() *time.Location {
	l, err := time.LoadLocation("Europe/Madrid")
	if err != nil {
		return time.UTC
	}
	return l
}()

func TestParseLine(t *testing.T) {
	cases := []struct {
		name string
		line string
		ok   bool
		due  string // 2006-01-02 15:04
		text string
	}{
		{"date only", "- [ ] 2026-08-26 · ⏰2026-08-27 · Enable the tick timer", true, "2026-08-27 00:00", "Enable the tick timer"},
		{"with time", "- [ ] 2026-08-26 · ⏰2026-08-27 09:30 · Call the dentist", true, "2026-08-27 09:30", "Call the dentist"},
		{"spanish", "- [ ] 2026-09-01 · ⏰2026-09-09 12:05 · Llamar a la gestoría por el IVA", true, "2026-09-09 12:05", "Llamar a la gestoría por el IVA"},
		{"no creation date", "- [ ] ⏰2026-09-09 · Revisar presupuesto", true, "2026-09-09 00:00", "Revisar presupuesto"},
		{"indented nested", "    - [ ] 2026-09-01 · ⏰2026-09-02 7:05 · Nested item", true, "2026-09-02 07:05", "Nested item"},
		{"due at the end", "- [ ] 2026-09-01 · Pay the invoice ⏰2026-09-03", true, "2026-09-03 00:00", "Pay the invoice"},
		{"space after clock", "- [ ] 2026-09-01 · ⏰ 2026-09-03 18:00 · Cena", true, "2026-09-03 18:00", "Cena"},
		{"ticked", "- [x] 2026-08-26 · ⏰2026-08-27 · Already done", false, "", ""},
		{"no clock", "- [ ] 2026-08-26 · Plain task", false, "", ""},
		{"prose mentioning a clock", "Reminder set ⏰2026-08-27 in passing", false, "", ""},
		{"impossible time", "- [ ] 2026-08-26 · ⏰2026-08-27 99:99 · Bad", false, "", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r, ok := parseLine(c.line, madrid)
			if ok != c.ok {
				t.Fatalf("ok = %v, want %v", ok, c.ok)
			}
			if !ok {
				return
			}
			if got := r.Due.Format("2006-01-02 15:04"); got != c.due {
				t.Errorf("due = %s, want %s", got, c.due)
			}
			if r.Text != c.text {
				t.Errorf("text = %q, want %q", r.Text, c.text)
			}
			if r.Due.Location() != madrid {
				t.Errorf("location = %v, want %v", r.Due.Location(), madrid)
			}
		})
	}
}

func writeNote(t *testing.T, vault, rel, body string) {
	t.Helper()
	p := filepath.Join(vault, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestScanSortsAndSkips(t *testing.T) {
	vault := t.TempDir()
	writeNote(t, vault, "Desk/Todo.md", strings.Join([]string{
		"# Todo",
		"- [ ] 2026-09-01 · ⏰2026-09-12 09:00 · Llamar al fontanero",
		"- [ ] 2026-09-01 · ⏰2026-09-10 · Send the quote",
		"- [x] 2026-09-01 · ⏰2026-09-02 · Done already",
		"",
	}, "\n"))
	writeNote(t, vault, "Work/Meetings/note.md", "- [ ] ⏰2026-09-11 18:30 · Prep the board deck\n")
	writeNote(t, vault, "Desk/Archive/old.md", "- [ ] ⏰2026-09-01 · Archived reminder\n")
	writeNote(t, vault, ".obsidian/plugins/x.md", "- [ ] ⏰2026-09-01 · Plugin noise\n")
	writeNote(t, vault, "Desk/notes.txt", "- [ ] ⏰2026-09-01 · Not markdown\n")

	rs, err := Scan(vault, madrid)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, r := range rs {
		got = append(got, r.Due.Format("2006-01-02 15:04")+" "+r.Text)
	}
	want := []string{
		"2026-09-10 00:00 Send the quote",
		"2026-09-11 18:30 Prep the board deck",
		"2026-09-12 09:00 Llamar al fontanero",
	}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("scan = %v, want %v", got, want)
	}
	if rs[0].Source != "Desk/Todo.md:3" {
		t.Errorf("source = %q, want Desk/Todo.md:3", rs[0].Source)
	}
}

func TestDueNowAndWhen(t *testing.T) {
	now := time.Date(2026, 9, 12, 10, 0, 0, 0, madrid)
	mk := func(s string) Reminder {
		d, err := time.ParseInLocation("2006-01-02 15:04", s, madrid)
		if err != nil {
			t.Fatal(err)
		}
		return Reminder{Due: d, Text: s}
	}
	cases := []struct {
		due  string
		due_ bool
		when string
	}{
		{"2026-09-09 00:00", true, "3d overdue"},
		{"2026-09-11 23:00", true, "1d overdue"},
		{"2026-09-12 00:00", true, "today"},
		{"2026-09-12 09:30", true, "today 09:30"},
		{"2026-09-12 18:00", false, "today 18:00"},
		{"2026-09-13 00:00", false, "tomorrow"},
		{"2026-09-13 08:15", false, "tomorrow 08:15"},
		{"2026-09-20 08:15", false, "2026-09-20"},
	}
	var all []Reminder
	wantDue := 0
	for _, c := range cases {
		r := mk(c.due)
		all = append(all, r)
		if c.due_ {
			wantDue++
		}
		if got := When(r, now); got != c.when {
			t.Errorf("When(%s) = %q, want %q", c.due, got, c.when)
		}
	}
	if n := len(DueNow(all, now)); n != wantDue {
		t.Errorf("DueNow = %d, want %d", n, wantDue)
	}
}

func TestKeyStability(t *testing.T) {
	d := time.Date(2026, 9, 12, 9, 30, 0, 0, madrid)
	a := Reminder{Due: d, Text: "Call the dentist", Source: "Desk/Todo.md:3"}
	b := Reminder{Due: d, Text: "Call the dentist", Source: "Work/other.md:9"}
	if Key(a) != Key(b) {
		t.Error("key must ignore the source")
	}
	c := Reminder{Due: d, Text: "Call the doctor"}
	if Key(a) == Key(c) {
		t.Error("editing the text must be a new reminder")
	}
	e := Reminder{Due: d.Add(time.Hour), Text: "Call the dentist"}
	if Key(a) == Key(e) {
		t.Error("editing the due date must be a new reminder")
	}
	if len(Key(a)) != 16 {
		t.Errorf("key length = %d, want 16", len(Key(a)))
	}
}

func TestStateRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := StatePath(dir)
	now := time.Date(2026, 9, 12, 10, 0, 0, 0, madrid)
	r := Reminder{Due: time.Date(2026, 9, 12, 9, 30, 0, 0, madrid), Text: "Call the dentist", Source: "Desk/Todo.md:3"}

	st, err := LoadState(path)
	if err != nil {
		t.Fatal(err)
	}
	if st.IsSent(Key(r)) {
		t.Fatal("empty state must not report a delivery")
	}
	st.MarkSent(Key(r), r, now)
	if err := st.Save(now); err != nil {
		t.Fatal(err)
	}

	st2, err := LoadState(path)
	if err != nil {
		t.Fatal(err)
	}
	if !st2.IsSent(Key(r)) {
		t.Fatal("delivery did not survive the round trip")
	}
	if d := st2.Sent[Key(r)]; d.Text != r.Text || d.SentAt != "2026-09-12T10:00:00" {
		t.Errorf("delivery = %+v", d)
	}
	if n := st2.Reset("dentist"); n != 1 {
		t.Fatalf("reset = %d, want 1", n)
	}
	if err := st2.Save(now); err != nil {
		t.Fatal(err)
	}
	st3, _ := LoadState(path)
	if st3.IsSent(Key(r)) {
		t.Error("reset must let the reminder fire again")
	}
}

func TestStatePrunesAndSurvivesCorruption(t *testing.T) {
	dir := t.TempDir()
	path := StatePath(dir)
	now := time.Date(2026, 9, 12, 10, 0, 0, 0, madrid)
	st, _ := LoadState(path)
	old := Reminder{Due: now.AddDate(-1, 0, 0), Text: "ancient"}
	st.MarkSent(Key(old), old, now.AddDate(-1, 0, 0))
	fresh := Reminder{Due: now, Text: "fresh"}
	st.MarkSent(Key(fresh), fresh, now)
	if err := st.Save(now); err != nil {
		t.Fatal(err)
	}
	st2, _ := LoadState(path)
	if st2.IsSent(Key(old)) {
		t.Error("deliveries past the retention must be pruned")
	}
	if !st2.IsSent(Key(fresh)) {
		t.Error("recent deliveries must be kept")
	}

	if err := os.WriteFile(path, []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	st3, err := LoadState(path)
	if err != nil || st3.IsSent(Key(fresh)) {
		t.Errorf("corrupt state must load empty, got err=%v", err)
	}
}

func TestAddAppends(t *testing.T) {
	dir := t.TempDir()
	todo := filepath.Join(dir, "Desk", "Todo.md")
	if err := os.MkdirAll(filepath.Dir(todo), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(todo, []byte("# Todo\n\n- [ ] 2026-09-01 · Existing task\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 9, 12, 10, 0, 0, 0, madrid)

	due, err := ParseAt("2026-09-13 09:30", madrid)
	if err != nil {
		t.Fatal(err)
	}
	line, err := Add(todo, now, due, "Call the dentist")
	if err != nil {
		t.Fatal(err)
	}
	if line != "- [ ] 2026-09-12 · ⏰2026-09-13 09:30 · Call the dentist" {
		t.Fatalf("line = %q", line)
	}
	dayOnly, err := ParseAt("2026-09-14", madrid)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Add(todo, now, dayOnly, "Sacar la basura"); err != nil {
		t.Fatal(err)
	}

	b, err := os.ReadFile(todo)
	if err != nil {
		t.Fatal(err)
	}
	want := "# Todo\n\n- [ ] 2026-09-01 · Existing task\n" +
		"- [ ] 2026-09-12 · ⏰2026-09-13 09:30 · Call the dentist\n" +
		"- [ ] 2026-09-12 · ⏰2026-09-14 · Sacar la basura\n"
	if string(b) != want {
		t.Errorf("todo =\n%q\nwant\n%q", b, want)
	}
	// The appended lines must scan back as reminders.
	rs, err := Scan(dir, madrid)
	if err != nil {
		t.Fatal(err)
	}
	if len(rs) != 2 || rs[0].Text != "Call the dentist" || rs[1].Text != "Sacar la basura" {
		t.Errorf("scan after add = %+v", rs)
	}
}

func TestAddCreatesMissingFileAndKeepsTrailingNewline(t *testing.T) {
	dir := t.TempDir()
	todo := filepath.Join(dir, "Desk", "Todo.md")
	now := time.Date(2026, 9, 12, 10, 0, 0, 0, madrid)
	if _, err := Add(todo, now, now, "first"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(todo, []byte("- [ ] no trailing newline"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Add(todo, now, now, "second"); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(todo)
	if !strings.HasPrefix(string(b), "- [ ] no trailing newline\n- [ ] 2026-09-12") {
		t.Errorf("todo = %q", b)
	}
}

func TestParseAtRejectsGarbage(t *testing.T) {
	if _, err := ParseAt("tomorrow", madrid); err == nil {
		t.Error("want an error for a non-ISO date")
	}
	if _, err := ParseAt("2026-13-40", madrid); err == nil {
		t.Error("want an error for an impossible date")
	}
}

func TestSplitArgs(t *testing.T) {
	got, err := splitArgs(`ntfy publish --title "qilla nudge" phone`)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"ntfy", "publish", "--title", "qilla nudge", "phone"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("splitArgs = %v, want %v", got, want)
	}
	if _, err := splitArgs(`echo "unbalanced`); err == nil {
		t.Error("want an error for an unbalanced quote")
	}
}

func TestNotifyUsesTheEnvOverride(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "out.txt")
	t.Setenv("QILLA_NOTIFY_CMD", "sh -c 'printf %s \"$1\" > "+out+"' sh")
	if err := Notify("Call the dentist  (today 09:30)"); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != "Call the dentist  (today 09:30)" {
		t.Errorf("notified %q", b)
	}
}
