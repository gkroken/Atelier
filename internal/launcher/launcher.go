// Package launcher starts, tracks and stops sessions. It turns an account and an IDE
// definition into a container through the backend (ADR-0003), and it is the proxy's
// Resolver: the one place that decides whether a request may reach a session.
package launcher

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/gkroken/Atelier/internal/backend"
	"github.com/gkroken/Atelier/internal/ide"
	"github.com/gkroken/Atelier/internal/proxy"
	"github.com/gkroken/Atelier/internal/session"
)

// Account is a user as the OS knows them. Spine #2 fills it from getpwnam and
// getgrouplist after sign-in; until then the spike binary fills it from os/user.
type Account struct {
	Name  string
	UID   int
	GID   int
	Group string // name of the primary group; the user name if empty
	Home  string // the home folder on the host; mounted at the same path inside
}

// Session is one running session.
type Session struct {
	ID          string
	Owner       string // Account.Name
	IDE         string
	ContainerID string
	Route       proxy.Route
	Started     time.Time
}

// Identify returns who is making a request, or false if nobody is signed in.
type Identify func(r *http.Request) (user string, ok bool)

// Options configure a Launcher.
type Options struct {
	// HealthTimeout bounds how long Start waits for the IDE's health check.
	HealthTimeout time.Duration
	Logger        *slog.Logger
}

// Launcher manages sessions on one backend.
type Launcher struct {
	be       backend.Backend
	identify Identify
	opts     Options
	health   *http.Client
	log      *slog.Logger

	mu       sync.Mutex
	sessions map[string]*Session
}

// New returns a Launcher.
func New(be backend.Backend, identify Identify, opts Options) *Launcher {
	if opts.HealthTimeout == 0 {
		opts.HealthTimeout = 60 * time.Second
	}
	log := opts.Logger
	if log == nil {
		log = slog.Default()
	}
	return &Launcher{
		be: be, identify: identify, opts: opts, log: log,
		health: &http.Client{
			Timeout:       2 * time.Second,
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
		sessions: map[string]*Session{},
	}
}

var _ proxy.Resolver = (*Launcher)(nil)

// Start creates and starts a session for acct running def, waits until the IDE passes
// its health check, and registers it. On any failure the container is removed.
func (l *Launcher) Start(ctx context.Context, acct Account, def ide.Definition) (*Session, error) {
	id := session.NewID()
	params := ide.Params{SessionID: id, User: acct.Name, Home: acct.Home, Prefix: proxy.Prefix + id}
	env, err := def.SessionEnv(params)
	if err != nil {
		return nil, fmt.Errorf("start %s session for %s: %w", def.Name, acct.Name, err)
	}
	group := acct.Group
	if group == "" {
		group = acct.Name
	}
	if !ide.ValidName(group) {
		// It goes into the container's /etc/group, so the same rule as user names.
		return nil, fmt.Errorf("start %s session for %s: group name %q not allowed", def.Name, acct.Name, group)
	}
	spec := backend.Spec{
		Name:       "atelier-" + id,
		Image:      def.Image,
		Entrypoint: def.Entrypoint,
		Cmd:        def.Cmd,
		Env:        env,
		UID:        acct.UID,
		GID:        acct.GID,
		// Same path inside as outside, as on Workbench: absolute paths in users' scripts
		// keep working.
		Home: backend.Mount{Source: acct.Home, Target: acct.Home},
		// Every field of these lines is checked: names by ValidName, the home by
		// SessionEnv's Params.Validate (ide.PasswdSafe); IDs are numbers.
		Files: []backend.File{
			{Path: "/etc/passwd", Mode: 0o644, Content: fmt.Appendf(nil, "%s:x:%d:%d::%s:/bin/bash\n", acct.Name, acct.UID, acct.GID, acct.Home)},
			{Path: "/etc/group", Mode: 0o644, Content: fmt.Appendf(nil, "%s:x:%d:%s\n", group, acct.GID, acct.Name)},
		},
		Labels: map[string]string{
			"atelier.session": id,
			"atelier.owner":   acct.Name,
			"atelier.ide":     def.Name,
		},
	}
	cid, err := l.be.Create(ctx, spec)
	if err != nil {
		return nil, fmt.Errorf("start %s session for %s: %w", def.Name, acct.Name, err)
	}
	fail := func(err error) (*Session, error) {
		if rerr := l.be.Remove(context.WithoutCancel(ctx), cid); rerr != nil {
			l.log.Warn("remove failed session container", "session", id, "err", rerr)
		}
		return nil, fmt.Errorf("start %s session for %s: %w", def.Name, acct.Name, err)
	}
	if err := l.be.Start(ctx, cid); err != nil {
		return fail(err)
	}
	target, err := l.waitHealthy(ctx, cid, def, params.Prefix)
	if err != nil {
		return fail(err)
	}
	s := &Session{
		ID: id, Owner: acct.Name, IDE: def.Name, ContainerID: cid,
		Route:   proxy.Route{Target: target, StripPrefix: def.StripPrefix},
		Started: time.Now(),
	}
	l.mu.Lock()
	l.sessions[id] = s
	l.mu.Unlock()
	l.log.Info("session started", "session", id, "owner", acct.Name, "ide", def.Name)
	return s, nil
}

// waitHealthy polls the container until it has an address and the IDE's health check
// passes, or the timeout runs out. A container that exits stops the wait at once.
func (l *Launcher) waitHealthy(parent context.Context, cid string, def ide.Definition, prefix string) (*url.URL, error) {
	// The health path is relative to the session root; an IDE that keeps the prefix
	// expects it on every request, health checks included.
	healthPath := def.Health.Path
	if !def.StripPrefix {
		healthPath = prefix + healthPath
	}
	ctx, cancel := context.WithTimeout(parent, l.opts.HealthTimeout)
	defer cancel()
	var target *url.URL
	for {
		st, err := l.be.Inspect(ctx, cid)
		if err != nil {
			return nil, err
		}
		if !st.Running {
			return nil, fmt.Errorf("container exited with code %d before the IDE was up", st.ExitCode)
		}
		if st.IP != "" {
			if target == nil {
				target = &url.URL{Scheme: "http", Host: net.JoinHostPort(st.IP, strconv.Itoa(def.Port))}
			}
			req, _ := http.NewRequestWithContext(ctx, http.MethodGet, target.String()+healthPath, nil)
			if resp, err := l.health.Do(req); err == nil {
				resp.Body.Close()
				if resp.StatusCode <= def.Health.MaxStatus {
					return target, nil
				}
			}
		}
		select {
		case <-ctx.Done():
			if err := parent.Err(); err != nil {
				// The caller gave up (the browser went away); not a slow IDE.
				return nil, fmt.Errorf("waiting for the IDE: %w", err)
			}
			return nil, fmt.Errorf("IDE not healthy after %v", l.opts.HealthTimeout)
		case <-time.After(50 * time.Millisecond):
		}
	}
}

// Resolve implements proxy.Resolver, and is where access to a session is decided:
// only the session's owner reaches it. Anyone else, including nobody signed in, gets
// ErrNoSession, the same answer as for a session that doesn't exist.
func (l *Launcher) Resolve(r *http.Request, id string) (proxy.Route, error) {
	user, ok := l.identify(r)
	if !ok {
		return proxy.Route{}, proxy.ErrNoSession
	}
	l.mu.Lock()
	s, found := l.sessions[id]
	l.mu.Unlock()
	if !found || s.Owner != user {
		return proxy.Route{}, proxy.ErrNoSession
	}
	return s.Route, nil
}

// ErrNotFound: no such session for that user.
var ErrNotFound = errors.New("session not found")

// Quit stops and removes a session's container. Files in the home folder stay. Only the
// owner may quit their session (admins come with spine #7).
func (l *Launcher) Quit(ctx context.Context, user, id string) error {
	l.mu.Lock()
	s, ok := l.sessions[id]
	if !ok || s.Owner != user {
		l.mu.Unlock()
		return ErrNotFound
	}
	delete(l.sessions, id)
	l.mu.Unlock()
	if err := l.be.Remove(ctx, s.ContainerID); err != nil && !errors.Is(err, backend.ErrNotFound) {
		return fmt.Errorf("quit session %s: %w", id, err)
	}
	l.log.Info("session quit", "session", id, "owner", user)
	return nil
}

// List returns user's sessions, oldest first. Never anyone else's.
func (l *Launcher) List(user string) []Session {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []Session
	for _, s := range l.sessions {
		if s.Owner == user {
			out = append(out, *s)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Started.Before(out[j].Started) })
	return out
}

// QuitAll quits every session, of every user: Atelier is shutting down, and until
// spine #3 reconciles containers on startup, sessions it no longer knows about would
// be unreachable. Errors are logged and the rest still quit.
func (l *Launcher) QuitAll(ctx context.Context) {
	l.mu.Lock()
	all := make([]*Session, 0, len(l.sessions))
	for _, s := range l.sessions {
		all = append(all, s)
	}
	l.mu.Unlock()
	for _, s := range all {
		if err := l.Quit(ctx, s.Owner, s.ID); err != nil && !errors.Is(err, ErrNotFound) {
			l.log.Warn("quit session on shutdown", "session", s.ID, "err", err)
		}
	}
}
