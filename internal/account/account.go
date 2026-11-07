// Package account resolves a signed-in name to the host account sessions run as
// (ADR-0005): the OS's own record (getpwnam through NSS, so SSSD on the work server),
// never what the person typed. Ownership is keyed on the canonical name the OS returns
// (pw_name), so "alice", "ALICE" and "alice@corp" are one account when SSSD says so
// (atelier-3b90b60-N3).
//
// Build with cgo (the default when a C compiler is present) and without the osusergo
// tag: only then does os/user ask NSS, and so SSSD. Without cgo it reads /etc/passwd
// alone and AD accounts are unknown.
package account

import (
	"errors"
	"fmt"
	"os/user"
	"path/filepath"
	"slices"
	"strconv"

	"github.com/gkroken/Atelier/internal/ide"
	"github.com/gkroken/Atelier/internal/launcher"
)

var (
	// ErrNoAccount: the OS has no account by that name, or the account has no account
	// ID (an AD user without uidNumber, which SSSD then doesn't return).
	ErrNoAccount = errors.New("account: no host account")
	// ErrRefused: the account exists but sessions may not run as it (root, a system
	// account, or a record Atelier can't use safely).
	ErrRefused = errors.New("account: sessions may not run as this account")
)

// MinUID is the lowest account ID sessions may run as. Below it are root and system
// accounts (Debian and RHEL both start ordinary users at 1000; AD IDs from SSSD are
// far higher).
const MinUID = 1000

// overflowID reports the IDs reserved for nobody/nogroup and the overflow and "-1"
// values: on NFS with root_squash, nobody owns every file root writes
// (atelier-7e6f55e-F1).
func overflowID(id int) bool {
	switch id {
	case 65534, 65535, 4294967294, 4294967295:
		return true
	}
	return false
}

// Account is a host account: what the launcher needs, and the supplementary group IDs
// (getgrouplist) for group grants (ADR-0013).
type Account struct {
	launcher.Account
	Groups []int
}

// OS is the lookup functions the resolver calls; tests replace them.
type OS struct {
	Lookup        func(name string) (*user.User, error)
	LookupGroupID func(gid string) (*user.Group, error)
	GroupIDs      func(u *user.User) ([]string, error)
}

// Host uses the host's NSS (os/user with cgo).
var Host = OS{
	Lookup:        user.Lookup,
	LookupGroupID: user.LookupGroupId,
	GroupIDs:      (*user.User).GroupIds,
}

// Resolve returns the host account for name. It never trusts name beyond the lookup:
// the result's Name is the OS's.
func (o OS) Resolve(name string) (Account, error) {
	u, err := o.Lookup(name)
	if err != nil {
		var unknown user.UnknownUserError
		if errors.As(err, &unknown) {
			return Account{}, ErrNoAccount
		}
		return Account{}, fmt.Errorf("account: look up %q: %w", name, err)
	}
	refuse := func(why string) (Account, error) {
		return Account{}, fmt.Errorf("%w: %s (%s)", ErrRefused, u.Username, why)
	}
	if !ide.ValidName(u.Username) {
		return refuse("the user name has characters Atelier doesn't allow")
	}
	uid, err := strconv.Atoi(u.Uid)
	if err != nil {
		return refuse("no numeric account ID")
	}
	gid, err := strconv.Atoi(u.Gid)
	if err != nil {
		return refuse("no numeric group ID")
	}
	if uid < MinUID {
		return refuse(fmt.Sprintf("account ID %d is below %d", uid, MinUID))
	}
	if gid == 0 {
		return refuse("primary group is root's")
	}
	if overflowID(uid) || overflowID(gid) {
		return refuse(fmt.Sprintf("account ID %d / group ID %d is nobody's or reserved", uid, gid))
	}
	if !filepath.IsAbs(u.HomeDir) || filepath.Clean(u.HomeDir) != u.HomeDir || u.HomeDir == "/" || !ide.PasswdSafe(u.HomeDir) {
		return refuse(fmt.Sprintf("home folder %q", u.HomeDir))
	}

	a := Account{Account: launcher.Account{Name: u.Username, UID: uid, GID: gid, Home: u.HomeDir}}
	if g, err := o.LookupGroupID(u.Gid); err == nil && ide.ValidName(g.Name) {
		a.Group = g.Name
	}
	ids, err := o.GroupIDs(u)
	if err != nil {
		return Account{}, fmt.Errorf("account: groups of %s: %w", u.Username, err)
	}
	for _, s := range ids {
		g, err := strconv.Atoi(s)
		if err != nil || g == 0 || g == gid {
			continue // root's group is never passed on; the primary is already GID
		}
		a.Groups = append(a.Groups, g)
	}
	slices.Sort(a.Groups)
	a.Groups = slices.Compact(a.Groups)
	return a, nil
}
