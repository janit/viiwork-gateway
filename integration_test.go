//go:build integration

// End-to-end over a real viiwork 2 mesh: a gateway that has actually joined,
// real fake nodes answering real HTTP, and the whole request pipeline in front
// of it — authentication, the access log, and the router.
package main_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hashicorp/memberlist"
	"github.com/janit/viiwork-gateway/internal/accesslog"
	"github.com/janit/viiwork-gateway/internal/auth"
	"github.com/janit/viiwork-gateway/internal/config"
	"github.com/janit/viiwork-gateway/internal/fleet"
	"github.com/janit/viiwork-gateway/internal/keys"
	"github.com/janit/viiwork-gateway/internal/proxy"
	"github.com/janit/viiwork/v2/mesh"
	"github.com/janit/viiwork/v2/mesh/meshtest"
	"github.com/janit/viiwork/v2/meshapi"
)

const testKey = "integration-test-key-at-least-24-chars"

var loopback = netip.MustParseAddr("127.0.0.1")

func fastTimings(tr *meshtest.Transport) func(*memberlist.Config) {
	return func(c *memberlist.Config) {
		c.Transport = tr
		c.ProbeInterval = 200 * time.Millisecond
		c.ProbeTimeout = 100 * time.Millisecond
		c.SuspicionMult = 2
		c.GossipInterval = 50 * time.Millisecond
		c.PushPullInterval = 500 * time.Millisecond
		c.TCPTimeout = 500 * time.Millisecond
		c.DeadNodeReclaimTime = time.Second
	}
}

// node is a viiwork 2 node: a mesh member plus the API surface the gateway
// actually reaches through.
type node struct {
	name    string
	mesh    *mesh.Mesh
	srv     *httptest.Server
	apiPort int

	mu        sync.Mutex
	models    []meshapi.ModelCapacity
	chatHits  int
	powerHits int
	hold      time.Duration
	credLeaks []string
}

func (n *node) setModels(m ...meshapi.ModelCapacity) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.models = m
}

func (n *node) setHold(d time.Duration) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.hold = d
}

func (n *node) counts() (chat, power int) {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.chatHits, n.powerHits
}

func (n *node) leaks() []string {
	n.mu.Lock()
	defer n.mu.Unlock()
	return append([]string(nil), n.credLeaks...)
}

// noteCredentials records anything that should never have left the gateway.
// The gateway strips credentials before forwarding, so a node's logs and
// prompt history never carry a gateway key.
func (n *node) noteCredentials(r *http.Request) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if v := r.Header.Get("Authorization"); v != "" {
		n.credLeaks = append(n.credLeaks, "Authorization: "+v)
	}
	if v := r.Header.Get("Cookie"); v != "" {
		n.credLeaks = append(n.credLeaks, "Cookie: "+v)
	}
}

func model(name string, slots, busy int) meshapi.ModelCapacity {
	return meshapi.ModelCapacity{
		Name: name, Engine: "llamacpp", Slots: slots, Busy: busy,
		Backends: 1, HealthyBackends: 1,
	}
}

func (n *node) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n.noteCredentials(r)
		switch {
		case r.URL.Path == meshapi.PathCapacity:
			n.mu.Lock()
			models := append([]meshapi.ModelCapacity(nil), n.models...)
			n.mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(meshapi.CapacityResponse{
				Node: n.name, Ver: "v2.0.0-beta1", Models: models,
			})

		case r.URL.Path == meshapi.PathChatCompletions:
			n.mu.Lock()
			n.chatHits++
			hold := n.hold
			n.mu.Unlock()
			if hold > 0 {
				time.Sleep(hold)
			}
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `{"node":%q,"object":"chat.completion"}`, n.name)

		case r.URL.Path == meshapi.PathPower || r.URL.Path == meshapi.PathMeshPower:
			n.mu.Lock()
			n.powerHits++
			n.mu.Unlock()
			w.WriteHeader(http.StatusOK)

		case r.URL.Path == meshapi.PathModels:
			fmt.Fprintf(w, `{"object":"list","data":[{"id":"m","owned_by":"local"}],"served_by":%q}`, n.name)

		case r.URL.Path == meshapi.PathAliases:
			fmt.Fprintf(w, `{"aliases":[{"name":"stable-coder","target":"m"}],"served_by":%q}`, n.name)

		case r.URL.Path == "/mesh":
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			fmt.Fprintf(w, `<!doctype html><title>mesh</title><p>served by %s</p>`, n.name)

		default:
			http.NotFound(w, r)
		}
	})
}

func startNode(t *testing.T, nw *meshtest.Network, name string, gossipPort int, tune func(*mesh.Options)) *node {
	t.Helper()
	n := &node{name: name}
	n.srv = httptest.NewServer(n.handler())
	t.Cleanup(n.srv.Close)

	_, portStr, err := net.SplitHostPort(n.srv.Listener.Addr().String())
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	n.apiPort, _ = strconv.Atoi(portStr)

	tr := nw.TransportAt(name, netip.AddrPortFrom(loopback, uint16(gossipPort)))
	o := mesh.Options{
		Name: name, Network: mesh.NetworkTailnet, BindPort: gossipPort,
		Advertise: loopback, APIPort: n.apiPort, Role: meshapi.RoleNode,
		Version: "v2.0.0-beta1", Enforce: mesh.EnforceFull,
		RejoinInterval: 300 * time.Millisecond,
		AddrCheck:      func(netip.Addr) error { return nil },
		Tune:           fastTimings(tr),
	}
	if tune != nil {
		tune(&o)
	}
	m, err := mesh.Start(context.Background(), o)
	if err != nil {
		t.Fatalf("start node %s: %v", name, err)
	}
	n.mesh = m
	t.Cleanup(func() { _ = m.Shutdown() })
	return n
}

// gateway is the real thing: a Fleet that has joined, behind the real
// authentication and access-log middleware.
type gateway struct {
	fleet *fleet.Fleet
	srv   *httptest.Server
}

func (g *gateway) do(t *testing.T, method, path, body string, withKey bool) *http.Response {
	t.Helper()
	var rdr io.Reader
	if body != "" {
		rdr = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, g.srv.URL+path, rdr)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	if withKey {
		req.Header.Set("Authorization", "Bearer "+testKey)
	}
	resp, err := g.srv.Client().Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	return resp
}

func (g *gateway) body(t *testing.T, resp *http.Response) string {
	t.Helper()
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return string(b)
}

func startGateway(t *testing.T, nw *meshtest.Network, gossipPort int, seeds []string, tweak func(*config.Config)) *gateway {
	t.Helper()

	set, err := keys.Load([]string{"VIIWORK_KEY_test=" + testKey})
	if err != nil {
		t.Fatalf("keys: %v", err)
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	listenAddr := ln.Addr().String()
	_ = ln.Close()

	cfg := config.Config{
		Listen:   listenAddr,
		NodeName: "gw",
		Mesh: config.MeshConfig{
			Network: "tailnet", BindPort: gossipPort, Advertise: loopback,
			Open: true, Enforce: "full", Seeds: seeds,
			RejoinInterval: 300 * time.Millisecond,
			CapacityPoll:   200 * time.Millisecond,
			StaleAfter:     time.Second,
		},
		CookieTTL: time.Hour, MaxBody: 1 << 20,
		MaxInFlight: 10000, BodyReadTimeout: 30 * time.Second,
	}
	if tweak != nil {
		tweak(&cfg)
	}

	tr := nw.TransportAt("gw", netip.AddrPortFrom(loopback, uint16(gossipPort)))
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	fl, err := fleet.Start(context.Background(), fleet.Options{
		Config: cfg, Version: "test", Log: io.Discard,
		Logf: func(string, ...any) {},
		MeshTune: func(o *mesh.Options) {
			o.AddrCheck = func(netip.Addr) error { return nil }
			o.Tune = fastTimings(tr)
			o.Feeders = []mesh.Feeder{mesh.SeedFeeder(cfg.Mesh.Seeds)}
		},
	})
	if err != nil {
		t.Fatalf("start gateway: %v", err)
	}
	t.Cleanup(func() { _ = fl.Close(time.Second) })

	mw := &auth.Middleware{Keys: set, TTL: cfg.CookieTTL}
	handler := mw.Wrap(accesslog.Wrap(
		proxy.NewRouter(fl, proxy.NewForwarder(logger), cfg.MaxBody, cfg.MaxInFlight,
			cfg.BodyReadTimeout, nil, logger),
		logger))

	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return &gateway{fleet: fl, srv: srv}
}

func within(t *testing.T, d time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out after %v waiting for %s", d, what)
}

// twoNodes is the standard fixture: node-a with free slots, node-b without,
// and a gateway that has joined and seen both.
func twoNodes(t *testing.T, tweak func(*config.Config)) (*meshtest.Network, *node, *node, *gateway) {
	t.Helper()
	return twoNodesWith(t, model("m", 2, 0), model("m", 2, 2), tweak)
}

// twoNodesWith sets each node's capacity BEFORE the gateway starts polling, so
// a test never races the first capacity poll: by the time the gateway can pick
// a node at all, the numbers it is picking on are the ones the test asked for.
func twoNodesWith(t *testing.T, aModel, bModel meshapi.ModelCapacity, tweak func(*config.Config)) (*meshtest.Network, *node, *node, *gateway) {
	t.Helper()
	nw := meshtest.NewNetwork()
	a := startNode(t, nw, "node-a", 18946, nil)
	b := startNode(t, nw, "node-b", 18947, func(o *mesh.Options) {
		o.Seeds = []string{nw.Addr("node-a").String()}
	})
	a.setModels(aModel)
	b.setModels(bModel)

	gw := startGateway(t, nw, 18948, []string{
		nw.Addr("node-a").String(), nw.Addr("node-b").String(),
	}, tweak)
	within(t, 5*time.Second, "the gateway to see both nodes", func() bool {
		return gw.fleet.NumAlive() == 3
	})
	within(t, 3*time.Second, "a routable model", func() bool {
		_, release, ok := gw.fleet.Pick("m", nil)
		if ok {
			release()
		}
		return ok
	})
	return nw, a, b, gw
}

// I1: the whole surface is behind a key, including the paths that would
// otherwise be harmless.
func TestIntegrationEveryPathNeedsAKey(t *testing.T) {
	_, a, b, gw := twoNodes(t, nil)

	for _, path := range []string{"/", "/mesh", "/v1/models", "/v1/aliases", "/v1/chat/completions"} {
		resp := gw.do(t, "GET", path, "", false)
		resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("%s: status = %d, want 401", path, resp.StatusCode)
		}
	}
	if chatA, _ := a.counts(); chatA != 0 {
		t.Errorf("node-a was reached %d times by unauthenticated requests", chatA)
	}
	if chatB, _ := b.counts(); chatB != 0 {
		t.Errorf("node-b was reached %d times by unauthenticated requests", chatB)
	}
}

// I2: the request lands on the node with free slots, and the gateway's own
// credential does not travel with it.
func TestIntegrationRoutesToTheFreestNode(t *testing.T) {
	_, a, b, gw := twoNodes(t, nil)

	resp := gw.do(t, "POST", "/v1/chat/completions", `{"model":"m"}`, true)
	body := gw.body(t, resp)

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%s)", resp.StatusCode, body)
	}
	if !strings.Contains(body, "node-a") {
		t.Errorf("answered by %q, want node-a", body)
	}
	chatA, _ := a.counts()
	chatB, _ := b.counts()
	if chatA != 1 || chatB != 0 {
		t.Errorf("chat hits: node-a %d, node-b %d; want 1 and 0", chatA, chatB)
	}
	if leaks := a.leaks(); len(leaks) != 0 {
		t.Errorf("credentials reached node-a: %v", leaks)
	}
}

// I3: a burst spreads. Capacity reports are a snapshot, so without local
// reservations every request in a burst would be sent at the same free slot.
func TestIntegrationBurstSpreadsAcrossNodes(t *testing.T) {
	// Both nodes report two free slots from the start, so the burst is
	// measured against the numbers this test intends rather than whatever the
	// first capacity poll happened to catch.
	_, a, b, gw := twoNodesWith(t, model("m", 2, 0), model("m", 2, 0), nil)
	a.setHold(200 * time.Millisecond)
	b.setHold(200 * time.Millisecond)

	// node-b must have been polled at least once, or node-a is the only
	// candidate and the spread has nothing to spread over.
	within(t, 3*time.Second, "node-b to have reported", func() bool {
		target, release, ok := gw.fleet.Pick("m", map[string]bool{"node-a": true})
		if ok {
			release()
		}
		return ok && target.Node == "node-b"
	})

	var wg sync.WaitGroup
	for range 6 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			resp := gw.do(t, "POST", "/v1/chat/completions", `{"model":"m"}`, true)
			resp.Body.Close()
		}()
	}
	wg.Wait()

	chatA, _ := a.counts()
	chatB, _ := b.counts()
	if chatA+chatB != 6 {
		t.Fatalf("nodes saw %d requests in total, want 6", chatA+chatB)
	}
	if chatA < 2 || chatB < 2 {
		t.Errorf("burst landed %d on node-a and %d on node-b; each should take "+
			"at least 2 of 6 when both report two free slots", chatA, chatB)
	}
}

// I4: a node that vanishes stops receiving traffic once its report goes stale.
func TestIntegrationRoutesAwayFromAVanishedNode(t *testing.T) {
	nw, a, b, gw := twoNodes(t, nil)
	b.setModels(model("m", 2, 0))

	nw.Unplug("node-a")
	within(t, 4*time.Second, "node-b to be the only candidate", func() bool {
		target, release, ok := gw.fleet.Pick("m", nil)
		if ok {
			release()
		}
		return ok && target.Node == "node-b"
	})

	beforeA, _ := a.counts()
	resp := gw.do(t, "POST", "/v1/chat/completions", `{"model":"m"}`, true)
	body := gw.body(t, resp)

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%s)", resp.StatusCode, body)
	}
	if !strings.Contains(body, "node-b") {
		t.Errorf("answered by %q, want node-b", body)
	}
	if afterA, _ := a.counts(); afterA != beforeA {
		t.Errorf("the unplugged node still received %d requests", afterA-beforeA)
	}
}

// I5: an alias is not in any capacity report. The gateway holds no alias
// table, so the view node resolves it.
func TestIntegrationUnknownModelGoesToTheViewNode(t *testing.T) {
	_, a, _, gw := twoNodes(t, func(c *config.Config) {
		c.ViewNodes = []string{"node-a"}
	})

	resp := gw.do(t, "POST", "/v1/chat/completions", `{"model":"stable-coder"}`, true)
	body := gw.body(t, resp)

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%s)", resp.StatusCode, body)
	}
	if !strings.Contains(body, "node-a") {
		t.Errorf("answered by %q, want the view node node-a", body)
	}
	if chatA, _ := a.counts(); chatA != 1 {
		t.Errorf("view node saw %d chat requests, want 1", chatA)
	}
}

// I6: alias writes are refused at the gateway, and never reach a node.
func TestIntegrationAliasWritesRefused(t *testing.T) {
	_, a, b, gw := twoNodes(t, nil)

	for _, c := range []struct{ method, path string }{
		{"PUT", "/v1/aliases/stable-coder"},
		{"POST", "/v1/aliases/stable-coder/revert"},
	} {
		resp := gw.do(t, c.method, c.path, `{"target":"m"}`, true)
		body := gw.body(t, resp)
		if resp.StatusCode != http.StatusForbidden {
			t.Errorf("%s %s: status = %d, want 403", c.method, c.path, resp.StatusCode)
		}
		if !strings.Contains(body, "forbidden_path") {
			t.Errorf("%s %s: body %q should carry forbidden_path", c.method, c.path, body)
		}
	}
	if len(a.leaks())+len(b.leaks()) != 0 {
		t.Error("a refused alias write still reached a node")
	}
}

// I7: reading the alias table is what the dashboards do, and it works.
func TestIntegrationAliasReadReachesTheViewNode(t *testing.T) {
	_, _, _, gw := twoNodes(t, func(c *config.Config) {
		c.ViewNodes = []string{"node-b"}
	})

	resp := gw.do(t, "GET", "/v1/aliases", "", true)
	body := gw.body(t, resp)

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if !strings.Contains(body, "stable-coder") || !strings.Contains(body, "node-b") {
		t.Errorf("body = %q, want the view node's alias table", body)
	}
}

// I8: chassis power stays a tailnet capability. Switching a machine off is
// the one thing an API key must never buy.
func TestIntegrationPowerPathsRefused(t *testing.T) {
	_, a, b, gw := twoNodes(t, nil)

	for _, path := range []string{"/v1/power", "/v1/mesh/power", "//v1/power", "/v1/./power"} {
		resp := gw.do(t, "POST", path, `{"action":"off"}`, true)
		resp.Body.Close()
		if resp.StatusCode != http.StatusForbidden {
			t.Errorf("%s: status = %d, want 403", path, resp.StatusCode)
		}
	}
	_, powerA := a.counts()
	_, powerB := b.counts()
	if powerA != 0 || powerB != 0 {
		t.Errorf("power endpoints were reached: node-a %d, node-b %d", powerA, powerB)
	}
}

// I9: the bare hostname serves the fleet-wide mesh view, with the gateway's
// own security headers over whatever the node sent.
func TestIntegrationLandingServesTheMeshView(t *testing.T) {
	_, _, _, gw := twoNodes(t, func(c *config.Config) {
		c.ViewNodes = []string{"node-a"}
	})

	resp := gw.do(t, "GET", "/", "", true)
	body := gw.body(t, resp)

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if !strings.Contains(body, "served by node-a") {
		t.Errorf("body = %q, want the node's /mesh page", body)
	}
	if got := resp.Header.Get("X-Content-Type-Options"); got != "nosniff" {
		t.Errorf("X-Content-Type-Options = %q, want nosniff", got)
	}
	if resp.Header.Get("Content-Security-Policy") == "" {
		t.Error("the mesh view was served without a Content-Security-Policy")
	}
}

// I10: in an open mesh the operator names the nodes allowed to serve
// dashboards, and that allowlist is authoritative — the view does not fall
// back to another node when the named one goes away.
func TestIntegrationViewAllowlistIsAuthoritative(t *testing.T) {
	nw, _, _, gw := twoNodes(t, func(c *config.Config) {
		c.ViewNodes = []string{"node-b"}
	})

	resp := gw.do(t, "GET", "/", "", true)
	body := gw.body(t, resp)
	if !strings.Contains(body, "served by node-b") {
		t.Fatalf("body = %q, want node-b although node-a sorts first", body)
	}

	nw.Unplug("node-b")
	within(t, 4*time.Second, "the view node to go away", func() bool {
		_, ok := gw.fleet.View()
		return !ok
	})

	resp = gw.do(t, "GET", "/", "", true)
	resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("/ with no eligible view node: status = %d, want 503", resp.StatusCode)
	}

	// Inference is unaffected: node-a still serves the model.
	resp = gw.do(t, "POST", "/v1/chat/completions", `{"model":"m"}`, true)
	body = gw.body(t, resp)
	if resp.StatusCode != http.StatusOK || !strings.Contains(body, "node-a") {
		t.Errorf("chat: status = %d body = %q, want 200 from node-a", resp.StatusCode, body)
	}
}

// I11: a secured gateway cannot join an open mesh, and says so honestly
// rather than routing into a mesh it never joined.
func TestIntegrationSecuredGatewayAgainstOpenNodes(t *testing.T) {
	nw := meshtest.NewNetwork()
	a := startNode(t, nw, "node-a", 18946, nil)
	a.setModels(model("m", 2, 0))

	gw := startGateway(t, nw, 18948, []string{nw.Addr("node-a").String()},
		func(c *config.Config) {
			c.Mesh.Open = false
			c.Mesh.SecretKey = make([]byte, 32)
		})

	time.Sleep(2 * time.Second)

	resp := gw.do(t, "POST", "/v1/chat/completions", `{"model":"m"}`, true)
	body := gw.body(t, resp)

	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 (%s)", resp.StatusCode, body)
	}
	if !strings.Contains(body, "no_healthy_node") {
		t.Errorf("body = %q, want no_healthy_node", body)
	}
	if chat, _ := a.counts(); chat != 0 {
		t.Errorf("the open node was reached %d times by a secured gateway", chat)
	}
}
