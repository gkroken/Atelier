//go:build linux && cgo

package pam

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The test stands in for pam_sss with pam_exec scripts in a private PAM folder
// (pam_start_confdir), so it needs no root and installs nothing into /etc/pam.d.
//
//	alice  password "right pass" (with a space, and a non-ASCII one: see users)
//	carol  password "carols", but account management refuses her (disabled in AD)
//	slow   password "slow", answers after a pause
//
// The auth script records what the conversation handed it, so the test can see the
// password reached PAM exactly.
const authScript = `#!/bin/sh
IFS= read -r pw
printf '%s' "$pw" > "$(dirname "$0")/seen-$PAM_USER"
case "$PAM_USER:$pw" in
"alice:right pass"|"carol:carols"|"émile:pässwörd") exit 0 ;;
"slow:slow") sleep 1; exit 0 ;;
esac
exit 1
`

const acctScript = `#!/bin/sh
[ "$PAM_USER" != carol ]
`

const service = `auth    [success=1 default=ignore] pam_exec.so expose_authtok quiet DIR/auth.sh
auth    requisite pam_deny.so
auth    required  pam_permit.so
account [success=1 default=ignore] pam_exec.so quiet DIR/acct.sh
account requisite pam_deny.so
account required  pam_permit.so
`

func testChecker(t *testing.T, concurrent int) (*Checker, string) {
	t.Helper()
	dir := t.TempDir()
	for name, body := range map[string]string{"auth.sh": authScript, "acct.sh": acctScript} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	conf := filepath.Join(dir, "pam.d")
	if err := os.Mkdir(conf, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(conf, "atelier-test"), []byte(strings.ReplaceAll(service, "DIR", dir)), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := New("atelier-test", conf, concurrent)
	if err != nil {
		t.Fatal(err)
	}
	return c, dir
}

// Without a confdir, the service's file must exist in the system folders, or New
// refuses (PAM would fall back to "other"). A temp folder stands in for /etc/pam.d.
func TestNewRefusesAMissingServiceFile(t *testing.T) {
	etc, vendor := t.TempDir(), t.TempDir()
	defer func(d []string) { systemDirs = d }(systemDirs)
	systemDirs = []string{etc, vendor}

	_, err := New("atelier", "", 1)
	if err == nil || !strings.Contains(err.Error(), filepath.Join(etc, "atelier")) {
		t.Fatalf("missing service: %v; want an error naming %s", err, filepath.Join(etc, "atelier"))
	}
	if err := os.Mkdir(filepath.Join(etc, "atelier"), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := New("atelier", "", 1); err == nil {
		t.Fatal("a folder named like the service was accepted")
	}
	if err := os.WriteFile(filepath.Join(vendor, "atelier"), []byte("auth required pam_deny.so\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := New("atelier", "", 1); err != nil {
		t.Fatalf("service in the vendor folder: %v", err)
	}
	if err := os.WriteFile(filepath.Join(etc, "atelier-2"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := New("atelier-2", "", 1); err != nil {
		t.Fatalf("service in /etc/pam.d: %v", err)
	}
	// With a confdir, only the confdir counts.
	if _, err := New("atelier-2", vendor, 1); err == nil {
		t.Fatal("service missing from the confdir was accepted")
	}
	for _, bad := range []string{"", ".", "..", "../atelier", "a/b", "a\x00b"} {
		if _, err := New(bad, etc, 1); err == nil || !strings.Contains(err.Error(), "bad service name") {
			t.Errorf("service %q: %v", bad, err)
		}
	}
}

func TestCheck(t *testing.T) {
	c, dir := testChecker(t, 4)
	for _, tc := range []struct {
		name, user, password string
		want                 error
	}{
		{"right password", "alice", "right pass", nil},
		{"non-ASCII", "émile", "pässwörd", nil},
		{"wrong password", "alice", "wrong", ErrAuth},
		{"password with extra", "alice", "right pass ", ErrAuth},
		{"unknown user", "bob", "right pass", ErrAuth},
		{"disabled account", "carol", "carols", ErrAccount},
		{"disabled account, wrong password", "carol", "nope", ErrAuth},
		{"empty password", "alice", "", ErrAuth},
		{"empty user", "", "right pass", ErrAuth},
		{"NUL in password", "alice", "right pass\x00more", ErrAuth},
		{"NUL in user", "alice\x00x", "right pass", ErrAuth},
		{"long password", "alice", strings.Repeat("x", maxPassword+1), ErrAuth},
		{"long user", strings.Repeat("a", maxUser+1), "right pass", ErrAuth},
	} {
		t.Run(tc.name, func(t *testing.T) {
			user, err := c.Check(context.Background(), tc.user, tc.password)
			if !errors.Is(err, tc.want) || (tc.want == nil) != (err == nil) {
				t.Fatalf("Check(%q) = %q, %v; want %v", tc.user, user, err, tc.want)
			}
			if err == nil && user != tc.user {
				t.Errorf("user %q, want %q", user, tc.user)
			}
		})
	}
	// The password reached the module exactly, through the conversation.
	if seen, err := os.ReadFile(filepath.Join(dir, "seen-alice")); err != nil || len(seen) == 0 {
		t.Errorf("auth script saw nothing for alice: %v", err)
	}
	if seen, _ := os.ReadFile(filepath.Join(dir, "seen-émile")); string(seen) != "pässwörd" {
		t.Errorf("auth script saw %q", seen)
	}
}

// Nothing reaches PAM for refused input: no module runs.
func TestRefusedInputNeverReachesPAM(t *testing.T) {
	c, dir := testChecker(t, 1)
	c.Check(context.Background(), "nul\x00user", "x")
	c.Check(context.Background(), "empty", "")
	for _, u := range []string{"nul", "empty"} {
		if _, err := os.Stat(filepath.Join(dir, "seen-"+u)); err == nil {
			t.Errorf("%s reached the PAM module", u)
		}
	}
}

func TestBusyAndCanceled(t *testing.T) {
	c, _ := testChecker(t, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	if _, err := c.Check(ctx, "slow", "slow"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("slow check with a short deadline: %v", err)
	}
	// The slow check still holds the only slot.
	if _, err := c.Check(context.Background(), "alice", "right pass"); !errors.Is(err, ErrBusy) {
		t.Fatalf("second check while one runs: %v, want ErrBusy", err)
	}
	// Once it finishes, the slot is free again.
	deadline := time.Now().Add(5 * time.Second)
	for {
		_, err := c.Check(context.Background(), "alice", "right pass")
		if err == nil {
			break
		}
		if !errors.Is(err, ErrBusy) || time.Now().After(deadline) {
			t.Fatalf("after the slow check: %v", err)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func TestMissingServiceIsNotAnAuthFailure(t *testing.T) {
	c, _ := testChecker(t, 1)
	c.service = "no-such-service"
	_, err := c.Check(context.Background(), "alice", "right pass")
	if err == nil || errors.Is(err, ErrAuth) {
		t.Fatalf("missing PAM service: %v; want an error that isn't ErrAuth", err)
	}
}

func TestClassify(t *testing.T) {
	for _, tc := range []struct {
		rc, stage int
		want      error
	}{
		{codeSuccess, 2, nil},
		{codeAuthErr, 1, ErrAuth},
		{codeUserUnknown, 1, ErrAuth},
		{codeUserUnknown, 0, ErrAuth},
		{codeAuthErr, 2, ErrAccount},
		{codeAcctExpired, 2, ErrAccount},
		{codeNewAuthtokReqd, 2, ErrAccount},
		{codePermDenied, 2, ErrAccount},
		{codeSystemErr, 2, ErrAccount},
		{codeAuthinfoUnavail, 1, ErrUnavailable},
		{codeAuthinfoUnavail, 2, ErrUnavailable},
	} {
		if err := classify(tc.rc, tc.stage); !errors.Is(err, tc.want) || (tc.want == nil) != (err == nil) {
			t.Errorf("classify(%d, %d) = %v, want %v", tc.rc, tc.stage, err, tc.want)
		}
	}
	if err := classify(codeSystemErr, 1); err == nil || errors.Is(err, ErrAuth) || errors.Is(err, ErrAccount) {
		t.Errorf("system error in auth: %v", err)
	}
}

// Account runs only the account stack: carol (disabled) is refused, alice and an
// unknown name pass or fail as the account script says, and the auth script (the
// password check) never runs.
func TestAccount(t *testing.T) {
	c, dir := testChecker(t, 2)
	if err := c.Account(context.Background(), "alice"); err != nil {
		t.Errorf("alice: %v", err)
	}
	for _, u := range []string{"carol", "", "nul\x00x", strings.Repeat("a", maxUser+1)} {
		if err := c.Account(context.Background(), u); !errors.Is(err, ErrAccount) {
			t.Errorf("%q: %v, want ErrAccount", u, err)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "seen-alice")); err == nil {
		t.Error("Account ran the password check")
	}
	// A service whose account stack asks for a password gets nothing and refuses.
	conf := filepath.Join(dir, "pam.d")
	os.WriteFile(filepath.Join(conf, "asks"), []byte(strings.ReplaceAll(
		"account [success=1 default=ignore] pam_exec.so expose_authtok quiet DIR/auth.sh\naccount requisite pam_deny.so\naccount required pam_permit.so\n", "DIR", dir)), 0o600)
	asks, err := New("asks", conf, 1)
	if err != nil {
		t.Fatal(err)
	}
	if err := asks.Account(context.Background(), "alice"); err == nil {
		t.Error("an account stack asking for a password passed with none")
	}
}
