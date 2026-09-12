package proxy

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/janit/viiwork-gateway/internal/auth"
	"github.com/janit/viiwork-gateway/internal/ratelimit"
)

// newTestRouterWithLimiter builds a router whose only node is upstream and
// whose per-key rate limiter the test controls. maxInFlight is left generous
// so the concurrency cap never confounds a rate-limiting assertion.
func newTestRouterWithLimiter(t *testing.T, upstream *httptest.Server, limiter *ratelimit.Limiter) *Router {
	t.Helper()
	fl := upstreamFleet(upstream)
	return NewRouter(fl, NewForwarder(nil), 16*1024*1024, 256, 30*time.Second, limiter, nil)
}

// inferenceRequest returns a POST that routes by model, authenticated as
// label — the form the router sees after auth.Middleware has run.
func inferenceRequest(label, body string) *http.Request {
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	return r.WithContext(auth.ContextWithLabel(r.Context(), label))
}

// TestRouterRateLimitRejectsOverBurst is the core TDD case: with a burst of 2,
// a key's first two inference requests reach the mesh and the third is
// rejected with 429 without the upstream ever seeing it.
func TestRouterRateLimitRejectsOverBurst(t *testing.T) {
	var reached atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	rt := newTestRouterWithLimiter(t, upstream, ratelimit.New(60, 2))

	for i := range 2 {
		rec := httptest.NewRecorder()
		rt.ServeHTTP(rec, inferenceRequest("alice", `{"model":"gemma"}`))
		if rec.Code != http.StatusOK {
			t.Fatalf("request %d within the burst: status %d, want 200", i+1, rec.Code)
		}
	}

	rec := httptest.NewRecorder()
	rt.ServeHTTP(rec, inferenceRequest("alice", `{"model":"gemma"}`))
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("request past the burst: status %d, want 429", rec.Code)
	}
	if got := reached.Load(); got != 2 {
		t.Fatalf("upstream saw %d requests, want 2: a throttled request must never reach the mesh", got)
	}
}

// TestRouterRateLimitedResponseIsUsableByAnOpenAIClient checks the wire shape.
// An SDK that cannot parse the error reports a transport failure instead of
// "you are being rate limited", and a client with no Retry-After retries in a
// tight loop — which is the behaviour the limit exists to stop.
func TestRouterRateLimitedResponseIsUsableByAnOpenAIClient(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer upstream.Close()

	// 30/min = one token every two seconds, so Retry-After should be 2.
	rt := newTestRouterWithLimiter(t, upstream, ratelimit.New(30, 1))
	rt.ServeHTTP(httptest.NewRecorder(), inferenceRequest("alice", `{"model":"gemma"}`))

	rec := httptest.NewRecorder()
	rt.ServeHTTP(rec, inferenceRequest("alice", `{"model":"gemma"}`))

	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status %d, want 429", rec.Code)
	}
	if got := rec.Header().Get("Retry-After"); got != "2" {
		t.Fatalf("Retry-After = %q, want %q", got, "2")
	}
	var body struct {
		Error struct {
			Message string `json:"message"`
			Type    string `json:"type"`
			Code    string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("response is not the OpenAI error envelope: %v (body %q)", err, rec.Body.String())
	}
	if body.Error.Code != "rate_limit_exceeded" {
		t.Fatalf("error code = %q, want %q", body.Error.Code, "rate_limit_exceeded")
	}
	if body.Error.Message == "" {
		t.Fatal("error message is empty")
	}
	// Every locally-generated response carries the hardening headers (H3).
	if rec.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Fatal("429 response is missing the gateway's security headers")
	}
}

// TestRouterRateLimitIsPerKeyLabel is the whole point of metering per key:
// one key exhausting its allowance must not throttle a different tenant.
func TestRouterRateLimitIsPerKeyLabel(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	rt := newTestRouterWithLimiter(t, upstream, ratelimit.New(60, 1))

	rt.ServeHTTP(httptest.NewRecorder(), inferenceRequest("alice", `{"model":"gemma"}`))
	rec := httptest.NewRecorder()
	rt.ServeHTTP(rec, inferenceRequest("alice", `{"model":"gemma"}`))
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("alice's second request: status %d, want 429", rec.Code)
	}

	rec = httptest.NewRecorder()
	rt.ServeHTTP(rec, inferenceRequest("bob", `{"model":"gemma"}`))
	if rec.Code != http.StatusOK {
		t.Fatalf("bob's first request: status %d, want 200 — bob was throttled by alice's traffic", rec.Code)
	}
}

// TestRouterRateLimitDoesNotMeterStickyPaths pins the documented scoping
// decision: the dashboards fire many requests per page load, so metering them
// on the same budget would let an operator lock their own browser out. The
// sticky path stays bounded by the in-flight cap and the body deadline.
func TestRouterRateLimitDoesNotMeterStickyPaths(t *testing.T) {
	var reached atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	rt := newTestRouterWithLimiter(t, upstream, ratelimit.New(60, 1))

	// Spend alice's entire allowance on the metered path.
	rt.ServeHTTP(httptest.NewRecorder(), inferenceRequest("alice", `{"model":"gemma"}`))
	rec := httptest.NewRecorder()
	rt.ServeHTTP(rec, inferenceRequest("alice", `{"model":"gemma"}`))
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("precondition failed: alice should be throttled, got %d", rec.Code)
	}

	before := reached.Load()
	for i := range 20 {
		r := httptest.NewRequest(http.MethodGet, "/dashboard", nil)
		r = r.WithContext(auth.ContextWithLabel(r.Context(), "alice"))
		rec := httptest.NewRecorder()
		rt.ServeHTTP(rec, r)
		if rec.Code != http.StatusOK {
			t.Fatalf("sticky request %d: status %d, want 200 — dashboards must not be rate limited", i+1, rec.Code)
		}
	}
	if got := reached.Load() - before; got != 20 {
		t.Fatalf("upstream saw %d sticky requests, want 20", got)
	}
}

// TestRouterRateLimitDoesNotMeterModelCatalogue matches the scoping the
// in-flight cap already uses: /v1/models is answered locally from the
// registry and costs no mesh work, so it is not metered.
func TestRouterRateLimitDoesNotMeterModelCatalogue(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer upstream.Close()

	rt := newTestRouterWithLimiter(t, upstream, ratelimit.New(60, 1))
	rt.ServeHTTP(httptest.NewRecorder(), inferenceRequest("alice", `{"model":"gemma"}`))

	for i := range 10 {
		r := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
		r = r.WithContext(auth.ContextWithLabel(r.Context(), "alice"))
		rec := httptest.NewRecorder()
		rt.ServeHTTP(rec, r)
		if rec.Code != http.StatusOK {
			t.Fatalf("catalogue request %d: status %d, want 200", i+1, rec.Code)
		}
	}
}

// TestRouterRateLimitIsCheckedBeforeTheBodyIsRead proves the ordering. A
// throttled request must cost a map lookup and nothing more — if the body
// were read first, an attacker past their limit could still make the gateway
// buffer up to MaxBody per request, which is exactly the memory the cap
// exists to protect. An oversized body would answer 413 if it were read.
func TestRouterRateLimitIsCheckedBeforeTheBodyIsRead(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer upstream.Close()

	fl := upstreamFleet(upstream)
	// maxBody of 32 bytes: any real body exceeds it.
	rt := NewRouter(fl, NewForwarder(nil), 32, 256, 30*time.Second, ratelimit.New(60, 1), nil)

	rt.ServeHTTP(httptest.NewRecorder(), inferenceRequest("alice", `{"model":"gemma"}`))

	rec := httptest.NewRecorder()
	rt.ServeHTTP(rec, inferenceRequest("alice", `{"model":"gemma","padding":"`+strings.Repeat("x", 4096)+`"}`))
	if rec.Code == http.StatusRequestEntityTooLarge {
		t.Fatal("throttled request was answered 413: the body was read before the rate limit was checked")
	}
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status %d, want 429", rec.Code)
	}
}

// TestRouterRateLimitedRequestConsumesNoInFlightToken guards the interaction
// between the two caps. If a throttled request took a concurrency token and
// failed to return it, a key hammering past its rate limit would drain the
// in-flight pool and take the gateway down for every tenant — turning a
// defence into an amplifier.
func TestRouterRateLimitedRequestConsumesNoInFlightToken(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	fl := upstreamFleet(upstream)
	// A cap of exactly 1 makes a single leaked token fatal and obvious.
	rt := NewRouter(fl, NewForwarder(nil), 16*1024*1024, 1, 30*time.Second, ratelimit.New(60, 1), nil)

	rt.ServeHTTP(httptest.NewRecorder(), inferenceRequest("alice", `{"model":"gemma"}`))
	for range 50 {
		rec := httptest.NewRecorder()
		rt.ServeHTTP(rec, inferenceRequest("alice", `{"model":"gemma"}`))
		if rec.Code != http.StatusTooManyRequests {
			t.Fatalf("expected 429 while throttled, got %d", rec.Code)
		}
	}

	// If any of those 50 leaked the single token, this answers 503.
	r := httptest.NewRequest(http.MethodGet, "/dashboard", nil)
	r = r.WithContext(auth.ContextWithLabel(r.Context(), "alice"))
	rec := httptest.NewRecorder()
	rt.ServeHTTP(rec, r)
	if rec.Code == http.StatusServiceUnavailable {
		t.Fatal("in-flight pool exhausted: a rate-limited request leaked a concurrency token")
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d, want 200", rec.Code)
	}
}

// TestRouterWithoutLimiterIsUnmetered keeps the nil case honest: NewRouter is
// called with a nil limiter in several existing tests, and by any deployment
// that sets VIIWORK_GW_RATE_PER_MIN=0.
func TestRouterWithoutLimiterIsUnmetered(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	rt := newTestRouterWithLimiter(t, upstream, nil)

	for i := range 50 {
		rec := httptest.NewRecorder()
		rt.ServeHTTP(rec, inferenceRequest("alice", `{"model":"gemma"}`))
		if rec.Code != http.StatusOK {
			t.Fatalf("request %d: status %d, want 200 with no limiter configured", i+1, rec.Code)
		}
	}
}

// TestRouterRateLimitRefillsAllowsTrafficAgain proves the limiter is a bucket
// and not a permanent ban: once enough time passes the key is served again.
func TestRouterRateLimitRefillsAllowsTrafficAgain(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	limiter := ratelimit.New(60, 1)
	now := time.Now()
	limiter.Now = func() time.Time { return now }
	rt := newTestRouterWithLimiter(t, upstream, limiter)

	rt.ServeHTTP(httptest.NewRecorder(), inferenceRequest("alice", `{"model":"gemma"}`))
	rec := httptest.NewRecorder()
	rt.ServeHTTP(rec, inferenceRequest("alice", `{"model":"gemma"}`))
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status %d, want 429 before any refill", rec.Code)
	}

	now = now.Add(time.Second) // one token at 60/min
	rec = httptest.NewRecorder()
	rt.ServeHTTP(rec, inferenceRequest("alice", `{"model":"gemma"}`))
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d, want 200 after the bucket refilled", rec.Code)
	}
}
