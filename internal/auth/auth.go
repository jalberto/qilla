// Package auth protects the web UI: one password (bcrypt hash in config),
// a signed session cookie, and a small login rate limit per IP.
package auth

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/bcrypt"
)

const (
	cookieName  = "qilla_session"
	sessionTTL  = 30 * 24 * time.Hour
	maxFailures = 5
	failWindow  = 15 * time.Minute
)

// Hash bcrypts a password for qilla.toml.
func Hash(password string) (string, error) {
	b, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	return string(b), err
}

// Guard is the middleware state.
type Guard struct {
	Hash   string // bcrypt hash; empty = auth disabled (loopback only)
	Secure bool   // set Secure on the cookie (behind TLS / Tailscale serve)
	Now    func() time.Time

	key   []byte // HMAC key for session cookies, per process
	mu    sync.Mutex
	fails map[string][]time.Time
}

// New creates a guard with an ephemeral signing key (restarting serve logs
// everyone out). Prefer NewPersistent so idle-exit/redeploy restarts don't.
func New(hash string, secure bool) *Guard {
	key := make([]byte, 32)
	rand.Read(key)
	return &Guard{Hash: hash, Secure: secure, Now: time.Now, key: key, fails: map[string][]time.Time{}}
}

// NewPersistent creates a guard whose signing key is stored at keyPath (created
// 0600 on first use) so sessions survive process restarts — qilla.service is
// socket-activated and exits on idle, so restarts are routine, not rare.
func NewPersistent(hash string, secure bool, keyPath string) (*Guard, error) {
	key, err := loadOrCreateKey(keyPath)
	if err != nil {
		return nil, err
	}
	return &Guard{Hash: hash, Secure: secure, Now: time.Now, key: key, fails: map[string][]time.Time{}}, nil
}

func loadOrCreateKey(path string) ([]byte, error) {
	if b, err := os.ReadFile(path); err == nil && len(b) == 32 {
		return b, nil
	}
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	if err := os.WriteFile(path, key, 0o600); err != nil {
		return nil, err
	}
	return key, nil
}

// Enabled reports whether a password is configured.
func (g *Guard) Enabled() bool { return g.Hash != "" }

// Wrap requires a valid session for everything except the login endpoints
// and /healthz. Unauthenticated API calls get 401; page loads get the login form.
func (g *Guard) Wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !g.Enabled() || r.URL.Path == "/healthz" || r.URL.Path == "/manifest.json" || r.URL.Path == "/icon.svg" {
			next.ServeHTTP(w, r)
			return
		}
		switch {
		case r.URL.Path == "/login" && r.Method == http.MethodPost:
			g.login(w, r)
			return
		case r.URL.Path == "/logout":
			http.SetCookie(w, &http.Cookie{Name: cookieName, Value: "", Path: "/", MaxAge: -1})
			http.Redirect(w, r, "/", http.StatusSeeOther)
			return
		}
		if g.valid(r) {
			next.ServeHTTP(w, r)
			return
		}
		if strings.HasPrefix(r.URL.Path, "/api/") {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(loginPage))
	})
}

func (g *Guard) login(w http.ResponseWriter, r *http.Request) {
	ip := clientIP(r)
	if g.tooMany(ip) {
		http.Error(w, "too many attempts, try later", http.StatusTooManyRequests)
		return
	}
	r.ParseForm()
	pw := r.Form.Get("password")
	if bcrypt.CompareHashAndPassword([]byte(g.Hash), []byte(pw)) != nil {
		g.fail(ip)
		http.Error(w, "wrong password", http.StatusUnauthorized)
		return
	}
	g.clear(ip)
	exp := g.Now().Add(sessionTTL)
	http.SetCookie(w, &http.Cookie{Name: cookieName, Value: g.token(exp), Path: "/", Expires: exp,
		HttpOnly: true, Secure: g.Secure, SameSite: http.SameSiteLaxMode})
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// token is "<unix-expiry>.<hmac>".
func (g *Guard) token(exp time.Time) string {
	e := strconv.FormatInt(exp.Unix(), 10)
	return e + "." + g.sign(e)
}

func (g *Guard) sign(s string) string {
	m := hmac.New(sha256.New, g.key)
	m.Write([]byte(s))
	return base64.RawURLEncoding.EncodeToString(m.Sum(nil))
}

func (g *Guard) valid(r *http.Request) bool {
	c, err := r.Cookie(cookieName)
	if err != nil {
		return false
	}
	e, sig, ok := strings.Cut(c.Value, ".")
	if !ok || !hmac.Equal([]byte(sig), []byte(g.sign(e))) {
		return false
	}
	exp, err := strconv.ParseInt(e, 10, 64)
	return err == nil && g.Now().Unix() < exp
}

func (g *Guard) tooMany(ip string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	cut := g.Now().Add(-failWindow)
	var keep []time.Time
	for _, t := range g.fails[ip] {
		if t.After(cut) {
			keep = append(keep, t)
		}
	}
	g.fails[ip] = keep
	return len(keep) >= maxFailures
}

func (g *Guard) fail(ip string) {
	g.mu.Lock()
	g.fails[ip] = append(g.fails[ip], g.Now())
	g.mu.Unlock()
}

func (g *Guard) clear(ip string) {
	g.mu.Lock()
	delete(g.fails, ip)
	g.mu.Unlock()
}

func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// IsLoopback reports whether a listen address is loopback-only.
func IsLoopback(listen string) bool {
	host, _, err := net.SplitHostPort(listen)
	if err != nil {
		return false
	}
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// RandomID is a short hex id (tests, cookies).
func RandomID() string {
	b := make([]byte, 8)
	rand.Read(b)
	return hex.EncodeToString(b)
}

const loginPage = `<!doctype html><html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><title>qilla · login</title>
<style>body{font:16px system-ui;display:grid;place-items:center;height:100vh;margin:0}form{display:grid;gap:.6rem;width:16rem}input,button{font:inherit;padding:.5rem}</style></head>
<body><form method="post" action="/login"><h1 style="font-size:1.1rem;margin:0">qilla</h1><input type="password" name="password" placeholder="password" autofocus required><button>enter</button></form></body></html>`
