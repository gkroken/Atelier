// Package ratelimit counts failures per key in a sliding window, to slow password
// guessing on the sign-in page.
//
// Atelier refuses further tries itself, well before AD's own lockout threshold, so a
// script hammering one name can't lock that person out of Windows (atelier-f7461ea-N2):
// once a name is limited, no more passwords for it reach PAM, and so AD, until the
// window has passed.
package ratelimit

import (
	"sync"
	"time"
)

// maxKeys bounds memory; past it the stalest keys are dropped.
const maxKeys = 100_000

// Limiter allows at most Max failures per key within Window.
type Limiter struct {
	max    int
	window time.Duration
	now    func() time.Time

	mu    sync.Mutex
	fails map[string][]time.Time
}

// New returns a Limiter allowing max failures per key per window.
func New(max int, window time.Duration) *Limiter {
	return &Limiter{max: max, window: window, now: time.Now, fails: map[string][]time.Time{}}
}

// Allowed reports whether key may try again, and if not, how long until it may.
func (l *Limiter) Allowed(key string) (bool, time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	f := l.recent(key, now)
	if len(f) < l.max {
		return true, 0
	}
	return false, f[0].Add(l.window).Sub(now)
}

// Fail records a failure for key.
func (l *Limiter) Fail(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	f := l.recent(key, now)
	if len(f) >= l.max {
		f = f[len(f)-l.max+1:] // keep at most max: older ones don't matter any more
	}
	l.fails[key] = append(f, now)
	if len(l.fails) > maxKeys {
		l.prune(now)
	}
}

// Reset forgets key's failures (after a successful sign-in).
func (l *Limiter) Reset(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.fails, key)
}

// recent drops key's failures older than the window. Called with l.mu held.
func (l *Limiter) recent(key string, now time.Time) []time.Time {
	f := l.fails[key]
	i := 0
	for i < len(f) && now.Sub(f[i]) >= l.window {
		i++
	}
	if i == len(f) {
		delete(l.fails, key)
		return nil
	}
	f = f[i:]
	l.fails[key] = f
	return f
}

// prune drops keys with no recent failure, then, if still too many, the keys whose
// latest failure is oldest. Called with l.mu held.
func (l *Limiter) prune(now time.Time) {
	for k := range l.fails {
		l.recent(k, now)
	}
	for len(l.fails) > maxKeys {
		var stalest string
		var at time.Time
		for k, f := range l.fails {
			if last := f[len(f)-1]; stalest == "" || last.Before(at) {
				stalest, at = k, last
			}
		}
		delete(l.fails, stalest)
	}
}
