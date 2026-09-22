// ask.go — `decide ask`: label-less typed decisions answered by a small local
// model, ported from qilla-deciders/deciders/ask.py (same prompt, same parsing,
// same trace files) so a routine decides without spawning Python.
//
// No training set, no labels: the caller names the option set at the call site
// and the decider model on the GPU (Lemonade) answers with one word. The
// distribution comes from the generated tokens' logprobs, so the same
// `unknown` escape as the trained arms applies — below the floor the answer is
// `unknown`, and an unknown is *undecided*, never a guess.
//
// Kinds: `choice` (an explicit option list), `noul` (yes/no) and `score` (an
// integer 0-100). `unknown` is always in the option set, passed or not.
//
// A backend failure is never a Go error: it is an `unknown` answer with the
// cause in Error. Only bad arguments return an error.

package decide

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Defaults for the [deciders] config block.
const (
	// DefaultLemonadeURL is Lemonade's OpenAI-compatible base.
	DefaultLemonadeURL = "http://127.0.0.1:13305/api/v1"
	// DefaultAskModel is how Lemonade registers the decider checkpoint
	// (`/api/v1/models`).
	DefaultAskModel = "decider-2b-GGUF-decider-2b-q8_0.gguf"
	// DefaultFloor mirrors Killa/Config/Variables.md `decider_conf_floor`.
	DefaultFloor = 0.85
	// DefaultPolicyVersion tags every trace line.
	DefaultPolicyVersion = "ask-v1"
	// AskTimeout: Lemonade must answer fast; a slow decider is a broken one.
	AskTimeout = 5 * time.Second
)

const (
	askMaxTokens   = 8  // one word is the whole answer
	askTopLogprobs = 10 // candidates per generated position
	// textConf is the confidence granted to an answer parsed out of the text
	// when logprobs are absent.
	textConf = 0.6
	// askPrefill is the prefilled assistant turn — the model resumes from
	// here, so the next token is the answer and not a "First, the user…".
	askPrefill = "Answer:"
	// httpErrorChars of an HTTP error body are surfaced in Error.
	httpErrorChars = 200

	// Unknown is the escape: always in the option set, never a guess.
	Unknown = "unknown"

	scoreMin, scoreMax = 0, 100
)

// noulOptions is what `noul` asks over.
var noulOptions = []string{"yes", "no"}

// AskRequest is one typed decision. Kind is choice | score | noul; Route is
// local | jev. The backend wiring below comes from the [deciders] config
// block; zero values fall back to the package defaults.
type AskRequest struct {
	Kind     string
	Options  []string
	Question string
	Text     string
	Floor    float64
	Route    string
	Public   bool
	Caller   string

	// URL is the Lemonade OpenAI-compatible base ([deciders] lemonade_url).
	URL string
	// Model is the decider checkpoint ([deciders] ask_model).
	Model string
	// PolicyVersion tags the trace ([deciders] policy_version).
	PolicyVersion string
	// JevEnabled gates the remote route ([deciders] jev_enabled).
	JevEnabled bool
	// JevURL and JevKey are the remote route's endpoint and bearer token
	// ([deciders] jev_url, secret `jev_key`).
	JevURL, JevKey string
	// Timeout overrides AskTimeout.
	Timeout time.Duration
	// Client overrides http.DefaultClient (tests).
	Client *http.Client
}

// AskResult is the decision. Kind is "choice" or "score" — `noul` answers as a
// choice over yes/no. Value is the score (nil = undecided); Label the chosen
// option. Error carries a backend failure; the result is still valid (unknown).
type AskResult struct {
	Kind     string
	Label    string
	Value    *int
	Conf     float64
	Dist     map[string]float64
	DistFrom string
	Route    string
	Model    string
	MS       int
	Error    string
}

// MarshalJSON writes the fields in the Python CLI's order, and keeps its two
// shape rules: a score carries `value` (null when undecided) and no `dist`, a
// choice carries `label` and `dist` (null when there is none).
func (r AskResult) MarshalJSON() ([]byte, error) {
	var b bytes.Buffer
	b.WriteByte('{')
	put := func(key string, v any) {
		if b.Len() > 1 {
			b.WriteByte(',')
		}
		k, _ := json.Marshal(key)
		b.Write(k)
		b.WriteByte(':')
		enc, err := json.Marshal(v)
		if err != nil {
			enc = []byte("null")
		}
		b.Write(enc)
	}
	put("kind", r.Kind)
	if r.Kind == "score" {
		put("value", r.Value)
		put("conf", r.Conf)
	} else {
		put("label", r.Label)
		put("conf", r.Conf)
		if r.Dist == nil {
			put("dist", nil)
		} else {
			put("dist", r.Dist)
		}
	}
	if r.DistFrom != "" {
		put("dist_from", r.DistFrom)
	}
	put("route", r.Route)
	put("model", r.Model)
	put("ms", r.MS)
	if r.Error != "" {
		put("error", r.Error)
	}
	b.WriteByte('}')
	return b.Bytes(), nil
}

// WithUnknown is the option set as the model sees it — `unknown` is always
// available, and always last.
func WithUnknown(options []string) []string {
	out := make([]string, 0, len(options)+1)
	for _, o := range options {
		if o != Unknown {
			out = append(out, o)
		}
	}
	return append(out, Unknown)
}

// AskOptions is the option set a request decides over, `unknown` included.
func (req AskRequest) AskOptions() []string {
	if req.Kind == "noul" {
		return WithUnknown(noulOptions)
	}
	return WithUnknown(req.Options)
}

func (req AskRequest) floor() float64 {
	if req.Floor > 0 {
		return req.Floor
	}
	return DefaultFloor
}

func (req AskRequest) model() string {
	if req.Model != "" {
		return req.Model
	}
	return DefaultAskModel
}

func (req AskRequest) baseURL() string {
	if req.URL != "" {
		return strings.TrimRight(req.URL, "/")
	}
	return DefaultLemonadeURL
}

func (req AskRequest) timeout() time.Duration {
	if req.Timeout > 0 {
		return req.Timeout
	}
	return AskTimeout
}

func (req AskRequest) client() *http.Client {
	if req.Client != nil {
		return req.Client
	}
	return http.DefaultClient
}

// PolicyVer is the trace's policy tag.
func (req AskRequest) PolicyVer() string {
	if req.PolicyVersion != "" {
		return req.PolicyVersion
	}
	return DefaultPolicyVersion
}

// Validate reports a bad call — the only thing Ask returns an error for.
func (req AskRequest) Validate() error {
	switch req.Kind {
	case "choice":
		if len(WithUnknown(req.Options)) < 2 {
			return fmt.Errorf("decide ask: choice needs at least one option")
		}
	case "score", "noul":
	default:
		return fmt.Errorf("decide ask: kind must be choice | score | noul, got %q", req.Kind)
	}
	switch req.Route {
	case "", "local":
	case "jev":
		if !req.Public {
			return fmt.Errorf("jev route requires public input")
		}
	default:
		return fmt.Errorf("decide ask: route must be local | jev, got %q", req.Route)
	}
	return nil
}

// --- prompt ----------------------------------------------------------------

// BuildPrompt is the Python `build_prompt`, word for word.
func BuildPrompt(kind string, options []string, question, text string) string {
	head := ""
	if q := strings.TrimSpace(question); q != "" {
		head = q + "\n\n"
	}
	var rule string
	if kind == "score" {
		rule = fmt.Sprintf(
			"Answer with a single integer from %d to %d, or the word %s if you cannot decide. "+
				"No other words, no punctuation, no explanation.", scoreMin, scoreMax, Unknown)
	} else {
		rule = "Answer with exactly one word from this list: " + strings.Join(options, ", ") +
			". Answer " + Unknown + " if you cannot decide. " +
			"No other words, no punctuation, no explanation."
	}
	return head + rule + "\n\nInput:\n" + text + "\n\nAnswer:"
}

// --- transport -------------------------------------------------------------

// askHTTPError means Lemonade answered, with an error — not the same thing as
// being down.
type askHTTPError struct {
	status int
	body   string
}

func (e *askHTTPError) Error() string {
	return fmt.Sprintf("http %d: %s", e.status, truncate(e.body, httpErrorChars))
}

// askDownError means the transport failed: nothing answered.
type askDownError struct{ cause string }

func (e *askDownError) Error() string { return e.cause }

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}

func postChat(ctx context.Context, req AskRequest, payload map[string]any) (map[string]any, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, &askDownError{cause: err.Error()}
	}
	url := req.baseURL() + "/chat/completions"
	hreq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, &askDownError{cause: err.Error()}
	}
	hreq.Header.Set("Content-Type", "application/json")
	resp, err := req.client().Do(hreq)
	if err != nil {
		return nil, &askDownError{cause: err.Error()}
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, &askDownError{cause: err.Error()}
	}
	if resp.StatusCode >= 400 {
		return nil, &askHTTPError{status: resp.StatusCode, body: string(raw)}
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, &askDownError{cause: "bad response: " + err.Error()}
	}
	return out, nil
}

// chat is one deterministic completion, with logprobs and no thinking.
//
// Two guards against the model's chain-of-thought preamble, which otherwise
// makes the first token "First"/"Hmm"/"<think>" and destroys the distribution:
// the no-thinking template hints, and a prefilled assistant turn so the very
// next token has to be the answer. A backend that rejects the prefilled turn
// (400/422) is retried once without it.
func chat(ctx context.Context, req AskRequest, prompt string) (map[string]any, error) {
	messages := []map[string]any{
		{"role": "user", "content": prompt},
		{"role": "assistant", "content": askPrefill},
	}
	payload := map[string]any{
		"model":                req.model(),
		"messages":             messages,
		"max_tokens":           askMaxTokens,
		"temperature":          0,
		"logprobs":             true,
		"top_logprobs":         askTopLogprobs,
		"chat_template_kwargs": map[string]any{"enable_thinking": false},
		"reasoning":            false,
	}
	body, err := postChat(ctx, req, payload)
	if err == nil {
		return body, nil
	}
	he, ok := err.(*askHTTPError)
	if !ok || (he.status != 400 && he.status != 422) {
		return nil, err // 404/401 are about the model or the key, not the prefill
	}
	payload["messages"] = messages[:1]
	return postChat(ctx, req, payload)
}

// --- response reading ------------------------------------------------------

func answerText(body map[string]any) string {
	content, _ := dig(body, "choices", 0, "message", "content").(string)
	content = strings.TrimSpace(content)
	content = strings.TrimPrefix(content, askPrefill)
	content = strings.ReplaceAll(content, "<think>", " ")
	content = strings.ReplaceAll(content, "</think>", " ")
	return strings.TrimSpace(content)
}

// candidate is one {token, logprob} the backend offered at a position.
type candidate struct {
	token   string
	logprob float64
}

// topLogprobs returns one candidate list per generated token — every position,
// not only the first: a model that opens with whitespace or a stray "Answer"
// still has the real answer a token or two later.
func topLogprobs(body map[string]any) [][]candidate {
	content, _ := dig(body, "choices", 0, "logprobs", "content").([]any)
	var out [][]candidate
	for i, e := range content {
		if i >= askMaxTokens {
			break
		}
		entry, ok := e.(map[string]any)
		if !ok {
			continue
		}
		tok, _ := entry["token"].(string)
		if cleanToken(tok) == "" {
			// A position that actually emitted whitespace or punctuation
			// holds no answer, whatever its candidate list contains.
			continue
		}
		var top []candidate
		if raw, ok := entry["top_logprobs"].([]any); ok {
			for _, c := range raw {
				m, ok := c.(map[string]any)
				if !ok {
					continue
				}
				t, okT := m["token"].(string)
				lp, okL := toFloat(m["logprob"])
				if !okT || !okL {
					continue
				}
				top = append(top, candidate{token: t, logprob: lp})
			}
		}
		if len(top) == 0 {
			lp, ok := toFloat(entry["logprob"])
			if !ok {
				continue
			}
			top = []candidate{{token: tok, logprob: lp}}
		}
		out = append(out, top)
	}
	return out
}

func dig(v any, path ...any) any {
	for _, p := range path {
		switch k := p.(type) {
		case string:
			m, ok := v.(map[string]any)
			if !ok {
				return nil
			}
			v = m[k]
		case int:
			a, ok := v.([]any)
			if !ok || k >= len(a) {
				return nil
			}
			v = a[k]
		}
	}
	return v
}

func toFloat(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case json.Number:
		f, err := n.Float64()
		return f, err == nil
	case int:
		return float64(n), true
	}
	return 0, false
}

// --- distributions ---------------------------------------------------------

func cleanToken(token string) string {
	return strings.ToLower(strings.Trim(strings.TrimSpace(token), "\"'`*.,:;!?"))
}

// distFromLogprobs spreads the probability mass per option: every candidate
// token that prefixes an option counts for it, renormalised over the set.
// Small models answer one word, so the first token already separates the
// options ("ye" -> yes, "un" -> unknown).
func distFromLogprobs(top []candidate, options []string) map[string]float64 {
	mass := make(map[string]float64, len(options))
	for _, o := range options {
		mass[o] = 0
	}
	hit := false
	for _, c := range top {
		token := cleanToken(c.token)
		if token == "" {
			continue
		}
		p := math.Exp(c.logprob)
		for _, option := range options {
			if strings.HasPrefix(strings.ToLower(option), token) {
				mass[option] += p
				hit = true
			}
		}
	}
	total := 0.0
	for _, m := range mass {
		total += m
	}
	if !hit || total <= 0 {
		return nil
	}
	out := make(map[string]float64, len(options))
	for _, o := range options {
		out[o] = mass[o] / total
	}
	return out
}

// scanLogprobs is the distribution at the first generated position that names
// an option.
func scanLogprobs(positions [][]candidate, options []string) map[string]float64 {
	for _, top := range positions {
		if d := distFromLogprobs(top, options); d != nil {
			return d
		}
	}
	return nil
}

// distFromAnswer is the fallback: read the LAST answer word (a thinking model
// puts its verdict at the end) and spread the rest of the mass evenly.
func distFromAnswer(text string, options []string) map[string]float64 {
	var words []string
	for _, w := range strings.Fields(text) {
		if c := cleanToken(w); c != "" {
			words = append(words, c)
		}
	}
	picked := ""
	for i := len(words) - 1; i >= 0 && picked == ""; i-- { // exact match wins
		for _, option := range options {
			if words[i] == strings.ToLower(option) {
				picked = option
				break
			}
		}
	}
	for i := len(words) - 1; i >= 0 && picked == ""; i-- {
		for _, option := range options {
			if strings.HasPrefix(strings.ToLower(option), words[i]) {
				picked = option
				break
			}
		}
	}
	if picked == "" {
		return nil
	}
	rest := (1.0 - textConf) / math.Max(1, float64(len(options)-1))
	out := make(map[string]float64, len(options))
	for _, o := range options {
		if o == picked {
			out[o] = textConf
		} else {
			out[o] = rest
		}
	}
	return out
}

var scoreRe = regexp.MustCompile(`\d+`)

// scoreFromAnswer is the last in-range integer in the answer.
func scoreFromAnswer(text string) *int {
	m := scoreRe.FindAllString(text, -1)
	for i := len(m) - 1; i >= 0; i-- {
		v, err := strconv.Atoi(m[i])
		if err != nil {
			continue
		}
		if v >= scoreMin && v <= scoreMax {
			return &v
		}
	}
	return nil
}

// scoreConf is the probability of the first generated token that opens the score.
func scoreConf(positions [][]candidate, answer string) (float64, bool) {
	head := cleanToken(answer)
	for _, top := range positions {
		best, found := 0.0, false
		for _, c := range top {
			token := cleanToken(c.token)
			if token == "" || !strings.HasPrefix(head, token) {
				continue
			}
			p := math.Exp(c.logprob)
			if !found || p > best {
				best, found = p, true
			}
		}
		if found {
			return best, true
		}
	}
	return 0, false
}

// --- the jev route ---------------------------------------------------------

// askJev is the opt-in remote route, off unless explicitly enabled and
// configured.
//
// TODO: the payload here is a placeholder — a generic {"text", "labels"} POST
// and a {"label", "conf"} reply. The real Jev API shape is not settled; do not
// treat this as its contract. Callers must pass Public: nothing sensitive goes
// off the machine.
func askJev(ctx context.Context, req AskRequest, options []string) (label string, conf float64, errMsg string) {
	if !req.JevEnabled {
		return Unknown, 0, "jev disabled"
	}
	if req.JevURL == "" || req.JevKey == "" {
		return Unknown, 0, "jev not configured"
	}
	payload, _ := json.Marshal(map[string]any{"text": req.Text, "labels": options})
	hreq, err := http.NewRequestWithContext(ctx, http.MethodPost, req.JevURL, bytes.NewReader(payload))
	if err != nil {
		return Unknown, 0, "jev unreachable"
	}
	hreq.Header.Set("Content-Type", "application/json")
	hreq.Header.Set("Authorization", "Bearer "+req.JevKey)
	resp, err := req.client().Do(hreq)
	if err != nil {
		return Unknown, 0, "jev unreachable"
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return Unknown, 0, "jev unreachable"
	}
	var body map[string]any
	if err := json.Unmarshal(raw, &body); err != nil {
		return Unknown, 0, "jev unreachable"
	}
	label, _ = body["label"].(string)
	if !contains(options, label) {
		label = Unknown
	}
	conf, _ = toFloat(body["conf"])
	return label, conf, ""
}

func contains(xs []string, s string) bool {
	for _, x := range xs {
		if x == s {
			return true
		}
	}
	return false
}

// --- the decision ----------------------------------------------------------

func round4(f float64) float64 { return math.Round(f*1e4) / 1e4 }

// Ask makes one typed decision. It never returns an error for a backend
// failure — that is an `unknown` answer with the cause in Error; only a bad
// request is an error.
func Ask(ctx context.Context, req AskRequest) (AskResult, error) {
	if err := req.Validate(); err != nil {
		return AskResult{}, err
	}
	floor := req.floor()
	options := req.AskOptions()
	route := req.Route
	if route == "" {
		route = "local"
	}
	model := req.model()
	started := time.Now()

	kind := "choice"
	if req.Kind == "score" {
		kind = "score"
	}
	var (
		errMsg   string
		dist     map[string]float64
		distFrom string
		label    = Unknown
		conf     float64
		value    *int
	)

	cctx, cancel := context.WithTimeout(ctx, req.timeout())
	defer cancel()

	if route == "jev" {
		label, conf, errMsg = askJev(cctx, req, options)
		model = "jev"
	} else {
		body, err := chat(cctx, req, BuildPrompt(req.Kind, options, req.Question, req.Text))
		switch {
		case err != nil:
			if he, ok := err.(*askHTTPError); ok {
				errMsg = he.Error() // "http 404: …" — the cause, not a guess
			} else {
				errMsg = "lemonade unreachable"
			}
		default:
			text := answerText(body)
			positions := topLogprobs(body)
			if kind == "score" {
				if value = scoreFromAnswer(text); value != nil {
					if c, ok := scoreConf(positions, strconv.Itoa(*value)); ok {
						conf = c
					} else {
						conf = textConf
						distFrom = "text"
					}
					label = strconv.Itoa(*value)
				}
			} else {
				dist = scanLogprobs(positions, options)
				if dist == nil {
					dist = distFromAnswer(text, options)
					if dist != nil {
						distFrom = "text"
					}
				}
				if dist != nil {
					label = argmax(dist, options)
					conf = dist[label]
				}
			}
		}
	}

	if conf < floor {
		label = Unknown
		value = nil
	}

	out := AskResult{
		Kind:     kind,
		Conf:     round4(conf),
		DistFrom: distFrom,
		Route:    route,
		Model:    model,
		MS:       int(math.Round(float64(time.Since(started).Microseconds()) / 1000)),
	}
	if kind == "score" {
		out.Value = value
	} else {
		out.Label = label
		if dist != nil {
			out.Dist = make(map[string]float64, len(dist))
			for o, p := range dist {
				out.Dist[o] = round4(p)
			}
		}
	}
	if errMsg != "" {
		out.Error = errMsg
		out.Label = Unknown
		out.Value = nil
	}
	return out, nil
}

// argmax picks the highest-mass option; a tie goes to the earlier option, as
// Python's max over the dict does, so the answer is deterministic.
func argmax(dist map[string]float64, options []string) string {
	best, bestP := "", math.Inf(-1)
	for _, o := range options {
		if dist[o] > bestP {
			best, bestP = o, dist[o]
		}
	}
	if best == "" {
		return Unknown
	}
	return best
}

// TraceLabel is the label as the trace records it — a score is its number, or
// unknown.
func TraceLabel(out AskResult) string {
	if out.Kind == "score" {
		if out.Value == nil {
			return Unknown
		}
		return strconv.Itoa(*out.Value)
	}
	if out.Label == "" {
		return Unknown
	}
	return out.Label
}

// AskAndTrace is Ask plus the decision-trace line (and the escape when the
// outcome is unknown). An unwritable trace is never fatal: the decision stands
// and the write error is returned alongside it.
func AskAndTrace(ctx context.Context, dir string, req AskRequest) (AskResult, error) {
	out, err := Ask(ctx, req)
	if err != nil {
		return out, err
	}
	if dir == "" {
		return out, nil
	}
	var options []string
	if req.Kind != "score" {
		options = req.AskOptions()
	} else {
		options = []string{}
	}
	line := TraceRecord(TraceInput{
		Kind:          out.Kind,
		Options:       options,
		Question:      req.Question,
		Text:          req.Text,
		Label:         TraceLabel(out),
		Conf:          out.Conf,
		Dist:          out.Dist,
		Route:         out.Route,
		Model:         out.Model,
		Floor:         req.floor(),
		PolicyVersion: req.PolicyVer(),
		MS:            out.MS,
		Caller:        req.Caller,
	})
	if werr := AppendTrace(dir, line, req.Text); werr != nil {
		return out, werr
	}
	return out, nil
}
