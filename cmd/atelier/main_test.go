package main

import (
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/gkroken/Atelier/internal/auth/devauth"
	"github.com/gkroken/Atelier/internal/config"
)

func load(t *testing.T, doc string) *config.Config {
	t.Helper()
	c, err := config.Load([]byte(doc), func(string) (string, bool) { return "s3cret", true })
	if err != nil {
		t.Fatal(err)
	}
	return c
}

const base = `
server:
  listen: LISTEN
  mainHost: atelier.example.org
  sessionsHost: sessions.atelier.example.org
TLS
auth:
AUTH
`

func doc(listen, tls, auth string) string {
	return strings.NewReplacer("LISTEN", listen, "TLS", tls, "AUTH", auth).Replace(base)
}

const withTLS = "  tls: {certFile: /etc/c.pem, keyFile: /etc/k.pem}"

// Beyond loopback only with TLS: a password never crosses the network in plain text.
func TestListenRule(t *testing.T) {
	for _, tc := range []struct {
		listen, tls string
		ok          bool
	}{
		{"127.0.0.1:8080", "", true},
		{"localhost:8080", "", true},
		{`"[::1]:8080"`, "", true},
		{"0.0.0.0:8443", "", false},
		{`":8443"`, "", false},
		{"192.168.1.5:8443", "", false},
		{"0.0.0.0:8443", withTLS, true},
		{`":8443"`, withTLS, true},
	} {
		err := listenRule(load(t, doc(tc.listen, tc.tls, "  pam: {}")))
		if (err == nil) != tc.ok {
			t.Errorf("listen %s tls=%v: %v", tc.listen, tc.tls != "", err)
		}
	}
}

func TestPasswordChecker(t *testing.T) {
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	c, err := passwordChecker(load(t, doc("127.0.0.1:8080", "", "  dev: {}")), "gakro", quiet)
	if dc, ok := c.(devauth.Checker); err != nil || !ok || dc.User != "gakro" || len(dc.Password) != 16 {
		t.Errorf("dev: %T %v", c, err)
	}
	oidcOnly := "  oidc: {issuer: https://idp.example.org, clientId: a, clientSecret: \"${S}\"}"
	if _, err := passwordChecker(load(t, doc("127.0.0.1:8080", "", oidcOnly)), "gakro", quiet); err == nil || !strings.Contains(err.Error(), "spine #2c") {
		t.Errorf("OIDC only: %v", err)
	}
	// PAM needs /etc/pam.d/<service>: a service that can't exist is refused by pam.New.
	if _, err := passwordChecker(load(t, doc("127.0.0.1:8080", "", "  pam: {service: atelier-test-missing-zz9}")), "gakro", quiet); err == nil {
		t.Error("PAM with a missing service file was accepted")
	}
}
