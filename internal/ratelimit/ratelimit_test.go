package ratelimit

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// clock is a hand-advanced time source. Rate limiting is entirely a function
// of elapsed time, so every test here pins it rather than sleeping: a test
// that sleeps is both slow and flaky on a loaded CI box.
type clock struct {
	mu  sync.Mutex
	now time.Time
}

func newClock() *clock {
	return &clock{now: time.Date(2026, 9, 2, 12, 0, 0, 0, time.UTC)}
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *clock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

func newTestLimiter(perMinute float64, burst int, c *clock) *Limiter {
	l := New(perMinute, burst)
	l.Now = c.Now
	return l
}

func TestAllowsUpToBurstThenRejects(t *testing.T) {
	c := newClock()
	// 60/min = one token per second, so within a single clock instant only
	// the burst is available.
	l := newTestLimiter(60, 3, c)

	for i := range 3 {
		if ok, _ := l.Allow("alice"); !ok {
			t.Fatalf("request %d of the burst was rejected; the whole burst must be allowed", i+1)
		}
	}
	if ok, _ := l.Allow("alice"); ok {
		t.Fatal("the request past the burst was allowed; the bucket should be empty")
	}
}

func TestRefillsAfterEnoughTimePasses(t *testing.T) {
	c := newClock()
	l := newTestLimiter(60, 2, c)

	l.Allow("alice")
	l.Allow("alice")
	if ok, _ := l.Allow("alice"); ok {
		t.Fatal("bucket should be empty after the burst is spent")
	}

	// One token per second at 60/min.
	c.advance(time.Second)
	if ok, _ := l.Allow("alice"); !ok {
		t.Fatal("a token should have refilled after one second at 60/min")
	}
	if ok, _ := l.Allow("alice"); ok {
		t.Fatal("only one token refills per second; the second request must be rejected")
	}
}

func TestLabelsHaveIndependentBuckets(t *testing.T) {
	c := newClock()
	l := newTestLimiter(60, 2, c)

	l.Allow("alice")
	l.Allow("alice")
	if ok, _ := l.Allow("alice"); ok {
		t.Fatal("alice's bucket should be empty")
	}

	// This is the whole point of per-key limiting: one noisy key must not
	// throttle every other tenant on the gateway.
	if ok, _ := l.Allow("bob"); !ok {
		t.Fatal("bob was throttled by alice's traffic; buckets must be per label")
	}
}

func TestRetryAfterReportsWhenTheNextTokenArrives(t *testing.T) {
	c := newClock()
	// 30/min = one token every two seconds.
	l := newTestLimiter(30, 1, c)

	l.Allow("alice")
	ok, retryAfter := l.Allow("alice")
	if ok {
		t.Fatal("second request should be rejected with a burst of 1")
	}
	if retryAfter <= 0 {
		t.Fatalf("retryAfter = %v; a throttled request must say how long to wait", retryAfter)
	}
	// A full token is two seconds away, and the bucket is empty.
	if got, want := retryAfter.Round(100*time.Millisecond), 2*time.Second; got != want {
		t.Fatalf("retryAfter = %v, want about %v", got, want)
	}

	// Half the wait elapsed means half the wait remains.
	c.advance(time.Second)
	_, retryAfter = l.Allow("alice")
	if got, want := retryAfter.Round(100*time.Millisecond), time.Second; got != want {
		t.Fatalf("after half the refill elapsed retryAfter = %v, want about %v", got, want)
	}
}

func TestAllowedRequestReportsNoRetryAfter(t *testing.T) {
	c := newClock()
	l := newTestLimiter(60, 1, c)

	ok, retryAfter := l.Allow("alice")
	if !ok {
		t.Fatal("first request must be allowed")
	}
	if retryAfter != 0 {
		t.Fatalf("retryAfter = %v for an allowed request, want 0", retryAfter)
	}
}

func TestIdleBucketNeverExceedsBurst(t *testing.T) {
	c := newClock()
	l := newTestLimiter(60, 3, c)

	// An hour idle would be 3600 tokens if refill were unbounded. The bucket
	// must still cap at the burst, or a key that goes quiet overnight could
	// come back and empty the gateway in one shot.
	c.advance(time.Hour)

	for i := range 3 {
		if ok, _ := l.Allow("alice"); !ok {
			t.Fatalf("request %d should be allowed from a full bucket", i+1)
		}
	}
	if ok, _ := l.Allow("alice"); ok {
		t.Fatal("an idle bucket accumulated more than the burst")
	}
}

func TestZeroRateDisablesLimiting(t *testing.T) {
	c := newClock()
	l := newTestLimiter(0, 0, c)

	// The documented escape hatch: VIIWORK_GW_RATE_PER_MIN=0 turns rate
	// limiting off entirely rather than throttling everything to nothing.
	for i := range 1000 {
		ok, retryAfter := l.Allow("alice")
		if !ok {
			t.Fatalf("request %d was throttled by a disabled limiter", i+1)
		}
		if retryAfter != 0 {
			t.Fatalf("disabled limiter returned retryAfter = %v, want 0", retryAfter)
		}
	}
}

func TestNilLimiterAllowsEverything(t *testing.T) {
	var l *Limiter

	// A nil *Limiter means "no limiting configured". The router holds one by
	// value-or-nil, and a nil check at every call site is easy to forget —
	// so the zero case is handled here instead.
	for range 100 {
		if ok, _ := l.Allow("alice"); !ok {
			t.Fatal("a nil limiter must allow every request")
		}
	}
}

func TestConcurrentAllowHandsOutExactlyTheBurst(t *testing.T) {
	c := newClock()
	// A rate this slow means no token measurably refills during the test, so
	// the number of allowed requests is exactly the burst — a precise
	// invariant to assert under -race, not an approximate one.
	l := newTestLimiter(0.000001, 50, c)

	var allowed atomic.Int64
	var wg sync.WaitGroup
	for range 500 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if ok, _ := l.Allow("alice"); ok {
				allowed.Add(1)
			}
		}()
	}
	wg.Wait()

	if got := allowed.Load(); got != 50 {
		t.Fatalf("%d requests allowed concurrently, want exactly the burst of 50", got)
	}
}

func TestConcurrentAllowAcrossLabelsIsRaceFree(t *testing.T) {
	c := newClock()
	l := newTestLimiter(6000, 100, c)

	// Distinct labels create distinct buckets, so this exercises concurrent
	// map writes specifically, not just contention on one bucket.
	var wg sync.WaitGroup
	for i := range 200 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 10 {
				l.Allow(string(rune('a' + i%26)))
			}
		}()
	}
	wg.Wait()
}
