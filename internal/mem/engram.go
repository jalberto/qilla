package mem

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Engram talks to an `engram serve` sidecar over HTTP (tcp or unix socket).
// project → Engram project, kind → Engram type, key → topic_key (upsert).
type Engram struct {
	c    *http.Client
	base string // "http://engram" for unix, or the http URL
	sess map[string]bool
}

// State has no Engram type of its own: it is stored as a config observation
// and recognised again by its "state/" topic_key.
var kindToType = map[string]string{Heuristic: "learning", Said: "discovery", Route: "pattern", Note: "config", State: "config"}
var typeToKind = map[string]string{"learning": Heuristic, "discovery": Said, "pattern": Route, "config": Note}

// NewEngram accepts http://host:port or unix:///path/to.sock.
func NewEngram(rawURL string) (*Engram, error) {
	e := &Engram{sess: map[string]bool{}}
	if strings.HasPrefix(rawURL, "unix://") {
		path := strings.TrimPrefix(rawURL, "unix://")
		e.c = &http.Client{Timeout: 15 * time.Second, Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, "unix", path)
			}}}
		e.base = "http://engram"
		return e, nil
	}
	if _, err := url.Parse(rawURL); err != nil {
		return nil, err
	}
	e.c = &http.Client{Timeout: 15 * time.Second}
	e.base = strings.TrimRight(rawURL, "/")
	return e, nil
}

func (e *Engram) Name() string { return "engram" }

func (e *Engram) do(ctx context.Context, method, path string, in, out any) error {
	var body io.Reader
	if in != nil {
		b, _ := json.Marshal(in)
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, e.base+path, body)
	if err != nil {
		return err
	}
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	res, err := e.c.Do(req)
	if err != nil {
		return fmt.Errorf("engram: %w (is qilla-engram.service running?)", err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(res.Body, 8<<20))
	if res.StatusCode >= 300 {
		return fmt.Errorf("engram %s %s: %d %s", method, path, res.StatusCode, firstLine(string(b)))
	}
	if out != nil && len(bytes.TrimSpace(b)) > 0 {
		return json.Unmarshal(b, out)
	}
	return nil
}

// session ensures one manual session per project (Engram requires it on writes).
func (e *Engram) session(ctx context.Context, project string) (string, error) {
	id := "qilla-" + project
	if e.sess[id] {
		return id, nil
	}
	err := e.do(ctx, "POST", "/sessions", map[string]any{"id": id, "project": project, "directory": "/qilla/" + project}, nil)
	if err != nil && !strings.Contains(err.Error(), "409") && !strings.Contains(err.Error(), "exists") {
		// a second POST for an existing id may 400/409; probe it
		var probe map[string]any
		if gerr := e.do(ctx, "GET", "/sessions/"+id, nil, &probe); gerr != nil {
			return "", err
		}
	}
	e.sess[id] = true
	return id, nil
}

type obs struct {
	ID            any     `json:"id"`
	Project       string  `json:"project"`
	Type          string  `json:"type"`
	Title         string  `json:"title"`
	Content       string  `json:"content"`
	TopicKey      string  `json:"topic_key"`
	RevisionCount int     `json:"revision_count"`
	DuplicateCnt  int     `json:"duplicate_count"`
	CreatedAt     string  `json:"created_at"`
	UpdatedAt     string  `json:"updated_at"`
	Score         float64 `json:"score"`
}

// entry maps an Engram observation to qilla's view; topic_key "kind/key" → key.
func (o obs) entry() Entry {
	kind := typeToKind[o.Type]
	if kind == "" {
		kind = Note
	}
	if strings.HasPrefix(o.TopicKey, State+"/") {
		kind = State
	}
	sup := 1 + o.RevisionCount + o.DuplicateCnt
	return Entry{ID: idString(o.ID), Project: o.Project, Kind: kind, Key: strings.TrimPrefix(o.TopicKey, kind+"/"), Text: o.Content,
		Support: sup, Created: parseT(o.CreatedAt), Updated: parseT(o.UpdatedAt), Score: o.Score}
}

func (e *Engram) Save(ctx context.Context, project, kind, key, text string) (Entry, error) {
	sid, err := e.session(ctx, project)
	if err != nil {
		return Entry{}, err
	}
	title := key
	if title == "" {
		title = firstLine(text)
		if len(title) > 80 {
			title = title[:80]
		}
	}
	body := map[string]any{"session_id": sid, "type": kindToType[kind], "title": title, "content": text, "project": project}
	if key != "" {
		body["topic_key"] = kind + "/" + key
	}
	var o obs
	if err := e.do(ctx, "POST", "/observations", body, &o); err != nil {
		return Entry{}, err
	}
	if o.Project == "" {
		o.Project = project
	}
	if o.Type == "" {
		o.Type = kindToType[kind]
	}
	if o.Content == "" {
		o.Content = text
	}
	en := o.entry()
	en.Key = key
	return en, nil
}

func (e *Engram) Search(ctx context.Context, project, kind, query string, n int) ([]Entry, error) {
	q := url.Values{"q": {query}, "limit": {strconv.Itoa(n)}}
	if project != "" {
		q.Set("project", project)
	} else {
		q.Set("all_projects", "true") // without either, Engram searches nothing
	}
	if kind != "" {
		q.Set("type", kindToType[kind])
	}
	var os []obs
	if err := e.do(ctx, "GET", "/search?"+q.Encode(), nil, &os); err != nil {
		if unknownProject(err) {
			return nil, nil // nothing stored for this project yet
		}
		return nil, err
	}
	out := make([]Entry, 0, len(os))
	for _, o := range os {
		out = append(out, o.entry())
	}
	return out, nil
}

func (e *Engram) Get(ctx context.Context, id string) (Entry, error) {
	var o obs
	if err := e.do(ctx, "GET", "/observations/"+id, nil, &o); err != nil {
		return Entry{}, err
	}
	return o.entry(), nil
}

func (e *Engram) Delete(ctx context.Context, id string) error {
	return e.do(ctx, "DELETE", "/observations/"+id, nil, nil)
}

func (e *Engram) Recent(ctx context.Context, project string, n int) ([]Entry, error) {
	q := url.Values{"limit": {strconv.Itoa(n)}}
	if project != "" {
		q.Set("project", project)
	} else {
		q.Set("all_projects", "true")
	}
	var os []obs
	if err := e.do(ctx, "GET", "/observations/recent?"+q.Encode(), nil, &os); err != nil {
		if unknownProject(err) {
			return nil, nil
		}
		return nil, err
	}
	out := make([]Entry, 0, len(os))
	for _, o := range os {
		out = append(out, o.entry())
	}
	return out, nil
}

func (e *Engram) Conflicts(ctx context.Context, project string) ([]Conflict, error) {
	q := url.Values{"status": {"pending"}, "limit": {"50"}}
	if project != "" {
		q.Set("project", project)
	} else {
		q.Set("all_projects", "true")
	}
	var raw struct {
		Relations []struct {
			SyncID   string `json:"sync_id"`
			Relation string `json:"relation"`
			Status   string `json:"judgment_status"`
			Source   any    `json:"source_id"`
			Target   any    `json:"target_id"`
		} `json:"relations"`
	}
	if err := e.do(ctx, "GET", "/conflicts?"+q.Encode(), nil, &raw); err != nil {
		if unknownProject(err) {
			return nil, nil
		}
		return nil, err
	}
	out := make([]Conflict, 0, len(raw.Relations))
	for _, r := range raw.Relations {
		out = append(out, Conflict{ID: r.SyncID, SourceID: idString(r.Source), TargetID: idString(r.Target), Relation: r.Relation, Status: r.Status})
	}
	return out, nil
}

func (e *Engram) Judge(ctx context.Context, relationID, relation, reason string) error {
	return e.do(ctx, "POST", "/conflicts/judge", map[string]any{"judgment_id": relationID, "relation": relation, "reason": reason, "confidence": 0.8}, nil)
}

func (e *Engram) Health(ctx context.Context) error {
	var h map[string]any
	if err := e.do(ctx, "GET", "/health", nil, &h); err != nil {
		return err
	}
	if h["status"] != "ok" {
		return fmt.Errorf("engram health: %v", h)
	}
	return nil
}

func idString(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case float64:
		return strconv.FormatInt(int64(t), 10)
	default:
		return fmt.Sprint(v)
	}
}

func parseT(s string) time.Time {
	for _, l := range []string{time.RFC3339Nano, time.RFC3339, "2006-01-02 15:04:05"} {
		if t, err := time.Parse(l, s); err == nil {
			return t
		}
	}
	return time.Time{}
}

func firstLine(s string) string {
	s, _, _ = strings.Cut(strings.TrimSpace(s), "\n")
	if len(s) > 200 {
		s = s[:200]
	}
	return s
}

// unknownProject matches Engram's 404 {"code":"unknown_project"} for a project
// that has no observations yet.
func unknownProject(err error) bool {
	return err != nil && (strings.Contains(err.Error(), "unknown_project") || strings.Contains(err.Error(), "not found"))
}
