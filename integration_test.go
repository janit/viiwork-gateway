//go:build integration

package main_test

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/janit/viiwork-gateway/internal/accesslog"
	"github.com/janit/viiwork-gateway/internal/auth"
	"github.com/janit/viiwork-gateway/internal/keys"
	"github.com/janit/viiwork-gateway/internal/mesh"
	"github.com/janit/viiwork-gateway/internal/proxy"
	"github.com/janit/viiwork/meshapi"
)

// syncBuffer is a bytes.Buffer safe for the concurrent writes an
// slog.Handler can make from more than one in-flight request.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

const apiKey = "integration-key-integration-key"

// deadAddr is a peer address that is advertised by a node but never answers:
// nothing listens on port 1, so a dial to it is refused immediately rather
// than timing out. It exists to prove the hearsay rule: a peer's report of
// another node's existence must never make that node routable, or elect it as
// the view, until it answers its own status poll.
const deadAddr = "127.0.0.1:1"

// fakeMeshNode is a minimal node answering the meshapi contract. serveChat
// distinguishes a viiwork node from a viiwork-nvidia one.
type fakeMeshNode struct {
	srv       *httptest.Server
	models    []string
	serveChat bool
	peers     []string
	served    chan string
	powerHit  chan string
}

func newMeshNode(t *testing.T, models []string, serveChat bool) *fakeMeshNode {
	t.Helper()
	n := &fakeMeshNode{
		models:    models,
		serveChat: serveChat,
		served:    make(chan string, 64),
		powerHit:  make(chan string, 8),
	}

	mux := http.NewServeMux()
	mux.HandleFunc(meshapi.PathStatus, func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(meshapi.StatusResponse{
			NodeID:          "node-" + models[0],
			Hostname:        "host-" + models[0],
			Models:          models,
			TotalInFlight:   0,
			HealthyBackends: 1,
			TotalBackends:   1,
		})
	})
	mux.HandleFunc(meshapi.PathCluster, func(w http.ResponseWriter, r *http.Request) {
		peers := make([]meshapi.ClusterPeerInfo, 0, len(n.peers))
		for _, addr := range n.peers {
			peers = append(peers, meshapi.ClusterPeerInfo{Addr: addr, Status: meshapi.StatusHealthy})
		}
		_ = json.NewEncoder(w).Encode(meshapi.ClusterResponse{NodeID: "node", Peers: peers})
	})
	mux.HandleFunc(meshapi.PathChatCompletions, func(w http.ResponseWriter, r *http.Request) {
		n.served <- models[0]
		if r.Header.Get("Authorization") != "" || r.Header.Get("Cookie") != "" {
			http.Error(w, "gateway leaked a credential to the mesh", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"hi"}}]}`))
	})
	mux.HandleFunc("/mesh", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte("<html>mesh dashboard</html>"))
	})
	// Chassis power control must never reach a mesh node: the gateway
	// refuses it outright. These handlers exist only so a regression that
	// forwards it anyway is caught by a recorded hit, not masked by a
	// coincidental 404 from an unregistered path.
	mux.HandleFunc(meshapi.PathPower, func(w http.ResponseWriter, r *http.Request) {
		n.powerHit <- meshapi.PathPower
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc(meshapi.PathMeshPower, func(w http.ResponseWriter, r *http.Request) {
		n.powerHit <- meshapi.PathMeshPower
		w.WriteHeader(http.StatusOK)
	})
	if serveChat {
		mux.HandleFunc("/chat", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/html")
			_, _ = w.Write([]byte("<html>chat</html>"))
		})
	}

	n.srv = httptest.NewServer(mux)
	t.Cleanup(n.srv.Close)
	return n
}

func (n *fakeMeshNode) addr() string { return strings.TrimPrefix(n.srv.URL, "http://") }

func TestGatewayEndToEnd(t *testing.T) {
	// A two-node heterogeneous mesh: one viiwork node and one nvidia-style
	// node that serves no /chat. Both are seeded: view election (F3) now
	// restricts eligibility to Seed nodes, so a viiwork reachable only by
	// discovery could never be elected regardless of FullUI, which is not
	// what this subtest is about — the model catalogue and gemma-routing
	// subtests below still exercise discovery/transitive-peer behaviour via
	// llama/nvidia's advertised dead peer.
	//
	// nvidia also advertises a dead peer alongside viiwork, giving it a
	// strictly higher PeerCount (2) than viiwork's (1). That is deliberate:
	// with FullUI honoured, viiwork still wins view election because it is
	// the only node in the FullUI-requiring tier. But if a regression ever
	// made election ignore FullUI, nvidia's higher PeerCount would win
	// outright — a real, deterministic failure, not a coin flip on whichever
	// node happens to get the lower ephemeral port from the OS.
	viiwork := newMeshNode(t, []string{"gemma"}, true)
	nvidia := newMeshNode(t, []string{"llama"}, false)
	nvidia.peers = []string{viiwork.addr(), deadAddr}
	viiwork.peers = []string{nvidia.addr()}

	set, err := keys.Load([]string{"VIIWORK_KEY_integration=" + apiKey})
	if err != nil {
		t.Fatalf("keys.Load: %v", err)
	}

	reg := mesh.New(mesh.Options{
		Seeds:          []string{nvidia.addr(), viiwork.addr()},
		Timeout:        2 * time.Second,
		DiscoveryEvery: 1,
	})
	for i := 0; i < 3; i++ {
		reg.Round(t.Context())
	}

	// logBuf captures the access log so the "routes to the node owning the
	// model" subtest below can assert on it directly, rather than trusting
	// that a model and node were merely computed somewhere.
	var logBuf syncBuffer
	logger := slog.New(slog.NewJSONHandler(&logBuf, nil))

	mw := &auth.Middleware{Keys: set, TTL: time.Hour}
	handler := mw.Wrap(accesslog.Wrap(
		proxy.NewRouter(reg, proxy.NewForwarder(logger), 1<<20, 10000, 30*time.Second, nil, logger),
		logger))

	gw := httptest.NewServer(handler)
	defer gw.Close()

	get := func(path string, withKey bool) *http.Response {
		t.Helper()
		req, _ := http.NewRequest("GET", gw.URL+path, nil)
		if withKey {
			req.Header.Set("Authorization", "Bearer "+apiKey)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		return resp
	}

	// sessionCookie is captured by the bootstrap subtest below and reused by
	// the cookie-authenticated inference subtest, so the subtests must run
	// in this order (t.Run executes them sequentially, not in parallel).
	var sessionCookie *http.Cookie

	t.Run("unauthenticated is refused", func(t *testing.T) {
		resp := get("/v1/models", false)
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401", resp.StatusCode)
		}
	})

	t.Run("catalogue is the union of a discovered mesh", func(t *testing.T) {
		resp := get("/v1/models", true)
		defer resp.Body.Close()

		var list struct {
			Data []struct {
				ID string `json:"id"`
			} `json:"data"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&list); err != nil {
			t.Fatalf("decode: %v", err)
		}
		ids := map[string]bool{}
		for _, e := range list.Data {
			ids[e.ID] = true
		}
		if !ids["gemma"] || !ids["llama"] {
			t.Fatalf("models = %v, want both gemma and llama (both seeded)", ids)
		}
		if len(ids) != 2 {
			t.Fatalf("models = %v, want exactly {gemma, llama}: a dead peer must contribute no model", ids)
		}
	})

	t.Run("a peer-advertised node that never answers stays unroutable", func(t *testing.T) {
		if n := reg.Snapshot().Node(deadAddr); n != nil && n.Healthy {
			t.Fatalf("dead peer address %s became healthy despite never answering a status poll", deadAddr)
		}
		if view, _ := reg.ViewAddr(); view == deadAddr {
			t.Fatalf("dead peer address %s was elected view node", deadAddr)
		}
	})

	t.Run("browser bootstrap issues a cookie", func(t *testing.T) {
		client := &http.Client{
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		}
		resp, err := client.Get(gw.URL + "/mesh?key=" + apiKey)
		if err != nil {
			t.Fatalf("GET: %v", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusSeeOther {
			t.Fatalf("status = %d, want 303", resp.StatusCode)
		}
		cookies := resp.Cookies()
		if len(cookies) != 1 {
			t.Fatalf("got %d cookies, want 1", len(cookies))
		}
		if strings.Contains(resp.Header.Get("Location"), "key=") {
			t.Error("the redirect still carries the key")
		}
		sessionCookie = cookies[0]
	})

	t.Run("routes to the node owning the model", func(t *testing.T) {
		req, _ := http.NewRequest("POST", gw.URL+meshapi.PathChatCompletions,
			strings.NewReader(`{"model":"gemma","messages":[]}`))
		req.Header.Set("Authorization", "Bearer "+apiKey)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("POST: %v", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, want 200 (a 500 means a credential leaked upstream)", resp.StatusCode)
		}
		select {
		case which := <-viiwork.served:
			if which != "gemma" {
				t.Fatalf("served by %q, want the gemma owner", which)
			}
		case <-time.After(time.Second):
			t.Fatal("the gemma request never reached the node that owns gemma")
		}

		// The spec's stated reason for labelled keys is that the access log
		// shows which key sent how much traffic to which node serving which
		// model. Router computes the model and node several layers below
		// accesslog.Wrap; this is the seam a passing unit test in either
		// package alone would not catch — only an end-to-end request through
		// the real handler chain proves the value actually reaches the log
		// line, not just some context a downstream package never reads back.
		//
		// accesslog.Wrap logs after the handler chain returns, which — with
		// FlushInterval: -1 streaming response bytes to the client as they
		// arrive — can genuinely land a moment after the client already has
		// its response, so this polls rather than checking once.
		deadline := time.Now().Add(time.Second)
		var lastLog string
		for {
			lastLog = logBuf.String()
			found := false
			var loggedNode string
			for _, line := range strings.Split(strings.TrimSpace(lastLog), "\n") {
				var entry struct {
					Path  string `json:"path"`
					Model string `json:"model"`
					Node  string `json:"node"`
				}
				if err := json.Unmarshal([]byte(line), &entry); err != nil {
					continue
				}
				if entry.Path == meshapi.PathChatCompletions && entry.Model == "gemma" {
					found = true
					loggedNode = entry.Node
					break
				}
			}
			if found {
				if loggedNode != viiwork.addr() {
					t.Fatalf("log line node = %q, want the gemma owner %s", loggedNode, viiwork.addr())
				}
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("no access log line recorded model=gemma for %s; log:\n%s",
					meshapi.PathChatCompletions, lastLog)
			}
			time.Sleep(5 * time.Millisecond)
		}
	})

	t.Run("routes to the node owning a model only the non-view node serves", func(t *testing.T) {
		// gemma's owner (viiwork) is also the elected view node, so a
		// regression that forwards every inference request to ViewAddr()
		// instead of the model's actual owner would still pass the gemma
		// case above. llama is owned only by nvidia, which is NOT the view
		// node, so this is the request shape that actually distinguishes
		// "routed by model ownership" from "routed to the dashboard node".
		req, _ := http.NewRequest("POST", gw.URL+meshapi.PathChatCompletions,
			strings.NewReader(`{"model":"llama","messages":[]}`))
		req.Header.Set("Authorization", "Bearer "+apiKey)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("POST: %v", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, want 200 (a 500 means a credential leaked upstream)", resp.StatusCode)
		}
		select {
		case which := <-nvidia.served:
			if which != "llama" {
				t.Fatalf("served by %q, want the llama owner", which)
			}
		case <-time.After(time.Second):
			t.Fatal("the llama request never reached the node that owns llama")
		}
	})

	t.Run("a cookie-authenticated request never leaks the cookie to the mesh", func(t *testing.T) {
		// stripCredentials deletes both Authorization and Cookie in one
		// place, but every other proxied request in this suite authenticates
		// with a bearer token, so a regression that stopped stripping ONLY
		// the Cookie header would otherwise sail through undetected. This
		// request carries a cookie and no Authorization header at all; the
		// fake node's 500-on-Cookie trap makes a 200 here proof the gateway
		// stripped it.
		if sessionCookie == nil {
			t.Fatal("no session cookie captured by the bootstrap subtest")
		}
		req, _ := http.NewRequest("POST", gw.URL+meshapi.PathChatCompletions,
			strings.NewReader(`{"model":"gemma","messages":[]}`))
		req.AddCookie(sessionCookie)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("POST: %v", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, want 200 (a 500 means the gateway's own cookie leaked upstream)", resp.StatusCode)
		}
		select {
		case which := <-viiwork.served:
			if which != "gemma" {
				t.Fatalf("served by %q, want the gemma owner", which)
			}
		case <-time.After(time.Second):
			t.Fatal("the cookie-authenticated request never reached the node that owns gemma")
		}
	})

	t.Run("dashboard goes to the node with the full UI", func(t *testing.T) {
		view, ok := reg.ViewAddr()
		if !ok {
			t.Fatal("no view node elected")
		}
		if view != viiwork.addr() {
			t.Fatalf("view = %s, want the full-surface node %s", view, viiwork.addr())
		}
		resp := get("/mesh", true)
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, want 200", resp.StatusCode)
		}
	})

	t.Run("chassis power is refused", func(t *testing.T) {
		req, _ := http.NewRequest("POST", gw.URL+meshapi.PathMeshPower, strings.NewReader("{}"))
		req.Header.Set("Authorization", "Bearer "+apiKey)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("POST: %v", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusForbidden {
			t.Fatalf("status = %d, want 403", resp.StatusCode)
		}
		// A 403 alone is only safe by accident: the fake nodes registered no
		// power handler before this fix would have turned a fall-through
		// into a 404, not a 403. Now that both nodes record a hit, prove no
		// node was ever contacted at all.
		select {
		case hit := <-viiwork.powerHit:
			t.Fatalf("viiwork node received a power request it should never have seen: %s", hit)
		default:
		}
		select {
		case hit := <-nvidia.powerHit:
			t.Fatalf("nvidia node received a power request it should never have seen: %s", hit)
		default:
		}
	})
}
