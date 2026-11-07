package devauth

import (
	"context"
	"errors"
	"testing"

	"github.com/gkroken/Atelier/internal/auth/pam"
)

func TestCheck(t *testing.T) {
	c := New("alice")
	if len(c.Password) != 16 {
		t.Fatalf("password %q", c.Password)
	}
	if u, err := c.Check(context.Background(), "alice", c.Password); err != nil || u != "alice" {
		t.Fatalf("right: %q %v", u, err)
	}
	for _, tc := range [][2]string{{"alice", "wrong"}, {"bob", c.Password}, {"ALICE", c.Password}, {"alice", ""}, {"", c.Password}, {"alice", c.Password + " "}, {"alice", "atelier-dev"}} {
		if _, err := c.Check(context.Background(), tc[0], tc[1]); !errors.Is(err, pam.ErrAuth) {
			t.Errorf("%q/%q: %v, want ErrAuth", tc[0], tc[1], err)
		}
	}
	if New("alice").Password == c.Password {
		t.Error("two runs got the same password")
	}
	// A Checker without a real password accepts nothing, the empty password included.
	if _, err := (Checker{User: "alice"}).Check(context.Background(), "alice", ""); !errors.Is(err, pam.ErrAuth) {
		t.Errorf("empty Checker accepted: %v", err)
	}
}
