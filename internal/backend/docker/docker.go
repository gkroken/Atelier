// Package docker implements backend.Backend against the Docker Engine API over its unix
// socket, using only the standard library. It is the only package that talks to Docker
// (ADR-0003); the socket is root-equivalent, so nothing else should hold it.
package docker

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strconv"
	"syscall"
	"time"

	"github.com/gkroken/Atelier/internal/backend"
)

// APIVersion is the Engine API version requested. 1.44 is Docker 25 and later.
const APIVersion = "v1.44"

// DefaultSocket is where the Docker daemon listens on a standard Linux install.
const DefaultSocket = "/var/run/docker.sock"

// DefaultNetwork is the bridge network every session container joins (ADR-0015).
const DefaultNetwork = "atelier-sessions"

// iccOption is the bridge driver option that, set to "false", drops traffic between
// containers on the same bridge. The host can still reach every container, which is how
// the proxy dials them.
const iccOption = "com.docker.network.bridge.enable_icc"

// ErrNetworkNotIsolated: the session network exists but lets containers reach each
// other. Atelier refuses to start sessions on it rather than change it.
var ErrNetworkNotIsolated = errors.New("session network does not isolate containers")

// Backend talks to one Docker daemon.
type Backend struct {
	client  *http.Client
	network string
}

var _ backend.Backend = (*Backend)(nil)

// New returns a Backend that dials the daemon at socketPath and puts every session on the
// bridge network named network (DefaultNetwork in production), creating it on first use.
func New(socketPath, network string) *Backend {
	tr := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", socketPath)
		},
		MaxIdleConns:    4,
		IdleConnTimeout: 30 * time.Second,
	}
	return &Backend{client: &http.Client{Transport: tr}, network: network}
}

// createBody is the subset of POST /containers/create that Atelier sets. Fields left out
// get Docker's defaults; the ones that matter for isolation are set explicitly.
type createBody struct {
	Image      string            `json:"Image"`
	Entrypoint []string          `json:"Entrypoint,omitempty"`
	Cmd        []string          `json:"Cmd,omitempty"`
	Env        []string          `json:"Env,omitempty"`
	User       string            `json:"User"`
	WorkingDir string            `json:"WorkingDir"`
	Labels     map[string]string `json:"Labels,omitempty"`
	HostConfig hostConfig        `json:"HostConfig"`
}

type hostConfig struct {
	// Mounts, not Binds: structured, so no path can smuggle in mount options, and a
	// missing source is an error instead of a folder silently created as root.
	Mounts      []mount  `json:"Mounts"`
	Privileged  bool     `json:"Privileged"`
	CapDrop     []string `json:"CapDrop"`
	SecurityOpt []string `json:"SecurityOpt"`
	// PublishAllPorts false and no PortBindings: the session port is reachable only on
	// the container network, never on a host interface.
	PublishAllPorts bool `json:"PublishAllPorts"`
	// NetworkMode is always the isolated session network, never the default bridge
	// (where containers reach each other) or "host".
	NetworkMode string `json:"NetworkMode"`
}

type mount struct {
	Type        string      `json:"Type"`
	Source      string      `json:"Source"`
	Target      string      `json:"Target"`
	ReadOnly    bool        `json:"ReadOnly"`
	BindOptions bindOptions `json:"BindOptions"`
}

type bindOptions struct {
	// rprivate: mounts made later under the home on the host don't propagate in, and
	// nothing mounted inside the container propagates out.
	Propagation string `json:"Propagation"`
}

// Create implements backend.Backend.
func (b *Backend) Create(ctx context.Context, spec backend.Spec) (string, error) {
	if err := backend.Validate(spec); err != nil {
		return "", err
	}
	if err := checkHome(spec); err != nil {
		return "", err
	}
	// The Engine API never pulls on create (only the CLI does), but check explicitly so
	// the error says what is wrong and so a future API change cannot pull behind our back.
	// The image reference goes into the path unescaped, as the Docker CLI sends it: the
	// daemon matches /images/{name:.*}/json, and an escaped "/" is not found.
	if err := b.do(ctx, http.MethodGet, "/images/"+spec.Image+"/json", nil, "", nil); err != nil {
		if errors.Is(err, backend.ErrNotFound) {
			return "", fmt.Errorf("create container: %s: %w", spec.Image, backend.ErrImageNotPresent)
		}
		return "", fmt.Errorf("create container: inspect image %s: %w", spec.Image, err)
	}

	// Checked on every create, not cached: a network deleted and re-made without
	// isolation must not be used. It is one small local request.
	if err := b.ensureNetwork(ctx); err != nil {
		return "", fmt.Errorf("create container: %w", err)
	}

	body := createBody{
		Image:      spec.Image,
		Entrypoint: spec.Entrypoint,
		Cmd:        spec.Cmd,
		Env:        spec.Env,
		User:       strconv.Itoa(spec.UID) + ":" + strconv.Itoa(spec.GID),
		WorkingDir: spec.Home.Target,
		Labels:     spec.Labels,
		HostConfig: hostConfig{
			Mounts: []mount{{
				Type:        "bind",
				Source:      spec.Home.Source,
				Target:      spec.Home.Target,
				BindOptions: bindOptions{Propagation: "rprivate"},
			}},
			CapDrop:     []string{"ALL"},
			SecurityOpt: []string{"no-new-privileges"},
			NetworkMode: b.network,
		},
	}
	q := ""
	if spec.Name != "" {
		q = "name=" + url.QueryEscape(spec.Name)
	}
	var created struct {
		ID string `json:"Id"`
	}
	if err := b.do(ctx, http.MethodPost, "/containers/create", body, q, &created); err != nil {
		return "", fmt.Errorf("create container from %s: %w", spec.Image, err)
	}
	if len(spec.Files) > 0 {
		if err := b.copyFiles(ctx, created.ID, spec.Files); err != nil {
			// Don't leave a half-built container behind.
			_ = b.Remove(context.WithoutCancel(ctx), created.ID)
			return "", err
		}
	}
	return created.ID, nil
}

type networkInfo struct {
	Driver  string            `json:"Driver"`
	Options map[string]string `json:"Options"`
}

// ensureNetwork makes sure the session network exists with inter-container traffic off,
// creating it if it is missing. An existing network that isn't an ICC-off bridge is
// refused, not modified: someone made it on purpose, and Docker can't change the option
// on a live network anyway.
func (b *Backend) ensureNetwork(ctx context.Context) error {
	path := "/networks/" + url.PathEscape(b.network)
	var info networkInfo
	err := b.do(ctx, http.MethodGet, path, nil, "", &info)
	if errors.Is(err, backend.ErrNotFound) {
		body := map[string]any{
			"Name":    b.network,
			"Driver":  "bridge",
			"Options": map[string]string{iccOption: "false"},
			"Labels":  map[string]string{"atelier.network": "sessions"},
		}
		cerr := b.do(ctx, http.MethodPost, "/networks/create", body, "", nil)
		var ae *apiError
		if cerr != nil && !(errors.As(cerr, &ae) && ae.Status == http.StatusConflict) {
			return fmt.Errorf("create network %s: %w", b.network, cerr)
		}
		// Created now, or by a concurrent create (409): read back what is really there.
		err = b.do(ctx, http.MethodGet, path, nil, "", &info)
	}
	if err != nil {
		return fmt.Errorf("inspect network %s: %w", b.network, err)
	}
	if info.Driver != "bridge" || info.Options[iccOption] != "false" {
		return fmt.Errorf("%w: %s (driver %q, %s=%q)", ErrNetworkNotIsolated, b.network, info.Driver, iccOption, info.Options[iccOption])
	}
	return nil
}

// checkHome refuses a home that isn't a real directory owned by the session's account:
// a symlink, a file or socket, a folder owned by someone else (another user's home, /home
// itself, anything of root's such as /var/run). The launcher fills Home.Source from the
// OS account database, and this is the last point that sees the real host path.
//
// The last component is checked with Lstat; parents may be symlinks (/home -> /mnt/home
// is common). The owner can't swap the folder between this check and the mount, because
// that needs write access to its parent, and homes live in root-owned folders.
func checkHome(spec backend.Spec) error {
	bad := func(format string, a ...any) error {
		return fmt.Errorf("%w: home %s: %s", backend.ErrInvalidSpec, spec.Home.Source, fmt.Sprintf(format, a...))
	}
	fi, err := os.Lstat(spec.Home.Source)
	if err != nil {
		return bad("%v", err)
	}
	if !fi.IsDir() {
		return bad("not a directory (%s)", fi.Mode().Type())
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return bad("no owner information")
	}
	if int(st.Uid) != spec.UID {
		return bad("owned by UID %d, not the session's UID %d", st.Uid, spec.UID)
	}
	return nil
}

// copyFiles writes files into a created container with PUT /containers/{id}/archive.
func (b *Backend) copyFiles(ctx context.Context, id string, files []backend.File) error {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	// Sorted so the archive is the same for the same spec.
	sorted := append([]backend.File(nil), files...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Path < sorted[j].Path })
	for _, f := range sorted {
		hdr := &tar.Header{
			Name:     f.Path[1:], // relative to "/", the extraction root
			Mode:     f.Mode,
			Size:     int64(len(f.Content)),
			Typeflag: tar.TypeReg,
			ModTime:  time.Unix(0, 0),
		}
		if err := tw.WriteHeader(hdr); err != nil {
			return fmt.Errorf("copy files into %s: %w", short(id), err)
		}
		if _, err := tw.Write(f.Content); err != nil {
			return fmt.Errorf("copy files into %s: %w", short(id), err)
		}
	}
	if err := tw.Close(); err != nil {
		return fmt.Errorf("copy files into %s: %w", short(id), err)
	}
	path := "/containers/" + url.PathEscape(id) + "/archive"
	if err := b.do(ctx, http.MethodPut, path, &buf, "path=%2F", nil); err != nil {
		return fmt.Errorf("copy files into %s: %w", short(id), err)
	}
	return nil
}

// Start implements backend.Backend.
func (b *Backend) Start(ctx context.Context, id string) error {
	err := b.do(ctx, http.MethodPost, "/containers/"+url.PathEscape(id)+"/start", nil, "", nil)
	if err != nil && !errors.Is(err, errNotModified) {
		return fmt.Errorf("start container %s: %w", short(id), err)
	}
	return nil
}

// Stop implements backend.Backend.
func (b *Backend) Stop(ctx context.Context, id string, timeout time.Duration) error {
	q := "t=" + strconv.Itoa(int(timeout.Round(time.Second)/time.Second))
	err := b.do(ctx, http.MethodPost, "/containers/"+url.PathEscape(id)+"/stop", nil, q, nil)
	if err != nil && !errors.Is(err, errNotModified) {
		return fmt.Errorf("stop container %s: %w", short(id), err)
	}
	return nil
}

// Remove implements backend.Backend. Anonymous volumes go with the container; the home
// bind mount is not a volume and is never touched.
func (b *Backend) Remove(ctx context.Context, id string) error {
	if err := b.do(ctx, http.MethodDelete, "/containers/"+url.PathEscape(id), nil, "force=1&v=1", nil); err != nil {
		return fmt.Errorf("remove container %s: %w", short(id), err)
	}
	return nil
}

// Inspect implements backend.Backend.
func (b *Backend) Inspect(ctx context.Context, id string) (backend.State, error) {
	var out struct {
		State struct {
			Running  bool `json:"Running"`
			ExitCode int  `json:"ExitCode"`
		} `json:"State"`
		NetworkSettings struct {
			Networks map[string]struct {
				IPAddress string `json:"IPAddress"`
			} `json:"Networks"`
		} `json:"NetworkSettings"`
	}
	if err := b.do(ctx, http.MethodGet, "/containers/"+url.PathEscape(id)+"/json", nil, "", &out); err != nil {
		return backend.State{}, fmt.Errorf("inspect container %s: %w", short(id), err)
	}
	st := backend.State{Running: out.State.Running, ExitCode: out.State.ExitCode}
	// A session container is attached to exactly one network; take its address. Sorted
	// so the choice is stable if that ever changes.
	names := make([]string, 0, len(out.NetworkSettings.Networks))
	for n := range out.NetworkSettings.Networks {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		if ip := out.NetworkSettings.Networks[n].IPAddress; ip != "" {
			st.IP = ip
			break
		}
	}
	return st, nil
}

// errNotModified is Docker's 304: start of a running or stop of a stopped container.
var errNotModified = errors.New("not modified")

// apiError is a non-2xx answer from the daemon.
type apiError struct {
	Status  int
	Message string
}

func (e *apiError) Error() string { return fmt.Sprintf("docker: %d: %s", e.Status, e.Message) }

// do sends one request. body is JSON-encoded unless it is an io.Reader (sent as a tar).
// out, when non-nil, receives the decoded JSON response.
func (b *Backend) do(ctx context.Context, method, path string, body any, query string, out any) error {
	u := "http://docker/" + APIVersion + path
	if query != "" {
		u += "?" + query
	}
	var rd io.Reader
	ctype := ""
	switch v := body.(type) {
	case nil:
	case io.Reader:
		rd, ctype = v, "application/x-tar"
	default:
		js, err := json.Marshal(v)
		if err != nil {
			return err
		}
		rd, ctype = bytes.NewReader(js), "application/json"
	}
	req, err := http.NewRequestWithContext(ctx, method, u, rd)
	if err != nil {
		return err
	}
	if ctype != "" {
		req.Header.Set("Content-Type", ctype)
	}
	resp, err := b.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusNotModified:
		return errNotModified
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		if out == nil {
			_, _ = io.Copy(io.Discard, resp.Body)
			return nil
		}
		return json.NewDecoder(resp.Body).Decode(out)
	}
	var msg struct {
		Message string `json:"message"`
	}
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if json.Unmarshal(raw, &msg) != nil || msg.Message == "" {
		msg.Message = string(raw)
	}
	e := &apiError{Status: resp.StatusCode, Message: msg.Message}
	if resp.StatusCode == http.StatusNotFound {
		return fmt.Errorf("%w (%v)", backend.ErrNotFound, e)
	}
	return e
}

func short(id string) string {
	if len(id) > 12 {
		return id[:12]
	}
	return id
}
