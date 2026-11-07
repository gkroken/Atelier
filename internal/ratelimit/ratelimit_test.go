package ratelimit

import (
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func testLimiter(max int, window time.Duration) (*Limiter, *time.Time) {
	l := New(max, window)
	clock := time.Date(2026, 11, 7, 9, 0, 0, 0, time.UTC)
	l.now = func() time.Time { return clock }
	return l, &clock
}

func TestLimit(t *testing.T) {
	l, clock := testLimiter(3, 15*time.Minute)
	for i := range 3 {
		if ok, _ := l.Try("alice"); !ok {
			t.Fatalf("refused attempt %d", i+1)
		}
		*clock = clock.Add(time.Minute)
	}
	ok, wait := l.Try("alice")
	if ok || wait != 12*time.Minute {
		t.Fatalf("4th attempt: %v, wait %v; want refused for 12m", ok, wait)
	}
	if ok, _ := l.Try("bob"); !ok {
		t.Fatal("another key refused")
	}
	// Refused attempts aren't recorded: once the first leaves the window, one more.
	*clock = clock.Add(12 * time.Minute)
	if ok, _ := l.Try("alice"); !ok {
		t.Fatal("still refused once the first attempt is out of the window")
	}
	if ok, _ := l.Try("alice"); ok {
		t.Fatal("two more allowed when one left the window")
	}
	l.Reset("alice")
	if ok, _ := l.Try("alice"); !ok {
		t.Fatal("refused after Reset")
	}
}

func TestRefund(t *testing.T) {
	l, _ := testLimiter(2, time.Hour)
	l.Try("alice")
	l.Try("alice")
	l.Refund("alice") // the directory was down for the second
	if ok, _ := l.Try("alice"); !ok {
		t.Fatal("refunded attempt still counted")
	}
	if ok, _ := l.Try("alice"); ok {
		t.Fatal("limit not applied after a refund")
	}
	l.Refund("nobody") // no attempts: nothing happens
	l.Refund("alice")
	l.Refund("alice")
	l.Refund("alice")
	if n := len(l.tries["alice"]); n != 0 {
		t.Errorf("%d attempts left after refunding all", n)
	}
}

// The reviewer's probe (atelier-2eead6a-F1): many parallel attempts for one name, each
// held for a while as a PAM check would be, and at most max get through.
func TestParallelAttemptsStopAtTheLimit(t *testing.T) {
	l := New(5, 15*time.Minute)
	var reached atomic.Int32
	var wg sync.WaitGroup
	start := make(chan struct{})
	for range 40 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			if ok, _ := l.Try("alice"); ok {
				reached.Add(1)
				time.Sleep(50 * time.Millisecond) // stand-in for PAM; the password is wrong
			}
		}()
	}
	close(start)
	wg.Wait()
	if n := reached.Load(); n != 5 {
		t.Fatalf("%d of 40 parallel attempts reached PAM, want 5", n)
	}
}

// Memory stays bounded: past maxKeys live keys, newcomers are refused, and once the
// window has passed the expired keys go and newcomers are welcome again.
func TestBoundedKeys(t *testing.T) {
	l, clock := testLimiter(2, time.Hour) // one attempt each: below the limit, still kept
	for i := range maxKeys + 10 {
		*clock = clock.Add(time.Millisecond)
		l.Try(strconv.Itoa(i))
	}
	if n := len(l.tries); n != maxKeys {
		t.Errorf("%d keys, want %d", n, maxKeys)
	}
	if _, kept := l.tries["0"]; !kept {
		t.Error("a live key below its limit was dropped")
	}
	*clock = clock.Add(time.Hour)
	if ok, _ := l.Try("newcomer"); !ok {
		t.Fatal("newcomer refused after every key expired")
	}
	if n := len(l.tries); n != 1 {
		t.Errorf("%d keys after the prune, want 1", n)
	}
}

// The reviewer's second probe (atelier-18cac6e-F1, below the limit): stopping one
// short of the limit before each spray gets no extra guesses.
func TestSprayBelowTheLimitGetsNoFreshGuesses(t *testing.T) {
	l, clock := testLimiter(5, 15*time.Minute)
	reached := 0
	for round := range 3 {
		for range 4 {
			if ok, _ := l.Try("alice"); ok {
				reached++
			}
		}
		for i := range maxKeys + 1 {
			*clock = clock.Add(time.Microsecond)
			l.Try("made-up-" + strconv.Itoa(round) + "-" + strconv.Itoa(i))
		}
	}
	if reached > 5 {
		t.Fatalf("%d passwords for alice reached PAM in one window, want at most 5", reached)
	}
}

// Past the cap, an attempt doesn't scan every key: a long spray stays fast.
func TestSprayStaysCheap(t *testing.T) {
	l, clock := testLimiter(1, time.Hour)
	start := time.Now()
	for i := range 3 * maxKeys {
		*clock = clock.Add(time.Microsecond)
		l.Try(strconv.Itoa(i))
	}
	if d := time.Since(start); d > 10*time.Second {
		t.Errorf("%d attempts took %v", 3*maxKeys, d)
	}
}

// The reviewer's probe (atelier-18cac6e-F1): a locked name stays locked through a
// spray of made-up names, and the spray can't fill the map with keys that never go.
func TestSprayDoesNotUnlockALockedName(t *testing.T) {
	l, clock := testLimiter(5, 15*time.Minute)
	for range 5 {
		l.Try("alice")
	}
	if ok, _ := l.Try("alice"); ok {
		t.Fatal("alice not locked")
	}
	for i := range maxKeys + 1 {
		*clock = clock.Add(time.Microsecond)
		l.Try("made-up-" + strconv.Itoa(i))
	}
	if ok, _ := l.Try("alice"); ok {
		t.Fatal("alice unlocked by the spray")
	}
	// Once alice's window has passed, she may try again.
	*clock = clock.Add(15 * time.Minute)
	if ok, _ := l.Try("alice"); !ok {
		t.Fatal("alice still locked after the window")
	}
}

// When every key is at its limit, new keys are refused (fail closed) instead of
// evicting a locked one, and the refusal doesn't rescan the map on every attempt.
func TestFullOfLockedKeysFailsClosed(t *testing.T) {
	l, clock := testLimiter(1, time.Hour)
	for i := range maxKeys {
		l.Try(strconv.Itoa(i)) // each is now at its limit of 1
	}
	*clock = clock.Add(time.Minute)
	if ok, wait := l.Try("newcomer"); ok || wait != time.Hour {
		t.Fatalf("new key with a map full of locked keys: %v %v; want refused", ok, wait)
	}
	if ok, _ := l.Try("0"); ok {
		t.Fatal("a locked key was evicted")
	}
	start := time.Now()
	for i := range 100_000 {
		l.Try("spray-" + strconv.Itoa(i))
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Errorf("100,000 refused new keys took %v: rescanning each time?", d)
	}
	// After the window everything has expired, and new keys are welcome again.
	*clock = clock.Add(time.Hour)
	if ok, _ := l.Try("newcomer"); !ok {
		t.Fatal("still refusing new keys after the window")
	}
}
