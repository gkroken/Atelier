package account

import (
	"errors"
	"os/user"
	"reflect"
	"strings"
	"testing"
)

// fakeOS is an NSS that, like SSSD with case_sensitive = false and a domain suffix,
// answers several spellings with one record.
func fakeOS(users map[string]*user.User, groups map[string][]string) OS {
	return OS{
		Lookup: func(name string) (*user.User, error) {
			if u, ok := users[name]; ok {
				return u, nil
			}
			return nil, user.UnknownUserError(name)
		},
		LookupGroupID: func(gid string) (*user.Group, error) {
			return &user.Group{Gid: gid, Name: "g" + gid}, nil
		},
		GroupIDs: func(u *user.User) ([]string, error) { return groups[u.Username], nil },
	}
}

func TestResolve(t *testing.T) {
	alice := &user.User{Uid: "1001", Gid: "1001", Username: "alice", HomeDir: "/home/alice"}
	o := fakeOS(map[string]*user.User{
		"alice": alice, "ALICE": alice, "alice@corp": alice,
		"root":     {Uid: "0", Gid: "0", Username: "root", HomeDir: "/root"},
		"daemon":   {Uid: "1", Gid: "1", Username: "daemon", HomeDir: "/usr/sbin"},
		"nouid":    {Uid: "", Gid: "1002", Username: "nouid", HomeDir: "/home/nouid"},
		"rootgrp":  {Uid: "1003", Gid: "0", Username: "rootgrp", HomeDir: "/home/rootgrp"},
		"relhome":  {Uid: "1004", Gid: "1004", Username: "relhome", HomeDir: "home/relhome"},
		"slash":    {Uid: "1005", Gid: "1005", Username: "slash", HomeDir: "/"},
		"colon":    {Uid: "1006", Gid: "1006", Username: "colon", HomeDir: "/home/a:b"},
		"dotdot":   {Uid: "1007", Gid: "1007", Username: "dotdot", HomeDir: "/home/../etc"},
		"badname":  {Uid: "1008", Gid: "1008", Username: "bad name", HomeDir: "/home/x"},
		"colonusr": {Uid: "1009", Gid: "1009", Username: "a:b", HomeDir: "/home/x"},
	}, map[string][]string{"alice": {"1001", "0", "2000", "2000", "x", "1500"}})

	for _, name := range []string{"alice", "ALICE", "alice@corp"} {
		a, err := o.Resolve(name)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		want := Account{Groups: []int{1500, 2000}}
		want.Name, want.UID, want.GID, want.Group, want.Home = "alice", 1001, 1001, "g1001", "/home/alice"
		if !reflect.DeepEqual(a, want) {
			t.Errorf("%s: got %+v, want %+v", name, a, want)
		}
	}

	for _, tc := range []struct {
		name string
		want error
		says string
	}{
		{"bob", ErrNoAccount, ""},
		{"root", ErrRefused, "below 1000"},
		{"daemon", ErrRefused, "below 1000"},
		{"nouid", ErrRefused, "no numeric account ID"},
		{"rootgrp", ErrRefused, "root's"},
		{"relhome", ErrRefused, "home folder"},
		{"slash", ErrRefused, "home folder"},
		{"colon", ErrRefused, "home folder"},
		{"dotdot", ErrRefused, "home folder"},
		{"badname", ErrRefused, "characters"},
		{"colonusr", ErrRefused, "characters"},
	} {
		_, err := o.Resolve(tc.name)
		if !errors.Is(err, tc.want) || !strings.Contains(err.Error(), tc.says) {
			t.Errorf("%s: %v, want %v saying %q", tc.name, err, tc.want, tc.says)
		}
	}
}

func TestLookupErrorIsNotNoAccount(t *testing.T) {
	o := fakeOS(nil, nil)
	o.Lookup = func(string) (*user.User, error) { return nil, errors.New("sssd: timed out") }
	if _, err := o.Resolve("alice"); err == nil || errors.Is(err, ErrNoAccount) || errors.Is(err, ErrRefused) {
		t.Fatalf("NSS failure: %v; want an error that is neither ErrNoAccount nor ErrRefused", err)
	}
}

// The real NSS: the test's own account resolves to itself (when it may run sessions).
func TestHostResolvesTheCurrentUser(t *testing.T) {
	me, err := user.Current()
	if err != nil {
		t.Skip(err)
	}
	a, err := Host.Resolve(me.Username)
	if errors.Is(err, ErrRefused) {
		t.Skipf("this account can't run sessions: %v", err)
	}
	if err != nil {
		t.Fatal(err)
	}
	if a.Name != me.Username || a.Home != me.HomeDir {
		t.Errorf("got %+v for %s", a, me.Username)
	}
	if _, err := Host.Resolve("atelier-no-such-user-x9"); !errors.Is(err, ErrNoAccount) {
		t.Errorf("unknown user: %v", err)
	}
}
