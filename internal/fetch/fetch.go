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
	"net/http"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"time"

	"github.com/jalberto/qilla/internal/decide"
)

// Rung names. They are stable: they appear in `tried[]`, in the ledger and in
// the fetch-route decider's options.
const (
	RungHister         = "hister"
	RungKarakeepLookup = "karakeep-lookup"
	RungDefuddle       = "defuddle"
	RungCurl           = "curl"
	RungLadder         = "ladder"
	RungMirror         = "mirror"
	RungObscura        = "obscura"
	RungKarakeepCrawl  = "karakeep-crawl"
	RungHeadless       = "headless"
	RungHeaded         = "headed"
)

// Lookups are JA's own copies of a page, always tried first.
var Lookups = []string{RungHister, RungKarakeepLookup}

// DefaultOrder is the method ladder when nothing is known about a domain.
var DefaultOrder = []string{RungDefuddle, RungCurl, RungLadder, RungMirror, RungObscura, RungKarakeepCrawl, RungHeadless, RungHeaded}

// MaxRung is the longest chosen order: the two lookups plus every method.
// --max-rung N stops after position N of the order actually chosen.
var MaxRung = len(Lookups) + len(DefaultOrder)

// Routes name how the order was chosen.
const (
	RouteLedger  = "ledger"
	RouteAsk     = "ask"
	RouteDefault = "default"
)

// Timeouts per rung. The headed rung gets more: a window has to come up.
const (
	RungTimeout   = 60 * time.Second
	HeadedTimeout = 90 * time.Second
	// LookupTimeout caps each of hister and karakeep-lookup.
	LookupTimeout = 5 * time.Second
	// LadderTimeout is the soft-paywall proxy's budget.
	LadderTimeout = 25 * time.Second
	// ChallengeWait is how long a challenge page is given to resolve itself
	// (Cloudflare interstitials usually do) before the ladder escalates.
	ChallengeWait = 15 * time.Second
	// KarakeepPoll / KarakeepWait bound the karakeep-crawl poll.
	KarakeepPoll = 3 * time.Second
	KarakeepWait = 60 * time.Second
)

// Defaults for the [fetch] URLs.
const (
	DefaultHisterURL = "http://127.0.0.1:4433"
	DefaultLadderURL = "http://nasdxp:8082"
)

// DefaultMirrors maps a host (and its subdomains) to a front-end mirror the
// `mirror` rung rewrites to.
func DefaultMirrors() map[string]string {
	return map[string]string{"reddit.com": "safereddit.com", "redd.it": "safereddit.com"}
}

// chromeUA is what the curl fallback claims to be.
const chromeUA = "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/129.0.0.0 Safari/537.36"

// Try is one rung's outcome, as `tried` reports it.
type Try struct {
	Rung string `json:"rung"`
	Pos  int    `json:"pos"`
	Kind string `json:"kind"`
	MS   int    `json:"ms"`
	Note string `json:"note,omitempty"`
}

// Result is the fetch: the text plus how it was obtained and what it is.
type Result struct {
	URL   string   `json:"url"`
	Rung  string   `json:"rung"`
	Pos   int      `json:"pos"`
	Kind  string   `json:"kind"`
	Conf  float64  `json:"conf"`
	Via   string   `json:"via"`
	Chars int      `json:"chars"`
	Order []string `json:"order"`
	Route string   `json:"route"`
	Tried []Try    `json:"tried"`

	Text string `json:"-"` // stdout, not part of the JSON shape
}

// OK reports a usable page.
func (r Result) OK() bool { return r.Kind == decide.PageContent }

// Runner executes one rung's command. Injected so tests never spawn a browser.
type Runner func(ctx context.Context, timeout time.Duration, name string, args ...string) (string, error)

// Options configure a climb. Zero values are the real ladder, except the
// HTTP rungs: an empty HisterURL/LadderURL/KarakeepURL skips that rung.
type Options struct {
	// MaxRung stops after this position of the chosen order (0 = all).
	MaxRung int
	// Profile, Session and Class are [browser] profile/session/class.
	Profile, Session, Class string
	// Ask is the decider backend (the CLI passes AskAndTrace): PageKind's
	// model fallback (caller "fetch") and the fetch-route choice (caller
	// "fetch-route"); nil = rules only, default order.
	Ask decide.PageKindAsk
	// Run executes a rung; nil = exec.
	Run Runner
	// Look reports whether a tool exists; nil = exec.LookPath.
	Look func(string) (string, error)
	// HTTP is the client the HTTP rungs use; nil = a plain client.
	HTTP *http.Client
	// HisterURL, LadderURL are [fetch] hister_url / ladder_url.
	HisterURL, LadderURL string
	// KarakeepURL and KarakeepKey are the karakeep API base and bearer key.
	KarakeepURL, KarakeepKey string
	// ReadOnly skips karakeep-crawl (it creates a bookmark).
	ReadOnly bool
	// Mirrors is [fetch] mirrors; nil = DefaultMirrors().
	Mirrors map[string]string
	// Route is [fetch] route: "auto" (default) or "default" (no ledger, no ask).
	Route string
	// LedgerPath is {state_dir}/fetch/ledger.json; empty = no ledger.
	LedgerPath string
	// ChallengeWait overrides the re-read window (tests).
	ChallengeWait *time.Duration
	// KarakeepPoll, KarakeepWait override the crawl poll (tests).
	KarakeepPoll, KarakeepWait time.Duration
	// Now is the clock for the ledger; nil = time.Now.
	Now func() time.Time
	// PrepareDisplay exports the session's WAYLAND_DISPLAY/DISPLAY before the
	// headed rung; nil = the real systemd lookup.
	PrepareDisplay func() error
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

func (o Options) client() *http.Client {
	if o.HTTP != nil {
		return o.HTTP
	}
	return http.DefaultClient
}

func (o Options) now() time.Time {
	if o.Now != nil {
		return o.Now()
	}
	return time.Now()
}

func (o Options) mirrors() map[string]string {
	if o.Mirrors != nil {
		return o.Mirrors
	}
	return DefaultMirrors()
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
//
// Order: hister and karakeep-lookup first; then, with route "auto", the
// ledger's rung for the domain (and the default order after it), else — the
// domain unknown and neither lookup had the page — the fetch-route decider's
// pick followed by the rest of the default order; else the default order.
func Fetch(ctx context.Context, url string, o Options) (Result, error) {
	if strings.TrimSpace(url) == "" {
		return Result{}, errors.New("fetch: no url")
	}
	res := Result{URL: url, Kind: decide.Unknown, Via: "rules", Route: RouteDefault}
	auto := o.Route != RouteDefault
	domain := Domain(url)
	var ledger Ledger
	methods := DefaultOrder
	if auto && o.LedgerPath != "" {
		ledger, _ = LoadLedger(o.LedgerPath)
		if e, ok := ledger[domain]; ok && e.OKCount >= 1 {
			if i := indexOf(DefaultOrder, e.Rung); i >= 0 {
				methods, res.Route = DefaultOrder[i:], RouteLedger
			}
		}
	}
	res.Order = append(append([]string{}, Lookups...), methods...)
	max := o.MaxRung
	if max <= 0 || max > len(res.Order) {
		max = len(res.Order)
	}
	tried := map[string]bool{}
	for pos := 1; pos <= max && pos <= len(res.Order); pos++ {
		name := res.Order[pos-1]
		if tried[name] {
			continue
		}
		tried[name] = true
		if pos == len(Lookups)+1 && auto && res.Route == RouteDefault && o.Ask != nil {
			if pick, ok := askRoute(ctx, url, o); ok {
				res.Route = RouteAsk
				rest := []string{pick}
				for _, r := range DefaultOrder {
					if r != pick {
						rest = append(rest, r)
					}
				}
				res.Order = append(append([]string{}, Lookups...), rest...)
				name = pick
				tried[name] = true
			}
		}
		started := time.Now()
		text, reread, note := rung(ctx, name, url, o)
		if note != "" && text == "" {
			res.Tried = append(res.Tried, Try{Rung: name, Pos: pos, Kind: "skipped", MS: ms(started), Note: note})
			continue
		}
		kind, conf, via := decide.PageKindWith(ctx, text, o.Ask)
		if kind == decide.PageChallenge && reread != nil {
			if t2, k2, c2, v2, ok := waitOutChallenge(ctx, reread, o); ok {
				text, kind, conf, via = t2, k2, c2, v2
			}
		}
		res.Tried = append(res.Tried, Try{Rung: name, Pos: pos, Kind: kind, MS: ms(started), Note: note})
		res.Rung, res.Pos, res.Kind, res.Conf, res.Via = name, pos, kind, conf, via
		res.Text, res.Chars = text, len(text)
		if kind == decide.PageContent {
			break
		}
	}
	if res.Rung == "" {
		// every rung was skipped
		res.Kind, res.Via = decide.PageEmpty, "rules"
	}
	if auto && o.LedgerPath != "" && domain != "" {
		if ledger == nil {
			ledger = Ledger{}
		}
		if ledger.Record(domain, res, o.now()) {
			_ = ledger.Save(o.LedgerPath) // a ledger write failure never fails a fetch
		}
	}
	return res, nil
}

func indexOf(list []string, s string) int {
	for i, v := range list {
		if v == s {
			return i
		}
	}
	return -1
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

// rung runs one named rung. note != "" with empty text means the rung was
// skipped (tool missing, or it failed); reread (may be nil) re-reads the page.
func rung(ctx context.Context, name, url string, o Options) (text string, reread func() (string, error), note string) {
	switch name {
	case RungHister:
		return rungHister(ctx, url, o)
	case RungKarakeepLookup:
		return rungKarakeepLookup(ctx, url, o)
	case RungDefuddle:
		return rungDefuddle(ctx, url, o)
	case RungCurl:
		return rungCurl(ctx, url, o)
	case RungLadder:
		return rungLadder(ctx, url, o)
	case RungMirror:
		return rungMirror(ctx, url, o)
	case RungObscura:
		return rungObscura(ctx, url, o)
	case RungKarakeepCrawl:
		return rungKarakeepCrawl(ctx, url, o)
	case RungHeadless, RungHeaded:
		return rungBrowser(ctx, url, o, name == RungHeaded)
	}
	return "", nil, fmt.Sprintf("%s: no such rung", name)
}

// rungDefuddle: article → markdown.
func rungDefuddle(ctx context.Context, url string, o Options) (string, func() (string, error), string) {
	if _, err := o.look()("defuddle"); err != nil {
		return "", nil, "defuddle not on PATH"
	}
	out, err := o.run()(ctx, RungTimeout, "defuddle", "parse", url, "--md")
	if err != nil {
		return "", nil, "defuddle: " + err.Error()
	}
	if strings.TrimSpace(out) == "" {
		return "", nil, "defuddle: empty"
	}
	return out, nil, ""
}

// rungCurl: curl + a tag strip.
func rungCurl(ctx context.Context, url string, o Options) (string, func() (string, error), string) {
	if _, err := o.look()("curl"); err != nil {
		return "", nil, "curl not on PATH"
	}
	out, err := o.run()(ctx, RungTimeout, "curl", "-sL", "-A", chromeUA, url)
	if err != nil {
		return "", nil, "curl: " + err.Error()
	}
	return StripTags(out), nil, ""
}

// rungMirror: rewrite a mirrored host (reddit → safereddit) and read that
// with defuddle, else curl.
func rungMirror(ctx context.Context, url string, o Options) (string, func() (string, error), string) {
	mu, ok := mirrorURL(url, o.mirrors())
	if !ok {
		return "", nil, "no mirror for this host"
	}
	text, _, n1 := rungDefuddle(ctx, mu, o)
	if text != "" {
		return text, nil, ""
	}
	text, _, n2 := rungCurl(ctx, mu, o)
	if text != "" {
		return text, nil, ""
	}
	return "", nil, "mirror " + mu + ": " + n1 + "; " + n2
}

// rungObscura: obscura's stealth fetch. "--" keeps a hostile url positional.
func rungObscura(ctx context.Context, url string, o Options) (string, func() (string, error), string) {
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
