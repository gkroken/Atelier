package docker

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/gkroken/Atelier/internal/backend"
)

// TestLive runs a real container. It needs a Docker daemon and an image already on the
// host, so it runs only when asked:
//
//	ATELIER_DOCKER_LIVE=1 [ATELIER_LIVE_IMAGE=rocker/rstudio:4.5] scripts/test.sh -run TestLive ./internal/backend/docker/
//
// It runs the container as the test's own account ID, with a scratch home under a temp
// dir, and checks from the outside what the container saw.
func TestLive(t *testing.T) {
	image := liveImage(t)
	uid, gid := os.Getuid(), os.Getgid()
	home := t.TempDir()
	if err := os.Chmod(home, 0o700); err != nil {
		t.Fatal(err)
	}
	passwd := fmt.Sprintf("tester:x:%d:%d::/home/tester:/bin/sh\n", uid, gid)

	b := liveBackend(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	id, err := b.Create(ctx, backend.Spec{
		Image:      image,
		Entrypoint: []string{"/bin/sh", "-c"},
		Cmd: []string{`set -e
			id -u > out.uid; id -g > out.gid; id -un > out.name
			cp /etc/passwd out.passwd
			grep '^CapEff:' /proc/self/status > out.caps
			grep -c ' /home/tester ' /proc/self/mountinfo > out.homemounts`},
		UID:    uid,
		GID:    gid,
		Home:   backend.Mount{Source: home, Target: "/home/tester"},
		Files:  []backend.File{{Path: "/etc/passwd", Mode: 0o644, Content: []byte(passwd)}},
		Labels: map[string]string{"atelier.test": "live"},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	t.Cleanup(func() {
		if err := b.Remove(context.Background(), id); err != nil {
			t.Errorf("Remove: %v", err)
		}
	})
	if err := b.Start(ctx, id); err != nil {
		t.Fatalf("Start: %v", err)
	}
	var st backend.State
	for {
		if st, err = b.Inspect(ctx, id); err != nil {
			t.Fatalf("Inspect: %v", err)
		}
		if !st.Running {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal("container still running after 60 s")
		case <-time.After(100 * time.Millisecond):
		}
	}
	if st.ExitCode != 0 {
		t.Fatalf("container exited %d", st.ExitCode)
	}

	read := func(name string) string {
		t.Helper()
		p := filepath.Join(home, name)
		data, err := os.ReadFile(p)
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		fi, _ := os.Stat(p)
		if sys := fi.Sys().(*syscall.Stat_t); int(sys.Uid) != uid || int(sys.Gid) != gid {
			t.Errorf("%s owned by %d:%d on the host, want %d:%d", name, sys.Uid, sys.Gid, uid, gid)
		}
		return strings.TrimSpace(string(data))
	}
	if got := read("out.uid"); got != fmt.Sprint(uid) {
		t.Errorf("uid in container = %s, want %d", got, uid)
	}
	if got := read("out.gid"); got != fmt.Sprint(gid) {
		t.Errorf("gid in container = %s, want %d", got, gid)
	}
	if got := read("out.name"); got != "tester" {
		t.Errorf("user name in container = %s, want tester (from the copied passwd)", got)
	}
	if got := read("out.passwd"); got+"\n" != passwd {
		t.Errorf("container /etc/passwd = %q, want only the owner's line", got)
	}
	if got := read("out.caps"); got != "CapEff:\t0000000000000000" {
		t.Errorf("effective capabilities = %q, want none", got)
	}
	if got := read("out.homemounts"); got != "1" {
		t.Errorf("home mounted %s times, want 1", got)
	}
}

const liveNet = "atelier-live-test-net"

// liveImage skips unless live tests were asked for, and returns the image to use.
func liveImage(t *testing.T) string {
	t.Helper()
	if os.Getenv("ATELIER_DOCKER_LIVE") != "1" {
		t.Skip("set ATELIER_DOCKER_LIVE=1 to run against the local Docker daemon")
	}
	if os.Getuid() == 0 {
		t.Skip("run as a non-root user: sessions never run as root")
	}
	if image := os.Getenv("ATELIER_LIVE_IMAGE"); image != "" {
		return image
	}
	return "rocker/rstudio:4.5"
}

// liveBackend returns a Backend on a scratch session network, removed after the test.
func liveBackend(t *testing.T) *Backend {
	t.Helper()
	b := New(DefaultSocket, liveNet)
	t.Cleanup(func() {
		// Best effort: another live test may still have containers on it.
		_ = b.do(context.Background(), "DELETE", "/networks/"+liveNet, nil, "", nil)
	})
	return b
}

// TestLiveSessionsCannotReachEachOther starts two sessions on the session network: B
// serves HTTP, A tries to connect to it. The host reaching B is the control that shows B
// was listening, so A's failure is the network refusing, not B being down.
func TestLiveSessionsCannotReachEachOther(t *testing.T) {
	image := liveImage(t)
	b := liveBackend(t)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	run := func(name string, env []string, script string) (string, string) {
		t.Helper()
		home := t.TempDir()
		id, err := b.Create(ctx, backend.Spec{
			Image:      image,
			Entrypoint: []string{"/bin/bash", "-c"},
			Cmd:        []string{script},
			Env:        env,
			UID:        os.Getuid(),
			GID:        os.Getgid(),
			Home:       backend.Mount{Source: home, Target: "/home/" + name},
			Labels:     map[string]string{"atelier.test": "live"},
		})
		if err != nil {
			t.Fatalf("Create %s: %v", name, err)
		}
		t.Cleanup(func() { _ = b.Remove(context.Background(), id) })
		if err := b.Start(ctx, id); err != nil {
			t.Fatalf("Start %s: %v", name, err)
		}
		return id, home
	}

	bid, _ := run("bob", nil, "exec python3 -m http.server 8787")
	st, err := b.Inspect(ctx, bid)
	if err != nil || st.IP == "" {
		t.Fatalf("Inspect bob: %+v %v", st, err)
	}
	target := net.JoinHostPort(st.IP, "8787")
	// Control: the host (where the proxy runs) reaches bob.
	var hostErr error
	for i := 0; i < 100; i++ {
		var c net.Conn
		if c, hostErr = net.DialTimeout("tcp", target, time.Second); hostErr == nil {
			c.Close()
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if hostErr != nil {
		t.Fatalf("host can't reach bob at %s, so the test proves nothing: %v", target, hostErr)
	}

	aid, ahome := run("alice", []string{"TARGET_IP=" + st.IP},
		`if timeout 3 bash -c 'echo > /dev/tcp/$TARGET_IP/8787'; then echo reached; else echo blocked; fi > out`)
	for {
		ast, err := b.Inspect(ctx, aid)
		if err != nil {
			t.Fatalf("Inspect alice: %v", err)
		}
		if !ast.Running {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal("alice still running")
		case <-time.After(100 * time.Millisecond):
		}
	}
	out, err := os.ReadFile(filepath.Join(ahome, "out"))
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(string(out)); got != "blocked" {
		t.Errorf("alice's session connecting to bob's %s: %s, want blocked", target, got)
	}
}
