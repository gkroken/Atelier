package launcher

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gkroken/Atelier/internal/backend"
	"github.com/gkroken/Atelier/internal/ide"
	"github.com/gkroken/Atelier/internal/proxy"
	"github.com/gkroken/Atelier/internal/session"
)

// fakeBackend keeps containers in memory. Every container "runs" on 127.0.0.1, where
// the test's IDE server listens.
type fakeBackend struct {
	mu       sync.Mutex
	specs    map[string]backend.Spec
	removed  []string
	exitCode int // when non-zero, containers report as exited
	n        int
}

func newFakeBackend() *fakeBackend { return &fakeBackend{specs: map[string]backend.Spec{}} }

func (f *fakeBackend) Create(_ context.Context, s backend.Spec) (string, error) {
	if err := backend.Validate(s); err != nil {
		return "", err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.n++
	id := "c" + strconv.Itoa(f.n)
	f.specs[id] = s
	return id, nil
}
func (f *fakeBackend) Start(context.Context, string) error { return nil }
func (f *fakeBackend) Stop(context.Context, string, time.Duration) error {
	return nil
}
func (f *fakeBackend) Remove(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.specs[id]; !ok {
		return backend.ErrNotFound
	}
	delete(f.specs, id)
	f.removed = append(f.removed, id)
	return nil
}
func (f *fakeBackend) Inspect(_ context.Context, id string) (backend.State, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.specs[id]; !ok {
		return backend.State{}, backend.ErrNotFound
	}
	if f.exitCode != 0 {
		return backend.State{Running: false, ExitCode: f.exitCode}, nil
	}
	return backend.State{Running: true, IP: "127.0.0.1"}, nil
}

func (f *fakeBackend) live() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.specs)
}

// ideServer is the test's IDE: it answers status on every path, and echoes the path.
func ideServer(t *testing.T, status int) (ide.Definition, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(status)
		io.WriteString(w, "ide:"+r.URL.Path)
	}))
	t.Cleanup(srv.Close)
	_, port, _ := net.SplitHostPort(srv.Listener.Addr().String())
	p, _ := strconv.Atoi(port)
	return ide.Definition{
		Name: "fake", Title: "Fake", Image: "fake:1", Port: p,
		Cmd: []string{"serve"}, StripPrefix: true,
		Health: ide.Health{Path: "/", MaxStatus: 399},
	}, srv
}

var alice = Account{Name: "alice", UID: 4321, GID: 4322, Group: "staff", Home: "/home/alice"}

// headerIdentify is a test-only Identify: the user is whatever X-Test-User says.
func headerIdentify(r *http.Request) (string, bool) {
	u := r.Header.Get("X-Test-User")
	return u, u != ""
}

func newLauncher(be backend.Backend) *Launcher {
	return New(be, headerIdentify, Options{
		HealthTimeout: 500 * time.Millisecond,
		Logger:        slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
}

func TestStartBuildsTheSpecFromAccountAndDefinition(t *testing.T) {
	be := newFakeBackend()
	def, _ := ideServer(t, 302)
	l := newLauncher(be)
	s, err := l.Start(context.Background(), alice, def)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if !session.ValidID(s.ID) || s.Owner != "alice" || s.IDE != "fake" {
		t.Errorf("session = %+v", s)
	}
	spec := be.specs[s.ContainerID]
	if spec.UID != 4321 || spec.GID != 4322 {
		t.Errorf("account = %d:%d", spec.UID, spec.GID)
	}
	if spec.Home != (backend.Mount{Source: "/home/alice", Target: "/home/alice"}) {
		t.Errorf("home = %+v, want the same path inside and out", spec.Home)
	}
	if spec.Image != "fake:1" || !slices.Equal(spec.Cmd, def.Cmd) || spec.Name != "atelier-"+s.ID {
		t.Errorf("spec = %+v", spec)
	}
	files := map[string]string{}
	for _, f := range spec.Files {
		files[f.Path] = string(f.Content)
	}
	if files["/etc/passwd"] != "alice:x:4321:4322::/home/alice:/bin/bash\n" {
		t.Errorf("passwd = %q", files["/etc/passwd"])
	}
	if files["/etc/group"] != "staff:x:4322:alice\n" {
		t.Errorf("group = %q", files["/etc/group"])
	}
	for _, want := range []string{"ATELIER_SESSION_ID=" + s.ID, "ATELIER_PREFIX=/s/" + s.ID, "USER=alice", "HOME=/home/alice"} {
		if !slices.Contains(spec.Env, want) {
			t.Errorf("env lacks %s: %v", want, spec.Env)
		}
	}
	if spec.Labels["atelier.session"] != s.ID || spec.Labels["atelier.owner"] != "alice" {
		t.Errorf("labels = %v", spec.Labels)
	}
	if s.Route.Target.Host != "127.0.0.1:"+strconv.Itoa(def.Port) || !s.Route.StripPrefix {
		t.Errorf("route = %+v", s.Route)
	}
}

// A session that never becomes healthy, or whose container exits, is not registered and
// its container is removed.
func TestStartCleansUpWhenTheIDENeverComesUp(t *testing.T) {
	t.Run("unhealthy", func(t *testing.T) {
		be := newFakeBackend()
		def, _ := ideServer(t, 503)
		l := newLauncher(be)
		if _, err := l.Start(context.Background(), alice, def); err == nil {
			t.Fatal("Start succeeded with an unhealthy IDE")
		}
		if be.live() != 0 || len(be.removed) != 1 || len(l.List("alice")) != 0 {
			t.Errorf("live=%d removed=%v listed=%d", be.live(), be.removed, len(l.List("alice")))
		}
	})
	t.Run("exited", func(t *testing.T) {
		be := newFakeBackend()
		be.exitCode = 1
		def, _ := ideServer(t, 200)
		l := newLauncher(be)
		start := time.Now()
		_, err := l.Start(context.Background(), alice, def)
		if err == nil || !strings.Contains(err.Error(), "exited with code 1") {
			t.Fatalf("err = %v", err)
		}
		if time.Since(start) > 250*time.Millisecond {
			t.Errorf("took %v: an exited container should stop the wait at once", time.Since(start))
		}
		if be.live() != 0 {
			t.Error("container left behind")
		}
	})
}

// Bad account values are refused before any container exists.
func TestStartRefusesBadAccounts(t *testing.T) {
	def, _ := ideServer(t, 200)
	for name, mod := range map[string]func(*Account){
		"root":            func(a *Account) { a.UID = 0 },
		"colon in name":   func(a *Account) { a.Name = "a:0:0" },
		"newline in name": func(a *Account) { a.Name = "a\nroot" },
		"colon in group":  func(a *Account) { a.Group = "staff:x:0:" },
		"relative home":   func(a *Account) { a.Home = "home/alice" },
		"root home":       func(a *Account) { a.Home = "/" },
		"colon in home":   func(a *Account) { a.Home = "/home/a:b" },
		"newline in home": func(a *Account) { a.Home = "/home/a\nroot:x:0:0::/:/bin/sh" },
	} {
		t.Run(name, func(t *testing.T) {
			be := newFakeBackend()
			acct := alice
			mod(&acct)
			if _, err := newLauncher(be).Start(context.Background(), acct, def); err == nil {
				t.Fatal("Start succeeded")
			}
			if be.n != 0 {
				t.Errorf("a container was created")
			}
		})
	}
}

func req(user string) *http.Request {
	r := httptest.NewRequest("GET", "/", nil)
	if user != "" {
		r.Header.Set("X-Test-User", user)
	}
	return r
}

// Only the owner resolves their session; everyone else gets the same answer as for a
// session that doesn't exist.
func TestResolveDeniesEveryoneButTheOwner(t *testing.T) {
	def, _ := ideServer(t, 200)
	l := newLauncher(newFakeBackend())
	s, err := l.Start(context.Background(), alice, def)
	if err != nil {
		t.Fatal(err)
	}
	if rt, err := l.Resolve(req("alice"), s.ID); err != nil || rt.Target == nil {
		t.Fatalf("owner: %+v %v", rt, err)
	}
	for name, c := range map[string]struct{ user, id string }{
		"another user":    {"bob", s.ID},
		"case variant":    {"Alice", s.ID},
		"not signed in":   {"", s.ID},
		"unknown session": {"alice", session.NewID()},
	} {
		t.Run(name, func(t *testing.T) {
			if rt, err := l.Resolve(req(c.user), c.id); !errors.Is(err, proxy.ErrNoSession) || rt.Target != nil {
				t.Errorf("Resolve = %+v, %v; want ErrNoSession", rt, err)
			}
		})
	}
}

func TestQuitAndListAreOwnerOnly(t *testing.T) {
	def, _ := ideServer(t, 200)
	be := newFakeBackend()
	l := newLauncher(be)
	a, _ := l.Start(context.Background(), alice, def)
	bob := Account{Name: "bob", UID: 5000, GID: 5000, Home: "/home/bob"}
	b, _ := l.Start(context.Background(), bob, def)

	if got := l.List("alice"); len(got) != 1 || got[0].ID != a.ID {
		t.Errorf("alice lists %v", got)
	}
	if got := l.List("mallory"); len(got) != 0 {
		t.Errorf("mallory lists %v", got)
	}
	if err := l.Quit(context.Background(), "alice", b.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("alice quitting bob's session: %v, want ErrNotFound", err)
	}
	if _, err := l.Resolve(req("bob"), b.ID); err != nil {
		t.Errorf("bob's session gone after alice's attempt: %v", err)
	}
	if err := l.Quit(context.Background(), "alice", a.ID); err != nil {
		t.Fatalf("alice quitting her own: %v", err)
	}
	if !slices.Contains(be.removed, a.ContainerID) || slices.Contains(be.removed, b.ContainerID) {
		t.Errorf("removed = %v", be.removed)
	}
	if _, err := l.Resolve(req("alice"), a.ID); !errors.Is(err, proxy.ErrNoSession) {
		t.Errorf("quit session still resolves: %v", err)
	}
}

// End to end through the real proxy: alice reaches her session, with the prefix
// stripped; bob gets the same 404 as a session that doesn't exist.
func TestThroughTheProxy(t *testing.T) {
	def, _ := ideServer(t, 200)
	l := newLauncher(newFakeBackend())
	s, err := l.Start(context.Background(), alice, def)
	if err != nil {
		t.Fatal(err)
	}
	front := httptest.NewServer(proxy.New(l, proxy.Options{Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}))
	defer front.Close()
	get := func(user, path string) (int, string) {
		r, _ := http.NewRequest("GET", front.URL+path, nil)
		if user != "" {
			r.Header.Set("X-Test-User", user)
		}
		resp, err := http.DefaultClient.Do(r)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
	if code, body := get("alice", "/s/"+s.ID+"/x"); code != 200 || body != "ide:/x" {
		t.Errorf("alice: %d %q", code, body)
	}
	codeB, bodyB := get("bob", "/s/"+s.ID+"/x")
	codeN, bodyN := get("bob", "/s/"+session.NewID()+"/x")
	if codeB != 404 || codeB != codeN || bodyB != bodyN {
		t.Errorf("bob on alice's session: %d %q; on a missing one: %d %q; want identical 404s", codeB, bodyB, codeN, bodyN)
	}
	if code, _ := get("", "/s/"+s.ID+"/x"); code != 404 {
		t.Errorf("not signed in: %d", code)
	}
}

// A start the caller cancels says so, not that the IDE was slow (atelier-3b90b60-N1).
func TestCanceledStartSaysCanceled(t *testing.T) {
	def, _ := ideServer(t, 503) // never healthy
	be := newFakeBackend()
	l := New(be, headerIdentify, Options{HealthTimeout: time.Minute, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	_, err := l.Start(ctx, alice, def)
	if !errors.Is(err, context.DeadlineExceeded) || strings.Contains(err.Error(), "not healthy") {
		t.Errorf("err = %v, want the caller's context error", err)
	}
	if be.live() != 0 {
		t.Error("container left behind")
	}
}
