package devauth

import (
	"context"
	"errors"
	"testing"

	"github.com/gkroken/Atelier/internal/auth/pam"
)

func TestCheck(t *testing.T) {
	c := Checker{User: "alice"}
	if u, err := c.Check(context.Background(), "alice", Password); err != nil || u != "alice" {
		t.Fatalf("right: %q %v", u, err)
	}
	for _, tc := range [][2]string{{"alice", "wrong"}, {"bob", Password}, {"ALICE", Password}, {"alice", ""}, {"", Password}, {"alice", Password + " "}} {
		if _, err := c.Check(context.Background(), tc[0], tc[1]); !errors.Is(err, pam.ErrAuth) {
			t.Errorf("%q/%q: %v, want ErrAuth", tc[0], tc[1], err)
		}
	}
	if _, err := (Checker{}).Check(context.Background(), "", Password); !errors.Is(err, pam.ErrAuth) {
		t.Errorf("empty Checker accepted: %v", err)
	}
}
