package launcher

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"testing"
	"time"

	"github.com/gkroken/Atelier/internal/backend/docker"
	"github.com/gkroken/Atelier/internal/ide"
	"github.com/gkroken/Atelier/internal/proxy"
)

// TestLiveStartupBench is the spine #0 startup measurement. It starts RStudio N times in
// a row, each from an image already on the host, and times every start from the moment
// Start is called (the click) to:
//
//	healthy  rserver answers its health check
//	page     the main page has loaded through the proxy (redirects followed)
//	rsession the client_init RPC has returned, so the R session is up: the IDE is usable
//
// It doesn't include the browser parsing and rendering RStudio's JavaScript. Each
// session is quit before the next starts. All starts share one scratch home, like one
// user starting sessions on the same day; the first is also the first in that home.
//
//	ATELIER_DOCKER_LIVE=1 ATELIER_BENCH=20 scripts/test.sh -run TestLiveStartupBench ./internal/launcher/
func TestLiveStartupBench(t *testing.T) {
	n, _ := strconv.Atoi(os.Getenv("ATELIER_BENCH"))
	if n <= 0 {
		t.Skip("set ATELIER_BENCH=<starts> (and ATELIER_DOCKER_LIVE=1) to measure startup")
	}
	acct := liveAccount(t)
	const netName = "atelier-live-bench-net"
	be := docker.New(docker.DefaultSocket, netName)
	t.Cleanup(func() { removeNetwork(netName) })
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	l := New(be, headerIdentify, Options{Logger: quiet})
	front := httptest.NewServer(proxy.New(l, proxy.Options{Logger: quiet}))
	defer front.Close()

	var healthy, page, rsession []time.Duration
	for i := 0; i < n; i++ {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		start := time.Now()
		s, err := l.Start(ctx, acct, ide.RStudio)
		if err != nil {
			cancel()
			t.Fatalf("start %d: %v", i+1, err)
		}
		h := time.Since(start)
		p, r := openRStudio(t, front.URL, s.ID, acct.Name, start)
		healthy, page, rsession = append(healthy, h), append(page, p), append(rsession, r)
		t.Logf("start %2d: healthy %v  page %v  rsession %v", i+1, ms(h), ms(p), ms(r))
		if err := l.Quit(ctx, acct.Name, s.ID); err != nil {
			t.Fatalf("quit %d: %v", i+1, err)
		}
		cancel()
	}
	for _, m := range []struct {
		name string
		d    []time.Duration
	}{{"healthy", healthy}, {"page", page}, {"rsession", rsession}} {
		med, p95, max := stats(m.d)
		t.Logf("RESULT %-8s n=%d median %v p95 %v max %v", m.name, len(m.d), ms(med), ms(p95), ms(max))
	}
	host, _ := os.Hostname()
	t.Logf("RESULT host %s, image %s", host, ide.RStudio.Image)
}

func ms(d time.Duration) string { return fmt.Sprintf("%.2fs", d.Seconds()) }

// stats returns the median, the 95th percentile (nearest rank) and the maximum.
func stats(ds []time.Duration) (med, p95, max time.Duration) {
	s := append([]time.Duration(nil), ds...)
	sort.Slice(s, func(i, j int) bool { return s[i] < s[j] })
	n := len(s)
	if n%2 == 1 {
		med = s[n/2]
	} else {
		med = (s[n/2-1] + s[n/2]) / 2
	}
	rank := (95*n + 99) / 100 // ceil(0.95 n)
	return med, s[rank-1], s[n-1]
}

func TestStats(t *testing.T) {
	var ds []time.Duration
	for i := 1; i <= 20; i++ {
		ds = append(ds, time.Duration(i)*time.Second)
	}
	med, p95, max := stats(ds)
	if med != 10500*time.Millisecond || p95 != 19*time.Second || max != 20*time.Second {
		t.Errorf("stats = %v %v %v", med, p95, max)
	}
	if med, p95, _ := stats([]time.Duration{3, 1, 2}); med != 2 || p95 != 3 {
		t.Errorf("odd: %v %v", med, p95)
	}
}

// watcherScript runs in the first session: it sets an inotify watch on the home (the
// kernel facility RStudio's file pane uses), then serves HTTP so the health check passes
// only once the watch is in place. When from-second.txt is written and closed, it copies
// what it read into seen-by-first.
const watcherScript = `
import ctypes, os, struct, threading, http.server
libc = ctypes.CDLL("libc.so.6", use_errno=True)
fd = libc.inotify_init()
IN_CLOSE_WRITE, IN_MOVED_TO = 0x8, 0x80
home = os.environ["ATELIER_HOME"]
if fd < 0 or libc.inotify_add_watch(fd, home.encode(), IN_CLOSE_WRITE | IN_MOVED_TO) < 0:
    raise SystemExit("inotify failed: %d" % ctypes.get_errno())
def watch():
    while True:
        buf = os.read(fd, 4096); i = 0
        while i < len(buf):
            wd, mask, cookie, ln = struct.unpack_from("iIII", buf, i)
            name = buf[i+16:i+16+ln].rstrip(b"\0").decode(); i += 16 + ln
            if name == "from-second.txt":
                data = open(os.path.join(home, name)).read()
                with open(os.path.join(home, "seen-by-first.tmp"), "w") as f:
                    f.write("inotify:" + data)
                os.rename(os.path.join(home, "seen-by-first.tmp"), os.path.join(home, "seen-by-first"))
threading.Thread(target=watch, daemon=True).start()
http.server.ThreadingHTTPServer(("0.0.0.0", int(os.environ["ATELIER_PORT"])), http.server.SimpleHTTPRequestHandler).serve_forever()
`

// writerScript runs in the second session: write the file, then serve HTTP.
const writerScript = `
import os, http.server
with open(os.path.join(os.environ["ATELIER_HOME"], "from-second.txt"), "w") as f:
    f.write("written in " + os.environ["ATELIER_SESSION_ID"])
http.server.ThreadingHTTPServer(("0.0.0.0", int(os.environ["ATELIER_PORT"])), http.server.SimpleHTTPRequestHandler).serve_forever()
`

// TestLiveFileFromSecondSessionSeenInFirst is the spine #0 shared-home check, through the
// launcher's real spec (same home, same account, ICC-off network): a file written in a
// second session is seen by an inotify watch in the first (ADR-0007).
func TestLiveFileFromSecondSessionSeenInFirst(t *testing.T) {
	acct := liveAccount(t)
	const netName = "atelier-live-share-net"
	be := docker.New(docker.DefaultSocket, netName)
	t.Cleanup(func() { removeNetwork(netName) })
	l := New(be, headerIdentify, Options{Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	def := func(name, script string) ide.Definition {
		return ide.Definition{
			Name: name, Title: name, Image: ide.RStudio.Image, Port: 8000,
			Entrypoint: []string{"python3", "-c"}, Cmd: []string{script},
			StripPrefix: true, // the test servers serve from /
			Health:      ide.Health{Path: "/", MaxStatus: 399},
		}
	}
	first, err := l.Start(ctx, acct, def("watcher", watcherScript))
	if err != nil {
		t.Fatalf("first session: %v", err)
	}
	t.Cleanup(func() { _ = l.Quit(context.Background(), acct.Name, first.ID) })
	wrote := time.Now()
	second, err := l.Start(ctx, acct, def("writer", writerScript))
	if err != nil {
		t.Fatalf("second session: %v", err)
	}
	t.Cleanup(func() { _ = l.Quit(context.Background(), acct.Name, second.ID) })

	seen := filepath.Join(acct.Home, "seen-by-first")
	for {
		data, err := os.ReadFile(seen)
		if err == nil {
			if want := "inotify:written in " + second.ID; string(data) != want {
				t.Fatalf("first session saw %q, want %q", data, want)
			}
			t.Logf("first session's inotify watch saw the second's file within %v of starting the second", ms(time.Since(wrote)))
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal("the first session never saw the file written in the second")
		case <-time.After(20 * time.Millisecond):
		}
	}
}
