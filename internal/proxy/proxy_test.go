package proxy

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/gkroken/Atelier/internal/session"
)

// seen is what the fake IDE received.
type seen struct {
	Method, Host, Path, RawPath, Query string
	Header                             http.Header
}

// fakeIDE echoes the request it received as JSON, sets a cookie of its own plus one
// that tries to overwrite Atelier's, and redirects /login the way RStudio does: to an
// absolute URL built from the Host header it was sent.
func fakeIDE(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/login" {
			http.Redirect(w, r, "http://"+r.Host+"/s/x/home", http.StatusFound)
			return
		}
		http.SetCookie(w, &http.Cookie{Name: "ide-cookie", Value: "1", Path: "/"})
		w.Header().Add("Set-Cookie", "atelier_session=planted; Path=/")
		json.NewEncoder(w).Encode(seen{r.Method, r.Host, r.URL.Path, r.URL.RawPath, r.URL.RawQuery, r.Header})
	}))
	t.Cleanup(srv.Close)
	return srv
}

// mapResolver resolves the sessions it holds and refuses every other ID.
type mapResolver map[string]Route

func (m mapResolver) Resolve(_ *http.Request, id string) (Route, error) {
	if r, ok := m[id]; ok {
		return r, nil
	}
	return Route{}, ErrNoSession
}

func mustURL(t *testing.T, s string) *url.URL {
	u, err := url.Parse(s)
	if err != nil {
		t.Fatal(err)
	}
	return u
}

func quiet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func setup(t *testing.T, strip bool) (front *httptest.Server, id string) {
	t.Helper()
	ide := fakeIDE(t)
	id = session.NewID()
	p := New(mapResolver{id: {Target: mustURL(t, ide.URL), StripPrefix: strip}},
		Options{Cookies: []string{"atelier_session"}, Logger: quiet()})
	front = httptest.NewServer(p)
	t.Cleanup(front.Close)
	return front, id
}

func noRedirect() *http.Client {
	return &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}

func get(t *testing.T, req *http.Request) (*http.Response, seen) {
	t.Helper()
	resp, err := noRedirect().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var s seen
	if resp.StatusCode == 200 {
		if err := json.NewDecoder(resp.Body).Decode(&s); err != nil {
			t.Fatal(err)
		}
	}
	return resp, s
}

// Requests for sessions the resolver refuses, and malformed paths, get a 404 and never
// reach any IDE.
func TestDeniesUnknownSessionsAndBadPaths(t *testing.T) {
	front, id := setup(t, true)
	other := session.NewID()
	for _, path := range []string{
		"/s/" + other + "/",               // well-formed, but not a session the resolver allows
		"/s/" + other,                     // same, without the slash: no redirect either way
		"/s/" + strings.ToUpper(id) + "/", // not a valid ID
		"/s/" + id[:25] + "/",
		"/s/../s/" + id + "/",
		"/s/",
		"/",
		"/x/" + id + "/",
	} {
		req, _ := http.NewRequest("GET", front.URL+path, nil)
		resp, _ := get(t, req)
		want := http.StatusNotFound
		if path == "/s/"+other {
			// Shape is valid, so it is redirected to the slash form, which then 404s.
			want = http.StatusMovedPermanently
		}
		if resp.StatusCode != want {
			t.Errorf("GET %s = %d, want %d", path, resp.StatusCode, want)
		}
	}
}

func TestRedirectsToTrailingSlash(t *testing.T) {
	front, id := setup(t, true)
	req, _ := http.NewRequest("GET", front.URL+"/s/"+id+"?a=1", nil)
	resp, _ := get(t, req)
	if resp.StatusCode != http.StatusMovedPermanently || resp.Header.Get("Location") != "/s/"+id+"/?a=1" {
		t.Errorf("got %d Location=%q", resp.StatusCode, resp.Header.Get("Location"))
	}
}

func TestStripPrefixKeepsEscapingAndQuery(t *testing.T) {
	for _, strip := range []bool{true, false} {
		t.Run(fmt.Sprint("strip=", strip), func(t *testing.T) {
			front, id := setup(t, strip)
			req, _ := http.NewRequest("GET", front.URL+"/s/"+id+"/files/a%2Fb%20c?x=1&y=2", nil)
			resp, s := get(t, req)
			if resp.StatusCode != 200 {
				t.Fatalf("status %d", resp.StatusCode)
			}
			wantRaw := "/files/a%2Fb%20c"
			if !strip {
				wantRaw = "/s/" + id + wantRaw
			}
			gotRaw := s.RawPath
			if gotRaw == "" {
				gotRaw = s.Path
			}
			if gotRaw != wantRaw || s.Query != "x=1&y=2" {
				t.Errorf("IDE saw raw path %q query %q, want %q x=1&y=2", gotRaw, s.Query, wantRaw)
			}
		})
	}
}

// The IDE sees the client's Host, so its absolute redirects point back at Atelier, not
// at the container (atelier-10f7686-N2).
func TestKeepsClientHostForRedirects(t *testing.T) {
	front, id := setup(t, true)
	req, _ := http.NewRequest("GET", front.URL+"/s/"+id+"/login", nil)
	req.Host = "atelier.example.org"
	resp, _ := get(t, req)
	if loc := resp.Header.Get("Location"); loc != "http://atelier.example.org/s/x/home" {
		t.Errorf("Location = %q, want it built from the client's Host", loc)
	}
	req, _ = http.NewRequest("GET", front.URL+"/s/"+id+"/", nil)
	req.Host = "atelier.example.org"
	_, s := get(t, req)
	if s.Host != "atelier.example.org" || s.Header.Get("X-Forwarded-Host") != "atelier.example.org" ||
		s.Header.Get("X-Forwarded-Proto") != "http" {
		t.Errorf("IDE saw Host=%q X-Forwarded-Host=%q X-Forwarded-Proto=%q", s.Host,
			s.Header.Get("X-Forwarded-Host"), s.Header.Get("X-Forwarded-Proto"))
	}
}

// No header a client sends can tell the IDE (or anything behind it) who the user is.
func TestStripsClientIdentityHeaders(t *testing.T) {
	front, id := setup(t, true)
	req, _ := http.NewRequest("GET", front.URL+"/s/"+id+"/", nil)
	spoof := []string{
		"X-Atelier-User", "X-Atelier-Uid", "X-Atelier-Anything",
		"Remote-User", "X-Remote-User", "X-Remote-Group", "X-User", "X-Email", "X-Groups",
		"X-Webauth-User", "X-Webauth-Email", "X-RStudio-Username",
		"X-Auth-Request-User", "X-Auth-Request-Email", "X-Auth-Request-Access-Token",
		"X-Forwarded-User", "X-Forwarded-Email", "X-Forwarded-Groups", "X-Forwarded-Preferred-Username",
		"Forwarded",
		"X-Real-Ip", "X-Original-Url", "X-Rewrite-Url",
		"X_Atelier_User", "X_remote_user", "Remote_user",
	}
	for _, h := range spoof {
		req.Header.Set(h, "root")
	}
	req.Header.Set("X-Forwarded-For", "10.6.6.6")
	req.Header.Set("X-Forwarded-Host", "evil.example")
	req.Header.Set("X-Forwarded-Proto", "https")
	req.Header.Set("X-Other", "kept")
	_, s := get(t, req)
	for _, h := range spoof {
		if v := s.Header.Get(h); v != "" {
			t.Errorf("IDE received %s: %q", h, v)
		}
	}
	if xff := s.Header.Get("X-Forwarded-For"); xff == "" || strings.Contains(xff, "10.6.6.6") {
		t.Errorf("X-Forwarded-For = %q, want only the real client address", xff)
	}
	if s.Header.Get("X-Forwarded-Host") == "evil.example" || s.Header.Get("X-Forwarded-Proto") == "https" {
		t.Errorf("client's X-Forwarded-Host/Proto were passed on: %v", s.Header)
	}
	if s.Header.Get("X-Other") != "kept" {
		t.Error("an ordinary header was dropped")
	}
}

// Atelier's cookie never reaches the IDE, the IDE's own cookies do, and the IDE can't
// set Atelier's cookie in the browser.
func TestAtelierCookieNeitherForwardedNorSettable(t *testing.T) {
	front, id := setup(t, true)
	req, _ := http.NewRequest("GET", front.URL+"/s/"+id+"/", nil)
	req.Header.Add("Cookie", "atelier_session=secret; ide-cookie=1")
	req.Header.Add("Cookie", "atelier_session=secret2")
	_, s := get(t, req)
	if c := strings.Join(s.Header.Values("Cookie"), " | "); strings.Contains(c, "atelier_session") {
		t.Errorf("IDE received Cookie %q", c)
	}

	// An empty first Cookie line must not let the next one through (atelier-fa69e6c-N3).
	req, _ = http.NewRequest("GET", front.URL+"/s/"+id+"/", nil)
	req.Header["Cookie"] = []string{"", "atelier_session=secret3", "ide-cookie=1"}
	resp, s := get(t, req)
	if c := strings.Join(s.Header.Values("Cookie"), " | "); strings.Contains(c, "atelier_session") || !strings.Contains(c, "ide-cookie=1") {
		t.Errorf("IDE received Cookie %q", c)
	}
	for _, sc := range resp.Header.Values("Set-Cookie") {
		if strings.HasPrefix(sc, "atelier_session") {
			t.Errorf("IDE set Atelier's cookie: %q", sc)
		}
	}
	if !strings.Contains(strings.Join(resp.Header.Values("Set-Cookie"), "\n"), "ide-cookie=1") {
		t.Error("the IDE's own cookie was dropped")
	}
}

// An IDE that is down gets a 502 that says nothing about where the container is.
func TestUnreachableSessionIs502WithoutDetails(t *testing.T) {
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	addr := ln.Addr().String()
	ln.Close() // nothing listens there now
	id := session.NewID()
	p := New(mapResolver{id: {Target: mustURL(t, "http://"+addr)}}, Options{Logger: quiet()})
	front := httptest.NewServer(p)
	defer front.Close()
	resp, err := http.Get(front.URL + "/s/" + id + "/")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway {
		t.Errorf("status %d, want 502", resp.StatusCode)
	}
	host, port, _ := net.SplitHostPort(addr)
	if strings.Contains(string(body), host) || strings.Contains(string(body), port) {
		t.Errorf("502 body leaks the address: %q", body)
	}
}

// A websocket upgrade goes through and both directions flow afterwards. The fake IDE
// answers 101 and echoes raw bytes, which is all the proxy has to carry.
func TestWebsocketUpgrade(t *testing.T) {
	ide := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Upgrade") != "websocket" || !strings.Contains(strings.ToLower(r.Header.Get("Connection")), "upgrade") {
			http.Error(w, "not an upgrade", 400)
			return
		}
		if r.URL.Path != "/ws" || r.Header.Get("Cookie") != "" {
			http.Error(w, "wrong path or cookie leaked: "+r.URL.Path+" "+r.Header.Get("Cookie"), 400)
			return
		}
		conn, brw, err := w.(http.Hijacker).Hijack()
		if err != nil {
			return
		}
		defer conn.Close()
		brw.WriteString("HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n\r\n")
		brw.Flush()
		io.Copy(conn, brw) // echo until the client closes
	}))
	defer ide.Close()
	id := session.NewID()
	p := New(mapResolver{id: {Target: mustURL(t, ide.URL), StripPrefix: true}},
		Options{Cookies: []string{"atelier_session"}, Logger: quiet()})
	front := httptest.NewServer(p)
	defer front.Close()

	conn, err := net.Dial("tcp", strings.TrimPrefix(front.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(5 * time.Second))
	fmt.Fprintf(conn, "GET /s/%s/ws HTTP/1.1\r\nHost: x\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n"+
		"Sec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\nSec-WebSocket-Version: 13\r\nCookie: atelier_session=secret\r\n\r\n", id)
	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusSwitchingProtocols {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("status %d: %s", resp.StatusCode, b)
	}
	for _, msg := range []string{"hello", "second frame"} {
		if _, err := conn.Write([]byte(msg)); err != nil {
			t.Fatal(err)
		}
		buf := make([]byte, len(msg))
		if _, err := io.ReadFull(br, buf); err != nil {
			t.Fatal(err)
		}
		if string(buf) != msg {
			t.Errorf("echo = %q, want %q", buf, msg)
		}
	}
}

// anyResolver accepts every ID and records what it was asked, to show the proxy's own ID
// check keeps malformed IDs away from the real resolver (atelier-fa69e6c-N2).
type anyResolver struct {
	target *url.URL
	asked  []string
}

func (a *anyResolver) Resolve(_ *http.Request, id string) (Route, error) {
	a.asked = append(a.asked, id)
	return Route{Target: a.target}, nil
}

func TestMalformedIDsNeverReachTheResolver(t *testing.T) {
	ide := fakeIDE(t)
	res := &anyResolver{target: mustURL(t, ide.URL)}
	front := httptest.NewServer(New(res, Options{Logger: quiet()}))
	defer front.Close()
	for _, id := range []string{"..", "x", strings.Repeat("a", 10000), "ABCDEFGHIJKLMNOPQRSTUVWXYZ", "abcdefghijklmnopqrstuvwxy1", "%2e%2e"} {
		req, _ := http.NewRequest("GET", front.URL+"/s/"+id+"/", nil)
		if resp, _ := get(t, req); resp.StatusCode != http.StatusNotFound {
			t.Errorf("/s/%.30s/ = %d, want 404", id, resp.StatusCode)
		}
	}
	if len(res.asked) != 0 {
		t.Errorf("resolver was asked about %d malformed IDs", len(res.asked))
	}
	good := session.NewID()
	req, _ := http.NewRequest("GET", front.URL+"/s/"+good+"/", nil)
	if resp, _ := get(t, req); resp.StatusCode != 200 || len(res.asked) != 1 {
		t.Errorf("valid ID: status %d, resolver asked %v", resp.StatusCode, res.asked)
	}
}
