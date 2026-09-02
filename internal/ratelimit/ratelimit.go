// Package ratelimit bounds how fast a single API key may drive the gateway.
//
// The gateway's concurrency cap (see internal/proxy.Router.tokens) bounds how
// many requests may be in flight at once, which bounds memory. It does not
// bound *rate*: one valid key can issue unlimited sequential requests, and
// every key holder can read the whole fleet's prompt history through the
// dashboards. This package closes that gap by metering each key separately,
// so one noisy or hostile key cannot consume the fleet on behalf of every
// other tenant.
//
// The implementation is a token bucket per key label. It is deliberately
// hand-rolled rather than pulled from golang.org/x/time/rate: this module has
// exactly one dependency and it is not worth a second one for sixty lines.
package ratelimit

import (
	"sync"
	"time"
)

// bucket is one key's allowance. Tokens refill lazily — computed from elapsed
// time on each access — so there is no background goroutine, no ticker, and
// nothing to shut down.
type bucket struct {
	tokens float64
	last   time.Time
}

// Limiter meters requests per key label. The zero value is not usable; call
// New. A nil *Limiter is usable and allows everything, which is what "rate
// limiting is switched off" looks like at a call site.
type Limiter struct {
	// ratePerSec is the sustained refill rate. Zero means limiting is
	// disabled entirely.
	ratePerSec float64
	// burst is the bucket capacity: the most requests that can be spent at
	// once after an idle period.
	burst float64

	// Now is injectable so tests can pin the clock instead of sleeping.
	// Defaults to time.Now when nil.
	Now func() time.Time

	mu sync.Mutex
	// buckets is keyed by the authenticated key's label. It cannot grow
	// without bound: labels come from the fixed keys.Set loaded at startup,
	// so the map is bounded by the number of configured keys and needs no
	// eviction. Anything unauthenticated is rejected upstream and never
	// reaches this map.
	buckets map[string]*bucket
}

// New returns a limiter allowing perMinute sustained requests per key, with
// bucket capacity burst. A non-positive perMinute disables limiting
// altogether (the documented VIIWORK_GW_RATE_PER_MIN=0 escape hatch). A burst
// below 1 is raised to 1 when limiting is enabled, since a bucket that can
// never hold a whole token would reject every request forever.
func New(perMinute float64, burst int) *Limiter {
	l := &Limiter{buckets: make(map[string]*bucket)}
	if perMinute <= 0 {
		return l // disabled: ratePerSec stays 0
	}
	l.ratePerSec = perMinute / 60
	l.burst = float64(burst)
	if l.burst < 1 {
		l.burst = 1
	}
	return l
}

func (l *Limiter) now() time.Time {
	if l.Now != nil {
		return l.Now()
	}
	return time.Now()
}

// Allow reports whether a request from the key labelled label may proceed,
// spending a token if so. When it may not, the returned duration is how long
// until a token is available, for a Retry-After header.
//
// A nil receiver, or a limiter built with a non-positive rate, allows
// everything.
func (l *Limiter) Allow(label string) (allowed bool, retryAfter time.Duration) {
	if l == nil || l.ratePerSec <= 0 {
		return true, 0
	}

	now := l.now()

	l.mu.Lock()
	defer l.mu.Unlock()

	b, ok := l.buckets[label]
	if !ok {
		// A label seen for the first time starts full, so a key's very first
		// request is never throttled.
		b = &bucket{tokens: l.burst, last: now}
		l.buckets[label] = b
	}

	// Refill for the time elapsed since this bucket was last touched, capped
	// at the burst: an idle key accumulates an allowance up to the bucket
	// size and no further.
	if elapsed := now.Sub(b.last); elapsed > 0 {
		b.tokens += elapsed.Seconds() * l.ratePerSec
		if b.tokens > l.burst {
			b.tokens = l.burst
		}
	}
	b.last = now

	if b.tokens >= 1 {
		b.tokens--
		return true, 0
	}

	// Time for the bucket to reach one whole token. Callers round this up to
	// whole seconds for Retry-After; it is returned unrounded so the caller
	// decides, and so tests can assert the real arithmetic.
	wait := time.Duration((1 - b.tokens) / l.ratePerSec * float64(time.Second))
	return false, wait
}
