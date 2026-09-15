package claude

import (
	"strings"
	"testing"
	"time"
)

func TestParseEnvelopeAndDenials(t *testing.T) {
	b := []byte(`{"type":"result","subtype":"success","is_error":false,"result":"ok","session_id":"abc","total_cost_usd":0,
	"usage":{"input_tokens":10,"output_tokens":5,"cache_creation_input_tokens":100,"cache_read_input_tokens":2000,"cache_creation":{"ephemeral_1h_input_tokens":100}},
	"permission_denials":[{"tool_name":"Bash","tool_input":{"command":"rm"}},{"tool_name":"Bash","tool_input":{}},{"tool_name":"WebFetch"}]}`)
	r, err := Parse(b)
	if err != nil {
		t.Fatal(err)
	}
	if r.SessionID != "abc" || r.Usage.CacheRead != 2000 || r.Usage.CacheCreation1h != 100 {
		t.Fatalf("fields: %+v", r)
	}
	if d := r.DeniedTools(); len(d) != 2 || d[0] != "Bash" || d[1] != "WebFetch" {
		t.Fatalf("denials: %v", d)
	}
}

func TestScrub(t *testing.T) {
	out := Scrub([]string{"HOME=/h", "CLAUDECODE=1", "CLAUDE_CODE_CHILD_SESSION=x", "ZMX_SESSION=k", "CREDENTIALS_DIRECTORY=/run/creds", "QILLA_SECRETS_DIR=/x", "PATH=/p"})
	if strings.Join(out, " ") != "HOME=/h PATH=/p" {
		t.Fatalf("scrub: %v", out)
	}
}

func TestRateLimitedParsesReset(t *testing.T) {
	now := time.Date(2026, 9, 12, 3, 10, 0, 0, time.FixedZone("CEST", 2*3600))
	r := Result{IsError: true, Result: "You've hit your session limit · resets 4:40am (Europe/Madrid)"}
	ok, at := r.RateLimited(now)
	if !ok || at.Hour() != 4 || at.Minute() != 42 || at.Day() != 12 {
		t.Fatalf("%v %v", ok, at)
	}
	r = Result{IsError: true, Result: "You've hit your weekly limit · resets 2am"}
	ok, at = r.RateLimited(now.Add(3 * time.Hour)) // 06:10 → 2am is tomorrow
	if !ok || at.Day() != 13 || at.Hour() != 2 {
		t.Fatalf("%v %v", ok, at)
	}
	if ok, _ := (Result{Result: "all good"}).RateLimited(now); ok {
		t.Fatal("not a limit")
	}
}
