package web

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gkroken/Atelier/internal/account"
	"github.com/gkroken/Atelier/internal/auth/pam"
	"github.com/gkroken/Atelier/internal/backend"
	"github.com/gkroken/Atelier/internal/ide"
	"github.com/gkroken/Atelier/internal/launcher"
	"github.com/gkroken/Atelier/internal/login"
)

const (
	mainHost     = "atelier.test:8443"
	sessionsHost = "sessions.atelier.test:8443"
	mainOrigin   = "https://" + mainHost
	sessOrigin   = "https://" + sessionsHost
)

// fakePAM knows a few users; it returns the name as typed, as pam_sss does.
type fakePAM struct {
	mu    sync.Mutex
	calls int
}

func (p *fakePAM) Check(_ context.Context, user, password string) (string, error) {
	p.mu.Lock()
	p.calls++
	p.mu.Unlock()
	switch {
	case strings.EqualFold(user, "alice") || user == "alice@corp":
		if password == "pw-a" {
			return user, nil
		}
	case user == "bob" && password == "pw-b", user == "noacct" && password == "pw-n":
		return user, nil
	case user == "disabled" && password == "pw-d":
		return "", pam.ErrAccount
	case user == "down":
		return "", pam.ErrUnavailable
	}
	return "", pam.ErrAuth
}

// accounts canonicalises like SSSD with case_sensitive = false and a domain suffix.
func accounts(name string) (account.Account, error) {
	var a account.Account
	switch strings.ToLower(strings.TrimSuffix(name, "@corp")) {
	case "alice":
		a.Name, a.UID = "alice", 1001
	case "bob":
		a.Name, a.UID = "bob", 1002
	default:
		return a, account.ErrNoAccount
	}
	a.GID, a.Home = a.UID, "/home/"+a.Name
	return a, nil
}

// runningBackend "runs" every container on 127.0.0.1, where the test's IDE listens.
type runningBackend struct {
	mu      sync.Mutex
	creates int
}

func (b *runningBackend) Create(context.Context, backend.Spec) (string, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.creates++
	return "c" + strconv.Itoa(b.creates), nil
}
func (b *runningBackend) Start(context.Context, string) error               { return nil }
func (b *runningBackend) Stop(context.Context, string, time.Duration) error { return nil }
func (b *runningBackend) Remove(context.Context, string) error              { return nil }
func (b *runningBackend) Inspect(context.Context, string) (backend.State, error) {
	return backend.State{Running: true, IP: "127.0.0.1"}, nil
}

type env struct {
	t      *testing.T
	front  *Front
	pam    *fakePAM
	be     *runningBackend
	l      *launcher.Launcher
	slept  []time.Duration
	ideGot []string // Cookie headers the IDE received
}

func newEnv(t *testing.T) *env {
	t.Helper()
	e := &env{t: t, pam: &fakePAM{}, be: &runningBackend{}}
	var mu sync.Mutex
	ideSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			return
		}
		mu.Lock()
		e.ideGot = append(e.ideGot, strings.Join(r.Header.Values("Cookie"), "; "))
		mu.Unlock()
		io.WriteString(w, "IDE page")
	}))
	t.Cleanup(ideSrv.Close)
	port, _ := strconv.Atoi(ideSrv.URL[strings.LastIndex(ideSrv.URL, ":")+1:])
	fake := ide.Definition{
		Name: "fake", Title: "Fake", Image: "fake:1", Port: port, Cmd: []string{"x"},
		StripPrefix: true, Health: ide.Health{Path: "/health", MaxStatus: 299},
	}
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	store := login.New()
	e.l = launcher.New(e.be, Identify(store), launcher.Options{Logger: quiet})
	e.front = New(Options{
		MainHost: mainHost, SessionsHost: sessionsHost, HTTPS: true,
		Launcher: e.l, Logins: store, Password: e.pam, Accounts: accounts, Logger: quiet,
		IDEs: func() []string { return []string{"fake"} },
		LookupIDE: func(name string) (ide.Definition, error) {
			if name == "fake" {
				return fake, nil
			}
			return ide.Definition{}, errors.New("unknown")
		},
	})
	e.front.sleep = func(d time.Duration) { e.slept = append(e.slept, d) }
	return e
}

// browser keeps cookies per host, like a real one with host-only cookies, and sends
// the fetch metadata a browser sends for its own same-origin form posts.
type browser struct {
	e    *env
	jar  map[string]map[string]string
	addr string
}

func (e *env) browser() *browser {
	return &browser{e: e, jar: map[string]map[string]string{}, addr: "192.0.2.10:5555"}
}

func (b *browser) do(method, rawURL string, form url.Values, headers map[string]string) *httptest.ResponseRecorder {
	b.e.t.Helper()
	u, _ := url.Parse(rawURL)
	var body io.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	}
	r := httptest.NewRequest(method, rawURL, body)
	r.RemoteAddr = b.addr
	if form != nil {
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	if method == http.MethodPost {
		r.Header.Set("Sec-Fetch-Site", "same-origin")
	}
	for k, v := range headers {
		if v == "" {
			r.Header.Del(k)
		} else {
			r.Header.Set(k, v)
		}
	}
	for name, v := range b.jar[u.Host] {
		r.AddCookie(&http.Cookie{Name: name, Value: v})
	}
	w := httptest.NewRecorder()
	b.e.front.ServeHTTP(w, r)
	for _, c := range w.Result().Cookies() {
		if b.jar[u.Host] == nil {
			b.jar[u.Host] = map[string]string{}
		}
		if c.MaxAge < 0 {
			delete(b.jar[u.Host], c.Name)
		} else {
			b.jar[u.Host][c.Name] = c.Value
		}
	}
	return w
}

func (b *browser) signIn(user, password string) *httptest.ResponseRecorder {
	return b.do("POST", mainOrigin+"/signin", url.Values{"user": {user}, "password": {password}}, nil)
}

// open follows the main host's /open/<id> through the ticket to the IDE page.
func (b *browser) open(id string) *httptest.ResponseRecorder {
	b.e.t.Helper()
	w := b.do("GET", mainOrigin+"/open/"+id, nil, nil)
	if w.Code != http.StatusSeeOther || !strings.HasPrefix(w.Header().Get("Location"), sessOrigin+enterPath+"?ticket=") {
		return w
	}
	w = b.do("GET", w.Header().Get("Location"), nil, nil)
	if w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/s/"+id+"/" {
		b.e.t.Fatalf("enter: %d to %q", w.Code, w.Header().Get("Location"))
	}
	return b.do("GET", sessOrigin+"/s/"+id+"/", nil, nil)
}

func (e *env) start(b *browser) string {
	e.t.Helper()
	w := b.do("POST", mainOrigin+"/start", url.Values{"ide": {"fake"}}, nil)
	loc := w.Header().Get("Location")
	if w.Code != http.StatusSeeOther || !strings.HasPrefix(loc, "/open/") {
		e.t.Fatalf("start: %d %q %s", w.Code, loc, w.Body)
	}
	return strings.TrimPrefix(loc, "/open/")
}

func TestSignInStartOpen(t *testing.T) {
	e := newEnv(t)
	b := e.browser()
	if w := b.do("GET", mainOrigin+"/", nil, nil); w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/signin" {
		t.Fatalf("home without sign-in: %d %q", w.Code, w.Header().Get("Location"))
	}
	w := b.signIn("alice", "pw-a")
	if w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/" {
		t.Fatalf("sign-in: %d %q %s", w.Code, w.Header().Get("Location"), w.Body)
	}
	c := w.Result().Cookies()[0]
	if c.Name != MainCookie || !c.Secure || !c.HttpOnly || c.SameSite != http.SameSiteLaxMode || c.Path != "/" || c.Domain != "" {
		t.Errorf("main cookie %+v", c)
	}
	if len(e.slept) != 0 {
		t.Errorf("a successful sign-in was delayed: %v", e.slept)
	}
	id := e.start(b)
	if w := b.do("GET", mainOrigin+"/", nil, nil); w.Code != 200 || !strings.Contains(w.Body.String(), "/open/"+id) || !strings.Contains(w.Body.String(), "Signed in as alice") {
		t.Fatalf("home: %d %s", w.Code, w.Body)
	}
	e.ideGot = nil
	w = b.open(id)
	if w.Code != 200 || w.Body.String() != "IDE page" {
		t.Fatalf("IDE through the sessions host: %d %s", w.Code, w.Body)
	}
	// The sessions host's cookie is its own, and neither Atelier cookie reaches the IDE.
	if b.jar[sessionsHost][SessionsCookie] == "" || b.jar[sessionsHost][SessionsCookie] == b.jar[mainHost][MainCookie] {
		t.Errorf("cookies: %v", b.jar)
	}
	if len(e.ideGot) != 1 || e.ideGot[0] != "" {
		t.Errorf("IDE received cookies %q", e.ideGot)
	}
}

// The deny cases: another signed-in user reaches neither the session, its ticket, its
// listing, nor its quit button.
func TestAnotherUserIsDenied(t *testing.T) {
	e := newEnv(t)
	alice, bob := e.browser(), e.browser()
	alice.signIn("alice", "pw-a")
	id := e.start(alice)
	bob.signIn("bob", "pw-b")
	bob.open(e.start(bob)) // bob has his own sessions-host cookie

	if w := bob.do("GET", mainOrigin+"/open/"+id, nil, nil); w.Code != http.StatusNotFound {
		t.Errorf("bob /open/<alice's>: %d, want 404", w.Code)
	}
	if w := bob.do("GET", sessOrigin+"/s/"+id+"/", nil, nil); w.Code != http.StatusNotFound {
		t.Errorf("bob /s/<alice's>/: %d, want 404", w.Code)
	}
	if w := bob.do("GET", mainOrigin+"/", nil, nil); strings.Contains(w.Body.String(), id) {
		t.Error("bob's home lists alice's session")
	}
	if w := bob.do("POST", mainOrigin+"/quit", url.Values{"id": {id}}, nil); w.Code != http.StatusNotFound {
		t.Errorf("bob quit alice's session: %d", w.Code)
	}
	if w := alice.open(id); w.Code != 200 {
		t.Errorf("alice's session gone after bob's attempts: %d", w.Code)
	}
}

// Ownership follows the OS's canonical name: alice signed in as ALICE and as alice@corp
// is one account with one session list (atelier-3b90b60-N3).
func TestSpellingsShareOneAccount(t *testing.T) {
	e := newEnv(t)
	a1, a2, a3 := e.browser(), e.browser(), e.browser()
	a1.signIn("alice", "pw-a")
	id := e.start(a1)
	a2.signIn("ALICE", "pw-a")
	a3.signIn("alice@corp", "pw-a")
	for _, b := range []*browser{a2, a3} {
		if w := b.do("GET", mainOrigin+"/", nil, nil); !strings.Contains(w.Body.String(), "/open/"+id) || !strings.Contains(w.Body.String(), "Signed in as alice.") {
			t.Errorf("home: %s", w.Body)
		}
		if w := b.open(id); w.Code != 200 {
			t.Errorf("open: %d", w.Code)
		}
	}
}

func TestUnknownHostRefusedBeforeRouting(t *testing.T) {
	e := newEnv(t)
	b := e.browser()
	b.signIn("alice", "pw-a")
	id := e.start(b)
	creates := e.be.creates
	for _, host := range []string{"evil.test", "atelier.test", "127.0.0.1:8443", "atelier.test:8443.evil.test", ""} {
		for _, path := range []string{"/", "/signin", "/s/" + id + "/", "/open/" + id} {
			r := httptest.NewRequest("GET", "/x", nil)
			r.URL.Path, r.Host = path, host
			w := httptest.NewRecorder()
			e.front.ServeHTTP(w, r)
			if w.Code != http.StatusMisdirectedRequest {
				t.Errorf("Host %q %s: %d, want 421", host, path, w.Code)
			}
		}
		r := httptest.NewRequest("POST", "/start", strings.NewReader("ide=fake"))
		r.Host = host
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		r.Header.Set("Sec-Fetch-Site", "same-origin")
		e.front.ServeHTTP(httptest.NewRecorder(), r)
	}
	if e.be.creates != creates {
		t.Error("a request for an unknown host started a session")
	}
	if w := b.do("GET", "https://ATELIER.test:8443/signin", nil, nil); w.Code != 200 {
		t.Errorf("upper-case Host: %d", w.Code) // hostnames are case-insensitive
	}
}

// Main-host writes refuse requests from anywhere but the main host's own pages, the
// sessions host included (it is same-site) (ADR-0016 amendment).
func TestMainHostRefusesCrossOriginWrites(t *testing.T) {
	for name, tc := range map[string]struct {
		headers map[string]string
		allow   bool
	}{
		"same-origin":                             {map[string]string{"Sec-Fetch-Site": "same-origin"}, true},
		"Origin is the main host":                 {map[string]string{"Sec-Fetch-Site": "", "Origin": mainOrigin}, true},
		"from the sessions host":                  {map[string]string{"Sec-Fetch-Site": "same-site", "Origin": sessOrigin}, false},
		"sessions-host Origin, no fetch metadata": {map[string]string{"Sec-Fetch-Site": "", "Origin": sessOrigin}, false},
		"cross-site":                              {map[string]string{"Sec-Fetch-Site": "cross-site", "Origin": "https://evil.test"}, false},
		"typed URL":                               {map[string]string{"Sec-Fetch-Site": "none"}, false},
		"main host over http":                     {map[string]string{"Sec-Fetch-Site": "", "Origin": "http://" + mainHost}, false},
		"null Origin":                             {map[string]string{"Sec-Fetch-Site": "", "Origin": "null"}, false},
		"no headers":                              {map[string]string{"Sec-Fetch-Site": ""}, false},
	} {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t)
			b := e.browser()
			b.signIn("alice", "pw-a")
			w := b.do("POST", mainOrigin+"/start", url.Values{"ide": {"fake"}}, tc.headers)
			if started := e.be.creates == 1; started != tc.allow {
				t.Errorf("started = %v, want %v (status %d)", started, tc.allow, w.Code)
			}
			if !tc.allow && w.Code != http.StatusForbidden {
				t.Errorf("status %d, want 403", w.Code)
			}
			// Sign-in and sign-out obey the same rule (no login CSRF).
			c := e.pam.calls
			b2 := e.browser()
			w = b2.do("POST", mainOrigin+"/signin", url.Values{"user": {"bob"}, "password": {"pw-b"}}, tc.headers)
			if got := b2.jar[mainHost][MainCookie] != ""; got != tc.allow || (!tc.allow && e.pam.calls != c) {
				t.Errorf("sign-in: signed in = %v, PAM calls %d -> %d; want %v", got, c, e.pam.calls, tc.allow)
			}
		})
	}
}

func TestTicketsAndCookiesDontCross(t *testing.T) {
	e := newEnv(t)
	b := e.browser()
	b.signIn("alice", "pw-a")
	id := e.start(b)
	w := b.do("GET", mainOrigin+"/open/"+id, nil, nil)
	ticketURL := w.Header().Get("Location")
	if w := b.do("GET", ticketURL, nil, nil); w.Code != http.StatusSeeOther {
		t.Fatalf("first use: %d", w.Code)
	}
	other := e.browser()
	if w := other.do("GET", ticketURL, nil, nil); w.Code != http.StatusForbidden || other.jar[sessionsHost][SessionsCookie] != "" {
		t.Errorf("second use: %d, cookie %q", w.Code, other.jar[sessionsHost][SessionsCookie])
	}
	// The main cookie's value is no good on the sessions host, and vice versa.
	mainTok, sessTok := b.jar[mainHost][MainCookie], b.jar[sessionsHost][SessionsCookie]
	x := e.browser()
	x.jar[sessionsHost] = map[string]string{SessionsCookie: mainTok, MainCookie: mainTok}
	if w := x.do("GET", sessOrigin+"/s/"+id+"/", nil, nil); w.Code != http.StatusSeeOther {
		t.Errorf("main token on the sessions host: %d, want a redirect to sign in", w.Code)
	}
	x.jar[mainHost] = map[string]string{MainCookie: sessTok}
	if w := x.do("GET", mainOrigin+"/", nil, nil); w.Code != http.StatusSeeOther {
		t.Errorf("sessions token on the main host: %d, want a redirect to sign in", w.Code)
	}
}

// A bookmark of a session URL, with nobody signed in: sessions host -> main host
// /open/<id> -> sign-in -> back to /open/<id> -> the session.
func TestBookmarkGoesRoundSignIn(t *testing.T) {
	e := newEnv(t)
	first := e.browser()
	first.signIn("alice", "pw-a")
	id := e.start(first)

	b := e.browser()
	w := b.do("GET", sessOrigin+"/s/"+id+"/", nil, nil)
	if w.Code != http.StatusSeeOther || w.Header().Get("Location") != mainOrigin+"/open/"+id {
		t.Fatalf("sessions host: %d %q", w.Code, w.Header().Get("Location"))
	}
	w = b.do("GET", mainOrigin+"/open/"+id, nil, nil)
	if want := "/signin?next=%2Fopen%2F" + id; w.Code != http.StatusSeeOther || w.Header().Get("Location") != want {
		t.Fatalf("main host: %d %q, want %q", w.Code, w.Header().Get("Location"), want)
	}
	w = b.do("POST", mainOrigin+"/signin", url.Values{"user": {"alice"}, "password": {"pw-a"}, "next": {"/open/" + id}}, nil)
	if w.Header().Get("Location") != "/open/"+id {
		t.Fatalf("after sign-in: %q", w.Header().Get("Location"))
	}
	if w := b.open(id); w.Code != 200 {
		t.Fatalf("open: %d", w.Code)
	}
	// Only a page load is redirected; a script's fetch gets the plain 404.
	if w := e.browser().do("GET", sessOrigin+"/s/"+id+"/api", nil, map[string]string{"Sec-Fetch-Mode": "cors"}); w.Code != http.StatusNotFound {
		t.Errorf("fetch without a cookie: %d", w.Code)
	}
}

func TestNextIsOnlyAnOpenPath(t *testing.T) {
	e := newEnv(t)
	for _, next := range []string{"//evil.test/", "https://evil.test/", "/open/../signout", "/open/x", "javascript:alert(1)", "/\\evil.test", "/s/abc/"} {
		b := e.browser()
		w := b.do("POST", mainOrigin+"/signin", url.Values{"user": {"alice"}, "password": {"pw-a"}, "next": {next}}, nil)
		if w.Header().Get("Location") != "/" {
			t.Errorf("next %q: redirected to %q", next, w.Header().Get("Location"))
		}
	}
}

func TestSignOutEndsBothHosts(t *testing.T) {
	e := newEnv(t)
	b := e.browser()
	b.signIn("alice", "pw-a")
	id := e.start(b)
	b.open(id)
	sess := b.jar[sessionsHost][SessionsCookie]
	b.do("POST", mainOrigin+"/signout", nil, nil)
	if b.jar[mainHost][MainCookie] != "" {
		t.Error("main cookie not cleared")
	}
	if w := b.do("GET", mainOrigin+"/", nil, nil); w.Code != http.StatusSeeOther {
		t.Errorf("home after sign-out: %d", w.Code)
	}
	b.jar[sessionsHost][SessionsCookie] = sess // the sessions host's cookie is still in the browser
	if w := b.do("GET", sessOrigin+"/s/"+id+"/", nil, nil); w.Code != http.StatusSeeOther {
		t.Errorf("session after sign-out: %d, want a redirect to sign in", w.Code)
	}
}

func TestFailedSignIns(t *testing.T) {
	for _, tc := range []struct {
		user, password string
		status         int
		says           string
	}{
		{"alice", "wrong-zz9", 401, "Wrong user name or password."},
		{"nobody", "pw-a", 401, "Wrong user name or password."},
		{"disabled", "pw-d", 401, "Wrong user name or password."}, // same words: no hint the password was right
		{"noacct", "pw-n", 403, "This account can&#39;t use Atelier."},
		{"down", "pw-down-zz9", 503, "Sign-in is unavailable"},
		{"alice", "", 401, "Enter your user name and password."},
	} {
		e := newEnv(t)
		b := e.browser()
		w := b.signIn(tc.user, tc.password)
		if w.Code != tc.status || !strings.Contains(w.Body.String(), tc.says) || b.jar[mainHost][MainCookie] != "" {
			t.Errorf("%s/%s: %d %q, cookie %q", tc.user, tc.password, w.Code, w.Body, b.jar[mainHost][MainCookie])
		}
		// Every failure is held to the minimum duration.
		if len(e.slept) != 1 || e.slept[0] <= 0 || e.slept[0] > DefaultMinFailure {
			t.Errorf("%s: slept %v, want one pause up to %v", tc.user, e.slept, DefaultMinFailure)
		}
		if strings.Contains(w.Body.String(), tc.password) && tc.password != "" {
			t.Errorf("%s: the page shows the password", tc.user)
		}
	}
}

func TestRateLimits(t *testing.T) {
	e := newEnv(t)
	b := e.browser()
	for range nameTries {
		b.signIn("alice", "wrong")
	}
	calls := e.pam.calls
	if w := b.signIn("alice", "pw-a"); w.Code != http.StatusTooManyRequests || e.pam.calls != calls {
		t.Fatalf("after %d failures: %d, PAM calls %d -> %d; want 429 without PAM", nameTries, w.Code, calls, e.pam.calls)
	}
	// Spelled differently, the same name is the same key.
	if w := b.signIn("ALICE", "pw-a"); w.Code != http.StatusTooManyRequests {
		t.Errorf("ALICE: %d", w.Code)
	}
	// Bob isn't affected.
	if w := b.signIn("bob", "pw-b"); w.Code != http.StatusSeeOther {
		t.Errorf("bob: %d", w.Code)
	}

	// The directory being down doesn't count against anyone.
	e = newEnv(t)
	b = e.browser()
	for range 3 * nameTries {
		if w := b.signIn("down", "x"); w.Code != http.StatusServiceUnavailable {
			t.Fatalf("down: %d", w.Code)
		}
	}

	// Per address: many names from one place.
	e = newEnv(t)
	b = e.browser()
	for i := range addrTries {
		b.signIn("guess"+strconv.Itoa(i), "x")
	}
	calls = e.pam.calls
	if w := b.signIn("bob", "pw-b"); w.Code != http.StatusTooManyRequests || e.pam.calls != calls {
		t.Errorf("from a busy address: %d", w.Code)
	}
	// A name refused for its address keeps its own attempts (refunded): bob, refused
	// once above, still has all of his from another address.
	other := e.browser()
	other.addr = "198.51.100.7:1234"
	for range nameTries - 1 {
		other.signIn("bob", "wrong")
	}
	if w := other.signIn("bob", "pw-b"); w.Code != http.StatusSeeOther {
		t.Errorf("bob's attempts were used up by the address refusal: %d", w.Code)
	}
}

func TestPagesHaveSecurityHeaders(t *testing.T) {
	e := newEnv(t)
	w := e.browser().do("GET", mainOrigin+"/signin", nil, nil)
	h := w.Header()
	if !strings.Contains(h.Get("Content-Security-Policy"), "frame-ancestors 'none'") || h.Get("X-Content-Type-Options") != "nosniff" || h.Get("Cache-Control") != "no-store" || h.Get("Referrer-Policy") != "no-referrer" {
		t.Errorf("headers %v", h)
	}
}

func TestSessionsHostServesNothingElse(t *testing.T) {
	e := newEnv(t)
	for _, path := range []string{"/", "/signin", "/open/x", "/_atelier/other"} {
		if w := e.browser().do("GET", sessOrigin+path, nil, nil); w.Code != http.StatusNotFound {
			t.Errorf("sessions host %s: %d", path, w.Code)
		}
	}
	if w := e.browser().do("POST", sessOrigin+"/signin", url.Values{"user": {"alice"}, "password": {"pw-a"}}, nil); w.Code != http.StatusNotFound || e.pam.calls != 0 {
		t.Errorf("sign-in on the sessions host: %d", w.Code)
	}
}
