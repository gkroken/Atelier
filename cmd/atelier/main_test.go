package main

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gkroken/Atelier/internal/backend"
	"github.com/gkroken/Atelier/internal/launcher"
)

func TestLoopbackOnly(t *testing.T) {
	for _, ok := range []string{"127.0.0.1:8080", "localhost:8080", "[::1]:8080", "127.0.0.2:0"} {
		if err := loopbackOnly(ok); err != nil {
			t.Errorf("%s refused: %v", ok, err)
		}
	}
	// Without sign-in, any of these would let other machines act as the account.
	for _, bad := range []string{":8080", "0.0.0.0:8080", "[::]:8080", "192.168.1.5:8080", "example.com:8080", "8080"} {
		if err := loopbackOnly(bad); err == nil {
			t.Errorf("%s accepted", bad)
		}
	}
}

// State-changing requests from another site (or with no sign of where they come from)
// are refused, so a web page can't start or quit sessions through the user's browser.
func TestSameOriginRefusesCrossSiteWrites(t *testing.T) {
	ran := false
	h := sameOrigin(func(w http.ResponseWriter, r *http.Request) { ran = true })
	for name, c := range map[string]struct {
		headers map[string]string
		allow   bool
	}{
		"same-origin fetch metadata": {map[string]string{"Sec-Fetch-Site": "same-origin"}, true},
		"matching Origin":            {map[string]string{"Origin": "http://127.0.0.1:8080"}, true},
		"cross-site":                 {map[string]string{"Sec-Fetch-Site": "cross-site", "Origin": "http://127.0.0.1:8080"}, false},
		"same-site":                  {map[string]string{"Sec-Fetch-Site": "same-site"}, false},
		"none (typed URL)":           {map[string]string{"Sec-Fetch-Site": "none"}, false},
		"other Origin":               {map[string]string{"Origin": "https://evil.example"}, false},
		"other port":                 {map[string]string{"Origin": "http://127.0.0.1:9999"}, false},
		"null Origin":                {map[string]string{"Origin": "null"}, false},
		"no headers":                 {nil, false},
	} {
		t.Run(name, func(t *testing.T) {
			ran = false
			r := httptest.NewRequest("POST", "http://127.0.0.1:8080/start", nil)
			for k, v := range c.headers {
				r.Header.Set(k, v)
			}
			w := httptest.NewRecorder()
			h(w, r)
			if ran != c.allow {
				t.Errorf("handler ran = %v, want %v (status %d)", ran, c.allow, w.Code)
			}
			if !c.allow && w.Code != http.StatusForbidden {
				t.Errorf("status %d, want 403", w.Code)
			}
		})
	}
}

// countingBackend refuses everything but counts Create calls, to show a refused request
// never got as far as making a container.
type countingBackend struct{ creates int }

func (c *countingBackend) Create(context.Context, backend.Spec) (string, error) {
	c.creates++
	return "", errors.New("test backend")
}
func (c *countingBackend) Start(context.Context, string) error               { return nil }
func (c *countingBackend) Stop(context.Context, string, time.Duration) error { return nil }
func (c *countingBackend) Remove(context.Context, string) error              { return nil }
func (c *countingBackend) Inspect(context.Context, string) (backend.State, error) {
	return backend.State{}, backend.ErrNotFound
}

const testToken = "0123456789abcdef0123456789abcdef0123456789abcdef"

func testHandler(t *testing.T) (http.Handler, *countingBackend) {
	be := &countingBackend{}
	acct := launcher.Account{Name: "alice", UID: 4321, GID: 4321, Home: "/home/alice"}
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	l := launcher.New(be, func(*http.Request) (string, bool) { return acct.Name, true }, launcher.Options{Logger: quiet})
	return newHandler(l, acct, testToken, "18080", quiet), be
}

func do(h http.Handler, method, target, host string, headers map[string]string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, target, strings.NewReader("ide=rstudio"))
	r.Host = host
	if method == "POST" {
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	for k, v := range headers {
		r.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

// DNS rebinding: a page on evil.example, re-pointed at 127.0.0.1, is same-origin with
// itself and even holds the token cookie in this test. Every route is still refused,
// because the Host isn't the spike's, and no container is made (atelier-bfa99dc-F1).
func TestRefusesForeignHostEvenSameOriginWithToken(t *testing.T) {
	h, be := testHandler(t)
	evil := "evil.example:18080"
	auth := map[string]string{
		"Cookie":         cookieName + "=" + testToken,
		"Sec-Fetch-Site": "same-origin",
		"Origin":         "http://" + evil,
	}
	for _, c := range []struct{ method, path string }{
		{"GET", "/"}, {"POST", "/start"}, {"POST", "/quit"},
		{"GET", "/s/abcdefghijklmnopqrstuvwxyz/"}, {"GET", "/?token=" + testToken},
	} {
		for _, host := range []string{evil, "127.0.0.1:9999", "127.0.0.1", "0.0.0.0:18080", ""} {
			if w := do(h, c.method, c.path, host, auth); w.Code != http.StatusMisdirectedRequest {
				t.Errorf("%s %s Host=%q: %d, want 421", c.method, c.path, host, w.Code)
			}
		}
	}
	if be.creates != 0 {
		t.Errorf("%d containers were requested", be.creates)
	}
}

// Without the token, nothing works even from the right host: another account on the
// machine can connect to loopback but doesn't have it (atelier-bfa99dc-N1).
func TestRequiresToken(t *testing.T) {
	h, be := testHandler(t)
	same := map[string]string{"Sec-Fetch-Site": "same-origin"}
	for _, c := range []struct {
		method, path string
		headers      map[string]string
	}{
		{"GET", "/", nil},
		{"POST", "/start", same},
		{"GET", "/s/abcdefghijklmnopqrstuvwxyz/", nil},
		{"GET", "/?token=wrong", nil},
		{"GET", "/", map[string]string{"Cookie": cookieName + "=wrong"}},
		{"GET", "/", map[string]string{"Cookie": cookieName + "=" + testToken[:47]}},
	} {
		if w := do(h, c.method, c.path, "127.0.0.1:18080", c.headers); w.Code != http.StatusForbidden {
			t.Errorf("%s %s %v: %d, want 403", c.method, c.path, c.headers, w.Code)
		}
	}
	if be.creates != 0 {
		t.Errorf("%d containers were requested", be.creates)
	}
}

func TestTokenURLSetsCookieAndDropsToken(t *testing.T) {
	h, _ := testHandler(t)
	for _, host := range []string{"127.0.0.1:18080", "localhost:18080", "[::1]:18080", "LOCALHOST:18080"} {
		w := do(h, "GET", "/?token="+testToken+"&x=1", host, nil)
		if w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/?x=1" {
			t.Errorf("Host %s: %d Location=%q", host, w.Code, w.Header().Get("Location"))
			continue
		}
		c := w.Result().Cookies()
		if len(c) != 1 || c[0].Name != cookieName || c[0].Value != testToken || !c[0].HttpOnly || c[0].SameSite != http.SameSiteStrictMode {
			t.Errorf("cookie = %+v", c)
		}
		if w := do(h, "GET", "/", host, map[string]string{"Cookie": cookieName + "=" + testToken}); w.Code != 200 {
			t.Errorf("with the cookie: %d", w.Code)
		}
	}
	// A same-origin start with the cookie gets as far as the backend.
	h, be := testHandler(t)
	do(h, "POST", "/start", "127.0.0.1:18080", map[string]string{"Cookie": cookieName + "=" + testToken, "Sec-Fetch-Site": "same-origin"})
	if be.creates != 1 {
		t.Errorf("creates = %d, want 1", be.creates)
	}
}
