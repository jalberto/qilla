package fetch

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	neturl "net/url"
	"strings"
	"time"
)

// maxBody caps any HTTP body a rung reads (hister stores whole pages).
const maxBody = 16 << 20

// httpDo sends one request and returns the body; a non-2xx status is an
// error carrying the status line.
func httpDo(ctx context.Context, o Options, timeout time.Duration, method, u string, hdr map[string]string, body any) ([]byte, int, error) {
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, 0, err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(cctx, method, u, rd)
	if err != nil {
		return nil, 0, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := o.client().Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if err != nil {
		return nil, resp.StatusCode, err
	}
	if resp.StatusCode/100 != 2 {
		return b, resp.StatusCode, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return b, resp.StatusCode, nil
}

// NormalizeURL is the equality key for "the same page": lower-case scheme and
// host, no www., no fragment, no utm_* params, no trailing slash.
func NormalizeURL(raw string) string {
	u, err := neturl.Parse(strings.TrimSpace(raw))
	if err != nil || u.Host == "" {
		return strings.TrimRight(strings.TrimSpace(raw), "/")
	}
	u.Scheme = strings.ToLower(u.Scheme)
	if u.Scheme == "http" {
		u.Scheme = "https"
	}
	u.Host = strings.TrimPrefix(strings.ToLower(u.Host), "www.")
	u.Fragment = ""
	q := u.Query()
	for k := range q {
		if strings.HasPrefix(strings.ToLower(k), "utm_") {
			q.Del(k)
		}
	}
	u.RawQuery = q.Encode()
	u.Path = strings.TrimRight(u.Path, "/")
	return u.String()
}

// histerDoc is the part of a hister /search document the rung reads.
type histerDoc struct {
	URL  string `json:"url"`
	Text string `json:"text"`
	HTML string `json:"html"`
}

// histerVariants are the spellings hister may have stored url under: its
// full-text index only ranks the page when the host matches (www. or not),
// so the query is retried with www. toggled and the trailing slash flipped.
func histerVariants(raw string) []string {
	out := []string{raw}
	add := func(s string) {
		for _, v := range out {
			if v == s {
				return
			}
		}
		out = append(out, s)
	}
	u, err := neturl.Parse(raw)
	if err != nil || u.Host == "" {
		return out
	}
	hosts := []string{u.Host}
	if strings.HasPrefix(u.Host, "www.") {
		hosts = append(hosts, strings.TrimPrefix(u.Host, "www."))
	} else {
		hosts = append(hosts, "www."+u.Host)
	}
	for _, h := range hosts {
		v := *u
		v.Host = h
		add(v.String())
		if strings.HasSuffix(v.Path, "/") {
			v.Path = strings.TrimRight(v.Path, "/")
		} else if v.RawQuery == "" {
			v.Path += "/"
		}
		add(v.String())
	}
	return out
}

// histerSearch asks hister for url (and its spelling variants); html=true
// includes the stored page. nil, nil = not visited.
func histerSearch(ctx context.Context, url string, o Options, html bool) (*histerDoc, error) {
	for _, v := range histerVariants(url) {
		doc, err := histerQuery(ctx, url, v, o, html)
		if err != nil || doc != nil {
			return doc, err
		}
	}
	return nil, nil
}

func histerQuery(ctx context.Context, url, text string, o Options, html bool) (*histerDoc, error) {
	base := strings.TrimRight(o.HisterURL, "/")
	q, _ := json.Marshal(map[string]any{
		"text": text, "semantic_enabled": false, "limit": 10,
		"include_text": true, "include_html": html,
	})
	b, _, err := httpDo(ctx, o, LookupTimeout, http.MethodGet,
		base+"/search?query="+neturl.QueryEscape(string(q)),
		map[string]string{"Origin": base}, nil) // hister 500s without Origin
	if err != nil {
		return nil, err
	}
	var out struct {
		Documents []histerDoc `json:"documents"`
	}
	if err := json.Unmarshal(b, &out); err != nil {
		return nil, fmt.Errorf("decode: %w", err)
	}
	want := NormalizeURL(url)
	for i := range out.Documents {
		if NormalizeURL(out.Documents[i].URL) == want {
			return &out.Documents[i], nil
		}
	}
	return nil, nil
}

// rungHister: the page as JA's browser history stored it. hister's text is
// often empty or a stub for reddit/x; then the stored html is stripped.
func rungHister(ctx context.Context, url string, o Options) (string, func() (string, error), string) {
	if o.HisterURL == "" {
		return "", nil, "hister_url not set"
	}
	ctx, cancel := context.WithTimeout(ctx, LookupTimeout)
	defer cancel()
	doc, err := histerSearch(ctx, url, o, false)
	if err != nil {
		return "", nil, "hister unreachable: " + err.Error()
	}
	if doc == nil {
		return "", nil, "hister: not visited"
	}
	text := strings.TrimSpace(doc.Text)
	if len(text) < 500 {
		if full, err := histerSearch(ctx, url, o, true); err == nil && full != nil {
			if t := StripTags(full.HTML); len(t) > len(text) {
				text = t
			}
		}
	}
	if text == "" {
		return "", nil, "hister: stored page has no text"
	}
	return text, nil, ""
}

// kkBookmark is the part of a karakeep bookmark the rungs read.
type kkBookmark struct {
	ID            string `json:"id"`
	Archived      bool   `json:"archived"`
	AlreadyExists bool   `json:"alreadyExists"`
	Content       struct {
		URL         string  `json:"url"`
		CrawlStatus string  `json:"crawlStatus"`
		HTMLContent *string `json:"htmlContent"`
	} `json:"content"`
}

func (o Options) kkHeaders() map[string]string {
	return map[string]string{"Authorization": "Bearer " + o.KarakeepKey}
}

func (o Options) kkBase() string { return strings.TrimRight(o.KarakeepURL, "/") + "/api/v1" }

// kkGet reads one bookmark with its content.
func kkGet(ctx context.Context, o Options, id string) (*kkBookmark, error) {
	b, _, err := httpDo(ctx, o, LookupTimeout, http.MethodGet,
		o.kkBase()+"/bookmarks/"+neturl.PathEscape(id)+"?includeContent=true", o.kkHeaders(), nil)
	if err != nil {
		return nil, err
	}
	var bm kkBookmark
	if err := json.Unmarshal(b, &bm); err != nil {
		return nil, fmt.Errorf("decode: %w", err)
	}
	return &bm, nil
}

// kkText is a crawled bookmark's text, "" unless the crawl succeeded.
func kkText(bm *kkBookmark) string {
	if bm == nil || bm.Content.CrawlStatus != "success" || bm.Content.HTMLContent == nil {
		return ""
	}
	return StripTags(*bm.Content.HTMLContent)
}

// rungKarakeepLookup: a bookmark JA already saved (search `url:<url>`).
func rungKarakeepLookup(ctx context.Context, url string, o Options) (string, func() (string, error), string) {
	if o.KarakeepURL == "" || o.KarakeepKey == "" {
		return "", nil, "karakeep url or key not set"
	}
	ctx, cancel := context.WithTimeout(ctx, LookupTimeout)
	defer cancel()
	b, _, err := httpDo(ctx, o, LookupTimeout, http.MethodGet,
		o.kkBase()+"/bookmarks/search?limit=10&q="+neturl.QueryEscape("url:"+url), o.kkHeaders(), nil)
	if err != nil {
		return "", nil, "karakeep unreachable: " + err.Error()
	}
	var out struct {
		Bookmarks []kkBookmark `json:"bookmarks"`
	}
	if err := json.Unmarshal(b, &out); err != nil {
		return "", nil, "karakeep: decode: " + err.Error()
	}
	want := NormalizeURL(url)
	for _, bm := range out.Bookmarks {
		if NormalizeURL(bm.Content.URL) != want {
			continue
		}
		full, err := kkGet(ctx, o, bm.ID)
		if err != nil {
			return "", nil, "karakeep: " + err.Error()
		}
		if t := kkText(full); strings.TrimSpace(t) != "" {
			return t, nil, ""
		}
		return "", nil, "karakeep: bookmark " + bm.ID + " has no crawled text"
	}
	return "", nil, "karakeep: not bookmarked"
}

// rungKarakeepCrawl: have karakeep crawl the page. A bookmark this rung
// created is archived and tagged qilla-fetch (whatever the crawl outcome) so
// it never shows in JA's reading list; a pre-existing one is left untouched.
func rungKarakeepCrawl(ctx context.Context, url string, o Options) (string, func() (string, error), string) {
	if o.KarakeepURL == "" || o.KarakeepKey == "" {
		return "", nil, "karakeep url or key not set"
	}
	if o.ReadOnly {
		return "", nil, "read-only: karakeep-crawl creates a bookmark"
	}
	b, _, err := httpDo(ctx, o, LookupTimeout, http.MethodPost, o.kkBase()+"/bookmarks", o.kkHeaders(),
		map[string]string{"type": "link", "url": url})
	if err != nil {
		return "", nil, "karakeep create: " + err.Error()
	}
	var bm kkBookmark
	if err := json.Unmarshal(b, &bm); err != nil || bm.ID == "" {
		return "", nil, "karakeep create: no bookmark id in reply"
	}
	created := !bm.AlreadyExists
	if created {
		defer kkHide(ctx, o, bm.ID)
	}
	poll, wait := o.KarakeepPoll, o.KarakeepWait
	if poll <= 0 {
		poll = KarakeepPoll
	}
	if wait <= 0 {
		wait = KarakeepWait
	}
	deadline := time.Now().Add(wait)
	for {
		full, err := kkGet(ctx, o, bm.ID)
		if err == nil {
			switch full.Content.CrawlStatus {
			case "success":
				if t := kkText(full); strings.TrimSpace(t) != "" {
					return t, nil, ""
				}
				return "", nil, "karakeep: crawl succeeded with no text"
			case "failure":
				return "", nil, "karakeep: crawl failed"
			}
		}
		if !time.Now().Add(poll).Before(deadline) {
			return "", nil, "karakeep: crawl not done in " + wait.String()
		}
		select {
		case <-ctx.Done():
			return "", nil, "karakeep: " + ctx.Err().Error()
		case <-time.After(poll):
		}
	}
}

// kkHide archives and tags a bookmark karakeep-crawl created. Errors are
// dropped: the text was already obtained (or not) and the bookmark stays.
func kkHide(ctx context.Context, o Options, id string) {
	ctx = context.WithoutCancel(ctx)
	base := o.kkBase() + "/bookmarks/" + neturl.PathEscape(id)
	_, _, _ = httpDo(ctx, o, LookupTimeout, http.MethodPatch, base, o.kkHeaders(), map[string]bool{"archived": true})
	_, _, _ = httpDo(ctx, o, LookupTimeout, http.MethodPost, base+"/tags", o.kkHeaders(),
		map[string]any{"tags": []map[string]string{{"tagName": "qilla-fetch"}}})
}

// rungLadder: the soft-paywall proxy's raw view, tag-stripped.
func rungLadder(ctx context.Context, url string, o Options) (string, func() (string, error), string) {
	if o.LadderURL == "" {
		return "", nil, "ladder_url not set"
	}
	b, code, err := httpDo(ctx, o, LadderTimeout, http.MethodGet, strings.TrimRight(o.LadderURL, "/")+"/raw/"+url, nil, nil)
	if err != nil && code == 0 {
		return "", nil, "ladder unreachable: " + err.Error()
	}
	text := StripTags(string(b))
	if err != nil && text == "" {
		return "", nil, "ladder: " + err.Error()
	}
	if text == "" {
		return "", nil, "ladder: empty"
	}
	return text, nil, ""
}

// mirrorURL rewrites url's host through the mirror table (a key matches the
// host and its subdomains).
func mirrorURL(raw string, mirrors map[string]string) (string, bool) {
	u, err := neturl.Parse(raw)
	if err != nil || u.Host == "" {
		return "", false
	}
	host := strings.ToLower(u.Hostname())
	for from, to := range mirrors {
		from = strings.ToLower(from)
		if host == from || strings.HasSuffix(host, "."+from) {
			u.Host = to
			return u.String(), true
		}
	}
	return "", false
}

// askRoute asks the fetch-route decider which method to start with. A HEAD
// probe (3 s) adds the status and server/cf-ray headers when it answers.
func askRoute(ctx context.Context, url string, o Options) (string, bool) {
	text := url
	if head := headProbe(ctx, url, o); head != "" {
		text += "\n" + head
	}
	res, err := o.Ask(ctx, askRouteRequest(text))
	if err != nil || res.Label == "" {
		return "", false
	}
	if indexOf(DefaultOrder, res.Label) < 0 {
		return "", false
	}
	return res.Label, true
}

func headProbe(ctx context.Context, url string, o Options) string {
	cctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(cctx, http.MethodHead, url, nil)
	if err != nil {
		return ""
	}
	req.Header.Set("User-Agent", chromeUA)
	resp, err := o.client().Do(req)
	if err != nil {
		return ""
	}
	resp.Body.Close()
	parts := []string{fmt.Sprintf("HEAD %d", resp.StatusCode)}
	for _, h := range []string{"Server", "Cf-Ray"} {
		if v := resp.Header.Get(h); v != "" {
			parts = append(parts, strings.ToLower(h)+": "+v)
		}
	}
	return strings.Join(parts, "\n")
}
