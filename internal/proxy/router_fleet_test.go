package proxy

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/janit/viiwork-gateway/internal/fleet"
	"github.com/janit/viiwork-gateway/internal/ratelimit"
)

func fleetRouter(fl Fleet) *Router {
	return NewRouter(fl, NewForwarder(nil), 16*1024*1024, highTestMaxInFlight, 30*time.Second, nil, nil)
}

// chatRequest is a POST to the inference path carrying a model, with a
// RequestInfo attached so the tests can read back what the access log would
// have recorded.
func chatRequest(body string) (*http.Request, *RequestInfo) {
	r := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body))
	info := &RequestInfo{}
	return r.WithContext(ContextWithRequestInfo(r.Context(), info)), info
}

// RT1: a picked node gets the request, and its reservation is given back.
func TestRouteByModelForwardsToPickedNode(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	fl := oneNodeFleet("node-a", addrOf(upstream))
	rt := fleetRouter(fl)

	req, info := chatRequest(`{"model":"gemma"}`)
	rec := httptest.NewRecorder()
	rt.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if fl.releaseCount() != 1 {
		t.Errorf("releases = %d, want 1", fl.releaseCount())
	}
	if info.Node != "node-a" || info.Model != "gemma" {
		t.Errorf("access log would record node=%q model=%q, want node-a/gemma", info.Node, info.Model)
	}
}

// RT2: a node that has left between two capacity polls costs one retry, not
// the request. The second pick must exclude the node already tried.
func TestRouteByModelRetriesOnAnotherNode(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	fl := &fakeFleet{targets: []fleet.Target{
		{Node: "node-a", APIAddr: closedPort(t)},
		{Node: "node-b", APIAddr: addrOf(upstream)},
	}}
	rt := fleetRouter(fl)

	req, info := chatRequest(`{"model":"gemma"}`)
	rec := httptest.NewRecorder()
	rt.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 from the retry", rec.Code)
	}
	if fl.pickCount() != 2 {
		t.Fatalf("Pick was called %d times, want 2", fl.pickCount())
	}
	if ex := fl.excludeAt(1); !ex["node-a"] {
		t.Errorf("the retry's exclude was %v, want node-a excluded", ex)
	}
	if fl.releaseCount() != 2 {
		t.Errorf("releases = %d, want 2: both reservations must be given back", fl.releaseCount())
	}
	if info.Node != "node-b" {
		t.Errorf("access log would record node=%q, want node-b", info.Node)
	}
}

// RT3: when the retry is spent too, the client is told — naming the last node
// tried, not the first.
func TestRouteByModelBothUnreachableYields502(t *testing.T) {
	fl := &fakeFleet{targets: []fleet.Target{
		{Node: "node-a", APIAddr: closedPort(t)},
		{Node: "node-b", APIAddr: closedPort(t)},
	}}
	rt := fleetRouter(fl)

	req, _ := chatRequest(`{"model":"gemma"}`)
	rec := httptest.NewRecorder()
	rt.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "node-b") {
		t.Errorf("body %q should name node-b, the last node tried", rec.Body.String())
	}
	if fl.releaseCount() != 2 {
		t.Errorf("releases = %d, want 2", fl.releaseCount())
	}
}

// RT4: a node's own 503 is its answer, not a transport failure. It passes
// through untouched, and no second node is tried — the node already knows
// about its own queue and spill.
func TestRouteByModelDoesNotRetryANodesOwn503(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer upstream.Close()

	fl := &fakeFleet{targets: []fleet.Target{
		{Node: "node-a", APIAddr: addrOf(upstream)},
		{Node: "node-b", APIAddr: addrOf(upstream)},
	}}
	rt := fleetRouter(fl)

	req, _ := chatRequest(`{"model":"gemma"}`)
	rec := httptest.NewRecorder()
	rt.ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want the node's own 503", rec.Code)
	}
	if fl.pickCount() != 1 {
		t.Errorf("Pick was called %d times, want 1: a node's own 503 is an answer", fl.pickCount())
	}
}

// RT5: a model no node reports — an alias, a pipeline, a model still loading —
// goes to the view node, which resolves it or answers 404. No reservation is
// taken, so none is released.
func TestRouteByModelUnknownModelGoesToTheViewNode(t *testing.T) {
	var gotPath string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	fl := viewOnlyFleet("node-v", addrOf(upstream))
	rt := fleetRouter(fl)

	req, info := chatRequest(`{"model":"stable-prose"}`)
	rec := httptest.NewRecorder()
	rt.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if gotPath != "/v1/chat/completions" {
		t.Errorf("upstream saw path %q", gotPath)
	}
	if fl.releaseCount() != 0 {
		t.Errorf("releases = %d, want 0: no node was reserved", fl.releaseCount())
	}
	if info.Node != "node-v" {
		t.Errorf("access log would record node=%q, want node-v", info.Node)
	}
}

// RT6: nothing routable and no view node is the only real 503 the gateway
// writes for itself.
func TestRouteByModelNoNodeAtAllYields503(t *testing.T) {
	rt := fleetRouter(emptyFleet())

	req, _ := chatRequest(`{"model":"gemma"}`)
	rec := httptest.NewRecorder()
	rt.ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "no_healthy_node") {
		t.Errorf("body %q should carry no_healthy_node", rec.Body.String())
	}
}

// RT7: ?host= is a node rule, not a gateway one. It must reach the node
// unread and unaltered.
func TestRouteByModelPassesHostPinThrough(t *testing.T) {
	var gotQuery string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	rt := fleetRouter(oneNodeFleet("node-a", addrOf(upstream)))

	r := httptest.NewRequest("POST", "/v1/chat/completions?host=node-b",
		strings.NewReader(`{"model":"gemma"}`))
	rt.ServeHTTP(httptest.NewRecorder(), r)

	if gotQuery != "host=node-b" {
		t.Errorf("upstream saw query %q, want host=node-b", gotQuery)
	}
}

// RT8: the retry must send the body the client actually posted, not the
// remains of a consumed reader.
func TestRouteByModelRetrySendsTheWholeBody(t *testing.T) {
	const body = `{"model":"gemma","prompt":"the quick brown fox jumps over the lazy dog"}`

	var gotBody string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	fl := &fakeFleet{targets: []fleet.Target{
		{Node: "node-a", APIAddr: closedPort(t)},
		{Node: "node-b", APIAddr: addrOf(upstream)},
	}}
	rt := fleetRouter(fl)

	req, _ := chatRequest(body)
	rec := httptest.NewRecorder()
	rt.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if gotBody != body {
		t.Errorf("the retry sent %q, want the full original body %q", gotBody, body)
	}
}

// AL1/AL2: every alias write is refused, on a normalised path, whatever the
// verb. A write here would repoint every node's traffic for a model name from
// outside the tailnet.
func TestAliasWritesAreRefused(t *testing.T) {
	var upstreamHits int
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamHits++
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	rt := fleetRouter(oneNodeFleet("node-a", addrOf(upstream)))

	cases := []struct{ method, path string }{
		{"PUT", "/v1/aliases/stable-coder"},
		{"DELETE", "/v1/aliases/x"},
		{"POST", "/v1/aliases/x/revert"},
		{"POST", "//v1/aliases/x/revert"},
		{"PUT", "/V1/ALIASES/x/"},
		{"PATCH", "/v1/aliases"},
		{"POST", "/v1/./aliases/x"},
		{"POST", "/v1/y/../aliases/x"},
	}
	for _, c := range cases {
		t.Run(c.method+" "+c.path, func(t *testing.T) {
			rec := httptest.NewRecorder()
			rt.ServeHTTP(rec, httptest.NewRequest(c.method, c.path, strings.NewReader(`{}`)))
			if rec.Code != http.StatusForbidden {
				t.Errorf("status = %d, want 403", rec.Code)
			}
			if !strings.Contains(rec.Body.String(), "forbidden_path") {
				t.Errorf("body %q should carry forbidden_path", rec.Body.String())
			}
		})
	}
	if upstreamHits != 0 {
		t.Errorf("%d refused alias writes still reached a node", upstreamHits)
	}
}

// AL3: reading the alias table is exactly what the dashboards do, and stays
// available.
func TestAliasReadsAreForwarded(t *testing.T) {
	var gotPaths []string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPaths = append(gotPaths, r.URL.Path)
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	rt := fleetRouter(oneNodeFleet("node-a", addrOf(upstream)))

	for _, c := range []struct{ method, path string }{
		{"GET", "/v1/aliases"},
		{"GET", "/v1/aliases?table=1"},
		{"HEAD", "/v1/aliases"},
	} {
		rec := httptest.NewRecorder()
		rt.ServeHTTP(rec, httptest.NewRequest(c.method, c.path, nil))
		if rec.Code != http.StatusOK {
			t.Errorf("%s %s: status = %d, want 200", c.method, c.path, rec.Code)
		}
	}
	if len(gotPaths) != 3 {
		t.Errorf("upstream saw %d requests, want 3: %v", len(gotPaths), gotPaths)
	}
}

// AL4: the deny is a path match, not a substring one. A different path that
// merely starts with the same letters is not an alias write.
func TestAliasDenyDoesNotOverreach(t *testing.T) {
	var hit bool
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hit = true
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	rt := fleetRouter(oneNodeFleet("node-a", addrOf(upstream)))

	rec := httptest.NewRecorder()
	rt.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/aliasesx", strings.NewReader(`{}`)))
	if rec.Code == http.StatusForbidden {
		t.Fatal("/v1/aliasesx was denied; the alias deny must not match on a prefix of a path segment")
	}
	if !hit {
		t.Error("/v1/aliasesx never reached a node")
	}
}

// MD1's other half: the catalogue is not metered. A dashboard polling it must
// not spend the key's inference allowance.
func TestModelsCatalogueIsNotRateLimited(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	// An allowance of one request per minute, already spent by an inference
	// call: the catalogue must still answer.
	rt := NewRouter(oneNodeFleet("node-a", addrOf(upstream)), NewForwarder(nil),
		16*1024*1024, highTestMaxInFlight, 30*time.Second, ratelimit.New(60, 1), nil)

	rt.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"gemma"}`)))

	for i := range 5 {
		rec := httptest.NewRecorder()
		rt.ServeHTTP(rec, httptest.NewRequest("GET", "/v1/models", nil))
		if rec.Code == http.StatusTooManyRequests {
			t.Fatalf("/v1/models was rate limited on request %d", i+1)
		}
	}
}

// waitForReleases polls until the fleet has had want reservations given back.
// The handler runs on the server's goroutine, so the client returning is not
// proof that the handler has.
func waitForReleases(t *testing.T, fl *fakeFleet, want int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for fl.releaseCount() < want && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if got := fl.releaseCount(); got != want {
		t.Errorf("releases = %d, want %d: a reservation was leaked", got, want)
	}
}

// streamThenDie sends one SSE event and then drops the connection, the way a
// node that crashes mid-generation does.
func streamThenDie() *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("data: tok\n\n"))
		w.(http.Flusher).Flush()
		conn, _, err := w.(http.Hijacker).Hijack()
		if err == nil {
			_ = conn.Close()
		}
	}))
}

func postChat(t *testing.T, gw *httptest.Server) {
	t.Helper()
	resp, err := http.Post(gw.URL+"/v1/chat/completions", "application/json",
		strings.NewReader(`{"model":"gemma","stream":true}`))
	if err != nil {
		return
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
}

// RT9: once a stream has started, a failure makes ReverseProxy abort the
// handler by panicking with http.ErrAbortHandler — only under a real
// http.Server, which is why these tests use one and not a ResponseRecorder.
// The reservation must still be given back, or Picker.held grows by one entry
// per aborted stream, for ever.
func TestRouteByModelReleasesWhenUpstreamDiesMidStream(t *testing.T) {
	upstream := streamThenDie()
	defer upstream.Close()

	fl := oneNodeFleet("node-a", addrOf(upstream))
	gw := httptest.NewServer(fleetRouter(fl))
	defer gw.Close()

	postChat(t, gw)
	waitForReleases(t, fl, 1)
}

// RT10: the same for a client that hangs up mid-stream — a "stop generating"
// button, the commonest way a stream ends early.
func TestRouteByModelReleasesWhenClientHangsUpMidStream(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		for {
			if _, err := w.Write([]byte("data: tok\n\n")); err != nil {
				return
			}
			w.(http.Flusher).Flush()
			select {
			case <-r.Context().Done():
				return
			case <-time.After(10 * time.Millisecond):
			}
		}
	}))
	defer upstream.Close()

	fl := oneNodeFleet("node-a", addrOf(upstream))
	gw := httptest.NewServer(fleetRouter(fl))
	defer gw.Close()

	resp, err := http.Post(gw.URL+"/v1/chat/completions", "application/json",
		strings.NewReader(`{"model":"gemma","stream":true}`))
	if err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 1)
	if _, err := resp.Body.Read(buf); err != nil {
		t.Fatalf("no first byte: %v", err)
	}
	_ = resp.Body.Close()

	waitForReleases(t, fl, 1)
}

// RT11: the retry's reservation is released on the same path.
func TestRouteByModelReleasesRetryWhenItDiesMidStream(t *testing.T) {
	upstream := streamThenDie()
	defer upstream.Close()

	fl := &fakeFleet{targets: []fleet.Target{
		{Node: "node-a", APIAddr: closedPort(t)},
		{Node: "node-b", APIAddr: addrOf(upstream)},
	}}
	gw := httptest.NewServer(fleetRouter(fl))
	defer gw.Close()

	postChat(t, gw)
	waitForReleases(t, fl, 2)
}
