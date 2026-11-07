package launcher

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"os/user"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gkroken/Atelier/internal/backend"
	"github.com/gkroken/Atelier/internal/backend/docker"
	"github.com/gkroken/Atelier/internal/ide"
	"github.com/gkroken/Atelier/internal/proxy"
)

const browserUA = "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/130.0 Safari/537.36"

// liveAccount is the test's own OS account with a scratch home, which is what the spike
// binary does too. Skips unless live tests were asked for.
func liveAccount(t *testing.T) Account {
	t.Helper()
	if os.Getenv("ATELIER_DOCKER_LIVE") != "1" {
		t.Skip("set ATELIER_DOCKER_LIVE=1 to run against the local Docker daemon")
	}
	u, err := user.Current()
	if err != nil {
		t.Fatal(err)
	}
	if u.Uid == "0" {
		t.Skip("run as a non-root user: sessions never run as root")
	}
	uid, _ := strconv.Atoi(u.Uid)
	gid, _ := strconv.Atoi(u.Gid)
	group := ""
	if g, err := user.LookupGroupId(u.Gid); err == nil {
		group = g.Name
	}
	home := t.TempDir()
	if err := os.Chmod(home, 0o700); err != nil {
		t.Fatal(err)
	}
	return Account{Name: u.Username, UID: uid, GID: gid, Group: group, Home: home}
}

// asUser adds the test's identity header to every request, and a browser user agent.
type asUser struct {
	user string
	next http.RoundTripper
}

func (a asUser) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("X-Test-User", a.user)
	r.Header.Set("User-Agent", browserUA)
	return a.next.RoundTrip(r)
}

func browser(t *testing.T, user string) *http.Client {
	jar, _ := cookiejar.New(nil)
	return &http.Client{Jar: jar, Timeout: 30 * time.Second, Transport: asUser{user, http.DefaultTransport}}
}

// TestLiveRStudioThroughTheProxy starts RStudio with the launcher and uses it the way a
// browser's first visit does, through the proxy: follow the redirects to the main page,
// then the client_init RPC, which starts the R session. Another user gets a 404. Quit
// removes the container.
//
//	ATELIER_DOCKER_LIVE=1 scripts/test.sh -run TestLive ./internal/launcher/
func TestLiveRStudioThroughTheProxy(t *testing.T) {
	acct := liveAccount(t)
	const netName = "atelier-live-launcher-net"
	be := docker.New(docker.DefaultSocket, netName)
	t.Cleanup(func() { removeNetwork(netName) })
	l := New(be, headerIdentify, Options{Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	front := httptest.NewServer(proxy.New(l, proxy.Options{Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}))
	defer front.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	start := time.Now()
	s, err := l.Start(ctx, acct, ide.RStudio)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = l.Quit(context.Background(), acct.Name, s.ID) })
	healthy := time.Since(start)

	c := browser(t, acct.Name)
	resp, err := c.Get(front.URL + "/s/" + s.ID + "/")
	if err != nil {
		t.Fatal(err)
	}
	page, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	final := resp.Request.URL
	if resp.StatusCode != 200 || !strings.HasPrefix(final.Path, "/s/"+s.ID+"/") || final.Host != strings.TrimPrefix(front.URL, "http://") {
		t.Fatalf("main page: %d at %s (want 200 under /s/%s/ on the proxy's host): %.200s", resp.StatusCode, final, s.ID, page)
	}
	pageLoaded := time.Since(start)

	// client_init, with RStudio's CSRF token from the cookie it set under /s/<id>.
	base, _ := url.Parse(front.URL + "/s/" + s.ID + "/")
	csrf := ""
	for _, ck := range c.Jar.Cookies(base) {
		if ck.Name == "rs-csrf-token" {
			csrf = ck.Value
		}
	}
	if csrf == "" {
		t.Fatalf("no rs-csrf-token cookie under %s", base.Path)
	}
	rq, _ := http.NewRequest("POST", front.URL+"/s/"+s.ID+"/rpc/client_init",
		strings.NewReader(`{"method":"client_init","params":[],"clientId":"x","clientVersion":""}`))
	rq.Header.Set("Content-Type", "application/json")
	rq.Header.Set("X-RS-CSRF-Token", csrf)
	resp, err = c.Do(rq)
	if err != nil {
		t.Fatal(err)
	}
	out, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || !strings.HasPrefix(string(out), `{"result"`) {
		t.Fatalf("client_init: %d %.200s", resp.StatusCode, out)
	}
	t.Logf("healthy %v, main page %v, R session up %v after Start was called",
		healthy.Round(time.Millisecond), pageLoaded.Round(time.Millisecond), time.Since(start).Round(time.Millisecond))

	// Another user: the same 404 as for nothing at all.
	resp, err = browser(t, "someone-else").Get(front.URL + "/s/" + s.ID + "/")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 404 {
		t.Errorf("another user got %d, want 404", resp.StatusCode)
	}

	if err := l.Quit(ctx, acct.Name, s.ID); err != nil {
		t.Fatalf("Quit: %v", err)
	}
	if _, err := be.Inspect(ctx, s.ContainerID); !errors.Is(err, backend.ErrNotFound) {
		t.Errorf("container still there after Quit: %v", err)
	}
}

// removeNetwork deletes a scratch network through the Docker API, best effort.
func removeNetwork(name string) {
	c := docker.New(docker.DefaultSocket, name)
	_ = c.RemoveNetwork(context.Background())
}
