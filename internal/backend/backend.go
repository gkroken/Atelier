// Package backend is the launcher's only view of where session containers run (ADR-0003).
// The Docker implementation lives in internal/backend/docker; a Kubernetes one would be
// another implementation of Backend, not a change to the launcher.
//
// A Spec can only express what the invariants allow: one bind mount (the owner's home,
// ADR-0006), a non-root account ID (ADR-0005), and small files copied into the container
// before it starts. There is no field for extra mounts, published ports or privileges, so
// no caller can ask for them. Validate is called by every implementation before it touches
// the runtime.
package backend

import (
	"context"
	"errors"
	"fmt"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// Errors every implementation returns (wrapped) so the launcher can tell cases apart.
var (
	// ErrImageNotPresent: the image is not on the host. Backends never pull on start
	// (ADR-0004); publishing an image is what pulls it.
	ErrImageNotPresent = errors.New("image not present on host")
	// ErrNotFound: no container with that ID.
	ErrNotFound = errors.New("container not found")
	// ErrInvalidSpec: the Spec breaks an invariant; nothing was created.
	ErrInvalidSpec = errors.New("invalid container spec")
)

// Spec describes one session container.
type Spec struct {
	// Name is the container name. Optional; must match nameRE when set.
	Name  string
	Image string
	// Entrypoint and Cmd override the image's; nil keeps the image's own.
	Entrypoint []string
	Cmd        []string
	// Env entries are "KEY=value".
	Env []string
	// UID and GID are the owner's real account IDs from the OS. Never 0.
	UID, GID int
	// Home is the owner's home folder: the only bind mount the container gets.
	Home Mount
	// Files are written into the container's filesystem after it is created and before
	// it starts (for example a passwd file naming only the owner). They are copies, not
	// mounts.
	Files []File
	// Labels are attached to the container, for finding Atelier's containers again.
	Labels map[string]string
}

// Mount is a host folder bind-mounted read-write into the container.
type Mount struct {
	Source string // absolute path on the host
	Target string // absolute path in the container
}

// File is a file copied into the container, owned by root.
type File struct {
	Path    string // absolute path in the container
	Mode    int64  // permission bits, e.g. 0o644
	Content []byte
}

// State is what Inspect reports about a container.
type State struct {
	Running  bool
	ExitCode int
	// IP is the container's address on its network, empty when it is not running.
	// Session ports are never published on the host; the proxy dials IP directly.
	IP string
}

// Backend creates and controls session containers.
type Backend interface {
	// Create validates spec, checks the image is already present, creates the container
	// and copies spec.Files into it. It does not start it. Returns the container ID.
	Create(ctx context.Context, spec Spec) (string, error)
	Start(ctx context.Context, id string) error
	// Stop stops a running container, waiting up to timeout before killing it.
	// Stopping a container that is already stopped is not an error.
	Stop(ctx context.Context, id string, timeout time.Duration) error
	// Remove deletes the container (force-stopping it if needed). Files in the home
	// folder are untouched.
	Remove(ctx context.Context, id string) error
	Inspect(ctx context.Context, id string) (State, error)
}

var (
	nameRE = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]{0,127}$`)
	// imageRE allows the characters of a Docker image reference (registry/repo:tag or
	// @sha256:digest) and nothing that would change a URL it is placed in.
	imageRE = regexp.MustCompile(`^[a-z0-9][a-zA-Z0-9._/:@-]{0,254}$`)
)

// Validate checks spec against the invariants. Every Backend calls it first.
func Validate(spec Spec) error {
	bad := func(format string, a ...any) error {
		return fmt.Errorf("%w: %s", ErrInvalidSpec, fmt.Sprintf(format, a...))
	}
	if !imageRE.MatchString(spec.Image) || strings.Contains(spec.Image, "..") || strings.Contains(spec.Image, "//") {
		return bad("image reference %q", spec.Image)
	}
	if spec.Name != "" && !nameRE.MatchString(spec.Name) {
		return bad("container name %q", spec.Name)
	}
	if spec.UID <= 0 || spec.GID <= 0 {
		// 0 is root; negative IDs mean the lookup was never done.
		return bad("account ID %d:%d must be a real non-root user and group", spec.UID, spec.GID)
	}
	if err := absClean(spec.Home.Source, filepath.IsAbs, filepath.Clean); err != nil {
		return bad("home source: %v", err)
	}
	if spec.Home.Source == "/" {
		return bad("home source is the host root")
	}
	if err := absClean(spec.Home.Target, path.IsAbs, path.Clean); err != nil {
		return bad("home target: %v", err)
	}
	if spec.Home.Target == "/" {
		return bad("home target is the container root")
	}
	for _, f := range spec.Files {
		if err := absClean(f.Path, path.IsAbs, path.Clean); err != nil || f.Path == "/" {
			return bad("file path %q", f.Path)
		}
		if f.Mode&^0o777 != 0 {
			return bad("file %s: mode %o has more than permission bits", f.Path, f.Mode)
		}
	}
	return nil
}

func absClean(p string, isAbs func(string) bool, clean func(string) string) error {
	if p == "" {
		return errors.New("empty")
	}
	if !isAbs(p) {
		return fmt.Errorf("%q is not absolute", p)
	}
	if clean(p) != p {
		return fmt.Errorf("%q is not clean (want %q)", p, clean(p))
	}
	return nil
}
