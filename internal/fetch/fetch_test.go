package fetch

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/jalberto/qilla/internal/decide"
)

const challengeText = `Just a moment...
Performing security verification...
Verification successful. Waiting for medium.com to respond...`

const blockedText = `Sorry, you have been blocked
Cloudflare Ray ID: 8f2a1c3d4e5f6789`

var articleText = strings.Repeat(`The port authority approved the dredging plan on Thursday. Work starts in May
and should finish before the summer season. Two contractors bid; the cheaper
one won. Fishermen objected to the timing, and the authority agreed to pause
during the spawning weeks. The cost is covered by the regional budget.
`, 8)

// fakeRunner answers per binary name from a table; a missing entry errors.
func fakeRunner(t *testing.T, table map[string]string, calls *[]string) Runner {
	t.Helper()
	return func(_ context.Context, _ time.Duration, name string, args ...string) (string, error) {
		key := name
		if len(args) > 0 {
			key = name + " " + strings.Join(args, " ")
		}
		if calls != nil {
			*calls = append(*calls, key)
		}
		for pat, out := range table {
			if strings.Contains(key, pat) {
				return out, nil
			}
		}
		return "", errNotFaked
	}
}

var errNotFaked = errNotFakedT{}

type errNotFakedT struct{}

func (errNotFakedT) Error() string { return "not faked" }

// allPresent says every tool is on PATH.
func allPresent(string) (string, error) { return "/usr/bin/fake", nil }

func noWait(t *testing.T) *time.Duration {
	t.Helper()
	d := time.Duration(0)
	return &d
}

// ranRungs drops the skipped tries.
func ranRungs(res Result) []Try {
	var out []Try
	for _, t := range res.Tried {
		if t.Kind != "skipped" {
			out = append(out, t)
		}
	}
	return out
}

func TestFetchStopsAtFirstContent(t *testing.T) {
	var calls []string
	res, err := Fetch(context.Background(), "https://example.com", Options{
		Look: allPresent,
		Run:  fakeRunner(t, map[string]string{"defuddle": articleText}, &calls),
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Rung != RungDefuddle || res.Pos != 3 || res.Kind != decide.PageContent || res.Via != "rules" {
		t.Fatalf("got rung %s@%d kind %q via %q, want defuddle@3/content/rules", res.Rung, res.Pos, res.Kind, res.Via)
	}
	if res.Route != RouteDefault {
		t.Fatalf("route = %q, want default", res.Route)
	}
	if res.Chars != len(articleText) {
		t.Fatalf("chars = %d, want %d", res.Chars, len(articleText))
	}
	if ran := ranRungs(res); len(ran) != 1 {
		t.Fatalf("tried = %+v, want one rung that ran", res.Tried)
	}
	for _, c := range calls {
		if strings.HasPrefix(c, "obscura") || strings.HasPrefix(c, "agent-browser") {
			t.Fatalf("escalated past rung 1: %q", c)
		}
	}
}

func TestFetchEscalatesToBrowser(t *testing.T) {
	var calls []string
	res, err := Fetch(context.Background(), "https://medium.com/x", Options{
		Look:          allPresent,
		ChallengeWait: noWait(t),
		Run: fakeRunner(t, map[string]string{
			"defuddle": blockedText,
			"obscura":  challengeText,
			"agent-browser --session qilla --profile p open": "ok",
			"get text body": articleText,
		}, &calls),
		Profile: "p",
		Session: "qilla",
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Rung != RungHeadless || res.Kind != decide.PageContent {
		t.Fatalf("got rung %s kind %q, want headless/content", res.Rung, res.Kind)
	}
	want := []Try{{Rung: RungDefuddle, Kind: decide.PageBlocked}, {Rung: RungObscura, Kind: decide.PageChallenge}, {Rung: RungHeadless, Kind: decide.PageContent}}
	ran := ranRungs(res)
	if len(ran) != 3 {
		t.Fatalf("tried = %+v, want three rungs that ran", res.Tried)
	}
	for i, w := range want {
		if ran[i].Kind != w.Kind || ran[i].Rung != w.Rung {
			t.Fatalf("ran[%d] = %+v, want %s/%s", i, ran[i], w.Rung, w.Kind)
		}
	}
	if strings.Contains(strings.Join(calls, "|"), "--headed") {
		t.Fatalf("headed rung reached: %v", calls)
	}
}

func TestFetchAllRungsBlocked(t *testing.T) {
	res, err := Fetch(context.Background(), "https://x.test", Options{
		MaxRung:       7, // through obscura
		Look:          allPresent,
		ChallengeWait: noWait(t),
		Run: fakeRunner(t, map[string]string{
			"defuddle": blockedText,
			"curl":     "<p>Sorry, you have been blocked</p> Cloudflare Ray ID: 1",
			"obscura":  blockedText,
		}, nil),
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.OK() {
		t.Fatalf("OK() on a blocked climb: %+v", res)
	}
	if res.Kind != decide.PageBlocked || res.Rung != RungObscura || res.Pos != 7 || len(res.Tried) != 7 {
		t.Fatalf("got %+v, want blocked at obscura@7", res)
	}
}

func TestFetchSkipsMissingTools(t *testing.T) {
	look := func(name string) (string, error) {
		if name == "obscura" {
			return "", errNotFaked
		}
		return "/usr/bin/fake", nil
	}
	res, err := Fetch(context.Background(), "https://x.test", Options{
		MaxRung: 7,
		Look:    look,
		Run:     fakeRunner(t, map[string]string{"defuddle": blockedText}, nil),
	})
	if err != nil {
		t.Fatal(err)
	}
	last := res.Tried[len(res.Tried)-1]
	if len(res.Tried) != 7 || last.Rung != RungObscura || last.Kind != "skipped" || last.Note == "" {
		t.Fatalf("tried = %+v, want a skipped obscura with a note", res.Tried)
	}
	if res.Rung != RungDefuddle {
		t.Fatalf("rung = %s, want defuddle (the last rung that ran)", res.Rung)
	}
}

// A challenge that clears on the re-read is content on the same rung.
func TestFetchChallengeClearsOnReread(t *testing.T) {
	n := 0
	wait := 2 * time.Second
	res, err := Fetch(context.Background(), "https://x.test", Options{
		MaxRung:       7,
		Look:          allPresent,
		ChallengeWait: &wait,
		Run: func(_ context.Context, _ time.Duration, name string, args ...string) (string, error) {
			if name != "obscura" {
				return blockedText, nil
			}
			n++
			if n == 1 {
				return challengeText, nil
			}
			return articleText, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Rung != RungObscura || res.Kind != decide.PageContent {
		t.Fatalf("got rung %s kind %q, want obscura/content", res.Rung, res.Kind)
	}
	if n != 2 {
		t.Fatalf("obscura calls = %d, want 2 (fetch + one re-fetch)", n)
	}
}

func TestFetchNoURL(t *testing.T) {
	if _, err := Fetch(context.Background(), "  ", Options{}); err == nil {
		t.Fatal("want an error for an empty url")
	}
}

func TestStripTags(t *testing.T) {
	got := StripTags(`<html><head><style>a{}</style></head><body><p>Hi &amp; bye</p><script>x()</script></body></html>`)
	if !strings.Contains(got, "Hi & bye") || strings.Contains(got, "x()") || strings.Contains(got, "<") {
		t.Fatalf("StripTags = %q", got)
	}
}

func TestResultJSONShape(t *testing.T) {
	res := Result{URL: "u", Rung: "curl", Pos: 4, Kind: "blocked", Conf: 0.99, Via: "rules", Chars: 10,
		Order: []string{"hister"}, Route: "default",
		Tried: []Try{{Rung: "defuddle", Pos: 3, Kind: "empty", MS: 3}}, Text: "secret"}
	raw, err := json.Marshal(res)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"url":"u","rung":"curl","pos":4,"kind":"blocked","conf":0.99,"via":"rules","chars":10,"order":["hister"],"route":"default","tried":[{"rung":"defuddle","pos":3,"kind":"empty","ms":3}]}`
	if string(raw) != want {
		t.Fatalf("json = %s\nwant %s", raw, want)
	}
}
