package decide

import (
	"context"
	"strings"
	"testing"
)

// obscuraInterstitial is what `obscura --stealth fetch --dump text` returns for
// a Cloudflare-fronted Medium article: the challenge banner, nothing else.
const obscuraInterstitial = `medium.com
Verifying you are human. This may take a few seconds.
medium.com needs to review the security of your connection before proceeding.
Performing security verification...
Verification successful. Waiting for medium.com to respond...
Ray ID: 8f2a1c3d4e5f6789
Performance & security by Cloudflare`

// cloudflareBlock is the "you have been blocked" page (Cloudflare error 1020).
const cloudflareBlock = `Sorry, you have been blocked
You are unable to access example.com
Why have I been blocked?
This website is using a security service to protect itself from online attacks.
The action you just performed triggered the security solution. There are several
actions that could trigger this block including submitting a certain word or
phrase, a SQL command or malformed data.
Cloudflare Ray ID: 8f2a1c3d4e5f6789 - Your IP: 203.0.113.9 - Performance & security by Cloudflare`

// article is a plain prose page: long enough, sentence punctuation, no markers.
var article = strings.Repeat(`The committee published its findings on Tuesday, after a review that ran for
eleven months. Auditors examined the procurement records of four departments.
They found that the tendering process had been followed in most cases, but that
documentation was missing for a handful of contracts. The chair said the gaps
were administrative rather than deliberate. Opposition members disagreed, and
asked for the file to be referred onward. A second report is expected in spring.
`, 6)

func TestPageKindRulesChallenge(t *testing.T) {
	kind, conf, via := PageKind(obscuraInterstitial)
	if kind != PageChallenge || via != "rules" || conf < 0.9 {
		t.Fatalf("got %q %v %q, want challenge/rules", kind, conf, via)
	}
}

func TestPageKindRulesBlocked(t *testing.T) {
	kind, _, via := PageKind(cloudflareBlock)
	if kind != PageBlocked || via != "rules" {
		t.Fatalf("got %q %q, want blocked/rules", kind, via)
	}
}

func TestPageKindRulesContent(t *testing.T) {
	if len(article) < contentMinChars {
		t.Fatalf("fixture too short: %d", len(article))
	}
	kind, conf, via := PageKind(article)
	if kind != PageContent || via != "rules" || conf != 0.95 {
		t.Fatalf("got %q %v %q, want content/0.95/rules", kind, conf, via)
	}
}

func TestPageKindRulesEmptyAndLogin(t *testing.T) {
	if kind, _, _ := PageKind("   \n "); kind != PageEmpty {
		t.Fatalf("blank text = %q, want empty", kind)
	}
	if kind, _, _ := PageKind("404"); kind != PageEmpty {
		t.Fatalf("very short text = %q, want empty", kind)
	}
	login := "Sign in to continue to your account. Email. Password. Forgot your password?"
	if kind, _, _ := PageKind(login); kind != PageLogin {
		t.Fatalf("login page = %q, want login", kind)
	}
	weak := "Welcome back. Log in with your username and password to see your feed."
	if kind, _, _ := PageKind(weak); kind != PageLogin {
		t.Fatalf("short login-ish page = %q, want login", kind)
	}
}

// inconclusive: long enough to escape `empty`, too short/markup-y for content.
const inconclusive = "Home Products Pricing Docs Blog Contact — <div id=app></div> loading"

func TestPageKindRulesAbstainWithoutAsk(t *testing.T) {
	kind, conf, via := PageKind(inconclusive)
	if kind != Unknown || conf != 0 || via != "rules" {
		t.Fatalf("got %q %v %q, want unknown/0/rules", kind, conf, via)
	}
}

func TestPageKindAskFallback(t *testing.T) {
	srv := serveBody(t, lemonade("challenge", [][]tok{{
		{"chall", 0.93}, {"cont", 0.04}, {"un", 0.03},
	}}), nil)
	ask := func(ctx context.Context, req AskRequest) (AskResult, error) {
		req.URL = srv.URL
		return Ask(ctx, req)
	}
	kind, conf, via := PageKindWith(context.Background(), inconclusive, ask)
	if kind != PageChallenge || via != "ask" {
		t.Fatalf("got %q %q, want challenge/ask", kind, via)
	}
	if conf < 0.9 {
		t.Fatalf("conf = %v, want ≈0.93", conf)
	}
}

func TestPageKindAskUnknownStaysUnknown(t *testing.T) {
	// Backend down: Ask answers unknown, PageKind must not upgrade it.
	ask := func(ctx context.Context, req AskRequest) (AskResult, error) {
		req.URL = "http://127.0.0.1:1"
		return Ask(ctx, req)
	}
	kind, _, via := PageKindWith(context.Background(), inconclusive, ask)
	if kind != Unknown || via != "ask" {
		t.Fatalf("got %q %q, want unknown/ask", kind, via)
	}
}

func TestPageKindAskSeesTruncatedText(t *testing.T) {
	var seen []map[string]any
	srv := serveBody(t, lemonade("content", [][]tok{{{"cont", 0.95}, {"un", 0.05}}}), &seen)
	long := strings.Repeat("x", 5000)
	ask := func(ctx context.Context, req AskRequest) (AskResult, error) {
		req.URL = srv.URL
		return Ask(ctx, req)
	}
	if kind, _, _ := PageKindWith(context.Background(), long, ask); kind != PageContent {
		t.Fatalf("kind = %q, want content", kind)
	}
	if len(seen) != 1 {
		t.Fatalf("requests = %d, want 1", len(seen))
	}
	raw, _ := seen[0]["messages"].([]any)
	joined := ""
	for _, m := range raw {
		mm, _ := m.(map[string]any)
		s, _ := mm["content"].(string)
		joined += s
	}
	if strings.Count(joined, "x") > pageKindMaxChars+64 {
		t.Fatalf("sent %d chars of text, want ≤ %d", strings.Count(joined, "x"), pageKindMaxChars)
	}
	if !strings.Contains(joined, PageKindQuestion) {
		t.Fatalf("prompt missing the question: %q", joined)
	}
}

// A fetched page is public input: PageKind's ask goes auto (Jev, kev fallback).
func TestPageKindAskIsPublicAuto(t *testing.T) {
	var got AskRequest
	PageKindWith(context.Background(), "short ambiguous page text that the rules do not settle at all, really", func(_ context.Context, req AskRequest) (AskResult, error) {
		got = req
		return AskResult{Label: Unknown}, nil
	})
	if !got.Public || got.Route != "auto" || got.Caller != "fetch" {
		t.Fatalf("request = %+v", got)
	}
}
