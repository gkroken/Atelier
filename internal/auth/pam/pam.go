//go:build linux && cgo

// Package pam checks a password through Linux-PAM, in process (ADR-0017). On the work
// server the service's stack is pam_sss, so SSSD checks the password against AD, and
// Atelier needs no root.
//
// The password is copied into C memory for the PAM conversation and wiped there when
// the check ends. The Go string the caller holds can't be wiped (Go strings are
// immutable); callers keep it short-lived and never log it.
package pam

/*
#cgo LDFLAGS: -lpam
#include <security/pam_appl.h>
#include <stdlib.h>
#include <string.h>

// The conversation's secrets, owned by C for the length of one check.
typedef struct {
	char *user;
	char *password;
} atelier_creds;

// atelier_conv answers PAM's prompts: the password to a hidden prompt, the user name
// to a visible one (some modules ask), nothing to messages. PAM frees the replies.
static int atelier_conv(int n, const struct pam_message **msg, struct pam_response **resp, void *data) {
	atelier_creds *c = data;
	if (n <= 0 || n > PAM_MAX_NUM_MSG) {
		return PAM_CONV_ERR;
	}
	struct pam_response *r = calloc(n, sizeof(struct pam_response));
	if (r == NULL) {
		return PAM_BUF_ERR;
	}
	for (int i = 0; i < n; i++) {
		const char *answer = NULL;
		switch (msg[i]->msg_style) {
		case PAM_PROMPT_ECHO_OFF:
			answer = c->password;
			break;
		case PAM_PROMPT_ECHO_ON:
			answer = c->user;
			break;
		case PAM_ERROR_MSG:
		case PAM_TEXT_INFO:
			continue;
		default:
			goto fail;
		}
		r[i].resp = strdup(answer);
		if (r[i].resp == NULL) {
			goto fail;
		}
	}
	*resp = r;
	return PAM_SUCCESS;
fail:
	for (int i = 0; i < n; i++) {
		if (r[i].resp != NULL) {
			explicit_bzero(r[i].resp, strlen(r[i].resp));
			free(r[i].resp);
		}
	}
	free(r);
	return PAM_CONV_ERR;
}

// atelier_check runs auth then account management for one user. *out_user gets the
// name PAM ended with (a module may map it); the caller frees it.
static int atelier_check(const char *service, const char *confdir, atelier_creds *c, int *stage, char **out_user) {
	struct pam_conv conv = { atelier_conv, c };
	pam_handle_t *h = NULL;
	int rc;
	*stage = 0;
	if (confdir != NULL) {
		rc = pam_start_confdir(service, c->user, &conv, confdir, &h);
	} else {
		rc = pam_start(service, c->user, &conv, &h);
	}
	if (rc != PAM_SUCCESS) {
		return rc;
	}
	*stage = 1;
	rc = pam_authenticate(h, PAM_SILENT | PAM_DISALLOW_NULL_AUTHTOK);
	if (rc == PAM_SUCCESS) {
		*stage = 2;
		rc = pam_acct_mgmt(h, PAM_SILENT | PAM_DISALLOW_NULL_AUTHTOK);
	}
	if (rc == PAM_SUCCESS) {
		const void *item = NULL;
		if (pam_get_item(h, PAM_USER, &item) == PAM_SUCCESS && item != NULL) {
			*out_user = strdup(item);
		}
	}
	pam_end(h, rc);
	return rc;
}

// atelier_account runs account management only, with no password: for a sign-in
// another method (OIDC) already authenticated. The conversation has no password to
// give, so a module that asks for one fails.
static int atelier_account(const char *service, const char *confdir, atelier_creds *c, int *stage) {
	struct pam_conv conv = { atelier_conv, c };
	pam_handle_t *h = NULL;
	int rc;
	*stage = 0;
	if (confdir != NULL) {
		rc = pam_start_confdir(service, c->user, &conv, confdir, &h);
	} else {
		rc = pam_start(service, c->user, &conv, &h);
	}
	if (rc != PAM_SUCCESS) {
		return rc;
	}
	*stage = 2;
	rc = pam_acct_mgmt(h, PAM_SILENT | PAM_DISALLOW_NULL_AUTHTOK);
	pam_end(h, rc);
	return rc;
}

static void atelier_wipe(char *s) {
	if (s != NULL) {
		explicit_bzero(s, strlen(s));
		free(s);
	}
}
*/
import "C"

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"unsafe"
)

var (
	// ErrAuth: the user name or password is wrong. An unknown user gets the same error,
	// so callers can't tell which, and neither can the person signing in.
	ErrAuth = errors.New("pam: wrong user name or password")
	// ErrAccount: the password was right but the account may not sign in (disabled,
	// expired, locked, outside its allowed hours, or its password must be changed).
	ErrAccount = errors.New("pam: account may not sign in")
	// ErrUnavailable: PAM couldn't reach the authentication service (SSSD or AD down).
	// Not the user's fault: it shouldn't count against them in a rate limit.
	ErrUnavailable = errors.New("pam: authentication service unavailable")
	// ErrBusy: too many checks are already running.
	ErrBusy = errors.New("pam: too many sign-ins at once")
)

// Limits on what's passed to PAM. A NUL would cut a C string short, so it's refused.
const (
	maxUser     = 256
	maxPassword = 4096
)

// Checker checks passwords with one PAM service.
type Checker struct {
	service string
	confdir string // "" for /etc/pam.d
	slots   chan struct{}
}

// systemDirs is where Linux-PAM looks for a service without a confdir: /etc/pam.d, then
// the vendor folder some distributions use.
var systemDirs = []string{"/etc/pam.d", "/usr/lib/pam.d"}

// New returns a Checker for service (/etc/pam.d/<service>). confdir, when not empty, is
// a folder to read the service from instead (pam_start_confdir; used by tests).
// At most concurrent checks run at once; more get ErrBusy.
//
// The service's file must exist. Without it Linux-PAM quietly uses /etc/pam.d/other,
// so sign-in would either fail for everyone as a "wrong password" or run through a
// stack ADR-0017 didn't choose (atelier-7e6f55e-F2).
func New(service, confdir string, concurrent int) (*Checker, error) {
	if service == "" || service == "." || service == ".." || strings.ContainsAny(service, "/\x00") {
		return nil, fmt.Errorf("pam: bad service name %q", service)
	}
	dirs := systemDirs
	if confdir != "" {
		dirs = []string{confdir}
	}
	found := false
	for _, d := range dirs {
		if fi, err := os.Stat(filepath.Join(d, service)); err == nil && fi.Mode().IsRegular() {
			found = true
			break
		}
	}
	if !found {
		return nil, fmt.Errorf("pam: no file for PAM service %q: create %s (ADR-0017); without it PAM falls back to the \"other\" service",
			service, filepath.Join(dirs[0], service))
	}
	if concurrent < 1 {
		concurrent = 1
	}
	return &Checker{service: service, confdir: confdir, slots: make(chan struct{}, concurrent)}, nil
}

// Check verifies user's password and that the account may sign in. It returns the
// user name PAM ended with, which the caller still resolves to the canonical OS
// account. If ctx ends first, Check returns ctx's error; the PAM call itself can't be
// interrupted and finishes in the background, still holding its slot.
func (c *Checker) Check(ctx context.Context, user, password string) (string, error) {
	if user == "" || len(user) > maxUser || strings.ContainsRune(user, 0) {
		return "", ErrAuth
	}
	if password == "" || len(password) > maxPassword || strings.ContainsRune(password, 0) {
		return "", ErrAuth
	}
	select {
	case c.slots <- struct{}{}:
	default:
		return "", ErrBusy
	}
	type result struct {
		user string
		err  error
	}
	done := make(chan result, 1)
	go func() {
		defer func() { <-c.slots }()
		u, err := c.check(user, password)
		done <- result{u, err}
	}()
	select {
	case r := <-done:
		return r.user, r.err
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

func (c *Checker) check(user, password string) (string, error) {
	service := C.CString(c.service)
	defer C.free(unsafe.Pointer(service))
	var confdir *C.char
	if c.confdir != "" {
		confdir = C.CString(c.confdir)
		defer C.free(unsafe.Pointer(confdir))
	}
	creds := (*C.atelier_creds)(C.calloc(1, C.size_t(unsafe.Sizeof(C.atelier_creds{}))))
	if creds == nil {
		return "", errors.New("pam: out of memory")
	}
	creds.user = C.CString(user)
	creds.password = C.CString(password)
	defer func() {
		C.atelier_wipe(creds.password)
		C.free(unsafe.Pointer(creds.user))
		C.free(unsafe.Pointer(creds))
	}()

	var stage C.int
	var out *C.char
	rc := C.atelier_check(service, confdir, creds, &stage, &out)
	if out != nil {
		defer C.free(unsafe.Pointer(out))
	}
	if err := classify(int(rc), int(stage)); err != nil {
		return "", err
	}
	if out == nil {
		return user, nil
	}
	return C.GoString(out), nil
}

// Account checks only that user's account may sign in now (pam_acct_mgmt): for a
// person another method, such as OIDC, has authenticated. With pam_sss this refuses
// accounts SSSD doesn't serve (local ones) and accounts disabled, expired or locked in
// AD (atelier-7e6f55e-N2). It returns nil, ErrAccount, ErrUnavailable, ErrBusy, ctx's
// error, or another failure; never ErrAuth.
func (c *Checker) Account(ctx context.Context, user string) error {
	if user == "" || len(user) > maxUser || strings.ContainsRune(user, 0) {
		return ErrAccount
	}
	select {
	case c.slots <- struct{}{}:
	default:
		return ErrBusy
	}
	done := make(chan error, 1)
	go func() {
		defer func() { <-c.slots }()
		done <- c.account(user)
	}()
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (c *Checker) account(user string) error {
	service := C.CString(c.service)
	defer C.free(unsafe.Pointer(service))
	var confdir *C.char
	if c.confdir != "" {
		confdir = C.CString(c.confdir)
		defer C.free(unsafe.Pointer(confdir))
	}
	creds := (*C.atelier_creds)(C.calloc(1, C.size_t(unsafe.Sizeof(C.atelier_creds{}))))
	if creds == nil {
		return errors.New("pam: out of memory")
	}
	creds.user = C.CString(user)
	creds.password = C.CString("") // nothing to give: a password prompt gets an empty answer
	defer func() {
		C.atelier_wipe(creds.password)
		C.free(unsafe.Pointer(creds.user))
		C.free(unsafe.Pointer(creds))
	}()
	var stage C.int
	rc := C.atelier_account(service, confdir, creds, &stage)
	err := classify(int(rc), int(stage))
	if errors.Is(err, ErrAuth) {
		return ErrAccount // stage 0 user-unknown: still "may not sign in"
	}
	return err
}

// classify maps a PAM result to an error. stage says where it stopped: 0 starting PAM,
// 1 authentication, 2 account management.
func classify(rc, stage int) error {
	if rc == C.PAM_SUCCESS {
		return nil
	}
	switch rc {
	case C.PAM_AUTHINFO_UNAVAIL:
		return ErrUnavailable
	case C.PAM_AUTH_ERR, C.PAM_USER_UNKNOWN, C.PAM_CRED_INSUFFICIENT, C.PAM_MAXTRIES:
		if stage == 2 {
			return ErrAccount // e.g. pam_deny or pam_sss refusing access in "account"
		}
		return ErrAuth
	case C.PAM_ACCT_EXPIRED, C.PAM_NEW_AUTHTOK_REQD, C.PAM_PERM_DENIED, C.PAM_AUTHTOK_EXPIRED:
		return ErrAccount
	}
	msg := C.GoString(C.pam_strerror(nil, C.int(rc)))
	if stage == 2 {
		return fmt.Errorf("%w: %s", ErrAccount, msg)
	}
	return fmt.Errorf("pam: %s (code %d)", msg, rc)
}

// PAM result codes, for tests (a _test.go file can't use cgo).
const (
	codeSuccess         = int(C.PAM_SUCCESS)
	codeAuthErr         = int(C.PAM_AUTH_ERR)
	codeUserUnknown     = int(C.PAM_USER_UNKNOWN)
	codeAuthinfoUnavail = int(C.PAM_AUTHINFO_UNAVAIL)
	codeAcctExpired     = int(C.PAM_ACCT_EXPIRED)
	codeNewAuthtokReqd  = int(C.PAM_NEW_AUTHTOK_REQD)
	codePermDenied      = int(C.PAM_PERM_DENIED)
	codeSystemErr       = int(C.PAM_SYSTEM_ERR)
)
