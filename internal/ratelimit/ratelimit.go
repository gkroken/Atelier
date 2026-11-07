// Package ratelimit counts sign-in attempts per key in a sliding window, to slow
// password guessing on the sign-in page.
//
// Atelier refuses further tries itself, well before AD's own lockout threshold, so a
// script hammering one name can't lock that person out of Windows (atelier-f7461ea-N2).
// The attempt is what's counted, in one locked step before PAM is called, so parallel
// requests can't slip past the limit while earlier ones are still being checked
// (atelier-2eead6a-F1): at most Max passwords per key per window reach PAM, and so AD.
// The caller resets the key after a successful sign-in, and refunds an attempt that
// never reached a password check (the directory was unreachable).
package ratelimit

import (
	"slices"
	"sync"
	"time"
)

// maxKeys bounds memory; past it the stalest tenth of the keys is dropped at once, so
// a spray of made-up names costs a sort per 10,000 new keys, not a scan per attempt
// (atelier-2eead6a-N1).
const maxKeys = 100_000

// Limiter allows at most max attempts per key within window.
type Limiter struct {
	max    int
	window time.Duration
	now    func() time.Time

	mu    sync.Mutex
	tries map[string][]time.Time
}

// New returns a Limiter allowing max attempts per key per window.
func New(max int, window time.Duration) *Limiter {
	return &Limiter{max: max, window: window, now: time.Now, tries: map[string][]time.Time{}}
}

// Try records an attempt for key and reports whether it may go ahead. If not, nothing
// is recorded, and wait says how long until the next attempt may.
func (l *Limiter) Try(key string) (ok bool, wait time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	t := l.recent(key, now)
	if len(t) >= l.max {
		return false, t[0].Add(l.window).Sub(now)
	}
	l.tries[key] = append(t, now)
	if len(l.tries) > maxKeys {
		l.prune(now)
	}
	return true, 0
}

// Refund takes back key's latest attempt: one that never reached a password check.
func (l *Limiter) Refund(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if t := l.tries[key]; len(t) > 1 {
		l.tries[key] = t[:len(t)-1]
	} else {
		delete(l.tries, key)
	}
}

// Reset forgets key's attempts (after a successful sign-in).
func (l *Limiter) Reset(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.tries, key)
}

// recent drops key's attempts older than the window. Called with l.mu held.
func (l *Limiter) recent(key string, now time.Time) []time.Time {
	t := l.tries[key]
	i := 0
	for i < len(t) && now.Sub(t[i]) >= l.window {
		i++
	}
	if i == len(t) {
		delete(l.tries, key)
		return nil
	}
	t = t[i:]
	l.tries[key] = t
	return t
}

// prune drops keys with no recent attempt, then, if still at least maxKeys, the tenth
// whose latest attempt is oldest. Called with l.mu held.
func (l *Limiter) prune(now time.Time) {
	for k := range l.tries {
		l.recent(k, now)
	}
	if len(l.tries) < maxKeys {
		return
	}
	type entry struct {
		key  string
		last time.Time
	}
	all := make([]entry, 0, len(l.tries))
	for k, t := range l.tries {
		all = append(all, entry{k, t[len(t)-1]})
	}
	slices.SortFunc(all, func(a, b entry) int { return a.last.Compare(b.last) })
	for _, e := range all[:len(all)-maxKeys*9/10] {
		delete(l.tries, e.key)
	}
}
