// Package login keeps who is signed in (spine #2), in memory until spine #3's database.
//
// A sign-in on the main host gets a main token (the main host's cookie). Opening a
// session goes through a single-use ticket: the main host makes one and redirects to
// the sessions host, which redeems it for a sessions token (that host's cookie). The
// two hosts never share a cookie (ADR-0016). Signing out ends the sign-in and every
// token made from it.
//
// Tokens are 256 random bits. Only their SHA-256 is kept, so the store (and later the
// database) never holds a usable token.
package login

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"sync"
	"time"

	"github.com/gkroken/Atelier/internal/account"
)

// Defaults: a person signs in once a week at most, and a forgotten browser is signed
// out after a day without use.
const (
	DefaultIdle   = 24 * time.Hour
	DefaultMax    = 7 * 24 * time.Hour
	TicketTTL     = time.Minute
	maxPerAccount = 50 // oldest sign-ins of an account are ended past this
)

// Login is one sign-in.
type Login struct {
	Account account.Account
	Method  string // "pam" or "oidc"
	Created time.Time

	seen     time.Time
	main     [32]byte
	sessions map[[32]byte]bool
}

type ticket struct {
	login   *Login
	to      string
	expires time.Time
}

// Store holds the sign-ins.
type Store struct {
	Idle, Max time.Duration
	now       func() time.Time

	mu       sync.Mutex
	main     map[[32]byte]*Login
	sessions map[[32]byte]*Login
	tickets  map[[32]byte]ticket
}

// New returns an empty store with the default lifetimes.
func New() *Store {
	return &Store{
		Idle: DefaultIdle, Max: DefaultMax, now: time.Now,
		main: map[[32]byte]*Login{}, sessions: map[[32]byte]*Login{}, tickets: map[[32]byte]ticket{},
	}
}

func newToken() (string, [32]byte) {
	b := make([]byte, 32)
	_, _ = rand.Read(b) // never fails on Linux (Go 1.24+)
	t := base64.RawURLEncoding.EncodeToString(b)
	return t, sha256.Sum256([]byte(t))
}

func hash(token string) [32]byte { return sha256.Sum256([]byte(token)) }

// Create signs acct in and returns the main token. Always a new token: a token from
// before sign-in never becomes a signed-in one (no session fixation).
func (s *Store) Create(acct account.Account, method string) (string, *Login) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	s.sweep(now)
	tok, h := newToken()
	l := &Login{Account: acct, Method: method, Created: now, seen: now, main: h, sessions: map[[32]byte]bool{}}
	s.main[h] = l
	s.capAccount(acct.Name)
	return tok, l
}

// Main returns the sign-in for a main-host token, and marks it used.
func (s *Store) Main(token string) (*Login, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	l := s.main[hash(token)]
	return l, s.use(l)
}

// Sessions returns the sign-in for a sessions-host token, and marks it used.
func (s *Store) Sessions(token string) (*Login, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	l := s.sessions[hash(token)]
	return l, s.use(l)
}

// use checks l is live, ending it if it has expired. Called with s.mu held.
func (s *Store) use(l *Login) bool {
	if l == nil {
		return false
	}
	now := s.now()
	if s.expired(l, now) {
		s.end(l)
		return false
	}
	l.seen = now
	return true
}

func (s *Store) expired(l *Login, now time.Time) bool {
	return now.Sub(l.seen) >= s.Idle || now.Sub(l.Created) >= s.Max
}

// Ticket makes a single-use ticket for l to carry to the sessions host, which will
// send the browser on to path to. It expires after TicketTTL.
func (s *Store) Ticket(l *Login, to string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	tok, h := newToken()
	s.tickets[h] = ticket{login: l, to: to, expires: s.now().Add(TicketTTL)}
	return tok
}

// Redeem uses up a ticket and returns a new sessions-host token for its sign-in, and
// the path the ticket was for. A used, expired or unknown ticket, or one whose
// sign-in has ended, is refused.
func (s *Store) Redeem(t string) (token, to string, ok bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	h := hash(t)
	tk, found := s.tickets[h]
	delete(s.tickets, h)
	if !found || !s.now().Before(tk.expires) || s.main[tk.login.main] != tk.login || !s.use(tk.login) {
		return "", "", false
	}
	tok, sh := newToken()
	s.sessions[sh] = tk.login
	tk.login.sessions[sh] = true
	return tok, tk.to, true
}

// End signs l out, with every token and ticket made from it.
func (s *Store) End(l *Login) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.end(l)
}

func (s *Store) end(l *Login) {
	delete(s.main, l.main)
	for h := range l.sessions {
		delete(s.sessions, h)
	}
	for h, t := range s.tickets {
		if t.login == l {
			delete(s.tickets, h)
		}
	}
}

// sweep ends expired sign-ins and drops expired tickets. Called with s.mu held.
func (s *Store) sweep(now time.Time) {
	for _, l := range s.main {
		if s.expired(l, now) {
			s.end(l)
		}
	}
	for h, t := range s.tickets {
		if !now.Before(t.expires) {
			delete(s.tickets, h)
		}
	}
}

// capAccount ends the oldest sign-ins of name beyond maxPerAccount. Called with s.mu
// held.
func (s *Store) capAccount(name string) {
	var mine []*Login
	for _, l := range s.main {
		if l.Account.Name == name {
			mine = append(mine, l)
		}
	}
	for len(mine) > maxPerAccount {
		oldest := 0
		for i, l := range mine {
			if l.Created.Before(mine[oldest].Created) {
				oldest = i
			}
		}
		s.end(mine[oldest])
		mine = append(mine[:oldest], mine[oldest+1:]...)
	}
}
