package mesh

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/janit/viiwork/meshapi"
)

// fakeNode is a stand-in viiwork node. serveChat is false for a
// viiwork-nvidia stand-in, which joins the same mesh but serves no /chat.
type fakeNode struct {
	srv       *httptest.Server
	status    atomic.Pointer[meshapi.StatusResponse]
	peers     atomic.Pointer[[]meshapi.ClusterPeerInfo]
	serveChat bool
	down      atomic.Bool
	statusHit atomic.Int64
}

func newFakeNode(t *testing.T, models []string, inFlight int64, serveChat bool) *fakeNode {
	t.Helper()
	f := &fakeNode{serveChat: serveChat}
	f.status.Store(&meshapi.StatusResponse{
		NodeID:          "node-" + models[0],
		Hostname:        "host-" + models[0],
		Models:          models,
		TotalInFlight:   inFlight,
		HealthyBackends: 1,
		TotalBackends:   1,
		Backends:        []meshapi.BackendInfo{{Status: meshapi.StatusHealthy, Model: models[0]}},
	})
	empty := []meshapi.ClusterPeerInfo{}
	f.peers.Store(&empty)

	mux := http.NewServeMux()
	mux.HandleFunc(meshapi.PathStatus, func(w http.ResponseWriter, r *http.Request) {
		f.statusHit.Add(1)
		if f.down.Load() {
			http.Error(w, "down", http.StatusServiceUnavailable)
			return
		}
		s := f.status.Load()
		s.ListenAddr = strings.TrimPrefix(f.srv.URL, "http://")
		_ = json.NewEncoder(w).Encode(s)
	})
	mux.HandleFunc(meshapi.PathCluster, func(w http.ResponseWriter, r *http.Request) {
		if f.down.Load() {
			http.Error(w, "down", http.StatusServiceUnavailable)
			return
		}
		s := f.status.Load()
		_ = json.NewEncoder(w).Encode(meshapi.ClusterResponse{
			NodeID: s.NodeID,
			Models: s.Models,
			Peers:  *f.peers.Load(),
		})
	})
	if serveChat {
		mux.HandleFunc("/chat", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/html")
			_, _ = w.Write([]byte("<html>chat</html>"))
		})
	}
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeNode) addr() string { return strings.TrimPrefix(f.srv.URL, "http://") }

func (f *fakeNode) advertise(peers ...*fakeNode) {
	list := make([]meshapi.ClusterPeerInfo, 0, len(peers))
	for _, p := range peers {
		s := p.status.Load()
		list = append(list, meshapi.ClusterPeerInfo{
			Addr:   p.addr(),
			Status: meshapi.StatusHealthy,
			Models: s.Models,
			// TotalInFlight deliberately omitted: it is omitempty on the wire
			// and the gateway must never rely on it.
		})
	}
	f.peers.Store(&list)
}

func testRegistry(t *testing.T, seeds ...string) *Registry {
	t.Helper()
	return New(Options{
		Seeds:          seeds,
		Timeout:        2 * time.Second,
		Interval:       10 * time.Millisecond,
		DiscoveryEvery: 1,
	})
}

// testRegistryPermissive is testRegistry with discovered-peer address
// validation (validPeerAddr) disabled. It exists for tests that exercise
// discovery mechanics (transitive discovery, round-robin among discovered
// peers, view election among discovered nodes) using httptest servers, which
// necessarily bind to loopback — an address validPeerAddr correctly and
// permanently rejects in production, opt-in or not. Tests that verify the
// validation itself (TestValidPeerAddr*, TestClusterRound*) must use
// testRegistry instead.
func testRegistryPermissive(t *testing.T, seeds ...string) *Registry {
	t.Helper()
	r := testRegistry(t, seeds...)
	// Set directly rather than through an exported method: this file is
	// package mesh, so the unexported field is reachable here, and nothing
	// outside the package (in particular, nothing in a normal production
	// build) needs a way to disable peer-address validation. See
	// testSkipPeerValidation's doc comment in registry.go.
	r.testSkipPeerValidation = true
	return r
}

func TestRoundPollsSeedAndIndexesModels(t *testing.T) {
	a := newFakeNode(t, []string{"gemma"}, 3, true)
	r := testRegistry(t, a.addr())

	r.Round(context.Background())

	snap := r.Snapshot()
	n := snap.Node(a.addr())
	if n == nil {
		t.Fatal("seed node missing from snapshot")
	}
	if !n.Healthy || !n.Routable() {
		t.Errorf("node not healthy: %+v", n)
	}
	if !n.InFlightKnown || n.InFlight != 3 {
		t.Errorf("InFlight = %d known=%v, want 3 true", n.InFlight, n.InFlightKnown)
	}
	if !n.FullUI {
		t.Error("node serving /chat should be marked FullUI")
	}
	got := snap.ModelIDs()
	if len(got) != 1 || got[0] != "gemma" {
		t.Errorf("ModelIDs = %v, want [gemma]", got)
	}
}

// Transitive discovery: the seed knows A, A knows B, B knows C. All must end
// up polled directly, without any of them being configured.
func TestTransitiveDiscovery(t *testing.T) {
	c := newFakeNode(t, []string{"granite"}, 0, true)
	b := newFakeNode(t, []string{"qwen"}, 0, true)
	a := newFakeNode(t, []string{"gemma"}, 0, true)
	a.advertise(b)
	b.advertise(c)

	r := testRegistryPermissive(t, a.addr())

	// One round per hop, plus one to status-poll the last discovery.
	for i := 0; i < 4; i++ {
		r.Round(context.Background())
	}

	snap := r.Snapshot()
	for _, want := range []*fakeNode{a, b, c} {
		n := snap.Node(want.addr())
		if n == nil {
			t.Fatalf("node %s was never discovered", want.addr())
		}
		if !n.Healthy {
			t.Errorf("node %s discovered but never status-polled", want.addr())
		}
	}
	if want := 3; len(snap.ModelIDs()) != want {
		t.Errorf("ModelIDs = %v, want %d models across the discovered mesh", snap.ModelIDs(), want)
	}
}

// A new host becomes usable within one poll interval of being advertised.
func TestNewHostAppearsWithoutRestart(t *testing.T) {
	a := newFakeNode(t, []string{"gemma"}, 0, true)
	r := testRegistryPermissive(t, a.addr())
	r.Round(context.Background())

	if _, ok := r.PickForModel("qwen"); ok {
		t.Fatal("qwen should not be routable yet")
	}

	b := newFakeNode(t, []string{"qwen"}, 0, true)
	a.advertise(b)

	r.Round(context.Background()) // discovers b
	r.Round(context.Background()) // status-polls b

	addr, ok := r.PickForModel("qwen")
	if !ok || addr != b.addr() {
		t.Fatalf("PickForModel(qwen) = %q, %v; want %s", addr, ok, b.addr())
	}
}

// meshapi's "absent is not zero", in the form that would actually bite: a node
// advertised by a peer but never successfully status-polled must not be
// routable at all. Its omitted total_in_flight would otherwise decode as 0,
// make it the least-loaded node in the fleet, and attract every request.
func TestPeerAdvertisedButUnreachableIsNotRoutable(t *testing.T) {
	ghost := newFakeNode(t, []string{"gemma"}, 0, true)
	ghost.down.Store(true)

	a := newFakeNode(t, []string{"gemma"}, 50, true)
	a.advertise(ghost)

	r := testRegistry(t, a.addr())
	for i := 0; i < 3; i++ {
		r.Round(context.Background())
	}

	for i := 0; i < 5; i++ {
		addr, ok := r.PickForModel("gemma")
		if !ok {
			t.Fatal("gemma should still be routable via the healthy node")
		}
		if addr == ghost.addr() {
			t.Fatalf("routed to a node that has never answered a status poll (%s)", addr)
		}
	}
}

func TestUnhealthyAfterThreeFailuresThenRecovers(t *testing.T) {
	a := newFakeNode(t, []string{"gemma"}, 0, true)
	r := testRegistry(t, a.addr())
	r.Round(context.Background())

	a.down.Store(true)
	for i := 0; i < FailuresBeforeUnhealthy; i++ {
		r.Round(context.Background())
	}
	if n := r.Snapshot().Node(a.addr()); n == nil || n.Healthy {
		t.Fatalf("node should be unhealthy after %d failures", FailuresBeforeUnhealthy)
	}
	if r.Snapshot().Node(a.addr()) == nil {
		t.Fatal("an unhealthy node must stay in the set so it can rejoin")
	}

	a.down.Store(false)
	r.Round(context.Background())
	if n := r.Snapshot().Node(a.addr()); n == nil || !n.Healthy {
		t.Fatal("node should rejoin by itself once it answers again")
	}
}

// The mesh is heterogeneous. A viiwork-nvidia stand-in serves no /chat and
// must not be elected view node while a full-surface node is healthy. Both
// nodes are seeded here: view eligibility is now restricted to Seed nodes
// (F3), so a viiwork stand-in reachable only by discovery would no longer be
// electable at all — this test is about FullUI ordering among eligible
// (seed) nodes, not about discovery, so both are seeded to isolate that.
func TestViewNodeAvoidsNodeWithoutChat(t *testing.T) {
	nvidia := newFakeNode(t, []string{"llama"}, 0, false)
	viiwork := newFakeNode(t, []string{"gemma"}, 0, true)
	nvidia.advertise(viiwork)
	viiwork.advertise(nvidia)

	r := testRegistryPermissive(t, nvidia.addr(), viiwork.addr())
	for i := 0; i < 3; i++ {
		r.Round(context.Background())
	}

	view, ok := r.ViewAddr()
	if !ok {
		t.Fatal("no view node elected")
	}
	if view != viiwork.addr() {
		t.Fatalf("view = %s, want the full-surface node %s", view, viiwork.addr())
	}
}

// TestDiscoveredNodeNeverElectedView is F3's registry-level guarantee: a node
// learned only through another node's peer advertisement — never an operator
// seed — must not become the view node even when it is healthy, serves the
// full UI, and self-reports a huge PeerCount. Only the seed is eligible.
func TestDiscoveredNodeNeverElectedView(t *testing.T) {
	seed := newFakeNode(t, []string{"gemma"}, 0, false) // no /chat, low PeerCount
	evil := newFakeNode(t, []string{"llama"}, 0, true)  // full UI
	// 50 fabricated, unreachable peer addresses: far more than the seed's
	// real PeerCount of 0, and enough to prove the point (F3's adversarial
	// scenario used 999) without paying for 999 unreachable dials per round
	// or tripping the discovery-intake cap this test isn't about.
	fakePeers := make([]meshapi.ClusterPeerInfo, 0, 50)
	for i := 0; i < 50; i++ {
		fakePeers = append(fakePeers, meshapi.ClusterPeerInfo{Addr: fmt.Sprintf("100.64.%d.%d:9000", i/256, i%256)})
	}
	evil.peers.Store(&fakePeers)
	seed.advertise(evil)

	r := testRegistryPermissive(t, seed.addr())
	for i := 0; i < 3; i++ {
		r.Round(context.Background())
	}

	// Sanity: evil really was discovered, healthy, and full-UI — otherwise
	// this test would pass vacuously.
	if n := r.Snapshot().Node(evil.addr()); n == nil || !n.Healthy || !n.FullUI {
		t.Fatalf("evil node not discovered/healthy/FullUI as expected: %+v", n)
	}

	view, ok := r.ViewAddr()
	if !ok {
		t.Fatal("no view node elected")
	}
	if view != seed.addr() {
		t.Fatalf("view = %s, want the seed node %s: a discovered node must never be elected view regardless of self-reported PeerCount/FullUI", view, seed.addr())
	}
}

func TestPickForModelRoundRobinsTiedNodes(t *testing.T) {
	a := newFakeNode(t, []string{"gemma"}, 0, true)
	b := newFakeNode(t, []string{"gemma"}, 0, true)
	a.advertise(b)

	r := testRegistryPermissive(t, a.addr())
	for i := 0; i < 3; i++ {
		r.Round(context.Background())
	}

	seen := map[string]int{}
	for i := 0; i < 20; i++ {
		addr, ok := r.PickForModel("gemma")
		if !ok {
			t.Fatal("gemma should be routable")
		}
		seen[addr]++
	}
	if len(seen) != 2 {
		t.Fatalf("round-robin visited %d nodes, want 2: %v", len(seen), seen)
	}
}

func TestNoHealthyNodesYieldsNoRoute(t *testing.T) {
	a := newFakeNode(t, []string{"gemma"}, 0, true)
	a.down.Store(true)
	r := testRegistry(t, a.addr())
	for i := 0; i <= FailuresBeforeUnhealthy; i++ {
		r.Round(context.Background())
	}
	if _, ok := r.PickForModel("gemma"); ok {
		t.Fatal("no route should exist when nothing is healthy")
	}
	if _, ok := r.ViewAddr(); ok {
		t.Fatal("no view node should exist when nothing is healthy")
	}
}

// Startup must not depend on any seed being up.
func TestUnreachableSeedsStillProduceASnapshot(t *testing.T) {
	r := testRegistry(t, "127.0.0.1:1")
	r.Round(context.Background())
	if r.Snapshot() == nil {
		t.Fatal("Snapshot must never be nil, even with every seed unreachable")
	}
	if _, ok := r.ViewAddr(); ok {
		t.Fatal("no view node should be reported")
	}
}

// A node advertising far more peer addresses than any real fleet could have
// must not be allowed to grow known without bound: every phantom address
// costs one goroutine and one TCP dial per poll interval, forever.
func TestKnownNodesAreCapped(t *testing.T) {
	a := newFakeNode(t, []string{"gemma"}, 0, true)

	// 100.64.0.0/10 (the tailnet's own CGNAT range), not 10.0.0.0/8: these
	// addresses must pass validPeerAddr's default (non-opt-in) checks so this
	// test still exercises the cap-flooding scenario it's named for, rather
	// than being satisfied vacuously by every address getting rejected as
	// RFC1918 before the cap is ever reached.
	peers := make([]meshapi.ClusterPeerInfo, 0, MaxKnownNodes+50)
	for i := 0; i < MaxKnownNodes+50; i++ {
		peers = append(peers, meshapi.ClusterPeerInfo{
			Addr: fmt.Sprintf("100.%d.%d.%d:9000", 64+i/65536, (i/256)%256, i%256),
		})
	}
	a.peers.Store(&peers)

	r := testRegistry(t, a.addr())
	r.Round(context.Background())

	r.mu.Lock()
	got := len(r.known)
	r.mu.Unlock()

	if got > MaxKnownNodes {
		t.Fatalf("known = %d, want capped at %d", got, MaxKnownNodes)
	}
	if got == 0 {
		t.Fatal("known is empty: the seed itself should still be tracked")
	}
}

// --- H1a: discovered peer address validation (SSRF via advertised addr) ---

// TestValidPeerAddrRejectsSSRFShapes proves every shape the adversarial
// review used to redirect the gateway's outbound polling is rejected:
// cloud-metadata-style link-local, loopback (v4 and v6), unspecified,
// path/query/fragment/userinfo injection, whitespace, and malformed
// host:port shapes (bad port, non-IP-literal host).
func TestValidPeerAddrRejectsSSRFShapes(t *testing.T) {
	bad := []string{
		"169.254.169.254:80",                 // IMDSv1 / cloud metadata (link-local)
		"127.0.0.1:9999",                     // loopback
		"[::1]:9999",                         // loopback, IPv6
		"0.0.0.0:80",                         // unspecified
		"internal-host/latest/meta-data#:80", // '/', '#', and not an IP literal
		"host:0?x=1&",                        // '?' and port 0
		"100.64.1.5/24:80",                   // '/'
		"100.64.1.5:80?x=1",                  // '?'
		"user@100.64.1.5:80",                 // '@' / userinfo
		"100.64.1.5:80#frag",                 // '#'
		"100.64.1.5: 80",                     // whitespace
		"example.internal:80",                // hostname, not an IP literal
		"100.64.1.5:0",                       // port 0
		"100.64.1.5:70000",                   // port out of range
		"100.64.1.5:abc",                     // non-numeric port
		"",                                   // empty
		"justahost",                          // no port at all
		"8.8.8.8:80",                         // public IPv4 (R1: allow-list, not deny-list)
		"2001:4860:4860::8888:80",            // public IPv6
		"[64:ff9b::a9fe:a9fe]:80",            // NAT64 well-known prefix embedding 169.254.169.254
		"100.63.255.255:80",                  // one address below the tailnet CGNAT range
		"100.128.0.0:80",                     // one address above the tailnet CGNAT range
		"100.64.1.5:+80",                     // M2: leading '+' is not canonical decimal
		"100.64.1.5:0080",                    // M2: leading zero is not canonical decimal
	}
	for _, addr := range bad {
		if err := validPeerAddr(addr, false); err == nil {
			t.Errorf("validPeerAddr(%q, false) = nil, want rejection", addr)
		}
	}
}

// TestValidPeerAddrAcceptsTailnetAddress proves the real mesh's own address
// space is never blocked: 100.64.0.0/10 is CGNAT, not RFC1918, so it must
// pass even with AllowPrivatePeers left at its default false.
func TestValidPeerAddrAcceptsTailnetAddress(t *testing.T) {
	for _, addr := range []string{"100.64.1.5:8080", "100.100.100.100:9000", "100.127.255.254:1"} {
		if err := validPeerAddr(addr, false); err != nil {
			t.Errorf("validPeerAddr(%q, false) = %v, want a tailnet address to pass", addr, err)
		}
	}
}

// TestValidPeerAddrTailnetExactBoundaries pins the exact edges of
// 100.64.0.0/10: the first and last addresses in range must pass, and the
// addresses immediately outside either edge must not.
func TestValidPeerAddrTailnetExactBoundaries(t *testing.T) {
	pass := []string{"100.64.0.0:80", "100.127.255.255:80"}
	for _, addr := range pass {
		if err := validPeerAddr(addr, false); err != nil {
			t.Errorf("validPeerAddr(%q, false) = %v, want the tailnet boundary address to pass", addr, err)
		}
	}
	fail := []string{"100.63.255.255:80", "100.128.0.0:80"}
	for _, addr := range fail {
		if err := validPeerAddr(addr, false); err == nil {
			t.Errorf("validPeerAddr(%q, false) = nil, want the address just outside 100.64.0.0/10 to be rejected", addr)
		}
	}
}

// TestValidPeerAddrPrivateRangeGatedByOptIn proves RFC1918 is rejected by
// default and accepted only once AllowPrivatePeers is set.
func TestValidPeerAddrPrivateRangeGatedByOptIn(t *testing.T) {
	if err := validPeerAddr("192.168.1.5:8080", false); err == nil {
		t.Fatal("192.168.x should be rejected by default")
	}
	if err := validPeerAddr("192.168.1.5:8080", true); err != nil {
		t.Fatalf("192.168.x should pass once AllowPrivatePeers is set: %v", err)
	}
	if err := validPeerAddr("10.0.0.5:8080", true); err != nil {
		t.Fatalf("10.0.0.0/8 should pass once AllowPrivatePeers is set: %v", err)
	}
}

// TestValidPeerAddrLoopbackAlwaysRejectedEvenWithOptIn proves the opt-in only
// widens the private-range gate, never loopback/link-local/unspecified.
func TestValidPeerAddrLoopbackAlwaysRejectedEvenWithOptIn(t *testing.T) {
	for _, addr := range []string{"127.0.0.1:9999", "169.254.169.254:80", "0.0.0.0:80"} {
		if err := validPeerAddr(addr, true); err == nil {
			t.Errorf("validPeerAddr(%q, true) = nil, want rejection even with AllowPrivatePeers", addr)
		}
	}
}

// TestClusterRoundRejectsSSRFPeerAddresses is the end-to-end form of the
// above: a malicious node's /v1/cluster response advertises attacker-chosen
// addresses, and none of them may reach `known` — which is what stops the
// next Round from ever dialing them.
func TestClusterRoundRejectsSSRFPeerAddresses(t *testing.T) {
	evil := newFakeNode(t, []string{"gemma"}, 0, true)
	bad := []string{
		"169.254.169.254:80",
		"127.0.0.1:9999",
		"192.168.50.1:8080",
		"evil-internal-host/../secrets#:80",
	}
	peers := make([]meshapi.ClusterPeerInfo, 0, len(bad))
	for _, a := range bad {
		peers = append(peers, meshapi.ClusterPeerInfo{Addr: a})
	}
	evil.peers.Store(&peers)

	r := testRegistry(t, evil.addr())
	r.Round(context.Background())

	r.mu.Lock()
	defer r.mu.Unlock()
	for _, a := range bad {
		if _, ok := r.known[a]; ok {
			t.Errorf("malicious peer address %q entered known; must be rejected before insertion or dial", a)
		}
	}
	if got := len(r.known); got != 1 {
		t.Errorf("known = %d entries, want 1 (only the seed itself): %v", got, r.known)
	}
}

// TestClusterRoundAcceptsTailnetPeerAddress is the positive counterpart: a
// legitimately-shaped discovered address must still make it into known.
func TestClusterRoundAcceptsTailnetPeerAddress(t *testing.T) {
	a := newFakeNode(t, []string{"gemma"}, 0, true)
	peers := []meshapi.ClusterPeerInfo{{Addr: "100.64.5.5:8080"}}
	a.peers.Store(&peers)

	r := testRegistry(t, a.addr())
	r.Round(context.Background())

	r.mu.Lock()
	_, ok := r.known["100.64.5.5:8080"]
	r.mu.Unlock()
	if !ok {
		t.Fatal("a valid tailnet-shaped discovered address should be added to known")
	}
}

// TestClusterRoundAllowsPrivatePeerWithOptIn proves the Options field
// actually reaches the validator through a full discovery round.
func TestClusterRoundAllowsPrivatePeerWithOptIn(t *testing.T) {
	a := newFakeNode(t, []string{"gemma"}, 0, true)
	peers := []meshapi.ClusterPeerInfo{{Addr: "192.168.9.9:8080"}}
	a.peers.Store(&peers)

	r := New(Options{
		Seeds:             []string{a.addr()},
		Timeout:           2 * time.Second,
		Interval:          10 * time.Millisecond,
		DiscoveryEvery:    1,
		AllowPrivatePeers: true,
	})
	r.Round(context.Background())

	r.mu.Lock()
	_, ok := r.known["192.168.9.9:8080"]
	r.mu.Unlock()
	if !ok {
		t.Fatal("a private-range discovered address should be added to known when AllowPrivatePeers is set")
	}
}

// --- H1b: bounded discovery response size (OOM via oversized poll body) ---

// TestOversizedStatusResponseFailsThePoll proves a response larger than
// maxPollResponseBytes fails the poll outright rather than being decoded
// (even partially): the node must not become healthy or routable off the
// back of it, and none of its bogus data may enter the snapshot.
func TestOversizedStatusResponseFailsThePoll(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc(meshapi.PathStatus, func(w http.ResponseWriter, r *http.Request) {
		// A syntactically valid StatusResponse, padded with a huge trailing
		// field, so that if the size limit were absent (or only checked
		// after a successful decode) this would decode cleanly and the node
		// would come up healthy.
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"total_in_flight":0,"healthy_backends":1,"total_backends":1,"padding":"`))
		pad := make([]byte, maxPollResponseBytes+4096)
		for i := range pad {
			pad[i] = 'a'
		}
		_, _ = w.Write(pad)
		_, _ = w.Write([]byte(`"}`))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	addr := strings.TrimPrefix(srv.URL, "http://")

	r := testRegistry(t, addr)

	if _, err := r.pollStatus(context.Background(), addr); err == nil {
		t.Fatal("pollStatus should fail closed on an oversized response body")
	}

	r.Round(context.Background())
	n := r.Snapshot().Node(addr)
	if n != nil && n.Healthy {
		t.Fatalf("node should not become healthy off an oversized status response: %+v", n)
	}
	if n != nil && n.Routable() {
		t.Fatal("node should not become routable off an oversized status response")
	}
}

// TestUndersizedStatusResponseStillWorks is the sanity companion: the bound
// must not be so tight that a real payload trips it.
func TestUndersizedStatusResponseStillWorks(t *testing.T) {
	a := newFakeNode(t, []string{"gemma"}, 3, true)
	r := testRegistry(t, a.addr())
	r.Round(context.Background())

	n := r.Snapshot().Node(a.addr())
	if n == nil || !n.Healthy {
		t.Fatalf("a normal-sized response should still succeed: %+v", n)
	}
}

// --- H1c: negative in-flight clamped to unknown (routing capture) ---

// TestNegativeInFlightTreatedAsUnknown proves a node reporting a negative
// total_in_flight cannot forge itself into the unique lowest-load slot: it
// must be excluded from CandidatesForModel entirely (InFlightKnown=false)
// rather than winning every routing pick, which would starve every honest
// node serving the same model.
func TestNegativeInFlightTreatedAsUnknown(t *testing.T) {
	honest := newFakeNode(t, []string{"gemma"}, 5, true)
	liar := newFakeNode(t, []string{"gemma"}, -1000000, true)

	r := testRegistry(t, honest.addr(), liar.addr())
	r.Round(context.Background())

	n := r.Snapshot().Node(liar.addr())
	if n == nil {
		t.Fatal("liar node missing from snapshot")
	}
	if n.InFlightKnown {
		t.Fatalf("negative total_in_flight must be treated as unknown, got InFlightKnown=true InFlight=%d", n.InFlight)
	}

	for i := 0; i < 20; i++ {
		addr, ok := r.PickForModel("gemma")
		if !ok {
			t.Fatal("gemma should still be routable")
		}
		if addr == liar.addr() {
			t.Fatalf("node reporting negative in-flight must never capture routing, picked %s", addr)
		}
	}
}

// TestGenuineZeroInFlightStaysKnown proves the clamp is specific to negative
// values: a real, reported zero must still count as known load, exactly as
// before.
func TestGenuineZeroInFlightStaysKnown(t *testing.T) {
	a := newFakeNode(t, []string{"gemma"}, 0, true)
	r := testRegistry(t, a.addr())
	r.Round(context.Background())

	n := r.Snapshot().Node(a.addr())
	if n == nil || !n.InFlightKnown || n.InFlight != 0 {
		t.Fatalf("genuine zero in-flight should stay known and zero: %+v", n)
	}
}
