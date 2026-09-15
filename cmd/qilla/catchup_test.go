package main

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/jalberto/qilla/internal/catchup"
	"github.com/jalberto/qilla/internal/remind"
)

// A quiet vault costs a session exactly one line.
func TestCatchupQuietIsOneLine(t *testing.T) {
	var b bytes.Buffer
	writeCatchup(&b, catchup.Result{}, nil, nil, time.Now(), false)
	out := b.String()
	if out != "n=0 jots=0 answers=0 open=0 log=0 due=0 state=0\n" {
		t.Fatalf("quiet vault must print one header line, got %q", out)
	}
}

// A quiet vault with state still hands the session its state, nothing else.
func TestCatchupQuietWithState(t *testing.T) {
	var b bytes.Buffer
	state := []catchup.StateRow{{Key: "chief/handoff", Text: "goal: ship the brief · pending: -"}}
	writeCatchup(&b, catchup.Result{}, nil, state, time.Now(), false)
	want := "n=0 jots=0 answers=0 open=0 log=0 due=0 state=1\nstate\tchief/handoff\tgoal: ship the brief · pending: -\n"
	if b.String() != want {
		t.Fatalf("state rows survive a quiet vault:\n got %q\nwant %q", b.String(), want)
	}
}

func TestCatchupRowsAreTSV(t *testing.T) {
	res := catchup.Result{
		Jots:    []catchup.Jot{{Date: "2026-09-12", Text: "call the notary"}},
		Answers: []catchup.Answer{{Stem: "Keep the deletes?", Reply: "yes"}},
		Opens:   []string{"Install hunk?"},
		Logs:    []catchup.LogLine{{Date: "2026-09-12", Text: "08:30 · brief ran"}},
	}
	due := []remind.Reminder{{Due: time.Now(), Text: "call the dentist"}}
	var b bytes.Buffer
	state := []catchup.StateRow{{Key: "chief/handoff", Text: "goal: ship the brief"}}
	writeCatchup(&b, res, due, state, time.Now(), false)
	lines := strings.Split(strings.TrimRight(b.String(), "\n"), "\n")
	if lines[0] != "n=5 jots=1 answers=1 open=1 log=1 due=1 state=1" {
		t.Fatalf("header: %q", lines[0])
	}
	if lines[1] != "state\tchief/handoff\tgoal: ship the brief" {
		t.Fatalf("state rows come before the delta: %q", lines[1])
	}
	if len(lines) != 6 {
		t.Fatalf("open stems are counted but not listed without --all: %q", lines)
	}
	for _, l := range lines[1:] {
		if !strings.Contains(l, "\t") || strings.TrimSpace(l) == "" {
			t.Fatalf("every row is a TSV line: %q", l)
		}
	}
	b.Reset()
	writeCatchup(&b, res, due, state, time.Now(), true)
	if !strings.Contains(b.String(), "open\tInstall hunk?") {
		t.Fatalf("--all lists open questions:\n%s", b.String())
	}
}
