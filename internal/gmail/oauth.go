package gmail

import (
	"context"
	"encoding/json"
	"fmt"

	"golang.org/x/oauth2"
	gmailapi "google.golang.org/api/gmail/v1"
)

// Scope is the only scope qilla asks for: read + modify labels, no send.
const Scope = gmailapi.GmailModifyScope

// clientSecrets is the installed-app OAuth client JSON Google Cloud exports.
type clientSecrets struct {
	Installed *clientBlock `json:"installed"`
	Web       *clientBlock `json:"web"`
}

type clientBlock struct {
	ClientID     string `json:"client_id"`
	ClientSecret string `json:"client_secret"`
	AuthURI      string `json:"auth_uri"`
	TokenURI     string `json:"token_uri"`
}

// ParseClientSecrets turns a Google client_secret_*.json into an oauth2 config
// (redirect URL is set per run by the loopback listener).
func ParseClientSecrets(b []byte) (*oauth2.Config, error) {
	var cs clientSecrets
	if err := json.Unmarshal(b, &cs); err != nil {
		return nil, fmt.Errorf("client secrets: %w", err)
	}
	blk := cs.Installed
	if blk == nil {
		blk = cs.Web
	}
	if blk == nil || blk.ClientID == "" {
		return nil, fmt.Errorf("client secrets: no installed/web block with a client_id")
	}
	if blk.AuthURI == "" {
		blk.AuthURI = "https://accounts.google.com/o/oauth2/auth"
	}
	if blk.TokenURI == "" {
		blk.TokenURI = "https://oauth2.googleapis.com/token"
	}
	return &oauth2.Config{
		ClientID:     blk.ClientID,
		ClientSecret: blk.ClientSecret,
		Endpoint:     oauth2.Endpoint{AuthURL: blk.AuthURI, TokenURL: blk.TokenURI},
		Scopes:       []string{Scope},
	}, nil
}

// persisting wraps a token source and writes every new token back through save,
// so a refresh is never lost.
type persisting struct {
	src  oauth2.TokenSource
	last string
	save func(*oauth2.Token) error
}

func (p *persisting) Token() (*oauth2.Token, error) {
	t, err := p.src.Token()
	if err != nil {
		return nil, err
	}
	if t.AccessToken != p.last {
		p.last = t.AccessToken
		if err := p.save(t); err != nil {
			return nil, fmt.Errorf("persist refreshed token: %w", err)
		}
	}
	return t, nil
}

// TokenSource refreshes tok when needed and persists the refreshed token.
func TokenSource(ctx context.Context, cfg *oauth2.Config, tok *oauth2.Token, save func(*oauth2.Token) error) oauth2.TokenSource {
	return oauth2.ReuseTokenSource(nil, &persisting{src: cfg.TokenSource(ctx, tok), last: tok.AccessToken, save: save})
}
