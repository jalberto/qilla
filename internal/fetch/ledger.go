package fetch

import (
	"encoding/json"
	"errors"
	neturl "net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/jalberto/qilla/internal/decide"
	"golang.org/x/net/publicsuffix"
)

// LedgerEntry is what the ladder remembers about one domain: the method rung
// that last produced content there, and how often fetches worked or not.
type LedgerEntry struct {
	Rung      string `json:"rung"`
	Kind      string `json:"kind"`
	OKCount   int    `json:"ok_count"`
	FailCount int    `json:"fail_count"`
	LastOK    string `json:"last_ok,omitempty"` // RFC3339
	LastVia   string `json:"last_via,omitempty"`
}

// Ledger is {state_dir}/fetch/ledger.json: domain → entry.
type Ledger map[string]LedgerEntry

// LedgerFile is the ledger's path under a state dir.
func LedgerFile(stateDir string) string { return filepath.Join(stateDir, "fetch", "ledger.json") }

// Domain is url's registrable host without www. ("old.reddit.com" →
// "reddit.com"); "" for an unparsable url.
func Domain(raw string) string {
	u, err := neturl.Parse(strings.TrimSpace(raw))
	if err != nil {
		return ""
	}
	host := strings.TrimPrefix(strings.ToLower(u.Hostname()), "www.")
	if host == "" {
		return ""
	}
	if d, err := publicsuffix.EffectiveTLDPlusOne(host); err == nil {
		return d
	}
	return host
}

// LoadLedger reads the ledger; a missing file is an empty ledger.
func LoadLedger(path string) (Ledger, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return Ledger{}, nil
	}
	if err != nil {
		return Ledger{}, err
	}
	l := Ledger{}
	if err := json.Unmarshal(b, &l); err != nil {
		return Ledger{}, err
	}
	return l, nil
}

// Save writes the ledger atomically.
func (l Ledger) Save(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(l, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// Record folds one fetch into the domain's entry and reports whether the
// ledger changed. Content from a method rung wins the domain; content from a
// lookup (hister, karakeep-lookup) is a copy of that one page, not a method,
// so it leaves the ledger alone. No content after a method rung ran counts
// a failure.
func (l Ledger) Record(domain string, res Result, now time.Time) bool {
	if res.Kind == decide.PageContent {
		if indexOf(DefaultOrder, res.Rung) < 0 {
			return false
		}
		e := l[domain]
		e.Rung, e.Kind, e.LastVia = res.Rung, res.Kind, res.Via
		e.OKCount++
		e.LastOK = now.UTC().Format(time.RFC3339)
		l[domain] = e
		return true
	}
	ran := false // a --max-rung cut before any method is not a domain failure
	for _, t := range res.Tried {
		if t.Kind != "skipped" && indexOf(DefaultOrder, t.Rung) >= 0 {
			ran = true
		}
	}
	if !ran {
		return false
	}
	e := l[domain]
	e.FailCount++
	l[domain] = e
	return true
}
