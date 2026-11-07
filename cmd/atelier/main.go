// Command atelier serves browser IDE sessions for the accounts of one host.
//
//	atelier -config atelier.yaml
//	atelier -config-check -config atelier.yaml
//
// People sign in on the main host with their host (AD) password through PAM, start
// sessions there, and use them on the sessions host (ADR-0016, ADR-0017). See
// deploy/config/atelier.example.yaml, and atelier.dev.yaml for a development machine.
//
// Atelier listens beyond loopback only with server.tls set, so passwords never cross
// the network in plain text. On shutdown (Ctrl-C, SIGTERM) every session is quit.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"os/user"
	"syscall"
	"time"

	"github.com/gkroken/Atelier/internal/account"
	"github.com/gkroken/Atelier/internal/auth/devauth"
	"github.com/gkroken/Atelier/internal/auth/pam"
	"github.com/gkroken/Atelier/internal/backend/docker"
	"github.com/gkroken/Atelier/internal/config"
	"github.com/gkroken/Atelier/internal/launcher"
	"github.com/gkroken/Atelier/internal/login"
	"github.com/gkroken/Atelier/internal/tlscert"
	"github.com/gkroken/Atelier/internal/web"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "atelier:", err)
		os.Exit(1)
	}
}

// pamConcurrency caps password checks in flight; more get "busy".
const pamConcurrency = 8

func run() error {
	cfgPath := flag.String("config", "", "atelier.yaml (ADR-0018)")
	check := flag.Bool("config-check", false, "check the -config file and exit, starting nothing")
	flag.Parse()

	if *check {
		return checkConfig(*cfgPath, os.Stdout)
	}
	if *cfgPath == "" {
		return errors.New("-config <atelier.yaml> is required; see deploy/config/atelier.example.yaml")
	}
	cfg, err := config.LoadFile(*cfgPath)
	if err != nil {
		return err
	}
	if err := listenRule(cfg); err != nil {
		return err
	}
	me, err := user.Current()
	if err != nil {
		return err
	}
	if me.Uid == "0" {
		return errors.New("refusing to run as root: run Atelier as its own account in the docker group (ADR-0017)")
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))

	var certs *tlscert.Source
	if t := cfg.Server.TLS; t != nil {
		if certs, err = tlscert.New(t.CertFile, t.KeyFile, log); err != nil {
			return err
		}
	}
	checker, err := passwordChecker(cfg, me.Username, log)
	if err != nil {
		return err
	}

	be := docker.New(cfg.Docker.Socket, cfg.Docker.Network)
	logins := login.New()
	l := launcher.New(be, web.Identify(logins), launcher.Options{Logger: log})
	front := web.New(web.Options{
		MainHost: cfg.Server.MainHost, SessionsHost: cfg.Server.SessionsHost, HTTPS: certs != nil,
		Launcher: l, Logins: logins, Password: checker, Accounts: account.Host.Resolve,
		DevBanner: cfg.Auth.Dev != nil, Logger: log,
	})

	ln, err := net.Listen("tcp", cfg.Server.Listen)
	if err != nil {
		return err
	}
	srv := &http.Server{
		Handler:           front,
		ReadHeaderTimeout: 10 * time.Second,
		// No WriteTimeout: IDE long-polls and websockets stay open.
	}
	scheme := "http"
	if certs != nil {
		srv.TLSConfig = certs.Config()
		scheme = "https"
	}
	fmt.Printf("Atelier: sign in at %s://%s/ (sessions on %s)\n", scheme, cfg.Server.MainHost, cfg.Server.SessionsHost)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	errc := make(chan error, 1)
	go func() {
		if certs != nil {
			errc <- srv.ServeTLS(ln, "", "") // the certificate comes from TLSConfig
			return
		}
		errc <- srv.Serve(ln)
	}()
	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}
	shutdown, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_ = srv.Shutdown(shutdown)
	l.QuitAll(shutdown)
	return nil
}

// listenRule: beyond loopback only with TLS, so a password never crosses the network
// in plain text.
func listenRule(cfg *config.Config) error {
	if cfg.Server.TLS == nil && !config.LoopbackListen(cfg.Server.Listen) {
		return fmt.Errorf("server.listen %q reaches other machines, so server.tls is required (passwords would cross the network in plain text)", cfg.Server.Listen)
	}
	return nil
}

// passwordChecker picks the sign-in the config turns on.
func passwordChecker(cfg *config.Config, me string, log *slog.Logger) (web.Checker, error) {
	if cfg.Auth.OIDC != nil {
		if cfg.Auth.PAM == nil && cfg.Auth.Dev == nil {
			return nil, errors.New("auth.oidc isn't supported yet (spine #2c); turn on auth.pam")
		}
		log.Warn("auth.oidc is set but isn't supported yet (spine #2c); ignoring it")
	}
	switch {
	case cfg.Auth.Dev != nil:
		c := devauth.New(me)
		log.Warn("development sign-in is on; never use auth.dev on a server", "user", me)
		// To the terminal only, never the log (it may be collected).
		fmt.Printf("Development sign-in: user %s, password %s (new each start)\n", me, c.Password)
		return c, nil
	case cfg.Auth.PAM != nil:
		return pam.New(cfg.Auth.PAM.Service, "", pamConcurrency)
	}
	return nil, errors.New("no sign-in method is on")
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
