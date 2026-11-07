// Package devauth is development-only sign-in: the account running Atelier, with the
// fixed password "atelier-dev". No PAM, so no sudo and no real password on a dev
// machine. config refuses it unless Atelier listens on loopback only, and Atelier logs
// a warning whenever it's on.
package devauth

import (
	"context"
	"crypto/subtle"

	"github.com/gkroken/Atelier/internal/auth/pam"
)

// Password is the one password devauth accepts.
const Password = "atelier-dev"

// Checker accepts only User with Password.
type Checker struct{ User string }

// Check returns pam.ErrAuth for anything but User and Password, like PAM would.
func (c Checker) Check(_ context.Context, user, password string) (string, error) {
	okUser := subtle.ConstantTimeCompare([]byte(user), []byte(c.User))
	okPass := subtle.ConstantTimeCompare([]byte(password), []byte(Password))
	if c.User == "" || okUser&okPass != 1 {
		return "", pam.ErrAuth
	}
	return c.User, nil
}
