package docker

import (
	"archive/tar"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gkroken/Atelier/internal/backend"
)

// fakeDaemon is a Docker Engine API stand-in on a unix socket. It records every request
// and answers from handlers keyed by "METHOD /path" (API version stripped).
type fakeDaemon struct {
	mu       sync.Mutex
	calls    []string
	bodies   map[string][]byte
	handlers map[string]func(w http.ResponseWriter, body []byte)
}

func newFake(t *testing.T) (*fakeDaemon, *Backend) {
	t.Helper()
	// Short dir: unix socket paths are limited to ~108 bytes and t.TempDir can be long.
	dir, err := os.MkdirTemp("", "atd")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	sock := filepath.Join(dir, "d.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeDaemon{bodies: map[string][]byte{}, handlers: map[string]func(http.ResponseWriter, []byte){}}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := r.Method + " " + strings.TrimPrefix(r.URL.Path, "/"+APIVersion)
		if r.URL.RawQuery != "" {
			key += "?" + r.URL.RawQuery
		}
		body, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		f.calls = append(f.calls, key)
		f.bodies[key] = body
		h := f.handlers[key]
		f.mu.Unlock()
		if h == nil {
			w.WriteHeader(http.StatusNotFound)
			io.WriteString(w, `{"message":"no such thing"}`)
			return
		}
		h(w, body)
	})}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })
	return f, New(sock)
}

func (f *fakeDaemon) on(key string, status int, resp string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.handlers[key] = func(w http.ResponseWriter, _ []byte) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		io.WriteString(w, resp)
	}
}

func (f *fakeDaemon) callList() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

func spec() backend.Spec {
	return backend.Spec{
		Name:  "atelier-test",
		Image: "rocker/rstudio:4.5",
		Cmd:   []string{"rserver"},
		Env:   []string{"USER=alice"},
		UID:   4321,
		GID:   4322,
		Home:  backend.Mount{Source: "/home/alice", Target: "/home/alice"},
		Files: []backend.File{
			{Path: "/etc/passwd", Mode: 0o644, Content: []byte("alice:x:4321:4322::/home/alice:/bin/sh\n")},
			{Path: "/etc/group", Mode: 0o644, Content: []byte("alice:x:4322:\n")},
		},
		Labels: map[string]string{"atelier.session": "s1"},
	}
}

func TestCreateSendsOnlyTheHomeMountAndNoPrivileges(t *testing.T) {
	f, b := newFake(t)
	f.on("GET /images/rocker/rstudio:4.5/json", 200, `{"Id":"sha256:img"}`)
	f.on("POST /containers/create?name=atelier-test", 201, `{"Id":"c0ffee"}`)
	f.on("PUT /containers/c0ffee/archive?path=%2F", 200, ``)

	id, err := b.Create(context.Background(), spec())
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if id != "c0ffee" {
		t.Fatalf("id = %q", id)
	}

	// Decode generically so a field we don't model (Binds, PortBindings, Privileged...)
	// still shows up if it is ever sent.
	var got map[string]any
	if err := json.Unmarshal(f.bodies["POST /containers/create?name=atelier-test"], &got); err != nil {
		t.Fatal(err)
	}
	if got["User"] != "4321:4322" {
		t.Errorf("User = %v, want 4321:4322", got["User"])
	}
	if got["WorkingDir"] != "/home/alice" {
		t.Errorf("WorkingDir = %v", got["WorkingDir"])
	}
	hc := got["HostConfig"].(map[string]any)
	for _, k := range []string{"Binds", "PortBindings", "VolumesFrom", "Devices", "CapAdd", "NetworkMode", "PidMode", "IpcMode", "UsernsMode"} {
		if _, ok := hc[k]; ok {
			t.Errorf("HostConfig.%s is set: %v", k, hc[k])
		}
	}
	if hc["Privileged"] != false || hc["PublishAllPorts"] != false {
		t.Errorf("Privileged=%v PublishAllPorts=%v", hc["Privileged"], hc["PublishAllPorts"])
	}
	if caps, _ := hc["CapDrop"].([]any); len(caps) != 1 || caps[0] != "ALL" {
		t.Errorf("CapDrop = %v, want [ALL]", hc["CapDrop"])
	}
	if so, _ := hc["SecurityOpt"].([]any); len(so) != 1 || so[0] != "no-new-privileges" {
		t.Errorf("SecurityOpt = %v", hc["SecurityOpt"])
	}
	mounts, _ := hc["Mounts"].([]any)
	if len(mounts) != 1 {
		t.Fatalf("Mounts = %v, want exactly the home", hc["Mounts"])
	}
	m := mounts[0].(map[string]any)
	if m["Type"] != "bind" || m["Source"] != "/home/alice" || m["Target"] != "/home/alice" || m["ReadOnly"] != false {
		t.Errorf("mount = %v", m)
	}

	// The files went in as a tar of root-owned regular files at the given paths.
	tr := tar.NewReader(strings.NewReader(string(f.bodies["PUT /containers/c0ffee/archive?path=%2F"])))
	var names []string
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if h.Uid != 0 || h.Gid != 0 || h.Typeflag != tar.TypeReg || h.Mode != 0o644 {
			t.Errorf("%s: uid=%d gid=%d type=%c mode=%o", h.Name, h.Uid, h.Gid, h.Typeflag, h.Mode)
		}
		names = append(names, h.Name)
	}
	if strings.Join(names, ",") != "etc/group,etc/passwd" {
		t.Errorf("tar entries = %v", names)
	}
}

func TestCreateRefusesMissingImageWithoutPulling(t *testing.T) {
	f, b := newFake(t) // no handlers: the image lookup 404s
	_, err := b.Create(context.Background(), spec())
	if !errors.Is(err, backend.ErrImageNotPresent) {
		t.Fatalf("err = %v, want ErrImageNotPresent", err)
	}
	for _, c := range f.callList() {
		if strings.Contains(c, "/images/create") || strings.Contains(c, "/containers/create") {
			t.Errorf("unexpected call %s", c)
		}
	}
}

func TestCreateRefusesInvalidSpecBeforeTouchingDocker(t *testing.T) {
	f, b := newFake(t)
	s := spec()
	s.UID = 0
	_, err := b.Create(context.Background(), s)
	if !errors.Is(err, backend.ErrInvalidSpec) {
		t.Fatalf("err = %v, want ErrInvalidSpec", err)
	}
	if calls := f.callList(); len(calls) != 0 {
		t.Errorf("Docker was called: %v", calls)
	}
}

func TestCreateRemovesContainerWhenCopyFails(t *testing.T) {
	f, b := newFake(t)
	f.on("GET /images/rocker/rstudio:4.5/json", 200, `{}`)
	f.on("POST /containers/create?name=atelier-test", 201, `{"Id":"c0ffee"}`)
	f.on("PUT /containers/c0ffee/archive?path=%2F", 500, `{"message":"disk full"}`)
	f.on("DELETE /containers/c0ffee?force=1&v=1", 204, ``)
	if _, err := b.Create(context.Background(), spec()); err == nil || !strings.Contains(err.Error(), "disk full") {
		t.Fatalf("err = %v, want the copy error", err)
	}
	calls := f.callList()
	if calls[len(calls)-1] != "DELETE /containers/c0ffee?force=1&v=1" {
		t.Errorf("last call = %s, want the remove", calls[len(calls)-1])
	}
}

func TestStartStopTolerateNotModified(t *testing.T) {
	f, b := newFake(t)
	f.on("POST /containers/c1/start", 304, ``)
	f.on("POST /containers/c1/stop?t=5", 304, ``)
	if err := b.Start(context.Background(), "c1"); err != nil {
		t.Errorf("Start: %v", err)
	}
	if err := b.Stop(context.Background(), "c1", 5*time.Second); err != nil {
		t.Errorf("Stop: %v", err)
	}
}

func TestNotFoundIsErrNotFound(t *testing.T) {
	_, b := newFake(t)
	if _, err := b.Inspect(context.Background(), "nope"); !errors.Is(err, backend.ErrNotFound) {
		t.Errorf("Inspect: %v", err)
	}
	if err := b.Remove(context.Background(), "nope"); !errors.Is(err, backend.ErrNotFound) {
		t.Errorf("Remove: %v", err)
	}
}

func TestInspect(t *testing.T) {
	f, b := newFake(t)
	f.on("GET /containers/c1/json", 200, `{"State":{"Running":true,"ExitCode":0},
		"NetworkSettings":{"Networks":{"bridge":{"IPAddress":"172.17.0.5"}}}}`)
	st, err := b.Inspect(context.Background(), "c1")
	if err != nil {
		t.Fatal(err)
	}
	if !st.Running || st.IP != "172.17.0.5" {
		t.Errorf("state = %+v", st)
	}
}

func TestErrorMessageFromDaemon(t *testing.T) {
	f, b := newFake(t)
	f.on("POST /containers/c1/start", 500, `{"message":"cannot start: boom"}`)
	err := b.Start(context.Background(), "c1")
	var ae *apiError
	if !errors.As(err, &ae) || ae.Status != 500 || ae.Message != "cannot start: boom" {
		t.Errorf("err = %v", err)
	}
}
