package fetch

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jalberto/qilla/internal/decide"
)

// headedLedger starts the climb at the headed rung (the ledger remembers it
// worked for the domain), so the whole climb is lookups + headed.
func headedLedger(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "ledger.json")
	if err := (Ledger{"x.test": {Rung: RungHeaded, OKCount: 1}}).Save(path); err != nil {
		t.Fatal(err)
	}
	return path
}

func count(calls []string, subs ...string) int {
	n := 0
	for _, c := range calls {
		all := true
		for _, s := range subs {
			if !strings.Contains(c, s) {
				all = false
			}
		}
		if all {
			n++
		}
	}
	return n
}

func headedAsk(label string, seen *[]decide.AskRequest) decide.PageKindAsk {
	return func(_ context.Context, r decide.AskRequest) (decide.AskResult, error) {
		if r.Caller == "fetch-headed" {
			if seen != nil {
				*seen = append(*seen, r)
			}
			return decide.AskResult{Kind: "choice", Label: label, Conf: 0.95}, nil
		}
		return decide.AskResult{Label: decide.Unknown}, nil
	}
}

func headedOpts(t *testing.T, calls *[]string, mode string) Options {
	return Options{
		Look:           allPresent,
		ChallengeWait:  noWait(t),
		PrepareDisplay: func() error { return nil },
		LedgerPath:     headedLedger(t),
		Headed:         mode,
		Run: fakeRunner(t, map[string]string{
			"open":          "ok",
			"close":         "",
			"get text body": challengeText,
		}, calls),
	}
}

func TestHeadedNeverSkips(t *testing.T) {
	var calls []string
	res, _ := Fetch(context.Background(), "https://x.test/a", headedOpts(t, &calls, HeadedNever))
	if count(calls, "--headed") != 0 || count(calls, "open") != 0 {
		t.Fatalf("never opened a window: %v", calls)
	}
	last := res.Tried[len(res.Tried)-1]
	if last.Rung != RungHeaded || last.Kind != "skipped" || !strings.Contains(last.Note, "never") {
		t.Fatalf("headed try = %+v", last)
	}
}

func TestHeadedAskYesNeedsHuman(t *testing.T) {
	var calls, notes []string
	var seen []decide.AskRequest
	o := headedOpts(t, &calls, HeadedAsk)
	o.Ask = headedAsk("yes", &seen)
	o.Notify = func(url, reason string) error { notes = append(notes, url+"|"+reason); return nil }
	res, _ := Fetch(context.Background(), "https://x.test/a", o)
	if !res.NeedsHuman || res.OK() {
		t.Fatalf("want needs_human, got %+v", res)
	}
	if count(calls, "open") != 0 || count(calls, "close") != 0 {
		t.Fatalf("no browser may be touched: %v", calls)
	}
	last := res.Tried[len(res.Tried)-1]
	if last.Kind != KindNeedsHuman {
		t.Fatalf("headed try = %+v", last)
	}
	if len(notes) != 1 || !strings.HasPrefix(notes[0], "https://x.test/a|") {
		t.Fatalf("notify = %v, want exactly one", notes)
	}
	if len(seen) != 1 || seen[0].Kind != "noul" || !seen[0].Public || seen[0].Question != HeadedQuestion ||
		!strings.HasPrefix(seen[0].Text, "https://x.test/a\n") {
		t.Fatalf("headed ask = %+v", seen)
	}
}

func TestHeadedAskNoOrUnknownOpens(t *testing.T) {
	for _, label := range []string{"no", decide.Unknown} {
		var calls []string
		o := headedOpts(t, &calls, HeadedAsk)
		o.Ask = headedAsk(label, nil)
		res, _ := Fetch(context.Background(), "https://x.test/a", o)
		if res.NeedsHuman || count(calls, "--headed", "open") != 1 {
			t.Fatalf("%s: want one headed open, got %v", label, calls)
		}
	}
}

func TestHeadedAskTextCapped(t *testing.T) {
	var seen []decide.AskRequest
	o := headedOpts(t, nil, HeadedAsk)
	o.Ask = headedAsk("no", &seen)
	o.LedgerPath = ""
	o.MaxRung = 0
	o.Run = fakeRunner(t, map[string]string{"defuddle": blockedText + strings.Repeat("x", 2000), "open": "ok", "get text body": blockedText}, nil)
	Fetch(context.Background(), "https://x.test/a", o)
	if len(seen) != 1 {
		t.Fatalf("asks = %d", len(seen))
	}
	if n := len([]rune(seen[0].Text)); n > len("https://x.test/a\nblocked\n")+headedAskChars {
		t.Fatalf("text %d runes, want ≤ url+kind+500", n)
	}
}

// The loop guard: a challenge page that never clears, re-read many times,
// opens exactly one window, and the session is closed after the climb ends
// without content.
func TestHeadedOpensOnceAndClosesOnFailure(t *testing.T) {
	var calls []string
	o := headedOpts(t, &calls, HeadedAlways)
	wait := 60 * time.Millisecond
	o.ChallengeWait = &wait
	o.ChallengePoll = time.Millisecond
	res, _ := Fetch(context.Background(), "https://x.test/a", o)
	if res.OK() {
		t.Fatalf("challenge must not be content: %+v", res)
	}
	if n := count(calls, "agent-browser", " open "); n != 1 {
		t.Fatalf("agent-browser open ran %d times, want 1: %v", n, calls)
	}
	if n := count(calls, "get text body"); n < 3 {
		t.Fatalf("want several re-reads, got %d", n)
	}
	// Reads go to the open session only: none may carry --headed, which
	// would let agent-browser launch a fresh window.
	if count(calls, "--headed", "get text body") != 0 {
		t.Fatalf("re-reads carried --headed: %v", calls)
	}
	lastOpen := -1
	for i, c := range calls {
		if strings.Contains(c, " open ") {
			lastOpen = i
		}
	}
	closed := false
	for _, c := range calls[lastOpen+1:] {
		if strings.HasSuffix(c, "close") {
			closed = true
		}
	}
	if !closed {
		t.Fatalf("session not closed after a failed headed climb: %v", calls)
	}
}

func TestHeadedNotClosedOnContent(t *testing.T) {
	var calls []string
	o := headedOpts(t, &calls, HeadedAlways)
	o.Run = fakeRunner(t, map[string]string{"open": "ok", "close": "", "get text body": articleText}, &calls)
	res, _ := Fetch(context.Background(), "https://x.test/a", o)
	if !res.OK() {
		t.Fatalf("want content, got %+v", res)
	}
	opened := false
	for _, c := range calls {
		if strings.Contains(c, " open ") {
			opened = true
		}
		if opened && strings.HasSuffix(c, "close") {
			t.Fatalf("a window that produced the page must stay: %v", calls)
		}
	}
}
