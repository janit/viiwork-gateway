package proxy

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// newTestRouterWithMaxInFlight is like newTestRouter but lets the test pick
// the concurrency cap instead of the "may as well be unlimited" default.
func newTestRouterWithMaxInFlight(t *testing.T, upstream *httptest.Server, maxInFlight int) *Router {
	t.Helper()
	fl := upstreamFleet(upstream)
	return NewRouter(fl, NewForwarder(nil), 16*1024*1024, maxInFlight, 30*time.Second, nil, nil)
}

// newUnreachableTestRouter builds a router whose only node is a
// (deliberately) closed port, so any forwarded request fails with 502 —
// used to prove a token is released even when the forward errors.
func newUnreachableTestRouter(t *testing.T, maxInFlight int) *Router {
	t.Helper()
	const unreachable = "127.0.0.1:1"
	fl := oneNodeFleet("node-a", unreachable)
	return NewRouter(fl, NewForwarder(nil), 16*1024*1024, maxInFlight, 30*time.Second, nil, nil)
}

// TestRouterInFlightCapRejectsOverflowAndReleasesOnCompletion is the core
// TDD case for H4/R1: with maxInFlight=2 and an upstream that blocks until
// signalled, two concurrent inference requests both reach the upstream; a
// third arriving while those two are in flight is rejected with 503 and
// Retry-After without ever reaching the upstream; releasing the first two
// frees their tokens so a subsequent request succeeds again.
func TestRouterInFlightCapRejectsOverflowAndReleasesOnCompletion(t *testing.T) {
	var reachedCount int32
	reached := make(chan struct{}, 2)
	release := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&reachedCount, 1)
		reached <- struct{}{}
		<-release
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	rt := newTestRouterWithMaxInFlight(t, upstream, 2)
	body := `{"model":"gemma"}`

	// Fire the first two concurrently; both must reach the (blocked) upstream.
	type result struct{ code int }
	results := make(chan result, 2)
	for i := 0; i < 2; i++ {
		go func() {
			rec := httptest.NewRecorder()
			rt.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body)))
			results <- result{rec.Code}
		}()
	}

	for i := 0; i < 2; i++ {
		select {
		case <-reached:
		case <-time.After(2 * time.Second):
			t.Fatalf("request %d never reached the upstream", i)
		}
	}

	// Both tokens are now held. A third request must fail fast with 503 and
	// Retry-After, and must NOT reach the upstream.
	rec3 := httptest.NewRecorder()
	rt.ServeHTTP(rec3, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body)))
	if rec3.Code != http.StatusServiceUnavailable {
		t.Fatalf("over-cap request: status = %d, want 503", rec3.Code)
	}
	if got := rec3.Header().Get("Retry-After"); got != "1" {
		t.Errorf("over-cap request: Retry-After = %q, want %q", got, "1")
	}
	if !strings.Contains(rec3.Body.String(), `"error"`) {
		t.Errorf("over-cap request: want an OpenAI-shaped error, got %q", rec3.Body.String())
	}
	if n := atomic.LoadInt32(&reachedCount); n != 2 {
		t.Fatalf("upstream was reached %d times, want exactly 2 (the third must not reach it)", n)
	}

	// Release the two blocked requests: their tokens must be freed.
	close(release)
	for i := 0; i < 2; i++ {
		select {
		case r := <-results:
			if r.code != http.StatusOK {
				t.Errorf("blocked request %d: status = %d, want 200", i, r.code)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("blocked request %d never completed", i)
		}
	}

	// A fresh request must now succeed — proof the tokens were released.
	// (release is closed, so the upstream handler's <-release returns
	// immediately on every subsequent call.)
	rec4 := httptest.NewRecorder()
	rt.ServeHTTP(rec4, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body)))
	if rec4.Code != http.StatusOK {
		t.Fatalf("post-release request: status = %d, want 200 (tokens should have been freed)", rec4.Code)
	}
}

// TestRouterModelsCatalogueTakesAnInFlightToken is MD1. A v2 node's own
// /v1/models is already the union across alive members and includes aliases,
// so the gateway forwards the catalogue to the view node instead of building
// it locally. That makes it a real upstream request holding a real connection,
// so unlike the power-path deny below it counts against the concurrency cap.
func TestRouterModelsCatalogueTakesAnInFlightToken(t *testing.T) {
	release := make(chan struct{})
	var releaseOnce sync.Once
	releaseAll := func() { releaseOnce.Do(func() { close(release) }) }

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-release
		w.WriteHeader(http.StatusOK)
	}))
	// Ordered so the blocked upstream is always let go before Close waits on
	// it, however this test exits.
	defer upstream.Close()
	defer releaseAll()

	rt := newTestRouterWithMaxInFlight(t, upstream, 1)

	holding := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		rec := httptest.NewRecorder()
		close(holding)
		rt.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"gemma"}`)))
	}()
	<-holding
	time.Sleep(50 * time.Millisecond) // let the goroutine acquire and block in the upstream call

	rec := httptest.NewRecorder()
	rt.ServeHTTP(rec, httptest.NewRequest("GET", "/v1/models", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("/v1/models while the cap is exhausted: status = %d, want 503", rec.Code)
	}

	releaseAll()
	<-done

	// With the token back, the catalogue is served by the view node.
	rec = httptest.NewRecorder()
	rt.ServeHTTP(rec, httptest.NewRequest("GET", "/v1/models", nil))
	if rec.Code != http.StatusOK {
		t.Errorf("/v1/models with a free token: status = %d, want 200", rec.Code)
	}
}

// TestRouterPowerDenyNotGatedByInFlightCap proves the power-path 403 deny
// happens regardless of the concurrency cap: it must still return 403 (not
// a 503) even when every token is held.
func TestRouterPowerDenyNotGatedByInFlightCap(t *testing.T) {
	release := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-release
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	rt := newTestRouterWithMaxInFlight(t, upstream, 1)

	holding := make(chan struct{})
	go func() {
		rec := httptest.NewRecorder()
		close(holding)
		rt.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"gemma"}`)))
	}()
	<-holding
	time.Sleep(50 * time.Millisecond)

	rec := httptest.NewRecorder()
	rt.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/power", strings.NewReader("{}")))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("/v1/power while cap exhausted: status = %d, want 403 (must not be gated by the cap)", rec.Code)
	}

	close(release)
}

// TestRouterInFlightTokenReleasedOnUpstreamError proves a request that ends
// in error (an unreachable node, 502) still releases its token: with
// maxInFlight=1, a second request within the cap must succeed (reach the
// unreachable node and get its own 502, not a 503-over-capacity) once the
// first has returned.
func TestRouterInFlightTokenReleasedOnUpstreamError(t *testing.T) {
	rt := newUnreachableTestRouter(t, 1)

	for i := 0; i < 2; i++ {
		rec := httptest.NewRecorder()
		rt.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"gemma"}`)))
		if rec.Code != http.StatusBadGateway {
			t.Fatalf("request %d: status = %d, want 502 (not 503 — the prior token must have been released)", i, rec.Code)
		}
	}
}

// TestRouterInFlightTokensNoRaceOrLeak hammers tryAcquire/release
// concurrently under -race to prove there is no data race and the token
// count never goes negative or leaks: after every goroutine finishes, a
// fresh acquire up to the full cap must still succeed exactly capSize times,
// and one more must fail.
func TestRouterInFlightTokensNoRaceOrLeak(t *testing.T) {
	const capSize = 8
	const workers = 64
	const iterations = 200

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	rt := newTestRouterWithMaxInFlight(t, upstream, capSize)

	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < iterations; i++ {
				if release, ok := rt.tryAcquire(); ok {
					release()
				}
			}
		}()
	}
	wg.Wait()

	var releases []func()
	for i := 0; i < capSize; i++ {
		release, ok := rt.tryAcquire()
		if !ok {
			t.Fatalf("acquire %d/%d failed: token leaked (fewer than %d slots available)", i+1, capSize, capSize)
		}
		releases = append(releases, release)
	}
	if _, ok := rt.tryAcquire(); ok {
		t.Fatalf("acquire succeeded beyond cap=%d: over-release/negative-count bug", capSize)
	}
	for _, release := range releases {
		release()
	}
}
