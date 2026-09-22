// Package fetch is the web-research ladder as code: climb the cheapest rung
// that works, classify what came back with decide.PageKind, and stop at the
// first rung that returned actual content. A caller never has to notice that a
// page was a bot check — the ladder escalates on its own.
package fetch

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"time"

	"github.com/jalberto/qilla/internal/decide"
)

// MaxRung is the whole ladder: defuddle/curl → obscura → agent-browser
// headless → agent-browser headed.
const MaxRung = 4

// Timeouts per rung. The headed rung gets more: a window has to come up.
const (
	RungTimeout   = 60 * time.Second
	HeadedTimeout = 90 * time.Second
	// ChallengeWait is how long a challenge page is given to resolve itself
	// (Cloudflare interstitials usually do) before the ladder escalates.
	ChallengeWait = 15 * time.Second
)

// chromeUA is what the curl fallback claims to be.
const chromeUA = "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/129.0.0.0 Safari/537.36"

// Try is one rung's outcome, as `tried` reports it.
type Try struct {
	Rung int    `json:"rung"`
	Kind string `json:"kind"`
	MS   int    `json:"ms"`
	Note string `json:"note,omitempty"`
}

// Result is the fetch: the text plus how it was obtained and what it is.
type Result struct {
	URL   string  `json:"url"`
	Rung  int     `json:"rung"`
	Kind  string  `json:"kind"`
	Conf  float64 `json:"conf"`
	Via   string  `json:"via"`
	Chars int     `json:"chars"`
	Tried []Try   `json:"tried"`

	Text string `json:"-"` // stdout, not part of the JSON shape
}

// OK reports a usable page.
func (r Result) OK() bool { return r.Kind == decide.PageContent }

// Runner executes one rung's command. Injected so tests never spawn a browser.
type Runner func(ctx context.Context, timeout time.Duration, name string, args ...string) (string, error)

// Options configure a climb. Zero values are the real ladder.
type Options struct {
	MaxRung int
	// Profile, Session and Class are [browser] profile/session/class.
	Profile, Session, Class string
	// Ask is the PageKind model fallback (the CLI passes AskAndTrace with
	// caller "fetch"); nil = rules only.
	Ask decide.PageKindAsk
	// Run executes a rung; nil = exec.
	Run Runner
	// Look reports whether a tool exists; nil = exec.LookPath.
	Look func(string) (string, error)
	// ChallengeWait overrides the re-read window (tests).
	ChallengeWait *time.Duration
	// PrepareDisplay exports the session's WAYLAND_DISPLAY/DISPLAY before the
	// headed rung; nil = the real systemd lookup.
	PrepareDisplay func() error
}

func (o Options) maxRung() int {
	if o.MaxRung <= 0 || o.MaxRung > MaxRung {
		return MaxRung
	}
	return o.MaxRung
}

func (o Options) session() string {
	if o.Session == "" {
		return "qilla"
	}
	return o.Session
}

func (o Options) class() string {
	if o.Class == "" {
		return "qilla-browser"
	}
	return o.Class
}

func (o Options) challengeWait() time.Duration {
	if o.ChallengeWait != nil {
		return *o.ChallengeWait
	}
	return ChallengeWait
}

func (o Options) run() Runner {
	if o.Run != nil {
		return o.Run
	}
	return execRun
}

func (o Options) look() func(string) (string, error) {
	if o.Look != nil {
		return o.Look
	}
	return exec.LookPath
}

// execRun runs a command with a timeout and returns its stdout. A non-zero
// exit with usable stdout is not an error: block pages exit non-zero often.
func execRun(ctx context.Context, timeout time.Duration, name string, args ...string) (string, error) {
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	c := exec.CommandContext(cctx, name, args...)
	var out, errb bytes.Buffer
	c.Stdout, c.Stderr = &out, &errb
	err := c.Run()
	text := out.String()
	if err != nil && strings.TrimSpace(text) == "" {
		msg := strings.TrimSpace(errb.String())
		if msg == "" {
			msg = err.Error()
		}
		return "", errors.New(msg)
	}
	return text, nil
}

// Fetch climbs the ladder for url. It returns a Result always; err is only a
// bad call (no url). A page that never yielded content comes back with the
// last kind seen, and the caller exits 3 on it.
func Fetch(ctx context.Context, url string, o Options) (Result, error) {
	if strings.TrimSpace(url) == "" {
		return Result{}, errors.New("fetch: no url")
	}
	res := Result{URL: url, Kind: decide.Unknown, Via: "rules"}
	for n := 1; n <= o.maxRung(); n++ {
		started := time.Now()
		text, reread, note := rung(ctx, n, url, o)
		if note != "" && text == "" {
			res.Tried = append(res.Tried, Try{Rung: n, Kind: "skipped", MS: ms(started), Note: note})
			continue
		}
		kind, conf, via := decide.PageKindWith(ctx, text, o.Ask)
		if kind == decide.PageChallenge && reread != nil {
			if t2, k2, c2, v2, ok := waitOutChallenge(ctx, reread, o); ok {
				text, kind, conf, via = t2, k2, c2, v2
			}
		}
		res.Tried = append(res.Tried, Try{Rung: n, Kind: kind, MS: ms(started), Note: note})
		res.Rung, res.Kind, res.Conf, res.Via = n, kind, conf, via
		res.Text, res.Chars = text, len(text)
		if kind == decide.PageContent {
			return res, nil
		}
	}
	if res.Rung == 0 {
		// every rung was skipped
		res.Kind, res.Via = decide.PageEmpty, "rules"
	}
	return res, nil
}

func ms(t time.Time) int { return int(time.Since(t).Milliseconds()) }

// waitOutChallenge re-reads the page until the challenge clears or the window
// closes. ok=false leaves the original verdict in place.
func waitOutChallenge(ctx context.Context, reread func() (string, error), o Options) (string, string, float64, string, bool) {
	deadline := time.Now().Add(o.challengeWait())
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return "", "", 0, "", false
		case <-time.After(3 * time.Second):
		}
		text, err := reread()
		if err != nil {
			return "", "", 0, "", false
		}
		kind, conf, via := decide.PageKindWith(ctx, text, o.Ask)
		if kind != decide.PageChallenge {
			return text, kind, conf, via, true
		}
	}
	return "", "", 0, "", false
}

// rung runs one rung. note != "" with empty text means the rung was skipped
// (tool missing, or it failed); reread (may be nil) re-reads the same page.
func rung(ctx context.Context, n int, url string, o Options) (text string, reread func() (string, error), note string) {
	switch n {
	case 1:
		return rung1(ctx, url, o)
	case 2:
		return rung2(ctx, url, o)
	case 3, 4:
		return rungBrowser(ctx, url, o, n == 4)
	}
	return "", nil, fmt.Sprintf("rung %d: no such rung", n)
}

// rung1: defuddle (article → markdown), else curl + a tag strip.
func rung1(ctx context.Context, url string, o Options) (string, func() (string, error), string) {
	if _, err := o.look()("defuddle"); err == nil {
		out, err := o.run()(ctx, RungTimeout, "defuddle", "parse", url, "--md")
		if err == nil && strings.TrimSpace(out) != "" {
			return out, nil, ""
		}
	}
	if _, err := o.look()("curl"); err != nil {
		return "", nil, "defuddle and curl not on PATH"
	}
	out, err := o.run()(ctx, RungTimeout, "curl", "-sL", "-A", chromeUA, url)
	if err != nil {
		return "", nil, "curl: " + err.Error()
	}
	return StripTags(out), nil, ""
}

// rung2: obscura's stealth fetch. "--" keeps a hostile url positional.
func rung2(ctx context.Context, url string, o Options) (string, func() (string, error), string) {
	if _, err := o.look()("obscura"); err != nil {
		return "", nil, "obscura not on PATH"
	}
	read := func() (string, error) {
		return o.run()(ctx, RungTimeout, "obscura", "--stealth", "fetch", "--dump", "text", "--", url)
	}
	out, err := read()
	if err != nil {
		return "", nil, "obscura: " + err.Error()
	}
	once := false
	return out, func() (string, error) {
		if once {
			return "", errors.New("obscura: re-fetched once already")
		}
		once = true
		return read()
	}, ""
}

// rungBrowser: agent-browser on the qilla session/profile, headless or headed.
func rungBrowser(ctx context.Context, url string, o Options, headed bool) (string, func() (string, error), string) {
	if _, err := o.look()("agent-browser"); err != nil {
		return "", nil, "agent-browser not on PATH"
	}
	timeout := RungTimeout
	base := []string{"--session", o.session()}
	if o.Profile != "" {
		base = append(base, "--profile", o.Profile)
	}
	if headed {
		timeout = HeadedTimeout
		prep := o.PrepareDisplay
		if prep == nil {
			prep = PrepareDisplay
		}
		if err := prep(); err != nil {
			return "", nil, "headed rung: " + err.Error()
		}
		// A daemon started without a display can never show a window.
		if _, err := o.run()(ctx, 15*time.Second, "agent-browser", "--session", o.session(), "close"); err != nil {
			_ = err // no daemon to close is fine
		}
		base = append(base, "--headed", "--args", "--class="+o.class())
	}
	ab := func(extra ...string) (string, error) {
		return o.run()(ctx, timeout, "agent-browser", append(append([]string{}, base...), extra...)...)
	}
	if _, err := ab("open", url); err != nil {
		return "", nil, "agent-browser open: " + err.Error()
	}
	read := func() (string, error) { return ab("get", "text", "body") }
	out, err := read()
	if err != nil {
		return "", nil, "agent-browser get: " + err.Error()
	}
	return out, read, ""
}

// PrepareDisplay exports the user session's WAYLAND_DISPLAY / DISPLAY /
// XDG_RUNTIME_DIR from systemd when they are not already set, so an
// agent-browser daemon started from a unit or a headless shell can still open
// a window on JA's desktop.
func PrepareDisplay() error {
	want := []string{"WAYLAND_DISPLAY", "DISPLAY", "XDG_RUNTIME_DIR"}
	missing := false
	for _, k := range want {
		if os.Getenv(k) == "" {
			missing = true
		}
	}
	if !missing {
		return nil
	}
	out, err := exec.Command("systemctl", "--user", "show-environment").Output()
	if err != nil {
		return fmt.Errorf("no display and systemctl --user show-environment failed: %w", err)
	}
	env := map[string]string{}
	for _, line := range strings.Split(string(out), "\n") {
		k, v, ok := strings.Cut(strings.TrimSpace(line), "=")
		if ok {
			env[k] = v
		}
	}
	for _, k := range want {
		if os.Getenv(k) == "" && env[k] != "" {
			os.Setenv(k, env[k])
		}
	}
	if os.Getenv("WAYLAND_DISPLAY") == "" && os.Getenv("DISPLAY") == "" {
		return errors.New("no display: a headed browser cannot open here")
	}
	return nil
}

var (
	tagRe    = regexp.MustCompile(`(?is)<(?:script|style|noscript)[^>]*>.*?</(?:script|style|noscript)>`)
	anyTagRe = regexp.MustCompile(`(?s)<[^>]*>`)
	wsRe     = regexp.MustCompile(`\n{3,}`)
)

// StripTags is the curl fallback's crude HTML → text: drop script/style, drop
// tags, collapse blank runs. Good enough to classify, never for rendering.
func StripTags(html string) string {
	s := tagRe.ReplaceAllString(html, "")
	s = anyTagRe.ReplaceAllString(s, " ")
	r := strings.NewReplacer("&nbsp;", " ", "&amp;", "&", "&lt;", "<", "&gt;", ">", "&quot;", `"`, "&#39;", "'")
	s = r.Replace(s)
	var lines []string
	for _, ln := range strings.Split(s, "\n") {
		lines = append(lines, strings.TrimSpace(strings.Join(strings.Fields(ln), " ")))
	}
	return strings.TrimSpace(wsRe.ReplaceAllString(strings.Join(lines, "\n"), "\n\n"))
}
