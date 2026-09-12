package proxy

import (
	"bufio"
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func addrOf(srv *httptest.Server) string {
	return strings.TrimPrefix(srv.URL, "http://")
}

func TestForwardProxiesRequestAndResponse(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/status" {
			t.Errorf("upstream path = %q", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"node_id":"a"}`))
	}))
	defer upstream.Close()

	f := NewForwarder(nil)
	rec := httptest.NewRecorder()
	f.To(rec, httptest.NewRequest("GET", "/v1/status", nil), addrOf(upstream))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "node_id") {
		t.Errorf("body = %q", rec.Body.String())
	}
}

// CORS headers from upstream must not survive. Everything is same-origin
// through the gateway, so emitting them would only let a third-party page
// drive the fleet from a visitor's browser.
func TestForwardStripsCORSHeaders(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Headers", "content-type")
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	f := NewForwarder(nil)
	rec := httptest.NewRecorder()
	f.To(rec, httptest.NewRequest("GET", "/v1/models", nil), addrOf(upstream))

	for k := range rec.Header() {
		if strings.HasPrefix(strings.ToLower(k), "access-control-") {
			t.Errorf("CORS header survived: %s", k)
		}
	}
}

// F3 defense-in-depth: every proxied response — the sticky/view HTML paths
// above all — must carry the anti-sniffing/anti-framing/CSP headers, since a
// hostile or merely compromised view node's raw response otherwise reaches
// the browser verbatim (see docs/security/adversarial-mesh.md finding F3).
func TestForwardSetsSecurityHeaders(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte("<html></html>"))
	}))
	defer upstream.Close()

	f := NewForwarder(nil)
	rec := httptest.NewRecorder()
	_ = f.To(rec, httptest.NewRequest("GET", "/", nil), addrOf(upstream))

	h := rec.Header()
	if got := h.Get("X-Content-Type-Options"); got != "nosniff" {
		t.Errorf("X-Content-Type-Options = %q, want nosniff", got)
	}
	if got := h.Get("X-Frame-Options"); got != "DENY" {
		t.Errorf("X-Frame-Options = %q, want DENY", got)
	}
	if got := h.Get("Content-Security-Policy"); got != contentSecurityPolicy {
		t.Errorf("Content-Security-Policy = %q, want %q", got, contentSecurityPolicy)
	}
	if !strings.Contains(h.Get("Content-Security-Policy"), "frame-ancestors 'none'") {
		t.Errorf("CSP missing frame-ancestors 'none': %q", h.Get("Content-Security-Policy"))
	}
}

// A malicious or compromised view node must not be able to weaken the
// gateway's framing/CSP protection by shipping its own permissive values —
// the gateway's value must win outright, with no duplicate or surviving
// upstream value left in the response.
func TestForwardSecurityHeadersOverrideHostileUpstream(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Frame-Options", "ALLOWALL")
		w.Header().Set("Content-Security-Policy", "default-src *")
		w.Header().Set("X-Content-Type-Options", "")
		_, _ = w.Write([]byte("<script>alert(document.cookie)</script>"))
	}))
	defer upstream.Close()

	f := NewForwarder(nil)
	rec := httptest.NewRecorder()
	_ = f.To(rec, httptest.NewRequest("GET", "/", nil), addrOf(upstream))

	h := rec.Header()
	if vals := h.Values("X-Frame-Options"); len(vals) != 1 || vals[0] != "DENY" {
		t.Errorf("X-Frame-Options = %v, want exactly [DENY]", vals)
	}
	if vals := h.Values("Content-Security-Policy"); len(vals) != 1 || vals[0] != contentSecurityPolicy {
		t.Errorf("Content-Security-Policy = %v, want exactly [%q]", vals, contentSecurityPolicy)
	}
	if vals := h.Values("X-Content-Type-Options"); len(vals) != 1 || vals[0] != "nosniff" {
		t.Errorf("X-Content-Type-Options = %v, want exactly [nosniff]", vals)
	}
}

// A compromised or hostile seed view node must not be able to plant a
// cookie at the gateway's authenticated origin by shipping its own
// Set-Cookie header; it must never reach the client.
func TestForwardStripsUpstreamSetCookie(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Set-Cookie", "evil=1; Path=/")
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	f := NewForwarder(nil)
	rec := httptest.NewRecorder()
	_ = f.To(rec, httptest.NewRequest("GET", "/", nil), addrOf(upstream))

	if got := rec.Header().Values("Set-Cookie"); len(got) != 0 {
		t.Errorf("Set-Cookie survived from upstream: %v", got)
	}
}

// FW1: a node that cannot be dialled is handed back to the router, not
// written to the client. Nothing may be written, because the router is about
// to try the same request on another node.
func TestForwardUnreachableIsReportedNotWritten(t *testing.T) {
	f := NewForwarder(nil)
	rec := httptest.NewRecorder()
	err := f.To(rec, httptest.NewRequest("GET", "/v1/models", nil), "127.0.0.1:1")

	if !errors.Is(err, ErrUpstreamUnreachable) {
		t.Fatalf("To returned %v, want an error wrapping ErrUpstreamUnreachable", err)
	}
	if rec.Code != http.StatusOK || rec.Body.Len() != 0 {
		t.Errorf("status %d with %d bytes written; an unreachable node must "+
			"leave the response untouched so the retry can use it",
			rec.Code, rec.Body.Len())
	}
	if got := rec.Header().Get("Content-Security-Policy"); got != "" {
		t.Errorf("headers were written too: CSP = %q", got)
	}
}

// FW2: a node that accepts the connection and then fails is NOT retryable —
// the request was already sent, and a generation may have started. That is
// written as a 502 immediately, with the security headers a locally generated
// response needs.
func TestForwardFailureAfterConnectIsWrittenAs502(t *testing.T) {
	// An upstream that accepts and closes without a response.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			_ = c.Close()
		}
	}()

	f := NewForwarder(nil)
	rec := httptest.NewRecorder()
	if err := f.To(rec, httptest.NewRequest("GET", "/v1/models", nil), ln.Addr().String()); err != nil {
		t.Fatalf("To returned %v, want nil: a failure after connect is not retryable", err)
	}
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `"error"`) {
		t.Errorf("502 should use the OpenAI error shape, got %q", rec.Body.String())
	}
	if got := rec.Header().Get("X-Content-Type-Options"); got != "nosniff" {
		t.Errorf("X-Content-Type-Options = %q, want nosniff", got)
	}
	if got := rec.Header().Get("Content-Security-Policy"); got != contentSecurityPolicy {
		t.Errorf("Content-Security-Policy = %q, want the gateway's own", got)
	}
}

// FW3: a client that hung up gets nothing, and is not an error.
func TestForwardCancelledClientWritesNothing(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	f := NewForwarder(nil)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/v1/models", nil).WithContext(ctx)
	if err := f.To(rec, req, "127.0.0.1:1"); err != nil {
		t.Fatalf("To returned %v, want nil for a cancelled client", err)
	}
	if rec.Body.Len() != 0 {
		t.Errorf("wrote %d bytes to a client that had gone away", rec.Body.Len())
	}
}

// writeUnreachable is what the router calls once the retry is spent. It is the
// 502 that FW1 deliberately did not write.
func TestWriteUnreachableCarriesSecurityHeaders(t *testing.T) {
	rec := httptest.NewRecorder()
	writeUnreachable(rec, "node-a")

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "node-a") {
		t.Errorf("body %q should name the node that could not be reached", rec.Body.String())
	}
	if got := rec.Header().Get("Content-Security-Policy"); got != contentSecurityPolicy {
		t.Errorf("Content-Security-Policy = %q, want the gateway's own", got)
	}
}

// The whole point of FlushInterval: -1. Without it, token streaming and every
// SSE endpoint arrive in one lump at close.
func TestForwardStreamsIncrementally(t *testing.T) {
	release := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("data: first\n\n"))
		w.(http.Flusher).Flush()
		<-release // hold the response open
		_, _ = w.Write([]byte("data: second\n\n"))
		w.(http.Flusher).Flush()
	}))
	defer upstream.Close()

	f := NewForwarder(nil)
	gw := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.To(w, r, addrOf(upstream))
	}))
	defer gw.Close()

	resp, err := http.Get(gw.URL + "/v1/mesh/stream")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()

	type read struct {
		line string
		err  error
	}
	got := make(chan read, 1)
	go func() {
		line, err := bufio.NewReader(resp.Body).ReadString('\n')
		got <- read{line, err}
	}()

	select {
	case r := <-got:
		if r.err != nil {
			t.Fatalf("read: %v", r.err)
		}
		if !strings.Contains(r.line, "first") {
			t.Fatalf("first chunk = %q", r.line)
		}
	case <-time.After(2 * time.Second):
		close(release)
		t.Fatal("first chunk never arrived: the proxy is buffering the stream")
	}
	close(release)
}

// A closed browser tab must stop the generation rather than leave GPUs busy.
func TestForwardCancelsUpstreamOnClientDisconnect(t *testing.T) {
	started := make(chan struct{})
	gone := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		// Signal that the handler is running rather than making the client
		// wait on a body byte this stream never sends.
		close(started)
		<-r.Context().Done()
		close(gone)
	}))
	defer upstream.Close()

	f := NewForwarder(nil)
	gw := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.To(w, r, addrOf(upstream))
	}))
	defer gw.Close()

	ctx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, "GET", gw.URL+"/v1/mesh/stream", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()

	<-started
	cancel()

	select {
	case <-gone:
	case <-time.After(3 * time.Second):
		t.Fatal("upstream request was not cancelled when the client went away")
	}
}
