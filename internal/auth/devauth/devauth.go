// Package devauth is development-only sign-in: the account running Atelier, with a
// random password made at startup and printed to the terminal, as Jupyter does with
// its token. No PAM, so no sudo and no real password on a dev machine; and since the
// password is new each run and only the terminal shows it, other accounts on a shared
// machine can't sign in as you over loopback (atelier-d57d7bd-N2). config refuses
// auth.dev unless Atelier listens on loopback only.
package devauth

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base32"
	"strings"

	"github.com/gkroken/Atelier/internal/auth/pam"
)

// Checker accepts only User with Password.
type Checker struct{ User, Password string }

// New returns a Checker for user with a fresh random password (80 bits, as 16
// lowercase letters and digits).
func New(user string) Checker {
	b := make([]byte, 10)
	_, _ = rand.Read(b) // never fails on Linux (Go 1.24+)
	return Checker{User: user, Password: strings.ToLower(base32.StdEncoding.EncodeToString(b))}
}

// Check returns pam.ErrAuth for anything but User and Password, like PAM would.
func (c Checker) Check(_ context.Context, user, password string) (string, error) {
	okUser := subtle.ConstantTimeCompare([]byte(user), []byte(c.User))
	okPass := subtle.ConstantTimeCompare([]byte(password), []byte(c.Password))
	if c.User == "" || len(c.Password) < 16 || okUser&okPass != 1 {
		return "", pam.ErrAuth
	}
	return c.User, nil
}
