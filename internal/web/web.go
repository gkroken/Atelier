// Package web is Atelier's HTTP front: sign-in and the home page on the main host,
// sessions on the sessions host (ADR-0016), and the checks every request passes first.
//
// Every request:
//   - must name one of the two configured hosts, or gets 421 before any routing
//     (against DNS rebinding, atelier-bfa99dc-F1);
//   - on the main host, a POST must come from the main host's own pages: Sec-Fetch-Site
//     "same-origin", or without it an Origin equal to the main origin. A sessions-host
//     page is same-site, not same-origin, so it can't post to the main host
//     (ADR-0016 amendment).
//
// Signing in: the password goes to the Checker (PAM), the name it returns to the
// account lookup, and only the OS's canonical name is kept (atelier-3b90b60-N3).
// Attempts are limited per typed name and per client address before the Checker is
// called; a failure always takes at least MinFailure, so the time taken doesn't tell
// an unknown name from a wrong password (atelier-7e6f55e-N1).
//
// Opening a session: the main host makes a single-use ticket and redirects to the
// sessions host, which swaps it for its own cookie. The cookies are host-only
// __Host- cookies, so neither host's reaches the other, and the proxy strips both
// before a request reaches an IDE.
package web

import (
	"context"
	"errors"
	"html/template"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/gkroken/Atelier/internal/account"
	"github.com/gkroken/Atelier/internal/auth/pam"
	"github.com/gkroken/Atelier/internal/ide"
	"github.com/gkroken/Atelier/internal/launcher"
	"github.com/gkroken/Atelier/internal/login"
	"github.com/gkroken/Atelier/internal/proxy"
	"github.com/gkroken/Atelier/internal/ratelimit"
	"github.com/gkroken/Atelier/internal/session"
)

// Cookie names. __Host-: Secure, Path=/, no Domain, so host-only (ADR-0016).
const (
	MainCookie     = "__Host-atelier"
	SessionsCookie = "__Host-atelier-s"
)

// enterPath is where the sessions host redeems a ticket. Outside /s/, so it never
// reaches an IDE.
const enterPath = "/_atelier/enter"

// Rate limits: per typed name, well under AD's usual lockout threshold; per client
// address, enough for a building behind one NAT address.
const (
	nameTries = 5
	addrTries = 30
	tryWindow = 15 * time.Minute
	// checkTimeout bounds one password check (an SSSD timeout is about 6 s).
	checkTimeout = 15 * time.Second
)

// DefaultMinFailure is how long a failed sign-in takes at least.
const DefaultMinFailure = time.Second

// Checker checks a password. pam.Checker and devauth.Checker are ones. Errors are
// pam's: ErrAuth, ErrAccount, ErrUnavailable, ErrBusy, or another failure.
type Checker interface {
	Check(ctx context.Context, user, password string) (string, error)
}

// Options configure the front.
type Options struct {
	MainHost, SessionsHost string // as in atelier.yaml, with a port unless 443 (or 80)
	HTTPS                  bool   // the URLs Atelier builds start https://
	Launcher               *launcher.Launcher
	Logins                 *login.Store
	Password               Checker
	Accounts               func(name string) (account.Account, error)
	DevBanner              bool // say on every page that dev sign-in is on
	// IDEs and LookupIDE default to the ide registry (tests pass their own).
	IDEs       func() []string
	LookupIDE  func(name string) (ide.Definition, error)
	MinFailure time.Duration
	Logger     *slog.Logger
}

// Front is the HTTP handler.
type Front struct {
	o              Options
	mainOrigin     string
	sessionsOrigin string
	names, addrs   *ratelimit.Limiter
	proxy          http.Handler
	sleep          func(time.Duration)
}

// Identify is the launcher's way to learn who a sessions-host request is from: the
// sessions cookie's sign-in, or nobody.
func Identify(logins *login.Store) launcher.Identify {
	return func(r *http.Request) (string, bool) {
		c, err := r.Cookie(SessionsCookie)
		if err != nil {
			return "", false
		}
		l, ok := logins.Sessions(c.Value)
		if !ok {
			return "", false
		}
		return l.Account.Name, true
	}
}

// New returns the front.
func New(o Options) *Front {
	if o.MinFailure == 0 {
		o.MinFailure = DefaultMinFailure
	}
	if o.IDEs == nil {
		o.IDEs, o.LookupIDE = ide.Names, ide.Lookup
	}
	scheme := "http://"
	if o.HTTPS {
		scheme = "https://"
	}
	return &Front{
		o:              o,
		mainOrigin:     scheme + o.MainHost,
		sessionsOrigin: scheme + o.SessionsHost,
		names:          ratelimit.New(nameTries, tryWindow),
		addrs:          ratelimit.New(addrTries, tryWindow),
		proxy:          proxy.New(o.Launcher, proxy.Options{Cookies: []string{MainCookie, SessionsCookie}, Logger: o.Logger}),
		sleep:          time.Sleep,
	}
}

func (f *Front) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch strings.ToLower(r.Host) {
	case f.o.MainHost:
		f.main(w, r)
	case f.o.SessionsHost:
		f.sessions(w, r)
	default:
		http.Error(w, "unknown host", http.StatusMisdirectedRequest)
	}
}

// ---- main host ----

func (f *Front) main(w http.ResponseWriter, r *http.Request) {
	pageHeaders(w)
	if r.Method == http.MethodPost && !f.sameOrigin(r) {
		http.Error(w, "cross-origin request refused", http.StatusForbidden)
		return
	}
	switch {
	case r.URL.Path == "/signin" && (r.Method == http.MethodGet || r.Method == http.MethodHead):
		if _, ok := f.signedIn(r); ok {
			http.Redirect(w, r, safeNext(r.URL.Query().Get("next")), http.StatusSeeOther)
			return
		}
		f.render(w, http.StatusOK, signinPage, map[string]any{"Next": safeNext(r.URL.Query().Get("next"))})
	case r.URL.Path == "/signin" && r.Method == http.MethodPost:
		f.signIn(w, r)
	case r.URL.Path == "/signout" && r.Method == http.MethodPost:
		if l, ok := f.signedIn(r); ok {
			f.o.Logins.End(l)
			f.o.Logger.Info("signed out", "user", l.Account.Name)
		}
		http.SetCookie(w, f.cookie(MainCookie, "", -1))
		http.Redirect(w, r, "/signin", http.StatusSeeOther)
	default:
		l, ok := f.signedIn(r)
		if !ok {
			next := ""
			if r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/open/") {
				next = r.URL.Path
			}
			http.Redirect(w, r, "/signin"+query("next", safeNext(next)), http.StatusSeeOther)
			return
		}
		f.signedInRoutes(w, r, l)
	}
}

func (f *Front) signedInRoutes(w http.ResponseWriter, r *http.Request, l *login.Login) {
	acct := l.Account.Account
	switch {
	case r.URL.Path == "/" && (r.Method == http.MethodGet || r.Method == http.MethodHead):
		f.render(w, http.StatusOK, homePage, map[string]any{
			"User": acct.Name, "Sessions": f.o.Launcher.List(acct.Name), "IDEs": f.o.IDEs(),
		})
	case r.URL.Path == "/start" && r.Method == http.MethodPost:
		def, err := f.o.LookupIDE(r.FormValue("ide"))
		if err != nil {
			http.Error(w, "unknown IDE", http.StatusBadRequest)
			return
		}
		s, err := f.o.Launcher.Start(r.Context(), acct, def)
		if err != nil {
			f.o.Logger.Error("start session", "user", acct.Name, "ide", def.Name, "err", err)
			http.Error(w, "The session could not start. Try again, or ask your administrator.", http.StatusInternalServerError)
			return
		}
		http.Redirect(w, r, "/open/"+s.ID, http.StatusSeeOther)
	case r.URL.Path == "/quit" && r.Method == http.MethodPost:
		if err := f.o.Launcher.Quit(r.Context(), acct.Name, r.FormValue("id")); err != nil {
			http.Error(w, "no such session", http.StatusNotFound)
			return
		}
		http.Redirect(w, r, "/", http.StatusSeeOther)
	case strings.HasPrefix(r.URL.Path, "/open/") && r.Method == http.MethodGet:
		id := strings.TrimPrefix(r.URL.Path, "/open/")
		if !f.owns(acct.Name, id) {
			http.NotFound(w, r)
			return
		}
		t := f.o.Logins.Ticket(l, proxy.Prefix+id+"/")
		http.Redirect(w, r, f.sessionsOrigin+enterPath+query("ticket", t), http.StatusSeeOther)
	default:
		http.NotFound(w, r)
	}
}

func (f *Front) owns(user, id string) bool {
	if !session.ValidID(id) {
		return false
	}
	for _, s := range f.o.Launcher.List(user) {
		if s.ID == id {
			return true
		}
	}
	return false
}

// signIn is POST /signin.
func (f *Front) signIn(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	user := strings.TrimSpace(r.PostFormValue("user"))
	password := r.PostFormValue("password")
	next := safeNext(r.PostFormValue("next"))
	fail := func(status int, msg string) {
		if d := f.o.MinFailure - time.Since(start); d > 0 {
			f.sleep(d)
		}
		f.render(w, status, signinPage, map[string]any{"Error": msg, "User": user, "Next": next})
	}
	if f.o.Password == nil {
		fail(http.StatusForbidden, "Password sign-in is off.")
		return
	}
	if user == "" || password == "" {
		fail(http.StatusUnauthorized, "Enter your user name and password.")
		return
	}
	// The address first, so it bounds the account lookups too.
	addrKey := "addr:" + clientAddr(r)
	if ok, _ := f.addrs.Try(addrKey); !ok {
		f.o.Logger.Warn("sign-in refused: too many attempts from this address", "addr", clientAddr(r))
		fail(http.StatusTooManyRequests, "Too many sign-in attempts from here. Wait a few minutes, then try again.")
		return
	}
	// Resolve the typed name to its account before any password check (no password is
	// needed for that). The name limit is keyed on the canonical name, so every
	// spelling SSSD maps to one AD account (alice, ALICE, alice@corp.example.com,
	// CORP\alice) shares one set of tries (atelier-d57d7bd-F1). A name without a usable
	// account never reaches PAM and gets the same answer as a wrong password
	// (atelier-d57d7bd-N1).
	pre, err := f.o.Accounts(user)
	switch {
	case err == nil:
	case errors.Is(err, account.ErrNoAccount), errors.Is(err, account.ErrRefused):
		f.o.Logger.Info("sign-in failed: no usable account", "user", user, "addr", clientAddr(r), "why", err)
		fail(http.StatusUnauthorized, "Wrong user name or password.")
		return
	default: // NSS (SSSD) didn't answer
		f.addrs.Refund(addrKey)
		f.o.Logger.Error("sign-in could not look up the account", "user", user, "err", err)
		fail(http.StatusServiceUnavailable, "Sign-in is unavailable right now. Try again in a minute.")
		return
	}
	nameKey := "name:" + pre.Name
	if ok, _ := f.names.Try(nameKey); !ok {
		f.addrs.Refund(addrKey) // this attempt never reached a password check
		f.o.Logger.Warn("sign-in refused: too many attempts for this account", "user", pre.Name, "addr", clientAddr(r))
		fail(http.StatusTooManyRequests, "Too many attempts for this user name. Wait a few minutes, then try again.")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), checkTimeout)
	defer cancel()
	pamUser, err := f.o.Password.Check(ctx, pre.Name, password)
	switch {
	case err == nil:
	case errors.Is(err, pam.ErrAuth), errors.Is(err, pam.ErrAccount):
		f.o.Logger.Info("sign-in failed", "user", pre.Name, "addr", clientAddr(r), "why", err)
		fail(http.StatusUnauthorized, "Wrong user name or password.")
		return
	default: // the directory is down, too busy, timed out, or misconfigured: not the user's doing
		f.names.Refund(nameKey)
		f.addrs.Refund(addrKey)
		f.o.Logger.Error("sign-in could not check the password", "user", pre.Name, "err", err)
		fail(http.StatusServiceUnavailable, "Sign-in is unavailable right now. Try again in a minute.")
		return
	}

	// PAM may hand back another name; it must still be the same account.
	acct, err := f.o.Accounts(pamUser)
	if err != nil || acct.Name != pre.Name {
		f.o.Logger.Error("sign-in: PAM's user isn't the account looked up", "typed", user, "account", pre.Name, "pam", pamUser, "err", err)
		fail(http.StatusUnauthorized, "Wrong user name or password.")
		return
	}
	f.names.Reset(nameKey)
	token, _ := f.o.Logins.Create(acct, "password")
	http.SetCookie(w, f.cookie(MainCookie, token, int(f.o.Logins.Max/time.Second)))
	f.o.Logger.Info("signed in", "user", acct.Name, "addr", clientAddr(r))
	http.Redirect(w, r, next, http.StatusSeeOther)
}

func (f *Front) signedIn(r *http.Request) (*login.Login, bool) {
	c, err := r.Cookie(MainCookie)
	if err != nil {
		return nil, false
	}
	return f.o.Logins.Main(c.Value)
}

// sameOrigin: the request comes from the main host's own pages.
func (f *Front) sameOrigin(r *http.Request) bool {
	if site := r.Header.Get("Sec-Fetch-Site"); site != "" {
		return site == "same-origin"
	}
	return r.Header.Get("Origin") == f.mainOrigin
}

// ---- sessions host ----

func (f *Front) sessions(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.URL.Path == enterPath && r.Method == http.MethodGet:
		pageHeaders(w)
		token, to, ok := f.o.Logins.Redeem(r.URL.Query().Get("ticket"))
		if !ok {
			f.render(w, http.StatusForbidden, errorPage, map[string]any{
				"Message": "This link has expired.", "Back": f.mainOrigin + "/",
			})
			return
		}
		http.SetCookie(w, f.cookie(SessionsCookie, token, int(f.o.Logins.Max/time.Second)))
		http.Redirect(w, r, to, http.StatusSeeOther)
	case strings.HasPrefix(r.URL.Path, proxy.Prefix):
		// Not signed in on this host: a page load goes round the main host for a ticket
		// (sign-in first if needed). Anything else gets the proxy's 404.
		if _, ok := Identify(f.o.Logins)(r); !ok && r.Method == http.MethodGet && r.Header.Get("Sec-Fetch-Mode") != "cors" {
			id, _, _ := strings.Cut(strings.TrimPrefix(r.URL.Path, proxy.Prefix), "/")
			if session.ValidID(id) {
				http.Redirect(w, r, f.mainOrigin+"/open/"+id, http.StatusSeeOther)
				return
			}
		}
		f.proxy.ServeHTTP(w, r)
	default:
		http.NotFound(w, r)
	}
}

// ---- helpers ----

func (f *Front) cookie(name, value string, maxAge int) *http.Cookie {
	return &http.Cookie{
		Name: name, Value: value, Path: "/", MaxAge: maxAge,
		Secure: true, HttpOnly: true, SameSite: http.SameSiteLaxMode,
	}
}

// clientAddr is the peer's IP. Atelier is not behind a proxy (it terminates TLS), so
// no forwarded header is trusted.
func clientAddr(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// safeNext keeps a post-sign-in destination only if it is the home page or a
// session's /open/<id>; anything else (another host, //evil, a script) becomes "/".
func safeNext(next string) string {
	if strings.HasPrefix(next, "/open/") && session.ValidID(strings.TrimPrefix(next, "/open/")) {
		return next
	}
	return "/"
}

func query(key, value string) string {
	if value == "" || (key == "next" && value == "/") {
		return ""
	}
	return "?" + url.Values{key: {value}}.Encode()
}

func pageHeaders(w http.ResponseWriter) {
	h := w.Header()
	h.Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; form-action 'self'; frame-ancestors 'none'; base-uri 'none'")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Referrer-Policy", "no-referrer")
	h.Set("Cache-Control", "no-store")
}

func (f *Front) render(w http.ResponseWriter, status int, t *template.Template, data map[string]any) {
	data["Dev"] = f.o.DevBanner
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	if err := t.Execute(w, data); err != nil {
		f.o.Logger.Error("render page", "err", err)
	}
}

const style = `<style>body{font:16px system-ui,sans-serif;max-width:40em;margin:3em auto;padding:0 1em}
.err{color:#a00}.dev{background:#fe9;padding:.5em}form.inline{display:inline}
label{display:block;margin:.5em 0}input[type=text],input[type=password]{width:100%;padding:.3em}</style>`

const devBanner = `{{if .Dev}}<p class="dev">Development sign-in is on: the password is in the terminal Atelier runs in.</p>{{end}}`

var signinPage = template.Must(template.New("signin").Parse(`<!doctype html><meta charset="utf-8">
<title>Sign in · Atelier</title>` + style + devBanner + `
<h1>Atelier</h1>
{{with .Error}}<p class="err">{{.}}</p>{{end}}
<form method="post" action="/signin">
<input type="hidden" name="next" value="{{.Next}}">
<label>User name <input type="text" name="user" value="{{.User}}" autocomplete="username" autofocus required></label>
<label>Password <input type="password" name="password" autocomplete="current-password" required></label>
<button>Sign in</button>
</form>`))

var homePage = template.Must(template.New("home").Parse(`<!doctype html><meta charset="utf-8">
<title>Atelier</title>` + style + devBanner + `
<h1>Atelier</h1>
<p>Signed in as {{.User}}. <form class="inline" method="post" action="/signout"><button>Sign out</button></form></p>
<h2>Your sessions</h2>
{{range .Sessions}}<p><a href="/open/{{.ID}}">{{.IDE}} · started {{.Started.Format "15:04"}}</a>
<form class="inline" method="post" action="/quit"><input type="hidden" name="id" value="{{.ID}}"><button>Quit</button></form></p>
{{else}}<p>None yet.</p>{{end}}
<h2>New session</h2>
<form method="post" action="/start">{{range .IDEs}}<button name="ide" value="{{.}}">{{.}}</button> {{end}}</form>`))

var errorPage = template.Must(template.New("error").Parse(`<!doctype html><meta charset="utf-8">
<title>Atelier</title>` + style + `
<p>{{.Message}}</p><p><a href="{{.Back}}">Back to Atelier</a></p>`))
