// Package ide holds the IDE definitions (ADR-0008). A definition is data: which port the
// IDE listens on, how to start it, how it wants the /s/<id>/ prefix handled, and how to
// tell it is up. Everything IDE-specific lives in one file per IDE here; the launcher and
// proxy read these fields and know nothing else about any IDE.
//
// Per-session values (the user name, the URL prefix, the port) reach the start command
// only through environment variables (see SessionEnv), never by pasting them into the command.
// So a definition's Entrypoint and Cmd are fixed strings, and a user name with shell
// metacharacters in it can't change what a "sh -c" script runs.
package ide

import (
	"fmt"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// Definition describes one IDE.
type Definition struct {
	// Name is the stable identifier stored with sessions, e.g. "rstudio".
	Name string
	// Title is what users see in the New Session dialog.
	Title string
	// Image is the image sessions use until admins publish their own (ADR-0014, spine
	// #7). It must already be on the host (ADR-0004).
	Image string
	// Port is the port the IDE listens on inside the container.
	Port int
	// Entrypoint and Cmd start the IDE. Fixed strings; read per-session values from the
	// variables SessionEnv sets.
	Entrypoint []string
	Cmd        []string
	// Env is extra fixed environment, "KEY=value".
	Env []string
	// StripPrefix: the proxy removes /s/<id> before forwarding, and the IDE is told the
	// prefix (through $ATELIER_PREFIX) so it can put it back on redirects and cookies.
	// False: the IDE expects the full path, prefix included.
	StripPrefix bool
	// Health is how the launcher tells the IDE is up after start.
	Health Health
}

// Health is an HTTP check against the IDE's port, on the path the IDE sees (after any
// prefix stripping).
type Health struct {
	Path string
	// MaxStatus is the highest HTTP status that counts as up. An IDE that redirects to
	// its sign-in or main page while starting answers 302, which means the server is up.
	MaxStatus int
}

// Params are the per-session values a definition's start command may use.
type Params struct {
	// SessionID is the session's stable ID. Unlike Prefix, it never changes for the life
	// of the session, so definitions key per-session state on it.
	SessionID string
	User      string // the account's name, as in the container's /etc/passwd
	Home      string // the home folder inside the container
	Prefix    string // the session's URL prefix, "/s/<id>", no trailing slash
}

// SessionEnv returns the full environment for a session: the definition's own entries, then
// the per-session ones. USER and HOME are set because programs inside look them up
// (RStudio's auth-none signs in as $USER). Later entries win in Docker, so the
// per-session values can't be overridden by the definition. It refuses Params that
// don't pass Validate: a definition keys per-session state on these values, and an
// empty or odd one would quietly put two sessions on the same state.
func (d Definition) SessionEnv(p Params) ([]string, error) {
	if err := p.Validate(); err != nil {
		return nil, err
	}
	env := append([]string(nil), d.Env...)
	return append(env,
		"USER="+p.User,
		"HOME="+p.Home,
		"ATELIER_SESSION_ID="+p.SessionID,
		"ATELIER_USER="+p.User,
		"ATELIER_HOME="+p.Home,
		"ATELIER_PREFIX="+p.Prefix,
		"ATELIER_PORT="+strconv.Itoa(d.Port),
	), nil
}

var (
	// sessionIDRE: lowercase letters and digits only, so the ID is safe as one path
	// segment (no "/", "..", or empty). Atelier's own IDs are 26 base32 characters.
	sessionIDRE = regexp.MustCompile(`^[a-z0-9]{8,64}$`)
	// userRE: POSIX portable user-name characters plus "@" (SSSD fully qualified names).
	// No ":" or newline, so the name is safe in a passwd line, and no "/".
	userRE = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9._@-]{0,63}$`)
	// prefixRE: one or more "/segment", no trailing slash.
	prefixRE = regexp.MustCompile(`^(/[A-Za-z0-9._-]+)+$`)
)

// Validate checks every field. Each one ends up in an environment variable that a
// definition may use in a path, so each must be present and unable to escape.
func (p Params) Validate() error {
	if !sessionIDRE.MatchString(p.SessionID) {
		return fmt.Errorf("session ID %q: want 8-64 lowercase letters or digits", p.SessionID)
	}
	if !userRE.MatchString(p.User) {
		return fmt.Errorf("user name %q not allowed", p.User)
	}
	if !path.IsAbs(p.Home) || path.Clean(p.Home) != p.Home || p.Home == "/" {
		return fmt.Errorf("home %q: want an absolute, clean path below /", p.Home)
	}
	if !prefixRE.MatchString(p.Prefix) {
		return fmt.Errorf("prefix %q: want /segment[/segment...] with no trailing slash", p.Prefix)
	}
	for _, seg := range strings.Split(p.Prefix[1:], "/") {
		if seg == "." || seg == ".." {
			return fmt.Errorf("prefix %q has a %q segment", p.Prefix, seg)
		}
	}
	return nil
}

var registry = map[string]Definition{}

func register(d Definition) {
	if _, dup := registry[d.Name]; dup {
		panic("ide: duplicate definition " + d.Name)
	}
	registry[d.Name] = d
}

// Lookup returns the definition named name.
func Lookup(name string) (Definition, error) {
	d, ok := registry[name]
	if !ok {
		return Definition{}, fmt.Errorf("unknown IDE %q", name)
	}
	return d, nil
}

// Names lists the registered IDEs, sorted.
func Names() []string {
	out := make([]string, 0, len(registry))
	for n := range registry {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}
