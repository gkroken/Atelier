// Package proxy is the reverse proxy in front of session containers, at /s/<id>/.
//
// It knows nothing about any IDE (ADR-0008): a Resolver tells it where a session lives
// and whether to strip the /s/<id> prefix. Who may reach a session is decided by the
// Resolver too, so there is one place for that decision; the proxy only turns its answer
// into 404 or a forwarded request.
//
// On the way in it removes anything a client could use to claim an identity (headers
// some backends trust, X-Forwarded-*) and Atelier's own cookies, which no IDE should see.
// On the way out it drops any attempt by an IDE to set those cookies.
package proxy

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"time"

	"github.com/gkroken/Atelier/internal/session"
)

// Prefix is the path every session lives under.
const Prefix = "/s/"

// ErrNoSession: the session doesn't exist or the requester may not reach it. The proxy
// answers both with the same 404, so a client can't tell one from the other.
var ErrNoSession = errors.New("no such session")

// Route is where a session's requests go.
type Route struct {
	// Target is the IDE's base URL, http://<container IP>:<port>.
	Target *url.URL
	// StripPrefix: forward /s/<id>/x as /x (the IDE adds the prefix back itself).
	StripPrefix bool
}

// Resolver finds a session's route for a request, and refuses (ErrNoSession) when the
// session doesn't exist or isn't the requester's.
type Resolver interface {
	Resolve(r *http.Request, sessionID string) (Route, error)
}

// Options configure a Proxy.
type Options struct {
	// Cookies are the names of Atelier's own cookies. They are removed from requests
	// before forwarding, and Set-Cookie headers for them are removed from responses.
	Cookies []string
	Logger  *slog.Logger
}

// Proxy serves /s/<id>/... .
type Proxy struct {
	resolver Resolver
	cookies  map[string]bool
	log      *slog.Logger
	rp       *httputil.ReverseProxy
}

// identityHeaders are request headers that some backend, proxy or IDE treats as a
// statement of who the user is. A client must never be able to send one through.
// X-Forwarded-* and Forwarded are already removed by ReverseProxy's Rewrite.
var identityHeaders = []string{
	"Remote-User", "X-Remote-User", "X-Remote-Group",
	"X-User", "X-Email", "X-Groups",
	"X-Webauth-User", "X-Webauth-Email",
	"X-RStudio-Username",
}

// identityPrefixes: any header starting with one of these is removed.
var identityPrefixes = []string{"X-Atelier-", "X-Auth-Request-", "X-Forwarded-"}

type routeKey struct{}

type routed struct {
	route Route
	rest  string // escaped path after /s/<id>, starting with "/"
	id    string
}

// New returns a Proxy.
func New(res Resolver, opts Options) *Proxy {
	p := &Proxy{resolver: res, cookies: map[string]bool{}, log: opts.Logger}
	if p.log == nil {
		p.log = slog.Default()
	}
	for _, c := range opts.Cookies {
		p.cookies[c] = true
	}
	p.rp = &httputil.ReverseProxy{
		Rewrite:        p.rewrite,
		ModifyResponse: p.modifyResponse,
		ErrorHandler:   p.errorHandler,
		// IDEs stream (RStudio's event long-poll, terminals): flush every write.
		FlushInterval: -1,
		Transport: &http.Transport{
			DialContext: (&net.Dialer{Timeout: 5 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
			// No ResponseHeaderTimeout: long-polls legitimately wait.
			MaxIdleConnsPerHost: 16,
			IdleConnTimeout:     90 * time.Second,
		},
	}
	return p
}

func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	esc := r.URL.EscapedPath()
	if !strings.HasPrefix(esc, Prefix) {
		http.NotFound(w, r)
		return
	}
	id, rest, hasSlash := strings.Cut(esc[len(Prefix):], "/")
	if !session.ValidID(id) {
		http.NotFound(w, r)
		return
	}
	if !hasSlash {
		// /s/<id> -> /s/<id>/ so the IDE's relative URLs resolve under the prefix.
		u := Prefix + id + "/"
		if r.URL.RawQuery != "" {
			u += "?" + r.URL.RawQuery
		}
		http.Redirect(w, r, u, http.StatusMovedPermanently)
		return
	}
	route, err := p.resolver.Resolve(r, id)
	if err != nil {
		if !errors.Is(err, ErrNoSession) {
			p.log.Warn("resolve session", "session", id, "err", err)
		}
		http.NotFound(w, r)
		return
	}
	ctx := context.WithValue(r.Context(), routeKey{}, routed{route: route, rest: "/" + rest, id: id})
	p.rp.ServeHTTP(w, r.WithContext(ctx))
}

func (p *Proxy) rewrite(pr *httputil.ProxyRequest) {
	rt := pr.In.Context().Value(routeKey{}).(routed)
	out := pr.Out
	out.URL.Scheme = rt.route.Target.Scheme
	out.URL.Host = rt.route.Target.Host
	if rt.route.StripPrefix {
		dec, err := url.PathUnescape(rt.rest)
		if err != nil {
			dec = rt.rest // can't happen: EscapedPath came from a parsed URL
		}
		out.URL.Path, out.URL.RawPath = dec, rt.rest
		if dec == rt.rest {
			out.URL.RawPath = ""
		}
	}
	// Keep the client's Host: IDEs build absolute redirects from it, and they must point
	// back at Atelier, not at the container's IP (atelier-10f7686-N2). Out starts as a
	// copy of In, so this already holds as long as nothing calls pr.SetURL (which clears
	// Host); the assignment says so explicitly.
	out.Host = pr.In.Host
	pr.SetXForwarded()

	for _, h := range identityHeaders {
		out.Header.Del(h)
	}
	for k := range out.Header {
		for _, pre := range identityPrefixes {
			// SetXForwarded's own X-Forwarded-For/Host/Proto are kept.
			if strings.HasPrefix(k, pre) && !(pre == "X-Forwarded-" && setByUs(k)) {
				out.Header.Del(k)
			}
		}
	}
	p.stripCookies(out)
}

func setByUs(h string) bool {
	return h == "X-Forwarded-For" || h == "X-Forwarded-Host" || h == "X-Forwarded-Proto"
}

// stripCookies removes Atelier's own cookies from the Cookie header, keeping the IDE's.
func (p *Proxy) stripCookies(r *http.Request) {
	if len(p.cookies) == 0 || r.Header.Get("Cookie") == "" {
		return
	}
	var keep []string
	for _, c := range r.Cookies() {
		if !p.cookies[c.Name] {
			keep = append(keep, c.Name+"="+c.Value)
		}
	}
	r.Header.Del("Cookie")
	if len(keep) > 0 {
		r.Header.Set("Cookie", strings.Join(keep, "; "))
	}
}

// modifyResponse drops Set-Cookie headers for Atelier's cookies: an IDE (or code a user
// runs in it) must not be able to overwrite or plant Atelier's session cookie.
func (p *Proxy) modifyResponse(resp *http.Response) error {
	if len(p.cookies) == 0 {
		return nil
	}
	sc := resp.Header.Values("Set-Cookie")
	if len(sc) == 0 {
		return nil
	}
	resp.Header.Del("Set-Cookie")
	for _, line := range sc {
		c, err := http.ParseSetCookie(line)
		if err == nil && p.cookies[c.Name] {
			continue
		}
		if err != nil {
			// Can't tell what it sets; check the name by hand before keeping it.
			name, _, _ := strings.Cut(line, "=")
			if p.cookies[strings.TrimSpace(name)] {
				continue
			}
		}
		resp.Header.Add("Set-Cookie", line)
	}
	return nil
}

// errorHandler answers when the IDE can't be reached. The body says nothing about where
// the container is.
func (p *Proxy) errorHandler(w http.ResponseWriter, r *http.Request, err error) {
	if errors.Is(err, context.Canceled) {
		return // the client went away
	}
	rt, _ := r.Context().Value(routeKey{}).(routed)
	p.log.Warn("proxy to session", "session", rt.id, "err", err)
	http.Error(w, "The session is not responding.", http.StatusBadGateway)
}
