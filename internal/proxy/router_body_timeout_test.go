package proxy

import (
	"bufio"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/janit/viiwork-gateway/internal/mesh"
)

// TestRouterSeversSlowBodyInBoundedTime is the direct reproduction of R2:
// 300 connections that send complete headers (Content-Length: 15MB) then
// send nothing further used to stay open indefinitely, each pinning a
// goroutine + FD. This drives one such connection over a raw socket — a
// real net/http.Client would try to be helpful about the stalled body, so a
// raw socket is what actually proves the server-side deadline, not the
// client's own behaviour.
func TestRouterSeversSlowBodyInBoundedTime(t *testing.T) {
	reached := false
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached = true
	}))
	defer upstream.Close()

	const bodyReadTimeout = 200 * time.Millisecond
	rt := newTestRouterWithBodyTimeout(t, upstream, bodyReadTimeout)
	gw := httptest.NewServer(rt)
	defer gw.Close()

	addr := strings.TrimPrefix(gw.URL, "http://")
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("dial gateway: %v", err)
	}
	defer conn.Close()

	// Complete headers advertising a large body, then exactly one byte of
	// body, then nothing else, ever — the finding's own reproduction.
	req := "POST /v1/chat/completions HTTP/1.1\r\n" +
		"Host: " + addr + "\r\n" +
		"Content-Length: 15728640\r\n" +
		"Content-Type: application/json\r\n" +
		"Connection: close\r\n" +
		"\r\n" +
		"{"
	start := time.Now()
	if _, err := conn.Write([]byte(req)); err != nil {
		t.Fatalf("write request: %v", err)
	}

	// A generous outer ceiling: if the fix regressed back to "no bound at
	// all", this read would block until the test binary's own timeout
	// kills it. Bounding it here turns that into a fast, readable failure.
	if err := conn.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatalf("SetReadDeadline: %v", err)
	}

	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("gateway never responded within 5s of the body stalling: %v (elapsed %v)", err, elapsed)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusRequestTimeout {
		body, _ := io.ReadAll(resp.Body)
		t.Errorf("status = %d, want 408 request timeout: %s", resp.StatusCode, body)
	}
	// Bounded, not instantaneous: should land near bodyReadTimeout, well
	// under the 5s outer ceiling above.
	if elapsed > 3*time.Second {
		t.Errorf("gateway took %v to sever a stalled body; the deadline (%v) should have fired well before this", elapsed, bodyReadTimeout)
	}
	if reached {
		t.Error("a request whose body never fully arrived must never be forwarded to a node")
	}
}

// TestRouterFastBodyStillSucceeds proves the deadline machinery does not
// interfere with an ordinary, promptly-sent request.
func TestRouterFastBodyStillSucceeds(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(body)
	}))
	defer upstream.Close()

	rt := newTestRouterWithBodyTimeout(t, upstream, 200*time.Millisecond)
	gw := httptest.NewServer(rt)
	defer gw.Close()

	resp, err := http.Post(gw.URL+"/v1/chat/completions", "application/json",
		strings.NewReader(`{"model":"gemma"}`))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("reading response: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", resp.StatusCode, body)
	}
	if !strings.Contains(string(body), "gemma") {
		t.Errorf("body = %q, want the echoed request", body)
	}
}

// TestRouterBodyReadDeadlineDoesNotCutLongResponse is the regression guard
// that matters most: it proves the body-read deadline is cleared before the
// response phase begins, so a response that legitimately takes longer than
// BodyReadTimeout to complete (a long inference generation, an SSE stream)
// is NOT severed. This is the test that would fail if the fix had instead
// used http.Server.ReadTimeout — the corrected finding's exact warning.
func TestRouterBodyReadDeadlineDoesNotCutLongResponse(t *testing.T) {
	const bodyReadTimeout = 150 * time.Millisecond
	const streamDelay = bodyReadTimeout * 4 // well past the body deadline

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Drain the (fast, small) request body first, as routeByModel does.
		_, _ = io.ReadAll(r.Body)

		flusher, ok := w.(http.Flusher)
		if !ok {
			t.Fatal("upstream ResponseWriter must support Flusher")
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("data: first\n\n"))
		flusher.Flush()

		time.Sleep(streamDelay)

		_, _ = w.Write([]byte("data: last\n\n"))
		flusher.Flush()
	}))
	defer upstream.Close()

	rt := newTestRouterWithBodyTimeout(t, upstream, bodyReadTimeout)
	gw := httptest.NewServer(rt)
	defer gw.Close()

	start := time.Now()
	resp, err := http.Post(gw.URL+"/v1/chat/completions", "application/json",
		strings.NewReader(`{"model":"gemma","stream":true}`))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("streamed response was cut short after %v (want it to run past the %v body-read "+
			"deadline uninterrupted): %v", elapsed, bodyReadTimeout, err)
	}
	if elapsed < streamDelay {
		t.Fatalf("test setup is wrong: response returned in %v, faster than the %v it was made to take", elapsed, streamDelay)
	}
	if !strings.Contains(string(body), "first") || !strings.Contains(string(body), "last") {
		t.Errorf("did not receive the full stream: %q", body)
	}
}

// TestRouterStickyPathSeversSlowBodyInBoundedTime is the sticky/view-path
// counterpart of TestRouterSeversSlowBodyInBoundedTime: the final
// whole-branch adversarial review found that the R2/H5 body-read deadline
// had been applied to routeByModel but NOT to routeToView, even though the
// gateway forwards ANY method/path through routeToView's default case — so
// a slow-body POST to a sticky path (e.g. /v1/mesh/prompt, or any unmatched
// path) used to be read with no deadline at all, while holding an in-flight
// concurrency token (H4), two FDs, and an upstream mesh connection. This
// drives that exact reproduction — a raw socket sending complete headers
// then withholding the body forever — at a sticky path, and proves it now
// gets 408 in bounded time, just like the inference path.
func TestRouterStickyPathSeversSlowBodyInBoundedTime(t *testing.T) {
	reached := false
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached = true
	}))
	defer upstream.Close()

	const bodyReadTimeout = 200 * time.Millisecond
	rt := newTestRouterWithBodyTimeout(t, upstream, bodyReadTimeout)
	gw := httptest.NewServer(rt)
	defer gw.Close()

	addr := strings.TrimPrefix(gw.URL, "http://")
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("dial gateway: %v", err)
	}
	defer conn.Close()

	// /v1/mesh/prompt matches none of the model-routed inference paths, so
	// it falls through ServeHTTP's default case into routeToView.
	req := "POST /v1/mesh/prompt HTTP/1.1\r\n" +
		"Host: " + addr + "\r\n" +
		"Content-Length: 15728640\r\n" +
		"Content-Type: application/json\r\n" +
		"Connection: close\r\n" +
		"\r\n" +
		"{"
	start := time.Now()
	if _, err := conn.Write([]byte(req)); err != nil {
		t.Fatalf("write request: %v", err)
	}

	if err := conn.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatalf("SetReadDeadline: %v", err)
	}

	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("gateway never responded within 5s of the sticky-path body stalling: %v (elapsed %v)", err, elapsed)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusRequestTimeout {
		body, _ := io.ReadAll(resp.Body)
		t.Errorf("status = %d, want 408 request timeout: %s", resp.StatusCode, body)
	}
	if elapsed > 3*time.Second {
		t.Errorf("gateway took %v to sever a stalled sticky-path body; the deadline (%v) should have fired well before this", elapsed, bodyReadTimeout)
	}
	if reached {
		t.Error("a sticky-path request whose body never fully arrived must never be forwarded to a node")
	}
}

// TestRouterStickyPathTimedOutBodyReleasesToken proves that a sticky-path
// request whose body read times out still releases its in-flight token
// (the defer release() in routeToView) — otherwise a handful of slow-body
// sticky requests would permanently wedge the concurrency cap (H4) for
// every tenant, which is the crux of the whole-branch finding this fix
// closes. maxInFlight is set to 1 so token exhaustion is directly
// observable: a second request while the first token is (wrongly) still
// held would get 503, not the upstream's real answer.
func TestRouterStickyPathTimedOutBodyReleasesToken(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	addr := strings.TrimPrefix(upstream.URL, "http://")
	reg := mesh.New(mesh.Options{Seeds: []string{addr}, Timeout: time.Second, DiscoveryEvery: 1})
	reg.SetSnapshotForTest(mesh.BuildSnapshot(map[string]*mesh.Node{
		addr: {
			Addr:            addr,
			Models:          []string{"gemma"},
			Healthy:         true,
			HealthyBackends: 1,
			InFlight:        0,
			InFlightKnown:   true,
			FullUI:          true,
			Seed:            true,
		},
	}, ""))

	const bodyReadTimeout = 200 * time.Millisecond
	rt := NewRouter(reg, NewForwarder(nil), 16*1024*1024, 1, bodyReadTimeout, nil, nil)
	gw := httptest.NewServer(rt)
	defer gw.Close()

	gwAddr := strings.TrimPrefix(gw.URL, "http://")
	conn, err := net.Dial("tcp", gwAddr)
	if err != nil {
		t.Fatalf("dial gateway: %v", err)
	}
	defer conn.Close()

	req := "POST /v1/mesh/prompt HTTP/1.1\r\n" +
		"Host: " + gwAddr + "\r\n" +
		"Content-Length: 15728640\r\n" +
		"Content-Type: application/json\r\n" +
		"Connection: close\r\n" +
		"\r\n" +
		"{"
	if _, err := conn.Write([]byte(req)); err != nil {
		t.Fatalf("write request: %v", err)
	}
	if err := conn.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatalf("SetReadDeadline: %v", err)
	}
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatalf("gateway never responded to the stalled body: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusRequestTimeout {
		t.Fatalf("status = %d, want 408", resp.StatusCode)
	}
	conn.Close()

	// The sole token must now be free: a fresh sticky GET must reach the
	// upstream and get 200, not 503-over-capacity.
	rec := httptest.NewRecorder()
	rt.ServeHTTP(rec, httptest.NewRequest("GET", "/mesh", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("post-timeout request: status = %d, want 200 (token should have been released)", rec.Code)
	}
}

// TestRouterGetStickyPathUnaffectedByBodyReadDeadline proves a GET
// sticky/view request — which has no body, and is never routed through
// readBody at all — is unaffected by the body-read deadline machinery even
// when its response takes far longer than BodyReadTimeout to complete.
func TestRouterGetStickyPathUnaffectedByBodyReadDeadline(t *testing.T) {
	const bodyReadTimeout = 150 * time.Millisecond
	const streamDelay = bodyReadTimeout * 4

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		flusher, ok := w.(http.Flusher)
		if !ok {
			t.Fatal("upstream ResponseWriter must support Flusher")
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("start"))
		flusher.Flush()

		time.Sleep(streamDelay)

		_, _ = w.Write([]byte("end"))
		flusher.Flush()
	}))
	defer upstream.Close()

	rt := newTestRouterWithBodyTimeout(t, upstream, bodyReadTimeout)
	gw := httptest.NewServer(rt)
	defer gw.Close()

	start := time.Now()
	resp, err := http.Get(gw.URL + "/v1/mesh/stream")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("GET stream was cut short after %v: %v", elapsed, err)
	}
	if elapsed < streamDelay {
		t.Fatalf("test setup is wrong: response returned in %v, faster than the %v it was made to take", elapsed, streamDelay)
	}
	if !strings.Contains(string(body), "start") || !strings.Contains(string(body), "end") {
		t.Errorf("did not receive the full GET response: %q", body)
	}
}

// TestRouterStickyPathPostBodyReadDeadlineDoesNotCutLongResponse is
// TestRouterBodyReadDeadlineDoesNotCutLongResponse's sticky-path
// counterpart, and the one that actually exercises the new readBody call
// in routeToView with a real (small, promptly-sent) body: it proves the
// deadline set around that read is cleared before forwarding, so a
// long-lived streaming response from the sticky/view path is not severed
// even though it legitimately runs far longer than BodyReadTimeout.
func TestRouterStickyPathPostBodyReadDeadlineDoesNotCutLongResponse(t *testing.T) {
	const bodyReadTimeout = 150 * time.Millisecond
	const streamDelay = bodyReadTimeout * 4

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)

		flusher, ok := w.(http.Flusher)
		if !ok {
			t.Fatal("upstream ResponseWriter must support Flusher")
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("data: first\n\n"))
		flusher.Flush()

		time.Sleep(streamDelay)

		_, _ = w.Write([]byte("data: last\n\n"))
		flusher.Flush()
	}))
	defer upstream.Close()

	rt := newTestRouterWithBodyTimeout(t, upstream, bodyReadTimeout)
	gw := httptest.NewServer(rt)
	defer gw.Close()

	start := time.Now()
	resp, err := http.Post(gw.URL+"/v1/mesh/prompt", "application/json",
		strings.NewReader(`{"prompt":"hi"}`))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("sticky-path streamed response was cut short after %v (want it to run past the %v "+
			"body-read deadline uninterrupted): %v", elapsed, bodyReadTimeout, err)
	}
	if elapsed < streamDelay {
		t.Fatalf("test setup is wrong: response returned in %v, faster than the %v it was made to take", elapsed, streamDelay)
	}
	if !strings.Contains(string(body), "first") || !strings.Contains(string(body), "last") {
		t.Errorf("did not receive the full stream: %q", body)
	}
}
