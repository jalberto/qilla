// Package gmail is the staging ledger and the trash applier behind
// `qilla gmail`: a routine stages message ids while it reads, a later run
// applies them (move to TRASH) with the Gmail API. Nothing here sends mail.
package gmail

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	gmailapi "google.golang.org/api/gmail/v1"
)

// Entry is one staged (or applied) Gmail action: trash this message.
type Entry struct {
	Account    string `json:"account"`
	ID         string `json:"id"`
	MsgvaultID int64  `json:"msgvault_id,omitempty"`
	StagedAt   string `json:"staged_at"`
	Note       string `json:"note,omitempty"`
	Attempts   int    `json:"attempts,omitempty"`
	Error      string `json:"error,omitempty"`
	AppliedAt  string `json:"applied_at,omitempty"`
}

// Key is the idempotency key of an entry.
func (e Entry) Key() string { return e.Account + "\x00" + e.ID }

// AppliedPath is the applied ledger next to the stage file.
func AppliedPath(stage string) string {
	return filepath.Join(filepath.Dir(stage), "gmail-applied.jsonl")
}

// Read loads a JSONL ledger; a missing file is an empty ledger.
func Read(path string) ([]Entry, error) {
	f, err := os.Open(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	defer f.Close()
	var out []Entry
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var e Entry
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
		out = append(out, e)
	}
	return out, sc.Err()
}

// Write replaces a JSONL ledger atomically.
func Write(path string, es []Entry) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	var b strings.Builder
	for _, e := range es {
		j, err := json.Marshal(e)
		if err != nil {
			return err
		}
		b.Write(j)
		b.WriteByte('\n')
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(b.String()), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// Append adds entries to a JSONL ledger, creating it if needed.
func Append(path string, es []Entry) error {
	if len(es) == 0 {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	for _, e := range es {
		j, err := json.Marshal(e)
		if err != nil {
			return err
		}
		if _, err := f.Write(append(j, '\n')); err != nil {
			return err
		}
	}
	return nil
}

// Stage appends entries that are not already pending; it reports how many were
// new. Idempotent on (account, id).
func Stage(path string, es []Entry) (int, error) {
	cur, err := Read(path)
	if err != nil {
		return 0, err
	}
	seen := map[string]bool{}
	for _, e := range cur {
		seen[e.Key()] = true
	}
	var add []Entry
	for _, e := range es {
		if e.Account == "" || e.ID == "" {
			return 0, fmt.Errorf("stage: account and id required")
		}
		if seen[e.Key()] {
			continue
		}
		seen[e.Key()] = true
		add = append(add, e)
	}
	return len(add), Append(path, add)
}

// ErrStale is `apply --only-if-run-ok` refusing: the reading run is older than
// what is staged, so the note is not yet the receipt for these messages.
var ErrStale = errors.New("stale run")

// Gate refuses when the newest ok run of the reading routine is missing or
// older than the newest staged entry.
func Gate(routine string, last time.Time, ok bool, es []Entry) error {
	if !ok {
		return fmt.Errorf("%w: routine %s never ran ok", ErrStale, routine)
	}
	var newest time.Time
	for _, e := range es {
		t, err := time.Parse(time.RFC3339, e.StagedAt)
		if err != nil {
			continue
		}
		if t.After(newest) {
			newest = t
		}
	}
	if newest.IsZero() {
		return nil
	}
	if last.Before(newest) {
		return fmt.Errorf("%w: newest ok %s run %s is older than the newest staged entry %s",
			ErrStale, routine, last.Format(time.RFC3339), newest.Format(time.RFC3339))
	}
	return nil
}

// Result is the one JSON line `qilla gmail apply` prints.
type Result struct {
	Applied int     `json:"applied"`
	Failed  []Entry `json:"failed"`
	Skipped int     `json:"skipped"`
}

// Options drive Apply. NewService is injectable so tests can hand back a Gmail
// service built on a fake RoundTripper.
type Options struct {
	Stage      string
	Applied    string
	Account    string // only this account; empty = all
	DryRun     bool
	Now        func() time.Time
	NewService func(ctx context.Context, account string) (*gmailapi.Service, error)
}

const chunk = 100

// Apply moves every pending entry to TRASH, per account, in chunks. Applied
// entries move to the applied ledger; failed ones stay staged with attempts++.
func Apply(ctx context.Context, o Options) (Result, error) {
	res := Result{Failed: []Entry{}}
	now := o.Now
	if now == nil {
		now = time.Now
	}
	pending, err := Read(o.Stage)
	if err != nil {
		return res, err
	}
	var keep, todo []Entry
	for _, e := range pending {
		if o.Account != "" && e.Account != o.Account {
			keep = append(keep, e)
			res.Skipped++
			continue
		}
		todo = append(todo, e)
	}
	if o.DryRun {
		res.Applied = len(todo)
		return res, nil
	}
	accounts := map[string][]Entry{}
	var order []string
	for _, e := range todo {
		if _, ok := accounts[e.Account]; !ok {
			order = append(order, e.Account)
		}
		accounts[e.Account] = append(accounts[e.Account], e)
	}
	sort.Strings(order)
	var applied []Entry
	stamp := now().UTC().Format(time.RFC3339)
	for _, acct := range order {
		es := accounts[acct]
		svc, err := o.NewService(ctx, acct)
		if err != nil {
			keep = append(keep, fail(es, err)...)
			res.Failed = append(res.Failed, fail(es, err)...)
			continue
		}
		for i := 0; i < len(es); i += chunk {
			batch := es[i:min(i+chunk, len(es))]
			ids := make([]string, len(batch))
			for j, e := range batch {
				ids[j] = e.ID
			}
			req := &gmailapi.BatchModifyMessagesRequest{
				Ids:            ids,
				AddLabelIds:    []string{"TRASH"},
				RemoveLabelIds: []string{"INBOX", "UNREAD"},
			}
			if err := svc.Users.Messages.BatchModify("me", req).Context(ctx).Do(); err != nil {
				keep = append(keep, fail(batch, err)...)
				res.Failed = append(res.Failed, fail(batch, err)...)
				continue
			}
			for _, e := range batch {
				e.AppliedAt = stamp
				e.Error = ""
				applied = append(applied, e)
			}
			res.Applied += len(batch)
		}
	}
	if err := Append(o.Applied, applied); err != nil {
		return res, err
	}
	return res, Write(o.Stage, keep)
}

func fail(es []Entry, err error) []Entry {
	out := make([]Entry, 0, len(es))
	for _, e := range es {
		e.Attempts++
		e.Error = firstLine(err.Error())
		out = append(out, e)
	}
	return out
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

// Untrash puts one message back in the inbox.
func Untrash(ctx context.Context, svc *gmailapi.Service, id string) error {
	_, err := svc.Users.Messages.Untrash("me", id).Context(ctx).Do()
	return err
}

// ProfileEmail is the address the token actually belongs to.
func ProfileEmail(ctx context.Context, svc *gmailapi.Service) (string, error) {
	p, err := svc.Users.GetProfile("me").Context(ctx).Do()
	if err != nil {
		return "", err
	}
	return p.EmailAddress, nil
}
