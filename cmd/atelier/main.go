// Command atelier is, for now, the spine #0 spike: a launcher and the /s/<id>/ proxy in
// one binary, for the one OS account that runs it.
//
// There is no sign-in yet (spine #2), so it guards itself the way Jupyter's single-user
// server does:
//   - it listens on loopback only;
//   - every request must carry a Host header naming that loopback address, so a website
//     can't reach it through DNS rebinding (atelier-bfa99dc-F1);
//   - every request must carry a random token, given once in the printed URL and then
//     kept in an HttpOnly, SameSite=Strict cookie, so other accounts on the same machine
//     can't use it just by connecting to loopback (atelier-bfa99dc-N1);
//   - writes must be same-origin.
//
// Even so, run it only on a machine you trust: the token is printed to your terminal.
//
//	atelier [-listen 127.0.0.1:8080] [-home DIR]
//
// Open the printed URL, press Start, and RStudio opens under /s/<id>/. Ctrl-C quits every
// session it started.
package main

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"html/template"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/gkroken/Atelier/internal/backend/docker"
	"github.com/gkroken/Atelier/internal/config"
	"github.com/gkroken/Atelier/internal/ide"
	"github.com/gkroken/Atelier/internal/launcher"
	"github.com/gkroken/Atelier/internal/proxy"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "atelier:", err)
		os.Exit(1)
	}
}

func run() error {
	listen := flag.String("listen", "127.0.0.1:8080", "address to listen on; loopback only until sign-in exists")
	home := flag.String("home", "", "home folder to mount (default: your own)")
	socket := flag.String("docker", docker.DefaultSocket, "Docker socket")
	network := flag.String("network", docker.DefaultNetwork, "session network")
	cfgPath := flag.String("config", "", "atelier.yaml (ADR-0018); replaces -listen, -docker and -network")
	check := flag.Bool("config-check", false, "check the -config file and exit, starting nothing")
	flag.Parse()

	if *check {
		return checkConfig(*cfgPath, os.Stdout)
	}
	if *cfgPath != "" {
		var set []string
		flag.Visit(func(f *flag.Flag) {
			if f.Name == "listen" || f.Name == "docker" || f.Name == "network" {
				set = append(set, "-"+f.Name)
			}
		})
		if len(set) > 0 {
			return fmt.Errorf("%s: set in the config file when -config is given", strings.Join(set, ", "))
		}
		cfg, err := config.LoadFile(*cfgPath)
		if err != nil {
			return err
		}
		// Spike: only these are used until sign-in (spine #2) and the two hosts exist.
		*listen, *socket, *network = cfg.Server.Listen, cfg.Docker.Socket, cfg.Docker.Network
	}

	if err := loopbackOnly(*listen); err != nil {
		return err
	}
	acct, err := currentAccount(*home)
	if err != nil {
		return err
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))

	be := docker.New(*socket, *network)
	// Spike only: there is one user, the account running atelier, and only loopback can
	// reach it. Spine #2 replaces this with the signed-in user.
	identify := func(*http.Request) (string, bool) { return acct.Name, true }
	l := launcher.New(be, identify, launcher.Options{Logger: log})

	ln, err := net.Listen("tcp", *listen)
	if err != nil {
		return err
	}
	_, port, _ := net.SplitHostPort(ln.Addr().String())
	token := newToken()
	handler := newHandler(l, acct, token, port, log)

	srv := &http.Server{
		Addr:              *listen,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		// No WriteTimeout: IDE long-polls and websockets stay open.
	}
	fmt.Printf("atelier spike: sessions run as %s (uid %d) with %s mounted.\n", acct.Name, acct.UID, acct.Home)
	fmt.Printf("Open http://127.0.0.1:%s/?token=%s\n", port, token)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(ln) }()
	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}
	shutdown, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_ = srv.Shutdown(shutdown)
	for _, s := range l.List(acct.Name) {
		if err := l.Quit(shutdown, acct.Name, s.ID); err != nil {
			log.Warn("quit session on exit", "session", s.ID, "err", err)
		}
	}
	return nil
}

// checkConfig is -config-check: load the file strictly, with the environment its
// placeholders need, and say so. It starts nothing.
func checkConfig(path string, out io.Writer) error {
	if path == "" {
		return errors.New("-config-check needs -config <file>")
	}
	if _, err := config.LoadFile(path); err != nil {
		return err
	}
	fmt.Fprintf(out, "config ok: %s\n", path)
	return nil
}

// cookieName holds the spike's access token in the browser. The proxy strips it before
// requests reach an IDE.
const cookieName = "atelier_spike"

// newHandler is the spike's whole HTTP surface, behind guard.
func newHandler(l *launcher.Launcher, acct launcher.Account, token, port string, log *slog.Logger) http.Handler {
	mux := http.NewServeMux()
	mux.Handle(proxy.Prefix, proxy.New(l, proxy.Options{Cookies: []string{cookieName}, Logger: log}))
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; form-action 'self'")
		_ = homePage.Execute(w, map[string]any{"User": acct.Name, "Home": acct.Home, "Sessions": l.List(acct.Name), "IDEs": ide.Names()})
	})
	mux.HandleFunc("POST /start", sameOrigin(func(w http.ResponseWriter, r *http.Request) {
		def, err := ide.Lookup(r.FormValue("ide"))
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		s, err := l.Start(r.Context(), acct, def)
		if err != nil {
			log.Error("start session", "err", err)
			http.Error(w, "Could not start the session: "+err.Error(), http.StatusInternalServerError)
			return
		}
		http.Redirect(w, r, proxy.Prefix+s.ID+"/", http.StatusSeeOther)
	}))
	mux.HandleFunc("POST /quit", sameOrigin(func(w http.ResponseWriter, r *http.Request) {
		if err := l.Quit(r.Context(), acct.Name, r.FormValue("id")); err != nil {
			http.Error(w, err.Error(), http.StatusNotFound)
			return
		}
		http.Redirect(w, r, "/", http.StatusSeeOther)
	}))
	return guard(mux, token, port)
}

// guard admits a request only if its Host is the loopback address the spike listens on
// and it carries the token. A request with ?token=<token> gets the cookie and is
// redirected to the same URL without the token, so it doesn't stay in the address bar.
func guard(next http.Handler, token, port string) http.Handler {
	allowed := map[string]bool{
		"127.0.0.1:" + port: true,
		"[::1]:" + port:     true,
		"localhost:" + port: true,
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !allowed[strings.ToLower(r.Host)] {
			http.Error(w, "unknown host", http.StatusMisdirectedRequest)
			return
		}
		if q := r.URL.Query(); q.Has("token") {
			if !tokenEqual(q.Get("token"), token) {
				http.Error(w, "wrong token", http.StatusForbidden)
				return
			}
			http.SetCookie(w, &http.Cookie{
				Name: cookieName, Value: token, Path: "/",
				HttpOnly: true, SameSite: http.SameSiteStrictMode,
			})
			q.Del("token")
			u := *r.URL
			u.RawQuery = q.Encode()
			http.Redirect(w, r, u.RequestURI(), http.StatusSeeOther)
			return
		}
		c, err := r.Cookie(cookieName)
		if err != nil || !tokenEqual(c.Value, token) {
			http.Error(w, "open the URL atelier printed when it started", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func tokenEqual(a, b string) bool {
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

func newToken() string {
	b := make([]byte, 24)
	_, _ = rand.Read(b) // never fails on Linux (Go 1.24+)
	return hex.EncodeToString(b)
}

// loopbackOnly refuses a listen address anyone but this machine can reach.
func loopbackOnly(addr string) error {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("-listen %q: %w", addr, err)
	}
	if host == "localhost" {
		return nil
	}
	if ip := net.ParseIP(host); ip == nil || !ip.IsLoopback() {
		return fmt.Errorf("-listen %q: the spike has no sign-in, so it listens on loopback only", addr)
	}
	return nil
}

// sameOrigin refuses state-changing requests that don't come from this page, so a
// website the user visits can't start or quit sessions through their browser.
func sameOrigin(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if site := r.Header.Get("Sec-Fetch-Site"); site != "" {
			if site != "same-origin" {
				http.Error(w, "cross-origin request refused", http.StatusForbidden)
				return
			}
		} else if o := r.Header.Get("Origin"); o != "" {
			u, err := url.Parse(o)
			if err != nil || u.Host != r.Host {
				http.Error(w, "cross-origin request refused", http.StatusForbidden)
				return
			}
		} else {
			http.Error(w, "request without Origin refused", http.StatusForbidden)
			return
		}
		h(w, r)
	}
}

// currentAccount is the OS account running atelier.
func currentAccount(home string) (launcher.Account, error) {
	u, err := user.Current()
	if err != nil {
		return launcher.Account{}, err
	}
	uid, _ := strconv.Atoi(u.Uid)
	gid, _ := strconv.Atoi(u.Gid)
	if uid == 0 {
		return launcher.Account{}, errors.New("refusing to run as root: sessions never run as root")
	}
	group := ""
	if g, err := user.LookupGroupId(u.Gid); err == nil {
		group = g.Name
	}
	if home == "" {
		home = u.HomeDir
	}
	home, err = filepath.Abs(home)
	if err != nil {
		return launcher.Account{}, err
	}
	return launcher.Account{Name: u.Username, UID: uid, GID: gid, Group: group, Home: home}, nil
}

var homePage = template.Must(template.New("home").Parse(`<!doctype html>
<title>Atelier (spike)</title>
<style>body{font:15px system-ui;margin:2rem;max-width:40rem} td,th{padding:.3rem .8rem;text-align:left}</style>
<h1>Atelier <small>spike</small></h1>
<p>Sessions run as <b>{{.User}}</b> with <code>{{.Home}}</code> mounted.</p>
<form method=post action=/start>
  <select name=ide>{{range .IDEs}}<option>{{.}}</option>{{end}}</select>
  <button>Start</button>
</form>
<table>
<tr><th>Session</th><th>IDE</th><th>Started</th><th></th></tr>
{{range .Sessions}}<tr>
  <td><a href="/s/{{.ID}}/">{{.ID}}</a></td><td>{{.IDE}}</td><td>{{.Started.Format "15:04:05"}}</td>
  <td><form method=post action=/quit><input type=hidden name=id value="{{.ID}}"><button>Quit</button></form></td>
</tr>{{else}}<tr><td colspan=4>No sessions.</td></tr>{{end}}
</table>
`))
