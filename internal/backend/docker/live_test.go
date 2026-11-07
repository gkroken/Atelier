package docker

import (
	"context"
	"fmt"
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
	if os.Getenv("ATELIER_DOCKER_LIVE") != "1" {
		t.Skip("set ATELIER_DOCKER_LIVE=1 to run against the local Docker daemon")
	}
	uid, gid := os.Getuid(), os.Getgid()
	if uid == 0 {
		t.Skip("run as a non-root user: sessions never run as root")
	}
	image := os.Getenv("ATELIER_LIVE_IMAGE")
	if image == "" {
		image = "rocker/rstudio:4.5"
	}
	home := t.TempDir()
	if err := os.Chmod(home, 0o700); err != nil {
		t.Fatal(err)
	}
	passwd := fmt.Sprintf("tester:x:%d:%d::/home/tester:/bin/sh\n", uid, gid)

	b := New(DefaultSocket)
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
