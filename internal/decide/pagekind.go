package decide

import (
	"context"
	"strings"
	"unicode"
)

// Page kinds — the closed set PageKind answers over. `unknown` is the escape
// (never a guess), as everywhere else in this package.
const (
	PageContent   = "content"
	PageChallenge = "challenge"
	PageBlocked   = "blocked"
	PageLogin     = "login"
	PageEmpty     = "empty"
)

// PageKinds is the option set, `unknown` excluded (AskOptions adds it).
var PageKinds = []string{PageContent, PageChallenge, PageBlocked, PageLogin, PageEmpty}

// PageKindQuestion is the question the model answers when the rules abstain.
const PageKindQuestion = "What kind of page is this text?"

// pageKindMaxChars is how much of the text the model sees.
const pageKindMaxChars = 1500

// contentMinChars is the prose length a rules verdict of `content` needs.
const contentMinChars = 1500

// emptyMaxChars: below this, with no marker, a page carries nothing.
const emptyMaxChars = 40

// marker is one lowercase substring and the kind it proves.
type marker struct {
	sub  string
	kind string
}

// markers are matched in order: a challenge banner beats the block wording a
// challenge page often also carries.
var markers = []marker{
	{"performing security verification", PageChallenge},
	{"verification successful. waiting for", PageChallenge},
	{"checking your browser", PageChallenge},
	{"cf-browser-verification", PageChallenge},
	{"just a moment...", PageChallenge},
	{"enable javascript and cookies to continue", PageChallenge},
	{"verify you are human", PageChallenge},

	{"sorry, you have been blocked", PageBlocked},
	{"cloudflare ray id", PageBlocked},
	{"access denied", PageBlocked},
	{"403 forbidden", PageBlocked},
	{"error: the request could not be satisfied", PageBlocked},
	{"you are unable to access", PageBlocked},

	{"sign in to continue", PageLogin},
	{"log in to continue", PageLogin},
	{"please log in", PageLogin},
	{"sign in to your account", PageLogin},
}

// loginHints are weaker login signals: they only count on a short page.
var loginHints = []string{"log in", "sign in", "password", "forgot your password", "username"}

// loginShortMax is how short a page must be for the weak login hints to count.
const loginShortMax = 800

// PageKind classifies fetched page text. Deterministic rules answer first (no
// model, no network); when they abstain and ask is usable the local decider
// gets the first 1500 characters. via is "rules" or "ask".
//
// It never errors: an unreachable backend is `unknown`, and the caller must
// not read a verdict of unknown as content.
func PageKind(text string) (kind string, conf float64, via string) {
	return PageKindWith(context.Background(), text, nil)
}

// PageKindAsk is the model fallback a caller injects: the same shape as
// AskAndTrace's result, reduced to what PageKind needs. nil = rules only.
type PageKindAsk func(ctx context.Context, req AskRequest) (AskResult, error)

// PageKindWith is PageKind with an explicit ask backend (the CLI passes an
// AskAndTrace closure with caller "fetch"; tests pass a fake). A nil ask means
// an inconclusive text stays `unknown` with via "rules".
func PageKindWith(ctx context.Context, text string, ask PageKindAsk) (kind string, conf float64, via string) {
	if k, c, ok := pageKindRules(text); ok {
		return k, c, "rules"
	}
	if ask == nil {
		return Unknown, 0, "rules"
	}
	snippet := text
	if len(snippet) > pageKindMaxChars {
		snippet = snippet[:pageKindMaxChars]
	}
	res, err := ask(ctx, AskRequest{
		Kind:     "choice",
		Options:  PageKinds,
		Question: PageKindQuestion,
		Text:     snippet,
		Caller:   "fetch",
		// Fetched web pages are public input: auto sends them to Jev (kev
		// when jev is disabled, down or over its cap).
		Public: true,
		Route:  "auto",
	})
	if err != nil || res.Label == "" {
		return Unknown, 0, "ask"
	}
	label := res.Label
	if !validPageKind(label) {
		label = Unknown
	}
	return label, res.Conf, "ask"
}

func validPageKind(s string) bool {
	for _, k := range PageKinds {
		if k == s {
			return true
		}
	}
	return s == Unknown
}

// pageKindRules is stage (a): the deterministic verdicts. ok=false means the
// rules abstain and the model should be asked.
func pageKindRules(text string) (kind string, conf float64, ok bool) {
	trimmed := strings.TrimSpace(text)
	low := strings.ToLower(trimmed)

	for _, m := range markers {
		if strings.Contains(low, m.sub) {
			return m.kind, 0.99, true
		}
	}

	if len(trimmed) < emptyMaxChars {
		return PageEmpty, 0.99, true
	}

	// A short page dominated by login vocabulary is a login wall.
	if len(trimmed) < loginShortMax {
		hits := 0
		for _, h := range loginHints {
			if strings.Contains(low, h) {
				hits++
			}
		}
		if hits >= 2 {
			return PageLogin, 0.9, true
		}
	}

	if len(trimmed) >= contentMinChars && hasProse(trimmed) {
		return PageContent, 0.95, true
	}
	return Unknown, 0, false
}

// hasProse reports sentence-shaped text: several sentence terminators and a
// body that is mostly letters and spaces, not markup or menu words.
func hasProse(s string) bool {
	sentences := 0
	for _, r := range s {
		if r == '.' || r == '!' || r == '?' || r == '。' {
			sentences++
		}
	}
	if sentences < 5 {
		return false
	}
	letters, total := 0, 0
	for _, r := range s {
		if unicode.IsSpace(r) {
			continue
		}
		total++
		if unicode.IsLetter(r) {
			letters++
		}
	}
	return total > 0 && float64(letters)/float64(total) > 0.7
}
