package launcher

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/gkroken/Atelier/internal/backend/docker"
	"github.com/gkroken/Atelier/internal/ide"
	"github.com/gkroken/Atelier/internal/proxy"
)

// liveSession starts def for the test's own account through the launcher and returns
// the proxy's URL and the session. Everything is cleaned up after the test.
func liveSession(t *testing.T, def ide.Definition, netName string) (front string, s *Session, acct Account) {
	t.Helper()
	acct = liveAccount(t)
	be := docker.New(docker.DefaultSocket, netName)
	t.Cleanup(func() { removeNetwork(netName) })
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	l := New(be, headerIdentify, Options{Logger: quiet})
	srv := httptest.NewServer(proxy.New(l, proxy.Options{Logger: quiet}))
	t.Cleanup(srv.Close)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	start := time.Now()
	s, err := l.Start(ctx, acct, def)
	if err != nil {
		t.Fatalf("Start %s: %v", def.Name, err)
	}
	t.Cleanup(func() { _ = l.Quit(context.Background(), acct.Name, s.ID) })
	t.Logf("%s healthy after %v", def.Name, time.Since(start).Round(time.Millisecond))
	return srv.URL, s, acct
}

// wsDial opens a websocket through the proxy at front to path, with the client's
// cookies, and returns the connection after a 101. Minimal RFC 6455 client: enough to
// show the upgrade reaches a real IDE and frames flow both ways.
func wsDial(t *testing.T, c *http.Client, front, path string) (net.Conn, *bufio.Reader) {
	t.Helper()
	conn, err := net.Dial("tcp", strings.TrimPrefix(front, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	conn.SetDeadline(time.Now().Add(30 * time.Second))
	key := make([]byte, 16)
	rand.Read(key)
	u, _ := http.NewRequest("GET", front+path, nil)
	var cookies []string
	for _, ck := range c.Jar.Cookies(u.URL) {
		cookies = append(cookies, ck.Name+"="+ck.Value)
	}
	host := strings.TrimPrefix(front, "http://")
	fmt.Fprintf(conn, "GET %s HTTP/1.1\r\nHost: %s\r\nOrigin: %s\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n"+
		"Sec-WebSocket-Key: %s\r\nSec-WebSocket-Version: 13\r\nUser-Agent: %s\r\nX-Test-User: %s\r\nCookie: %s\r\n\r\n",
		path, host, front, base64.StdEncoding.EncodeToString(key), browserUA, c.Transport.(asUser).user, strings.Join(cookies, "; "))
	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusSwitchingProtocols {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 300))
		t.Fatalf("websocket %s: %d %s", path, resp.StatusCode, b)
	}
	return conn, br
}

// TestLiveCodeServerThroughTheProxy: code-server's workbench page and a static asset load
// through /s/<id>/, and its websocket upgrades through the proxy.
//
//	ATELIER_DOCKER_LIVE=1 scripts/test.sh -run TestLiveCodeServer ./internal/launcher/
func TestLiveCodeServerThroughTheProxy(t *testing.T) {
	front, s, acct := liveSession(t, ide.CodeServer, "atelier-live-cs-net")
	c := browser(t, acct.Name)
	resp, err := c.Get(front + "/s/" + s.ID + "/")
	if err != nil {
		t.Fatal(err)
	}
	page, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || !strings.HasPrefix(resp.Request.URL.Path, "/s/"+s.ID+"/") || !strings.Contains(string(page), "workbench") {
		t.Fatalf("main page: %d at %s: %.300s", resp.StatusCode, resp.Request.URL, page)
	}
	// One of the page's own relative asset URLs must load through the prefix.
	m := regexp.MustCompile(`(?:src|href)="(\./_static/[^"]+)"`).FindSubmatch(page)
	if m == nil {
		t.Fatalf("no relative asset in the page: %.500s", page)
	}
	asset := resp.Request.URL.ResolveReference(mustParse(t, string(m[1])))
	ar, err := c.Get(asset.String())
	if err != nil {
		t.Fatal(err)
	}
	ar.Body.Close()
	if ar.StatusCode != 200 || !strings.HasPrefix(asset.Path, "/s/"+s.ID+"/") {
		t.Errorf("asset %s: %d", asset.Path, ar.StatusCode)
	}
	// code-server's remote-connection websocket: the upgrade must reach code-server.
	conn, _ := wsDial(t, c, front, "/s/"+s.ID+"/?reconnectionToken=atelier-test&reconnection=false&skipWebSocketFrames=false")
	conn.Close()

	// State in a folder of the session's own; settings shared with every session.
	base := filepath.Join(acct.Home, ".local/share/code-server")
	link := filepath.Join(base, "atelier", s.ID, "User/settings.json")
	if dest, err := os.Readlink(link); err != nil || dest != filepath.Join(base, "User/settings.json") {
		t.Errorf("%s -> %q (%v), want a link to the shared settings", link, dest, err)
	}
}

func mustParse(t *testing.T, s string) *url.URL {
	t.Helper()
	u, err := url.Parse(s)
	if err != nil {
		t.Fatal(err)
	}
	return u
}

// wsWriteText sends one masked text frame (clients must mask, RFC 6455 5.3).
func wsWriteText(t *testing.T, conn net.Conn, msg []byte) {
	t.Helper()
	hdr := []byte{0x81}
	switch n := len(msg); {
	case n < 126:
		hdr = append(hdr, 0x80|byte(n))
	case n < 1<<16:
		hdr = append(hdr, 0x80|126, byte(n>>8), byte(n))
	default:
		t.Fatalf("message too long: %d", n)
	}
	mask := make([]byte, 4)
	rand.Read(mask)
	hdr = append(hdr, mask...)
	body := make([]byte, len(msg))
	for i := range msg {
		body[i] = msg[i] ^ mask[i%4]
	}
	if _, err := conn.Write(append(hdr, body...)); err != nil {
		t.Fatal(err)
	}
}

// wsReadText reads frames until a complete text message arrives; control frames are
// skipped (a ping isn't answered; the test is short).
func wsReadText(t *testing.T, br *bufio.Reader) []byte {
	t.Helper()
	var msg []byte
	for {
		h := make([]byte, 2)
		if _, err := io.ReadFull(br, h); err != nil {
			t.Fatal(err)
		}
		fin, op := h[0]&0x80 != 0, h[0]&0x0f
		n := uint64(h[1] & 0x7f)
		switch n {
		case 126:
			b := make([]byte, 2)
			io.ReadFull(br, b)
			n = uint64(b[0])<<8 | uint64(b[1])
		case 127:
			b := make([]byte, 8)
			io.ReadFull(br, b)
			n = 0
			for _, x := range b {
				n = n<<8 | uint64(x)
			}
		}
		if h[1]&0x80 != 0 {
			t.Fatal("server frame is masked")
		}
		payload := make([]byte, n)
		if _, err := io.ReadFull(br, payload); err != nil {
			t.Fatal(err)
		}
		if op >= 0x8 {
			if op == 0x8 {
				t.Fatalf("server closed the websocket: %q", payload)
			}
			continue
		}
		msg = append(msg, payload...)
		if fin {
			return msg
		}
	}
}

// TestLiveJupyterLabKernelOverWebsocket: JupyterLab loads through /s/<id>/ (prefix kept),
// a Python kernel starts through its REST API, and a kernel_info_request sent over the
// kernel's websocket channel gets a kernel_info_reply back: messages both ways over a
// real IDE's websocket, through the launcher and proxy (atelier-b5a2831-N1).
//
//	ATELIER_DOCKER_LIVE=1 scripts/test.sh -run TestLiveJupyterLab ./internal/launcher/
func TestLiveJupyterLabKernelOverWebsocket(t *testing.T) {
	front, s, acct := liveSession(t, ide.JupyterLab, "atelier-live-jl-net")
	c := browser(t, acct.Name)
	prefix := front + "/s/" + s.ID
	resp, err := c.Get(prefix + "/lab")
	if err != nil {
		t.Fatal(err)
	}
	page, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || !strings.Contains(string(page), "JupyterLab") {
		t.Fatalf("/lab: %d at %s: %.300s", resp.StatusCode, resp.Request.URL, page)
	}
	xsrf := ""
	for _, ck := range c.Jar.Cookies(resp.Request.URL) {
		if ck.Name == "_xsrf" {
			xsrf = ck.Value
		}
	}
	if xsrf == "" {
		t.Fatal("no _xsrf cookie")
	}
	rq, _ := http.NewRequest("POST", prefix+"/api/kernels", strings.NewReader(`{"name":"python3"}`))
	rq.Header.Set("Content-Type", "application/json")
	rq.Header.Set("X-XSRFToken", xsrf)
	resp, err = c.Do(rq)
	if err != nil {
		t.Fatal(err)
	}
	var kernel struct{ ID string }
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 201 || json.Unmarshal(body, &kernel) != nil || kernel.ID == "" {
		t.Fatalf("start kernel: %d %.300s", resp.StatusCode, body)
	}

	conn, br := wsDial(t, c, front, "/s/"+s.ID+"/api/kernels/"+kernel.ID+"/channels?session_id=atelier-test")
	req := map[string]any{
		"header": map[string]any{"msg_id": "atelier-1", "username": acct.Name, "session": "atelier-test",
			"msg_type": "kernel_info_request", "version": "5.3", "date": time.Now().UTC().Format(time.RFC3339)},
		"parent_header": map[string]any{}, "metadata": map[string]any{}, "content": map[string]any{},
		"channel": "shell", "buffers": []any{},
	}
	out, _ := json.Marshal(req)
	start := time.Now()
	wsWriteText(t, conn, out)
	for {
		var m struct {
			Channel string
			Header  struct {
				MsgType string `json:"msg_type"`
			}
			ParentHeader struct {
				MsgID string `json:"msg_id"`
			} `json:"parent_header"`
			Content struct {
				Status   string
				Language struct{ Name string } `json:"language_info"`
			}
		}
		raw := wsReadText(t, br)
		if err := json.Unmarshal(raw, &m); err != nil {
			t.Fatalf("not JSON: %.200s", raw)
		}
		if m.Header.MsgType == "kernel_info_reply" && m.ParentHeader.MsgID == "atelier-1" {
			if m.Channel != "shell" || m.Content.Status != "ok" || m.Content.Language.Name != "python" {
				t.Errorf("reply: %.300s", raw)
			}
			t.Logf("kernel_info_reply over the websocket after %v", time.Since(start).Round(time.Millisecond))
			return
		}
	}
}
