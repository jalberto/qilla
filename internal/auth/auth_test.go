package auth

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

func guard(t *testing.T) (*Guard, http.Handler) {
	t.Helper()
	h, err := Hash("secreto")
	if err != nil {
		t.Fatal(err)
	}
	g := New(h, false)
	ok := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("in")) })
	return g, g.Wrap(ok)
}

func post(h http.Handler, pw, ip string) *httptest.ResponseRecorder {
	form := url.Values{"password": {pw}}
	r := httptest.NewRequest("POST", "/login", strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.RemoteAddr = ip + ":1234"
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func TestNoCookieIs401(t *testing.T) {
	_, h := guard(t)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "/api/jobs", nil))
	if w.Code != 401 {
		t.Fatalf("api without cookie: %d", w.Code)
	}
	w = httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "/", nil))
	if w.Code != 401 || !strings.Contains(w.Body.String(), "<form") {
		t.Fatalf("page without cookie shows login form: %d", w.Code)
	}
}

func TestLoginSetsCookieAndGrantsAccess(t *testing.T) {
	_, h := guard(t)
	w := post(h, "secreto", "10.0.0.1")
	if w.Code != 303 || len(w.Result().Cookies()) != 1 {
		t.Fatalf("login: %d cookies %d", w.Code, len(w.Result().Cookies()))
	}
	c := w.Result().Cookies()[0]
	if !c.HttpOnly {
		t.Fatal("cookie must be HttpOnly")
	}
	r := httptest.NewRequest("GET", "/api/jobs", nil)
	r.AddCookie(c)
	w = httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 200 || w.Body.String() != "in" {
		t.Fatalf("with cookie: %d %s", w.Code, w.Body.String())
	}
}

func TestWrongPasswordAndRateLimit(t *testing.T) {
	g, h := guard(t)
	for i := 0; i < 5; i++ {
		if w := post(h, "nope", "10.0.0.2"); w.Code != 401 {
			t.Fatalf("attempt %d: %d", i, w.Code)
		}
	}
	if w := post(h, "secreto", "10.0.0.2"); w.Code != 429 {
		t.Fatalf("6th attempt must be rate limited even with the right password: %d", w.Code)
	}
	if w := post(h, "secreto", "10.0.0.3"); w.Code != 303 {
		t.Fatalf("other IP unaffected: %d", w.Code)
	}
	g.Now = func() time.Time { return time.Now().Add(16 * time.Minute) }
	if w := post(h, "secreto", "10.0.0.2"); w.Code != 303 {
		t.Fatalf("window passed, login works: %d", w.Code)
	}
}

func TestExpiredSession(t *testing.T) {
	g, h := guard(t)
	w := post(h, "secreto", "10.0.0.4")
	c := w.Result().Cookies()[0]
	g.Now = func() time.Time { return time.Now().Add(31 * 24 * time.Hour) }
	r := httptest.NewRequest("GET", "/api/jobs", nil)
	r.AddCookie(c)
	w = httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 401 {
		t.Fatalf("expired cookie: %d", w.Code)
	}
}

func TestDisabledWhenNoHash(t *testing.T) {
	g := New("", false)
	h := g.Wrap(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("in")) }))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "/api/jobs", nil))
	if w.Code != 200 {
		t.Fatal("no hash → open (loopback only, enforced by serve)")
	}
}

func TestIsLoopback(t *testing.T) {
	for addr, want := range map[string]bool{"127.0.0.1:7433": true, "localhost:7433": true, "[::1]:7433": true, "0.0.0.0:7433": false, "100.64.0.1:7433": false} {
		if IsLoopback(addr) != want {
			t.Errorf("%s: want %v", addr, want)
		}
	}
}
