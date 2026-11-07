package ide

import (
	"context"
	"crypto/rand"
	"encoding/base32"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gkroken/Atelier/internal/backend"
	"github.com/gkroken/Atelier/internal/backend/docker"
)

// TestLiveRStudioSessionsKeepSeparateState starts two RStudio sessions from the RStudio
// definition on one home folder, signs in to both and starts their R sessions, then
// checks each has its own RStudio state folder (atelier-10f7686-F1) and none was made
// in the shared default location. Needs Docker and the definition's image on the host:
//
//	ATELIER_DOCKER_LIVE=1 scripts/test.sh -run TestLive ./internal/ide/
//
// The requests go straight to the container IP with the prefix already stripped, which
// is what the proxy will send.
func TestLiveRStudioSessionsKeepSeparateState(t *testing.T) {
	if os.Getenv("ATELIER_DOCKER_LIVE") != "1" {
		t.Skip("set ATELIER_DOCKER_LIVE=1 to run against the local Docker daemon")
	}
	uid, gid := os.Getuid(), os.Getgid()
	if uid == 0 {
		t.Skip("run as a non-root user: sessions never run as root")
	}
	const net_ = "atelier-live-ide-net"
	b := docker.New(docker.DefaultSocket, net_)
	t.Cleanup(func() {
		// Best effort, after the containers (cleanups run last-in first-out).
		c := &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", docker.DefaultSocket)
		}}}
		req, _ := http.NewRequest(http.MethodDelete, "http://docker/"+docker.APIVersion+"/networks/"+net_, nil)
		if resp, err := c.Do(req); err == nil {
			resp.Body.Close()
		}
	})
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	home := t.TempDir()
	if err := os.Chmod(home, 0o700); err != nil {
		t.Fatal(err)
	}
	d := RStudio
	ids := []string{newID(t), newID(t)}
	for _, sid := range ids {
		start := time.Now()
		p := Params{SessionID: sid, User: "alice", Home: "/home/alice", Prefix: "/s/" + sid}
		env, err := d.SessionEnv(p)
		if err != nil {
			t.Fatal(err)
		}
		cid, err := b.Create(ctx, backend.Spec{
			Image:      d.Image,
			Entrypoint: d.Entrypoint,
			Cmd:        d.Cmd,
			Env:        env,
			UID:        uid,
			GID:        gid,
			Home:       backend.Mount{Source: home, Target: p.Home},
			Files: []backend.File{
				{Path: "/etc/passwd", Mode: 0o644, Content: fmt.Appendf(nil, "alice:x:%d:%d::/home/alice:/bin/bash\n", uid, gid)},
				{Path: "/etc/group", Mode: 0o644, Content: fmt.Appendf(nil, "alice:x:%d:\n", gid)},
			},
			Labels: map[string]string{"atelier.test": "live"},
		})
		if err != nil {
			t.Fatalf("Create %s: %v", sid, err)
		}
		t.Cleanup(func() { _ = b.Remove(context.Background(), cid) })
		if err := b.Start(ctx, cid); err != nil {
			t.Fatalf("Start %s: %v", sid, err)
		}
		st, err := b.Inspect(ctx, cid)
		if err != nil || st.IP == "" {
			t.Fatalf("Inspect %s: %+v %v", sid, st, err)
		}
		base := "http://" + net.JoinHostPort(st.IP, strconv.Itoa(d.Port))
		waitHealthy(ctx, t, base, d.Health)
		healthy := time.Since(start)
		signInAndStartR(ctx, t, base)
		t.Logf("session %s: healthy after %v, R session up after %v", sid[:6], healthy.Round(time.Millisecond), time.Since(start).Round(time.Millisecond))
	}

	shared := filepath.Join(home, ".local/share/rstudio/sessions")
	if _, err := os.Stat(shared); err == nil {
		t.Errorf("%s exists: a session used the shared state folder", shared)
	}
	for _, sid := range ids {
		active, _ := filepath.Glob(filepath.Join(home, ".local/share/rstudio/atelier", sid, "sessions/active/session-*"))
		if len(active) != 1 {
			t.Errorf("session %s: active session folders = %v, want exactly one under its own data folder", sid, active)
		}
	}
}

// waitHealthy polls the definition's health check until it passes.
func waitHealthy(ctx context.Context, t *testing.T, base string, h Health) {
	t.Helper()
	c := noRedirects()
	for {
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, base+h.Path, nil)
		if resp, err := c.Do(req); err == nil {
			resp.Body.Close()
			if resp.StatusCode <= h.MaxStatus {
				return
			}
		}
		select {
		case <-ctx.Done():
			t.Fatalf("%s never became healthy", base)
		case <-time.After(50 * time.Millisecond):
		}
	}
}

// signInAndStartR does what a browser's first visit does: the auth-none sign-in, which
// sets RStudio's cookies, then the client_init RPC, which starts rsession.
func signInAndStartR(ctx context.Context, t *testing.T, base string) {
	t.Helper()
	const ua = "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/130.0 Safari/537.36"
	c := noRedirects()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, base+"/auth-sign-in?appUri=%2F", nil)
	req.Header.Set("User-Agent", ua)
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	var cookies []string
	csrf := ""
	for _, ck := range resp.Cookies() {
		cookies = append(cookies, ck.Name+"="+ck.Value)
		if ck.Name == "rs-csrf-token" {
			csrf = ck.Value
		}
	}
	if csrf == "" {
		t.Fatalf("sign-in set no CSRF cookie (status %d)", resp.StatusCode)
	}
	body := `{"method":"client_init","params":[],"clientId":"x","clientVersion":""}`
	req, _ = http.NewRequestWithContext(ctx, http.MethodPost, base+"/rpc/client_init", strings.NewReader(body))
	req.Header.Set("User-Agent", ua)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Cookie", strings.Join(cookies, "; "))
	req.Header.Set("X-RS-CSRF-Token", csrf)
	resp, err = c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	out, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || !strings.HasPrefix(string(out), `{"result"`) {
		t.Fatalf("client_init: %d %.200s", resp.StatusCode, out)
	}
}

func noRedirects() *http.Client {
	return &http.Client{
		Timeout:       30 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

func newID(t *testing.T) string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return strings.ToLower(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(b))
}
