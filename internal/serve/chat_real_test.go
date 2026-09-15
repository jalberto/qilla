package serve

import (
	"os"
	"testing"
)

// Smoke against a real Claude Code transcript, opt-in:
//
//	QILLA_SMOKE_WORKDIR=~/Notes QILLA_SMOKE_SID=<session id> go test ./internal/serve -run Real -v
func TestRealTranscriptSmoke(t *testing.T) {
	wd, sid := os.Getenv("QILLA_SMOKE_WORKDIR"), os.Getenv("QILLA_SMOKE_SID")
	if wd == "" || sid == "" {
		t.Skip("set QILLA_SMOKE_WORKDIR and QILLA_SMOKE_SID")
	}
	p := transcriptPath(wd, sid)
	turns, err := readTurns(p, 6)
	if err != nil {
		t.Fatalf("%s: %v", p, err)
	}
	if len(turns) == 0 {
		t.Fatalf("%s: no turns parsed", p)
	}
	t.Logf("path %s · attached=%v", p, attached(sid))
	for _, tr := range turns {
		txt := tr.Text
		if len(txt) > 90 {
			txt = txt[:90] + "…"
		}
		t.Logf("%s %-9s tools=%d %q", tr.Time.Local().Format("15:04:05"), tr.Role, tr.Tools, txt)
	}
}
