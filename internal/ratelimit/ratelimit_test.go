package ratelimit

import (
	"strconv"
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
		if ok, _ := l.Allowed("alice"); !ok {
			t.Fatalf("refused after %d failures", i)
		}
		l.Fail("alice")
		*clock = clock.Add(time.Minute)
	}
	ok, wait := l.Allowed("alice")
	if ok || wait != 12*time.Minute {
		t.Fatalf("after 3 failures: %v, wait %v; want refused for 12m", ok, wait)
	}
	if ok, _ := l.Allowed("bob"); !ok {
		t.Fatal("another key refused")
	}
	// The oldest failure leaves the window: one more try.
	*clock = clock.Add(12 * time.Minute)
	if ok, _ := l.Allowed("alice"); !ok {
		t.Fatal("still refused once the first failure is out of the window")
	}
	l.Fail("alice")
	if ok, _ := l.Allowed("alice"); ok {
		t.Fatal("allowed again right after another failure")
	}
	l.Reset("alice")
	if ok, _ := l.Allowed("alice"); !ok {
		t.Fatal("refused after Reset")
	}
}

// Failures while refused still count (a caller may record them), but only the latest
// max matter, so memory per key stays bounded.
func TestBoundedPerKey(t *testing.T) {
	l, _ := testLimiter(3, time.Hour)
	for range 1000 {
		l.Fail("alice")
	}
	if n := len(l.fails["alice"]); n != 3 {
		t.Errorf("%d failures kept, want 3", n)
	}
}

func TestBoundedKeys(t *testing.T) {
	l, clock := testLimiter(1, time.Hour)
	for i := range maxKeys + 10 {
		*clock = clock.Add(time.Millisecond)
		l.Fail(strconv.Itoa(i))
	}
	if len(l.fails) > maxKeys {
		t.Errorf("%d keys, want at most %d", len(l.fails), maxKeys)
	}
	// The newest are kept; the stalest went.
	if ok, _ := l.Allowed(strconv.Itoa(maxKeys + 9)); ok {
		t.Error("newest key forgotten")
	}
	if ok, _ := l.Allowed("0"); !ok {
		t.Error("stalest key kept")
	}
}
