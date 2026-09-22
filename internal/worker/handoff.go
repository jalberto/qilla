package worker

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// RecordSink is the one seam for a run's brain-state bookkeeping: the run
// record that would otherwise land in <state_dir>/runs and the runs ledger
// (which feeds `qilla runs` and the watermarks gathers read, e.g. meetings'
// "processed since the last ok run"). The default (Worker.Sink == nil) is the
// state dir; `qilla run --once --local` on a worker host installs a
// HandoffSink so nothing is written to brain state and the brain merges the
// lines on its next harvest (host-roles-worker §5, §7). The routine's own
// output note is rendered normally either way.
type RecordSink interface {
	Save(Record) error
}

// HandoffSink appends one line per run to <vault>/Qilla/Handoff/<host>.md:
//
//   - [ ] YYYY-MM-DD HH:MM · <host> · ledger:<routine> · <json>
type HandoffSink struct {
	Vault string
	Host  string // short hostname
	Now   func() time.Time
}

// HandoffPath is the vault-relative handoff note for host.
func HandoffPath(host string) string {
	return filepath.Join("Qilla", "Handoff", host+".md")
}

// ShortHost is os.Hostname() without its domain suffix.
func ShortHost() string {
	h, _ := os.Hostname()
	if i := strings.IndexByte(h, '.'); i >= 0 {
		h = h[:i]
	}
	return h
}

// Save appends the record's ledger line.
func (h HandoffSink) Save(r Record) error {
	now := time.Now
	if h.Now != nil {
		now = h.Now
	}
	sum := struct {
		OK       bool    `json:"ok"`
		Skipped  bool    `json:"skipped,omitempty"`
		Started  string  `json:"started"`
		Duration float64 `json:"duration_s"`
		Digest   string  `json:"digest,omitempty"`
		Error    string  `json:"error,omitempty"`
		Result   string  `json:"result,omitempty"`
	}{r.Error == "", r.Skipped, r.Started.Format(time.RFC3339), r.Duration.Seconds(), r.Digest, firstLine(r.Error), firstLine(r.Result)}
	b, err := json.Marshal(sum)
	if err != nil {
		return err
	}
	p := filepath.Join(h.Vault, HandoffPath(h.Host))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(p, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = fmt.Fprintf(f, "- [ ] %s · %s · ledger:%s · %s\n", now().Format("2006-01-02 15:04"), h.Host, r.Routine, b)
	return err
}
