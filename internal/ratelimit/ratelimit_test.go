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

func TestBoundedKeys(t *testing.T) {
	l, clock := testLimiter(1, time.Hour)
	for i := range maxKeys + 10 {
		*clock = clock.Add(time.Millisecond)
		l.Try(strconv.Itoa(i))
	}
	if n := len(l.tries); n > maxKeys || n < maxKeys*9/10 {
		t.Errorf("%d keys, want between %d and %d", n, maxKeys*9/10, maxKeys)
	}
	// The newest are kept; the stalest went.
	if ok, _ := l.Try(strconv.Itoa(maxKeys + 9)); ok {
		t.Error("newest key forgotten")
	}
	if ok, _ := l.Try("0"); !ok {
		t.Error("stalest key kept")
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
