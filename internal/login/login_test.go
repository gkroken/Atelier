package login

import (
	"testing"
	"time"

	"github.com/gkroken/Atelier/internal/account"
)

func acct(name string) account.Account {
	var a account.Account
	a.Name, a.UID, a.GID, a.Home = name, 1001, 1001, "/home/"+name
	return a
}

func testStore() (*Store, *time.Time) {
	s := New()
	clock := time.Date(2026, 11, 7, 9, 0, 0, 0, time.UTC)
	s.now = func() time.Time { return clock }
	return s, &clock
}

func TestSignInTicketSessions(t *testing.T) {
	s, _ := testStore()
	tok, l := s.Create(acct("alice"), "pam")
	if got, ok := s.Main(tok); !ok || got != l || got.Account.Name != "alice" {
		t.Fatalf("Main = %v %v", got, ok)
	}
	// The main token isn't a sessions token, and a sessions token isn't a main one.
	if _, ok := s.Sessions(tok); ok {
		t.Error("main token accepted on the sessions host")
	}
	ticket := s.Ticket(l, "/s/abc/")
	if _, ok := s.Main(ticket); ok {
		t.Error("ticket accepted as a main token")
	}
	stok, to, ok := s.Redeem(ticket)
	if !ok || to != "/s/abc/" || stok == tok {
		t.Fatalf("Redeem = %q %q %v", stok, to, ok)
	}
	if got, ok := s.Sessions(stok); !ok || got != l {
		t.Fatalf("Sessions = %v %v", got, ok)
	}
	if _, ok := s.Main(stok); ok {
		t.Error("sessions token accepted on the main host")
	}
	// Single use.
	if _, _, ok := s.Redeem(ticket); ok {
		t.Error("ticket redeemed twice")
	}
	for _, bad := range []string{"", "x", tok, stok} {
		if _, ok := s.Main(bad + "x"); ok {
			t.Errorf("Main(%q) accepted", bad+"x")
		}
		if _, _, ok := s.Redeem(bad); ok {
			t.Errorf("Redeem(%q) accepted", bad)
		}
	}
}

func TestEveryCreateIsANewToken(t *testing.T) {
	s, _ := testStore()
	a, _ := s.Create(acct("alice"), "pam")
	b, _ := s.Create(acct("alice"), "pam")
	if a == b || len(a) != 43 {
		t.Errorf("tokens %q %q", a, b)
	}
}

func TestStoreKeepsOnlyHashes(t *testing.T) {
	s, _ := testStore()
	tok, l := s.Create(acct("alice"), "pam")
	tk := s.Ticket(l, "/s/x/")
	for h := range s.main {
		if string(h[:]) == tok {
			t.Error("raw main token stored")
		}
	}
	for h := range s.tickets {
		if string(h[:]) == tk {
			t.Error("raw ticket stored")
		}
	}
}

func TestEndSignsOutEverywhere(t *testing.T) {
	s, _ := testStore()
	tok, l := s.Create(acct("alice"), "pam")
	stok, _, _ := s.Redeem(s.Ticket(l, "/s/a/"))
	pending := s.Ticket(l, "/s/b/")
	other, _ := s.Create(acct("alice"), "pam")
	s.End(l)
	if _, ok := s.Main(tok); ok {
		t.Error("main token works after sign-out")
	}
	if _, ok := s.Sessions(stok); ok {
		t.Error("sessions token works after sign-out")
	}
	if _, _, ok := s.Redeem(pending); ok {
		t.Error("ticket works after sign-out")
	}
	if _, ok := s.Main(other); !ok {
		t.Error("signing out one browser signed out another")
	}
}

func TestExpiry(t *testing.T) {
	s, clock := testStore()

	// Idle: a day without use ends it; use keeps it going.
	tok, l := s.Create(acct("alice"), "pam")
	stok, _, _ := s.Redeem(s.Ticket(l, "/s/a/"))
	*clock = clock.Add(DefaultIdle - time.Minute)
	if _, ok := s.Sessions(stok); !ok {
		t.Fatal("ended before the idle timeout")
	}
	*clock = clock.Add(DefaultIdle - time.Minute) // used via the sessions host just now
	if _, ok := s.Main(tok); !ok {
		t.Fatal("use on the sessions host didn't count")
	}
	*clock = clock.Add(DefaultIdle)
	if _, ok := s.Main(tok); ok {
		t.Fatal("still signed in after a day idle")
	}
	if _, ok := s.Sessions(stok); ok {
		t.Fatal("sessions token outlived the sign-in")
	}

	// Max: a week, however busy.
	tok, _ = s.Create(acct("bob"), "pam")
	for range 7 * 24 {
		*clock = clock.Add(time.Hour)
		s.Main(tok)
	}
	if _, ok := s.Main(tok); ok {
		t.Fatal("signed in past the maximum")
	}

	// Tickets: a minute.
	_, l = s.Create(acct("carol"), "pam")
	tk := s.Ticket(l, "/s/c/")
	*clock = clock.Add(TicketTTL)
	if _, _, ok := s.Redeem(tk); ok {
		t.Fatal("expired ticket redeemed")
	}
}

func TestTicketOfAnExpiredSignInIsRefused(t *testing.T) {
	s, clock := testStore()
	_, l := s.Create(acct("alice"), "pam")
	*clock = clock.Add(DefaultIdle + time.Second)
	tk := s.Ticket(l, "/s/a/")
	if _, _, ok := s.Redeem(tk); ok {
		t.Fatal("ticket from an expired sign-in redeemed")
	}
}

func TestSweepAndCap(t *testing.T) {
	s, clock := testStore()
	s.Create(acct("old"), "pam")
	*clock = clock.Add(DefaultIdle)
	s.Create(acct("new"), "pam")
	if len(s.main) != 1 {
		t.Errorf("%d sign-ins after the sweep, want 1", len(s.main))
	}
	first, _ := s.Create(acct("alice"), "pam")
	for range maxPerAccount {
		*clock = clock.Add(time.Second)
		s.Create(acct("alice"), "pam")
	}
	if _, ok := s.Main(first); ok {
		t.Error("oldest sign-in kept past the per-account cap")
	}
	n := 0
	for _, l := range s.main {
		if l.Account.Name == "alice" {
			n++
		}
	}
	if n != maxPerAccount {
		t.Errorf("alice has %d sign-ins, want %d", n, maxPerAccount)
	}
}
