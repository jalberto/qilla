package main

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"golang.org/x/oauth2"
	gmailapi "google.golang.org/api/gmail/v1"
	"google.golang.org/api/option"

	"github.com/jalberto/qilla/internal/config"
	"github.com/jalberto/qilla/internal/gmail"
	"github.com/jalberto/qilla/internal/install"
	"github.com/jalberto/qilla/internal/ledger"
	"github.com/jalberto/qilla/internal/queue"
)

const gmailUsage = `usage: qilla gmail <command>

  auth <account>                          loopback OAuth (interactive); token -> secret gmail-<account>
  stage --account A --id ID [--msgvault-id N] [--note "…"]
  stage --stdin                           one JSON object per line
  staged [--ids] [--account A] [--json]   pending entries
  apply [--account A] [--only-if-run-ok R] [--dry-run] [--json]
  untrash --account A --id ID`

// exitErr carries a non-1 exit code out of cmdGmail (2 = refused, not broken).
type exitErr struct {
	code int
	err  error
}

func (e exitErr) Error() string { return e.err.Error() }
func (e exitErr) Unwrap() error { return e.err }

// exitCode is the status a failed command exits with (1 unless it asked for more).
func exitCode(err error) int {
	var e exitErr
	if errors.As(err, &e) {
		return e.code
	}
	return 1
}

// cmdGmail: qilla gmail — the staging ledger and the trash applier. The only
// mutating verbs are apply and untrash; nothing here ever sends mail.
func cmdGmail(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("%s", gmailUsage)
	}
	cfg, err := config.Load(config.DefaultPath())
	if err != nil {
		return err
	}
	ctx := context.Background()
	switch args[0] {
	case "auth":
		return gmailAuth(ctx, cfg, args[1:])
	case "stage":
		return gmailStage(cfg, args[1:])
	case "staged":
		return gmailStaged(cfg, args[1:])
	case "apply":
		return gmailApply(ctx, cfg, args[1:])
	case "untrash":
		return gmailUntrash(ctx, cfg, args[1:])
	}
	return fmt.Errorf("unknown gmail command %q\n%s", args[0], gmailUsage)
}

// --- secrets -----------------------------------------------------------

var secretSanitize = regexp.MustCompile(`[^a-z0-9_-]+`)

// secretName is the `qilla secret` name holding an account's OAuth token.
func secretName(account string) string {
	return "gmail-" + strings.Trim(secretSanitize.ReplaceAllString(strings.ToLower(account), "-"), "-")
}

func credsDir() string { return install.CredsDir(filepath.Dir(config.DefaultPath())) }

// readSecret prefers the decrypted copy systemd hands the unit, and falls back
// to decrypting the stored credential itself.
func readSecret(name string) ([]byte, error) {
	for _, env := range []string{"QILLA_SECRETS_DIR", "CREDENTIALS_DIRECTORY"} {
		if d := os.Getenv(env); d != "" {
			if b, err := os.ReadFile(filepath.Join(d, name)); err == nil {
				return b, nil
			}
		}
	}
	p := filepath.Join(credsDir(), name+".cred")
	if _, err := os.Stat(p); err != nil {
		return nil, fmt.Errorf("secret %s not found — run `qilla gmail auth <account>`", name)
	}
	out, err := exec.Command("systemd-creds", "decrypt", "--user", "--name="+name, p, "-").Output()
	if err != nil {
		return nil, fmt.Errorf("systemd-creds decrypt %s: %w", name, err)
	}
	return out, nil
}

// writeSecret stores (or replaces) a secret with the same encryption
// `qilla secret set` uses.
func writeSecret(name string, val []byte) error {
	dir := credsDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	out := filepath.Join(dir, name+".cred")
	cmd := exec.Command("systemd-creds", "encrypt", "--user", "--name="+name, "-", out)
	cmd.Stdin = strings.NewReader(strings.TrimRight(string(val), "\r\n"))
	if b, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("systemd-creds encrypt: %v: %s", err, strings.TrimSpace(string(b)))
	}
	return os.Chmod(out, 0o600)
}

// --- client ------------------------------------------------------------

func oauthConfig(cfg *config.Config) (*oauth2.Config, error) {
	if cfg.Gmail.ClientSecrets == "" {
		return nil, fmt.Errorf("[gmail].client_secrets not set in %s", cfg.Path)
	}
	b, err := os.ReadFile(cfg.Gmail.ClientSecrets)
	if err != nil {
		return nil, err
	}
	return gmail.ParseClientSecrets(b)
}

// service builds a Gmail client for an account from its stored token,
// refreshing and persisting the token when it has expired.
func service(ctx context.Context, cfg *config.Config, account string) (*gmailapi.Service, error) {
	oc, err := oauthConfig(cfg)
	if err != nil {
		return nil, err
	}
	name := secretName(account)
	raw, err := readSecret(name)
	if err != nil {
		return nil, err
	}
	var tok oauth2.Token
	if err := json.Unmarshal(raw, &tok); err != nil {
		return nil, fmt.Errorf("secret %s: %w", name, err)
	}
	src := gmail.TokenSource(ctx, oc, &tok, func(t *oauth2.Token) error {
		b, err := json.Marshal(t)
		if err != nil {
			return err
		}
		return writeSecret(name, b)
	})
	return gmailapi.NewService(ctx, option.WithTokenSource(src))
}

// --- auth --------------------------------------------------------------

func gmailAuth(ctx context.Context, cfg *config.Config, args []string) error {
	if len(args) != 1 {
		return fmt.Errorf("usage: qilla gmail auth <account>")
	}
	account := args[0]
	oc, err := oauthConfig(cfg)
	if err != nil {
		return err
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return err
	}
	defer ln.Close()
	oc.RedirectURL = fmt.Sprintf("http://127.0.0.1:%d/", ln.Addr().(*net.TCPAddr).Port)
	buf := make([]byte, 16)
	rand.Read(buf)
	state := base64.RawURLEncoding.EncodeToString(buf)

	type result struct {
		code string
		err  error
	}
	res := make(chan result, 1)
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if q.Get("state") != state {
			http.Error(w, "state mismatch", http.StatusBadRequest)
			res <- result{err: errors.New("state mismatch")}
			return
		}
		if e := q.Get("error"); e != "" {
			http.Error(w, e, http.StatusBadRequest)
			res <- result{err: errors.New(e)}
			return
		}
		fmt.Fprintln(w, "qilla: authorised, you can close this tab.")
		res <- result{code: q.Get("code")}
	})}
	go srv.Serve(ln)
	defer srv.Close()

	fmt.Fprintf(os.Stderr, "open this URL as %s:\n%s\n", account,
		oc.AuthCodeURL(state, oauth2.AccessTypeOffline, oauth2.ApprovalForce))
	var r result
	select {
	case r = <-res:
	case <-time.After(5 * time.Minute):
		return errors.New("timed out waiting for the browser callback")
	}
	if r.err != nil {
		return r.err
	}
	tok, err := oc.Exchange(ctx, r.code)
	if err != nil {
		return err
	}
	svc, err := gmailapi.NewService(ctx, option.WithTokenSource(oauth2.StaticTokenSource(tok)))
	if err != nil {
		return err
	}
	got, err := gmail.ProfileEmail(ctx, svc)
	if err != nil {
		return err
	}
	if !strings.EqualFold(got, account) {
		return fmt.Errorf("authorised %s, expected %s — sign in with the right account", got, account)
	}
	if tok.RefreshToken == "" {
		return errors.New("no refresh token returned — revoke qilla's access at myaccount.google.com and retry")
	}
	b, err := json.Marshal(tok)
	if err != nil {
		return err
	}
	if err := writeSecret(secretName(account), b); err != nil {
		return err
	}
	fmt.Printf("stored %s — run `qilla init`, then `systemctl --user restart qilla.service`\n", secretName(account))
	return nil
}

// --- stage / staged ----------------------------------------------------

func gmailStage(cfg *config.Config, args []string) error {
	fs := flag.NewFlagSet("gmail stage", flag.ContinueOnError)
	account := fs.String("account", "", "account the message belongs to")
	id := fs.String("id", "", "gmail message id")
	mvID := fs.Int64("msgvault-id", 0, "msgvault row id")
	note := fs.String("note", "", "why")
	stdin := fs.Bool("stdin", false, "read JSON lines from stdin instead")
	if err := fs.Parse(args); err != nil {
		return err
	}
	now := time.Now().UTC().Format(time.RFC3339)
	var es []gmail.Entry
	if *stdin {
		dec := json.NewDecoder(os.Stdin)
		for {
			var e gmail.Entry
			if err := dec.Decode(&e); err != nil {
				if errors.Is(err, io.EOF) {
					break
				}
				return err
			}
			if e.StagedAt == "" {
				e.StagedAt = now
			}
			es = append(es, e)
		}
	} else {
		if *account == "" || *id == "" {
			return fmt.Errorf("usage: qilla gmail stage --account A --id ID [--msgvault-id N] [--note \"…\"]")
		}
		es = []gmail.Entry{{Account: *account, ID: *id, MsgvaultID: *mvID, Note: *note, StagedAt: now}}
	}
	n, err := gmail.Stage(cfg.Gmail.StageFile, es)
	if err != nil {
		return err
	}
	fmt.Printf("{\"staged\":%d,\"duplicate\":%d}\n", n, len(es)-n)
	return nil
}

func gmailStaged(cfg *config.Config, args []string) error {
	fs := flag.NewFlagSet("gmail staged", flag.ContinueOnError)
	ids := fs.Bool("ids", false, "msgvault ids (or gmail ids) space-separated on one line")
	account := fs.String("account", "", "only this account")
	asJSON := fs.Bool("json", false, "JSON array")
	if err := fs.Parse(args); err != nil {
		return err
	}
	es, err := gmail.Read(cfg.Gmail.StageFile)
	if err != nil {
		return err
	}
	var sel []gmail.Entry
	for _, e := range es {
		if *account == "" || e.Account == *account {
			sel = append(sel, e)
		}
	}
	switch {
	case *ids:
		out := make([]string, 0, len(sel))
		for _, e := range sel {
			if e.MsgvaultID != 0 {
				out = append(out, fmt.Sprint(e.MsgvaultID))
			} else {
				out = append(out, e.ID)
			}
		}
		if len(out) > 0 {
			fmt.Println(strings.Join(out, " "))
		}
	case *asJSON:
		if sel == nil {
			sel = []gmail.Entry{}
		}
		b, err := json.Marshal(sel)
		if err != nil {
			return err
		}
		fmt.Println(string(b))
	default:
		for _, e := range sel {
			fmt.Printf("%s\t%s\t%d\t%s\t%s\n", e.StagedAt, e.Account, e.MsgvaultID, e.ID, e.Note)
		}
	}
	return nil
}

// --- apply / untrash ---------------------------------------------------

func gmailApply(ctx context.Context, cfg *config.Config, args []string) error {
	fs := flag.NewFlagSet("gmail apply", flag.ContinueOnError)
	account := fs.String("account", "", "only this account")
	onlyIf := fs.String("only-if-run-ok", "", "refuse unless this routine's newest ok run is newer than the newest staged entry")
	dry := fs.Bool("dry-run", false, "count what would be applied, touch nothing")
	fs.Bool("json", true, "output is always one JSON line")
	if err := fs.Parse(args); err != nil {
		return err
	}
	stage := cfg.Gmail.StageFile
	if *onlyIf != "" {
		es, err := gmail.Read(stage)
		if err != nil {
			return err
		}
		last, ok, err := lastOKRun(ctx, cfg, *onlyIf)
		if err != nil {
			return err
		}
		if err := gmail.Gate(*onlyIf, last, ok, es); err != nil {
			return exitErr{2, err}
		}
	}
	res, err := gmail.Apply(ctx, gmail.Options{
		Stage: stage, Applied: gmail.AppliedPath(stage), Account: *account, DryRun: *dry,
		NewService: func(ctx context.Context, acct string) (*gmailapi.Service, error) {
			return service(ctx, cfg, acct)
		},
	})
	if err != nil {
		return err
	}
	b, err := json.Marshal(res)
	if err != nil {
		return err
	}
	fmt.Println(string(b))
	if len(res.Failed) > 0 {
		return exitErr{1, fmt.Errorf("%d staged action(s) failed, kept for the next slot", len(res.Failed))}
	}
	return nil
}

// lastOKRun is `qilla runs last <routine>` without the formatting.
func lastOKRun(ctx context.Context, cfg *config.Config, routine string) (time.Time, bool, error) {
	q, err := queue.Open(cfg.DBPath())
	if err != nil {
		return time.Time{}, false, err
	}
	defer q.Close()
	if _, err := ledger.New(q.DB(), nil); err != nil {
		return time.Time{}, false, err
	}
	return lastOK(ctx, q.DB(), routine)
}

func gmailUntrash(ctx context.Context, cfg *config.Config, args []string) error {
	fs := flag.NewFlagSet("gmail untrash", flag.ContinueOnError)
	account := fs.String("account", "", "account the message belongs to")
	id := fs.String("id", "", "gmail message id")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *account == "" || *id == "" {
		return fmt.Errorf("usage: qilla gmail untrash --account A --id ID")
	}
	svc, err := service(ctx, cfg, *account)
	if err != nil {
		return err
	}
	if err := gmail.Untrash(ctx, svc, *id); err != nil {
		return err
	}
	fmt.Printf("{\"untrashed\":\"%s\"}\n", *id)
	return nil
}
