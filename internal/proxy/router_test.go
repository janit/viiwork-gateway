package proxy

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/janit/viiwork-gateway/internal/mesh"
)

func TestRouterDeniesChassisPowerPaths(t *testing.T) {
	reached := false
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached = true
	}))
	defer upstream.Close()

	rt := newTestRouter(t, upstream)

	for _, path := range []string{"/v1/power", "/v1/mesh/power"} {
		rec := httptest.NewRecorder()
		rt.ServeHTTP(rec, httptest.NewRequest("POST", path, strings.NewReader("{}")))

		if rec.Code != http.StatusForbidden {
			t.Errorf("%s: status = %d, want 403", path, rec.Code)
		}
		if !strings.Contains(rec.Body.String(), "error") {
			t.Errorf("%s: want an OpenAI-shaped error, got %q", path, rec.Body.String())
		}
	}
	if reached {
		t.Error("a denied path must not contact any node")
	}
}

// TestRouterDeniesNormalisedChassisPowerPaths proves the power-path deny
// evaluates the same canonical form a node might resolve to, so path tricks
// that survive net/http's decoding (duplicate slashes, dot segments, a
// trailing slash, case) never reach a node. See H6 / adversarial-proxy P1.
func TestRouterDeniesNormalisedChassisPowerPaths(t *testing.T) {
	deniedPaths := []string{
		"/v1/power",
		"//v1/power",
		"/v1/./power",
		"/v1/power/",
		"/v1//power",
		"/v1/x/../power",
		"/v1/mesh/power",
		"//v1/mesh/power",
		"/v1/mesh/./power",
		"/v1/mesh/power/",
		"/V1/POWER",
		"/v1/po%77er", // percent-encoded 'w'; net/http decodes this to /v1/power
	}

	for _, path := range deniedPaths {
		t.Run(path, func(t *testing.T) {
			hit := false
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				hit = true
			}))
			defer upstream.Close()

			rt := newTestRouter(t, upstream)
			rec := httptest.NewRecorder()
			req := httptest.NewRequest("POST", path, strings.NewReader("{}"))

			rt.ServeHTTP(rec, req)

			if rec.Code != http.StatusForbidden {
				t.Errorf("status = %d, want 403", rec.Code)
			}
			if !strings.Contains(rec.Body.String(), "error") {
				t.Errorf("want an OpenAI-shaped error, got %q", rec.Body.String())
			}
			if hit {
				t.Error("a denied path must not contact any node")
			}
		})
	}
}

// TestRouterDoesNotFalselyDenyLookalikePaths proves the normalisation used by
// the power deny matches only the exact power paths, never as a prefix or
// substring, so legitimate endpoints that merely start with or embed "power"
// keep working.
func TestRouterDoesNotFalselyDenyLookalikePaths(t *testing.T) {
	allowedPaths := []string{
		"/v1/powerful",
		"/v1/power/status",
		"/v1/models",
		"/v1/chat/completions",
		"/mesh",
	}

	for _, path := range allowedPaths {
		t.Run(path, func(t *testing.T) {
			hit := false
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				hit = true
				w.WriteHeader(http.StatusOK)
			}))
			defer upstream.Close()

			rt := newTestRouter(t, upstream)
			rec := httptest.NewRecorder()
			var req *http.Request
			if path == "/v1/chat/completions" {
				req = httptest.NewRequest("POST", path, strings.NewReader(`{"model":"gemma"}`))
			} else {
				req = httptest.NewRequest("GET", path, nil)
			}

			rt.ServeHTTP(rec, req)

			if rec.Code == http.StatusForbidden {
				t.Errorf("status = 403, want this non-power path to NOT be denied by the power-path rule")
			}
			if path != "/v1/models" && !hit {
				t.Errorf("%s: expected the request to reach the node, it did not", path)
			}
		})
	}
}

func TestIsPowerPath(t *testing.T) {
	cases := []struct {
		path string
		want bool
	}{
		{"/v1/power", true},
		{"//v1/power", true},
		{"/v1/./power", true},
		{"/v1/power/", true},
		{"/v1//power", true},
		{"/v1/x/../power", true},
		{"/v1/mesh/power", true},
		{"//v1/mesh/power", true},
		{"/v1/mesh/./power", true},
		{"/v1/mesh/power/", true},
		{"/V1/POWER", true},
		{"/v1/powerful", false},
		{"/v1/power/status", false},
		{"/v1/models", false},
		{"/v1/chat/completions", false},
		{"/mesh", false},
	}
	for _, c := range cases {
		if got := isPowerPath(c.path); got != c.want {
			t.Errorf("isPowerPath(%q) = %v, want %v", c.path, got, c.want)
		}
	}
}

// Locally-generated responses (the /v1/models catalogue, and every error the
// router writes itself) must carry the same F3 defense-in-depth headers as
// proxied responses — see docs/security/adversarial-mesh.md finding F3.
func TestRouterLocalResponsesCarrySecurityHeaders(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	assertSecurityHeaders := func(t *testing.T, h http.Header) {
		t.Helper()
		if got := h.Get("X-Content-Type-Options"); got != "nosniff" {
			t.Errorf("X-Content-Type-Options = %q, want nosniff", got)
		}
		if got := h.Get("X-Frame-Options"); got != "DENY" {
			t.Errorf("X-Frame-Options = %q, want DENY", got)
		}
		if got := h.Get("Content-Security-Policy"); got != contentSecurityPolicy {
			t.Errorf("Content-Security-Policy = %q, want %q", got, contentSecurityPolicy)
		}
	}

	t.Run("models catalogue", func(t *testing.T) {
		rt := newTestRouter(t, upstream)
		rec := httptest.NewRecorder()
		rt.ServeHTTP(rec, httptest.NewRequest("GET", "/v1/models", nil))
		assertSecurityHeaders(t, rec.Header())
	})

	t.Run("power path denial", func(t *testing.T) {
		rt := newTestRouter(t, upstream)
		rec := httptest.NewRecorder()
		rt.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/power", nil))
		assertSecurityHeaders(t, rec.Header())
	})

	t.Run("oversized body rejection", func(t *testing.T) {
		rt := newTestRouterWithMaxBody(t, upstream, 8)
		rec := httptest.NewRecorder()
		rt.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/completions",
			strings.NewReader(strings.Repeat("x", 500))))
		assertSecurityHeaders(t, rec.Header())
	})

	t.Run("over in-flight capacity", func(t *testing.T) {
		rec := httptest.NewRecorder()
		writeOverCapacity(rec)
		assertSecurityHeaders(t, rec.Header())
	})
}

func TestRouterAggregatesModelsLocally(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("/v1/models must be answered by the gateway, not proxied (got %s)", r.URL.Path)
	}))
	defer upstream.Close()

	rt := newTestRouter(t, upstream)
	rec := httptest.NewRecorder()
	rt.ServeHTTP(rec, httptest.NewRequest("GET", "/v1/models", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var got struct {
		Object string `json:"object"`
		Data   []struct {
			ID      string `json:"id"`
			Object  string `json:"object"`
			OwnedBy string `json:"owned_by"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v (%s)", err, rec.Body.String())
	}
	if got.Object != "list" {
		t.Errorf("object = %q, want list", got.Object)
	}
	if len(got.Data) != 1 || got.Data[0].ID != "gemma" {
		t.Fatalf("data = %+v, want one entry for gemma", got.Data)
	}
	if got.Data[0].Object != "model" {
		t.Errorf("entry object = %q, want model", got.Data[0].Object)
	}
}

func TestRouterRoutesByModelInBody(t *testing.T) {
	var gotPath, gotBody string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		b := make([]byte, r.ContentLength)
		_, _ = r.Body.Read(b)
		gotBody = string(b)
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	rt := newTestRouter(t, upstream)
	rec := httptest.NewRecorder()
	body := `{"model":"gemma","messages":[{"role":"user","content":"hi"}]}`
	rt.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body)))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if gotPath != "/v1/chat/completions" {
		t.Errorf("upstream path = %q", gotPath)
	}
	if !strings.Contains(gotBody, `"model":"gemma"`) {
		t.Errorf("body not forwarded intact: %q", gotBody)
	}
}

func TestRouterUnknownModelStillReachesTheMesh(t *testing.T) {
	hit := false
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hit = true
		w.WriteHeader(http.StatusNotFound)
	}))
	defer upstream.Close()

	rt := newTestRouter(t, upstream)
	rec := httptest.NewRecorder()
	rt.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"never-heard-of-it"}`)))

	if !hit {
		t.Fatal("an unknown model should still be offered to the mesh; its 404 is the honest one")
	}
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want the upstream's 404", rec.Code)
	}
}

func TestRouterOversizedBodyRejected(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("an oversized body must not be forwarded")
	}))
	defer upstream.Close()

	rt := newTestRouterWithMaxBody(t, upstream, 32)
	rec := httptest.NewRecorder()
	big := `{"model":"gemma","prompt":"` + strings.Repeat("x", 500) + `"}`
	rt.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/completions", strings.NewReader(big)))

	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413", rec.Code)
	}
}

func TestRouterStickyPathsGoToViewNode(t *testing.T) {
	var paths []string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	rt := newTestRouter(t, upstream)
	for _, p := range []string{"/mesh", "/", "/v1/cluster", "/v1/mesh/stream", "/v1/prompts"} {
		rec := httptest.NewRecorder()
		rt.ServeHTTP(rec, httptest.NewRequest("GET", p, nil))
		if rec.Code != http.StatusOK {
			t.Errorf("%s: status = %d, want 200", p, rec.Code)
		}
	}
	if len(paths) != 5 {
		t.Fatalf("upstream saw %d requests, want 5: %v", len(paths), paths)
	}
}

func TestRouterNoHealthyNodesYields503(t *testing.T) {
	rt := newEmptyRouter(t)

	rec := httptest.NewRecorder()
	rt.ServeHTTP(rec, httptest.NewRequest("GET", "/mesh", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("sticky path: status = %d, want 503", rec.Code)
	}

	rec = httptest.NewRecorder()
	rt.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"gemma"}`)))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("model-routed: status = %d, want 503", rec.Code)
	}

	// /v1/models still answers, with an empty list: the gateway knows the
	// answer itself and an empty catalogue is the truthful one.
	rec = httptest.NewRecorder()
	rt.ServeHTTP(rec, httptest.NewRequest("GET", "/v1/models", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("/v1/models: status = %d, want 200", rec.Code)
	}
}

// errAfterReader returns some bytes and then a non-EOF error, simulating a
// client that disconnects partway through uploading its body. This must not
// be mistaken for an oversized body.
type errAfterReader struct {
	data []byte
	err  error
}

func (e *errAfterReader) Read(p []byte) (int, error) {
	if len(e.data) > 0 {
		n := copy(p, e.data)
		e.data = e.data[n:]
		return n, nil
	}
	return 0, e.err
}

func TestRouterTruncatedBodyYields400NotOversize(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("a truncated body must not be forwarded")
	}))
	defer upstream.Close()

	rt := newTestRouter(t, upstream)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/v1/chat/completions", nil)
	errRead := &errAfterReader{data: []byte(`{"model":"gemma"`), err: io.ErrUnexpectedEOF}
	req.Body = io.NopCloser(errRead)

	rt.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (not 413 — this is not an oversize body)", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "error") {
		t.Errorf("want an OpenAI-shaped error, got %q", rec.Body.String())
	}
}

func newTestRouter(t *testing.T, upstream *httptest.Server) *Router {
	t.Helper()
	return newTestRouterWithMaxBody(t, upstream, 16*1024*1024)
}

func newTestRouterWithMaxBody(t *testing.T, upstream *httptest.Server, maxBody int64) *Router {
	t.Helper()
	reg := mesh.New(mesh.Options{
		Seeds:          []string{addrOf(upstream)},
		Timeout:        time.Second,
		DiscoveryEvery: 1,
	})
	// Drive the registry with a stub node rather than a real poll: this test
	// is about routing, not discovery.
	reg.SetSnapshotForTest(mesh.BuildSnapshot(map[string]*mesh.Node{
		addrOf(upstream): {
			Addr:            addrOf(upstream),
			Models:          []string{"gemma"},
			Healthy:         true,
			HealthyBackends: 1,
			InFlight:        0,
			InFlightKnown:   true,
			FullUI:          true,
			// Seed: true — this stub node is meant to be the view node (see
			// TestRouterStickyPathsGoToViewNode); view election (F3) now
			// restricts eligibility to Seed nodes.
			Seed: true,
		},
	}, ""))
	return NewRouter(reg, NewForwarder(nil), maxBody, highTestMaxInFlight, 30*time.Second, nil, nil)
}

// newTestRouterWithBodyTimeout is like newTestRouter but lets the test pick
// a short body-read deadline, to exercise R2/H5 without waiting out the
// production default.
func newTestRouterWithBodyTimeout(t *testing.T, upstream *httptest.Server, bodyReadTimeout time.Duration) *Router {
	t.Helper()
	reg := mesh.New(mesh.Options{
		Seeds:          []string{addrOf(upstream)},
		Timeout:        time.Second,
		DiscoveryEvery: 1,
	})
	reg.SetSnapshotForTest(mesh.BuildSnapshot(map[string]*mesh.Node{
		addrOf(upstream): {
			Addr:            addrOf(upstream),
			Models:          []string{"gemma"},
			Healthy:         true,
			HealthyBackends: 1,
			InFlight:        0,
			InFlightKnown:   true,
			FullUI:          true,
			Seed:            true,
		},
	}, ""))
	return NewRouter(reg, NewForwarder(nil), 16*1024*1024, highTestMaxInFlight, bodyReadTimeout, nil, nil)
}

func newEmptyRouter(t *testing.T) *Router {
	t.Helper()
	reg := mesh.New(mesh.Options{Seeds: []string{"127.0.0.1:1"}, Timeout: time.Second})
	return NewRouter(reg, NewForwarder(nil), 16*1024*1024, highTestMaxInFlight, 30*time.Second, nil, nil)
}

// highTestMaxInFlight is used by tests that are not exercising the
// in-flight cap itself, so pre-existing routing assertions keep behaving as
// if there were no cap at all.
const highTestMaxInFlight = 10000
