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
	"sync"
	"time"
)

// maxKeys bounds memory: at most maxKeys keys of at most max timestamps each. Only
// keys whose attempts have all left the window are ever dropped. Dropping a key with
// any attempt still inside its window would reset its count and hand it fresh guesses
// (atelier-18cac6e-F1, at or below its limit alike). So when the map is full of live
// keys, new keys are refused until room frees up (fail closed): during a spray of
// 100,000+ names brand-new names wait up to a window, but no name's count is ever lost.
const maxKeys = 100_000

// prunePause is how long after a prune that left the map over 99% full the next one
// waits, so a
// spray against a full map doesn't scan it on every attempt (atelier-2eead6a-N1).
const prunePause = time.Second

// Limiter allows at most max attempts per key within window.
type Limiter struct {
	max    int
	window time.Duration
	now    func() time.Time

	mu      sync.Mutex
	tries   map[string][]time.Time
	noPrune time.Time // no prune before this (the last one freed nothing)
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
	if len(t) == 0 && len(l.tries) >= maxKeys {
		if now.Before(l.noPrune) || !l.prune(now) {
			return false, l.window
		}
	}
	l.tries[key] = append(t, now)
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

// prune drops keys with no attempt inside the window, and reports whether there is
// room for a new key. Called with l.mu held.
func (l *Limiter) prune(now time.Time) bool {
	for k := range l.tries {
		l.recent(k, now)
	}
	if len(l.tries) >= maxKeys*99/100 {
		// Little or nothing freed: the map refills at once, so wait before scanning
		// again (atelier-b2bd82e-N1). Room left now still goes to new keys.
		l.noPrune = now.Add(prunePause)
	}
	return len(l.tries) < maxKeys
}
