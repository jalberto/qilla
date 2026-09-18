package star

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"go.starlark.net/starlark"
)

// maxBody caps what http() reads into memory.
const maxBody = 32 << 20

// ── capability helpers ───────────────────────────────────────────────────
//
// Only injected when routine.toml declares the matching capability; see
// internal/manifest.Capabilities and the package doc.

// hostAllowed reports whether host[:port] is declared (case-insensitive,
// matched exactly as written in routine.toml).
func (r *runner) hostAllowed(host string) bool {
	c := r.env.Caps
	if c == nil || c.HTTP == nil {
		return false
	}
	host = strings.ToLower(host)
	for _, h := range c.HTTP.Hosts {
		if strings.ToLower(strings.TrimSpace(h)) == host {
			return true
		}
	}
	return false
}

func (r *runner) methodAllowed(m string) bool {
	if r.env.Caps == nil {
		return false
	}
	for _, x := range r.env.Caps.HTTP.MethodList() {
		if x == m {
			return true
		}
	}
	return false
}

// addAction records a suppressed side effect for ctx.actions / _dry_actions.
func (r *runner) addAction(kind, target string) {
	r.mu.Lock()
	r.actions = append(r.actions, map[string]any{"kind": kind, "target": target})
	r.mu.Unlock()
	d := starlark.NewDict(2)
	d.SetKey(starlark.String("kind"), starlark.String(kind))
	d.SetKey(starlark.String("target"), starlark.String(target))
	r.actionList().Append(d)
}

// actionList is the mutable list exposed as ctx.actions.
func (r *runner) actionList() *starlark.List {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.actionsVal == nil {
		r.actionsVal = starlark.NewList(nil)
	}
	return r.actionsVal
}

// bHTTP performs a scoped HTTP request.
func (r *runner) bHTTP(_ *starlark.Thread, b *starlark.Builtin, args starlark.Tuple, kw []starlark.Tuple) (starlark.Value, error) {
	var method, raw string
	var headers starlark.Value = starlark.None
	var jsonBody starlark.Value = starlark.None
	var body starlark.Value = starlark.None
	var timeout starlark.Value = starlark.Float(30)
	if err := starlark.UnpackArgs(b.Name(), args, kw, "method", &method, "url", &raw,
		"headers?", &headers, "json?", &jsonBody, "body?", &body, "timeout?", &timeout); err != nil {
		return nil, err
	}
	method = strings.ToUpper(strings.TrimSpace(method))
	u, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("http: bad url")
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, fmt.Errorf("http: url must be http or https")
	}
	if !r.hostAllowed(u.Host) {
		return nil, fmt.Errorf("capability http: host %s not declared in routine.toml", u.Host)
	}
	if !r.methodAllowed(method) {
		return nil, fmt.Errorf("capability http: method %s not declared in routine.toml", method)
	}
	if r.env.DryRun && method != "GET" {
		r.addAction("http", method+" "+raw)
		d := starlark.NewDict(6)
		d.SetKey(starlark.String("status"), starlark.MakeInt(0))
		d.SetKey(starlark.String("headers"), starlark.NewDict(0))
		d.SetKey(starlark.String("body"), starlark.String(""))
		d.SetKey(starlark.String("json"), starlark.None)
		d.SetKey(starlark.String("error"), starlark.None)
		d.SetKey(starlark.String("dry"), starlark.Bool(true))
		return d, nil
	}

	var payload io.Reader
	contentType := ""
	if jsonBody != starlark.None {
		g, err := toGo(jsonBody)
		if err != nil {
			return nil, fmt.Errorf("http: json= %v", err)
		}
		enc, err := json.Marshal(g)
		if err != nil {
			return nil, fmt.Errorf("http: json= %v", err)
		}
		payload = strings.NewReader(string(enc))
		contentType = "application/json"
	} else if body != starlark.None {
		s, ok := starlark.AsString(body)
		if !ok {
			return nil, fmt.Errorf("http: body must be a string")
		}
		payload = strings.NewReader(s)
	}

	secs, ok := starlark.AsFloat(timeout)
	if !ok {
		return nil, fmt.Errorf("http: timeout must be a number of seconds")
	}
	if secs <= 0 || math.IsNaN(secs) {
		secs = 30
	}

	req, err := http.NewRequestWithContext(r.ctx, method, raw, payload)
	if err != nil {
		return nil, fmt.Errorf("http: %s %s: request could not be built", method, u.Host)
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	if d, ok := headers.(*starlark.Dict); ok {
		for _, k := range d.Keys() {
			v, _, _ := d.Get(k)
			ks, _ := starlark.AsString(k)
			vs, _ := starlark.AsString(v)
			req.Header.Set(ks, vs)
		}
	}
	client := &http.Client{
		Timeout: time.Duration(secs * float64(time.Second)),
		CheckRedirect: func(rq *http.Request, via []*http.Request) error {
			if !r.hostAllowed(rq.URL.Host) {
				return fmt.Errorf("capability http: redirect to host %s not declared in routine.toml", rq.URL.Host)
			}
			if len(via) >= 10 {
				return fmt.Errorf("http: too many redirects")
			}
			return nil
		},
	}
	resp, err := client.Do(req)
	if err != nil {
		// Starlark has no try/except: a transport failure comes back as a
		// value so a gather can degrade it into a row.
		return httpFailure(method, u, err), nil
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if err != nil {
		return httpFailure(method, u, errReadBody), nil
	}
	hd := starlark.NewDict(len(resp.Header))
	for k, vs := range resp.Header {
		hd.SetKey(starlark.String(strings.ToLower(k)), starlark.String(strings.Join(vs, ", ")))
	}
	out := starlark.NewDict(5)
	out.SetKey(starlark.String("status"), starlark.MakeInt(resp.StatusCode))
	out.SetKey(starlark.String("headers"), hd)
	out.SetKey(starlark.String("body"), starlark.String(data))
	parsed := starlark.Value(starlark.None)
	if strings.Contains(strings.ToLower(resp.Header.Get("Content-Type")), "json") {
		var v any
		if json.Unmarshal(data, &v) == nil {
			if sv, err := toStar(v); err == nil && sv != nil {
				parsed = sv
			}
		}
	}
	out.SetKey(starlark.String("json"), parsed)
	out.SetKey(starlark.String("error"), starlark.None)
	return out, nil
}

// errReadBody marks a response whose body could not be read to the end.
var errReadBody = errors.New("reading the response failed")

// httpFailure turns a transport-level failure into the http() result shape:
// status 0 and a short reason. The request is never echoed — its headers may
// carry a secret — and neither is the transport error's own text.
func httpFailure(method string, u *url.URL, err error) *starlark.Dict {
	reason := "request failed"
	switch {
	case errors.Is(err, errReadBody):
		reason = "reading the response failed"
	case strings.Contains(err.Error(), "capability http: redirect to host"):
		// CheckRedirect's own message: hosts only, no header values.
		reason = "redirect off the allowlist"
	case strings.Contains(err.Error(), "http: too many redirects"):
		reason = "too many redirects"
	case errors.Is(err, context.DeadlineExceeded) || os.IsTimeout(err):
		reason = "timed out"
	case errors.Is(err, context.Canceled):
		reason = "cancelled"
	}
	d := starlark.NewDict(5)
	d.SetKey(starlark.String("status"), starlark.MakeInt(0))
	d.SetKey(starlark.String("headers"), starlark.NewDict(0))
	d.SetKey(starlark.String("body"), starlark.String(""))
	d.SetKey(starlark.String("json"), starlark.None)
	d.SetKey(starlark.String("error"),
		starlark.String(fmt.Sprintf("http: %s %s://%s: %s", method, u.Scheme, u.Host, reason)))
	return d
}

// bSecret reads <secrets_dir>/<name> so the script never builds key paths.
func (r *runner) bSecret(_ *starlark.Thread, b *starlark.Builtin, args starlark.Tuple, kw []starlark.Tuple) (starlark.Value, error) {
	var name string
	if err := starlark.UnpackArgs(b.Name(), args, kw, "name", &name); err != nil {
		return nil, err
	}
	if name == "" || strings.ContainsAny(name, "/\\") {
		return nil, fmt.Errorf("secret %s not available (list it in requires.secrets)", name)
	}
	if r.env.SecretsDir == "" {
		return nil, fmt.Errorf("secret %s not available (list it in requires.secrets)", name)
	}
	data, err := os.ReadFile(filepath.Join(r.env.SecretsDir, name))
	if err != nil {
		return nil, fmt.Errorf("secret %s not available (list it in requires.secrets)", name)
	}
	return starlark.String(strings.TrimSpace(string(data))), nil
}

// writeAllowed reports whether a cleaned vault-relative path sits under a
// declared prefix. A prefix ending in "/" is a directory, else an exact file.
func (r *runner) writeAllowed(clean string) bool {
	c := r.env.Caps
	if c == nil || c.Write == nil {
		return false
	}
	for _, p := range c.Write.Paths {
		dir := strings.HasSuffix(p, "/")
		cp := filepath.Clean(p)
		if dir {
			if clean == cp || strings.HasPrefix(clean, cp+string(filepath.Separator)) {
				return true
			}
			continue
		}
		if clean == cp {
			return true
		}
	}
	return false
}

// bWrite writes a vault file atomically inside a declared prefix. True when
// the content changed, False when it was already identical (or dry run).
func (r *runner) bWrite(_ *starlark.Thread, b *starlark.Builtin, args starlark.Tuple, kw []starlark.Tuple) (starlark.Value, error) {
	var p, text string
	if err := starlark.UnpackArgs(b.Name(), args, kw, "path", &p, "text", &text); err != nil {
		return nil, err
	}
	if filepath.IsAbs(p) {
		return nil, fmt.Errorf("capability write: path %s must be vault-relative", p)
	}
	clean := filepath.Clean(p)
	if clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) || strings.Contains(p, "..") {
		return nil, fmt.Errorf("capability write: path %s escapes the vault", p)
	}
	if !r.writeAllowed(clean) {
		return nil, fmt.Errorf("capability write: path %s not declared in routine.toml", p)
	}
	if r.env.DryRun {
		r.addAction("write", clean)
		return starlark.False, nil
	}
	full := filepath.Join(r.env.Vault, clean)
	if old, err := os.ReadFile(full); err == nil && string(old) == text {
		return starlark.False, nil
	}
	dir := filepath.Dir(full)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	tmp, err := os.CreateTemp(dir, ".qilla-write-*")
	if err != nil {
		return nil, err
	}
	name := tmp.Name()
	if _, err := tmp.WriteString(text); err != nil {
		tmp.Close()
		os.Remove(name)
		return nil, err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(name)
		return nil, err
	}
	if err := os.Chmod(name, 0o644); err != nil {
		os.Remove(name)
		return nil, err
	}
	if err := os.Rename(name, full); err != nil {
		os.Remove(name)
		return nil, err
	}
	return starlark.True, nil
}
