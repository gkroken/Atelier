//go:build !(linux && cgo)

package pam

import (
	"context"
	"errors"
)

var (
	ErrAuth        = errors.New("pam: wrong user name or password")
	ErrAccount     = errors.New("pam: account may not sign in")
	ErrUnavailable = errors.New("pam: authentication service unavailable")
	ErrBusy        = errors.New("pam: too many sign-ins at once")
)

// Checker is unavailable without cgo on Linux; Check always fails.
type Checker struct{}

func New(service, confdir string, concurrent int) (*Checker, error) {
	return nil, errors.New("pam: this build has no PAM support (needs Linux and cgo)")
}

func (c *Checker) Check(ctx context.Context, user, password string) (string, error) {
	return "", errors.New("pam: this build has no PAM support (needs Linux and cgo)")
}
