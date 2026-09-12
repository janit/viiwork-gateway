package main

import (
	"bufio"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/janit/viiwork-gateway/internal/config"
	"github.com/janit/viiwork-gateway/internal/fleet"
	"github.com/janit/viiwork-gateway/internal/keys"
)

const key = "aaaaaaaaaaaaaaaaaaaaaaaaaaaa"

func testHandler(t *testing.T) http.Handler {
	t.Helper()
	return testHandlerWithFleet(t, stubFleet{})
}

func testHandlerWithFleet(t *testing.T, fl stubFleet) http.Handler {
	t.Helper()
	set, err := keys.Load([]string{"VIIWORK_KEY_janit=" + key})
	if err != nil {
		t.Fatalf("keys.Load: %v", err)
	}
	cfg := config.Config{
		CookieTTL: time.Hour,
		MaxBody:   1024,
	}
	return buildHandler(fl, set, cfg, slog.Default())
}

// The whole surface is behind authentication. No exceptions, including the
// paths the gateway answers itself.
func TestEveryPathRequiresAKey(t *testing.T) {
	h := testHandler(t)
	for _, path := range []string{
		"/", "/mesh", "/chat", "/health",
		"/v1/models", "/v1/status", "/v1/cluster",
		"/v1/mesh/stream", "/v1/prompts", "/v1/power",
	} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("GET", path, nil))
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("%s: status = %d, want 401 with no credential", path, rec.Code)
		}
	}
}

// An authenticated request reaches the mesh. The catalogue is the view node's
// answer now rather than one the gateway assembles, so this proves the whole
// chain — auth, logging, routing, forwarding — carries it there and back.
func TestAuthenticatedModelsRequestSucceeds(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			t.Errorf("view node saw path %q", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "" {
			t.Errorf("credential reached the node: %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"object":"list","data":[]}`))
	}))
	defer upstream.Close()

	h := testHandlerWithFleet(t, fleetAt(strings.TrimPrefix(upstream.URL, "http://")))
	req := httptest.NewRequest("GET", "/v1/models", nil)
	req.Header.Set("Authorization", "Bearer "+key)
	rec := httptest.NewRecorder()

	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `"list"`) {
		t.Errorf("body = %q", rec.Body.String())
	}
}

// TestFullChainSeversStalledStickyBody guards FIX 3: the body-read deadline
// (R2/H5, and its sticky-path extension in routeToView) depends on
// accesslog.recorder.Unwrap reaching the REAL connection through the full
// auth -> accesslog -> router chain that buildHandler composes in
// production. A unit test that calls Router.readBody directly, or that
// drives the router in isolation with an httptest.ResponseRecorder, cannot
// catch a future middleware inserted into that chain without its own
// Unwrap: http.NewResponseController would then fail closed with
// http.ErrNotSupported and the deadline would silently degrade to
// unbounded — readBody logs a warning and proceeds without ever returning
// an error for it (see router.go's readBody). So this drives a raw socket
// through buildHandler's own composition, exactly as production runs it,
// and must observe a 408 in bounded time. If the Unwrap chain ever breaks,
// this test hangs until its own outer deadline and fails loudly instead of
// silently passing.
func TestFullChainSeversStalledStickyBody(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("a stalled body must never be forwarded to a mesh node")
	}))
	defer upstream.Close()
	addr := strings.TrimPrefix(upstream.URL, "http://")

	fl := fleetAt(addr)

	set, err := keys.Load([]string{"VIIWORK_KEY_janit=" + key})
	if err != nil {
		t.Fatalf("keys.Load: %v", err)
	}

	const bodyReadTimeout = 200 * time.Millisecond
	cfg := config.Config{
		CookieTTL:       time.Hour,
		MaxBody:         1024,
		MaxInFlight:     10,
		BodyReadTimeout: bodyReadTimeout,
	}

	h := buildHandler(fl, set, cfg, slog.Default())
	gw := httptest.NewServer(h)
	defer gw.Close()

	gwAddr := strings.TrimPrefix(gw.URL, "http://")
	conn, err := net.Dial("tcp", gwAddr)
	if err != nil {
		t.Fatalf("dial gateway: %v", err)
	}
	defer conn.Close()

	// A sticky/unmatched path (routeToView's default case), complete
	// headers advertising a large body, then exactly one byte of body and
	// nothing else, ever. Authenticated, so the request actually reaches
	// the router rather than being turned away by auth first.
	req := "POST /v1/mesh/prompt HTTP/1.1\r\n" +
		"Host: " + gwAddr + "\r\n" +
		"Authorization: Bearer " + key + "\r\n" +
		"Content-Length: 15728640\r\n" +
		"Content-Type: application/json\r\n" +
		"Connection: close\r\n" +
		"\r\n" +
		"{"
	start := time.Now()
	if _, err := conn.Write([]byte(req)); err != nil {
		t.Fatalf("write request: %v", err)
	}

	// A generous outer ceiling: if the Unwrap chain were broken, this read
	// would block until the test binary's own timeout kills it. Bounding it
	// here turns that into a fast, readable failure instead.
	if err := conn.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatalf("SetReadDeadline: %v", err)
	}

	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("gateway never responded within 5s of the stalled sticky-path body "+
			"(the Unwrap chain from accesslog through to the real conn is likely broken): %v (elapsed %v)", err, elapsed)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusRequestTimeout {
		body, _ := io.ReadAll(resp.Body)
		t.Errorf("status = %d, want 408 request timeout: %s", resp.StatusCode, body)
	}
	if elapsed > 3*time.Second {
		t.Errorf("gateway took %v to sever a stalled sticky-path body; the deadline (%v) should have fired well before this", elapsed, bodyReadTimeout)
	}
}

// Denial must sit behind authentication, so an anonymous prod does not learn
// which paths exist.
func TestPowerDeniedForAValidKey(t *testing.T) {
	h := testHandler(t)
	req := httptest.NewRequest("POST", "/v1/mesh/power", strings.NewReader("{}"))
	req.Header.Set("Authorization", "Bearer "+key)
	rec := httptest.NewRecorder()

	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rec.Code)
	}
}

// TestFullChainRateLimitsPerKeyNotGlobally is the composition test for
// per-key rate limiting, and it exists for one specific silent-failure mode:
// the limiter meters by the label auth.Middleware puts in the request
// context, and if that label failed to reach the router — a future middleware
// that rebuilds the request without the context, say — every key would share
// a single empty-label bucket. The limiter would still "work" in its own unit
// tests while silently metering the whole fleet as one tenant, and the first
// symptom would be one customer's traffic throttling another's.
//
// So this drives the REAL buildHandler composition with two REAL keys and
// asserts they have independent budgets.
func TestFullChainRateLimitsPerKeyNotGlobally(t *testing.T) {
	const (
		aliceKey = "aaaaaaaaaaaaaaaaaaaaaaaaaaaa"
		bobKey   = "bbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	)
	set, err := keys.Load([]string{
		"VIIWORK_KEY_alice=" + aliceKey,
		"VIIWORK_KEY_bob=" + bobKey,
	})
	if err != nil {
		t.Fatalf("keys.Load: %v", err)
	}

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()
	addr := strings.TrimPrefix(upstream.URL, "http://")

	cfg := config.Config{
		CookieTTL:       time.Hour,
		MaxBody:         1 << 20,
		MaxInFlight:     256,
		BodyReadTimeout: 30 * time.Second,
		RatePerMin:      60,
		RateBurst:       2,
	}
	fl := fleetAt(addr)
	h := buildHandler(fl, set, cfg, slog.Default())

	post := func(apiKey string) int {
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
			strings.NewReader(`{"model":"gemma"}`))
		req.Header.Set("Authorization", "Bearer "+apiKey)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec.Code
	}

	// Alice spends her whole burst, then hits the wall.
	for i := range 2 {
		if got := post(aliceKey); got != http.StatusOK {
			t.Fatalf("alice request %d: status %d, want 200", i+1, got)
		}
	}
	if got := post(aliceKey); got != http.StatusTooManyRequests {
		t.Fatalf("alice past her burst: status %d, want 429", got)
	}

	// Bob is a different tenant and must be untouched by that. If this fails
	// with 429, the label is not reaching the limiter and every key is
	// sharing one bucket.
	if got := post(bobKey); got != http.StatusOK {
		t.Fatalf("bob's first request: status %d, want 200 — keys are sharing a single rate-limit bucket", got)
	}
}

// TestFullChainRateLimitCanBeDisabled proves the documented escape hatch
// survives composition too: RatePerMin=0 must mean unmetered, not "burst of
// zero, reject everything".
func TestFullChainRateLimitCanBeDisabled(t *testing.T) {
	set, err := keys.Load([]string{"VIIWORK_KEY_janit=" + key})
	if err != nil {
		t.Fatalf("keys.Load: %v", err)
	}

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()
	addr := strings.TrimPrefix(upstream.URL, "http://")

	cfg := config.Config{
		CookieTTL:       time.Hour,
		MaxBody:         1 << 20,
		MaxInFlight:     256,
		BodyReadTimeout: 30 * time.Second,
		RatePerMin:      0, // disabled
		RateBurst:       240,
	}
	fl := fleetAt(addr)
	h := buildHandler(fl, set, cfg, slog.Default())

	for i := range 50 {
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
			strings.NewReader(`{"model":"gemma"}`))
		req.Header.Set("Authorization", "Bearer "+key)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("request %d: status %d, want 200 with rate limiting disabled", i+1, rec.Code)
		}
	}
}

// stubFleet is a one-node Fleet: these tests exercise the handler pipeline
// (auth, logging, rate limiting, routing), not the membership behind it.
type stubFleet struct{ target fleet.Target }

func (f stubFleet) Pick(string, map[string]bool) (fleet.Target, func(), bool) {
	if f.target.APIAddr == "" {
		return fleet.Target{}, func() {}, false
	}
	return f.target, func() {}, true
}

func (f stubFleet) View() (fleet.Target, bool) {
	return f.target, f.target.APIAddr != ""
}

func fleetAt(addr string) stubFleet {
	return stubFleet{target: fleet.Target{Node: "node-a", APIAddr: addr}}
}
