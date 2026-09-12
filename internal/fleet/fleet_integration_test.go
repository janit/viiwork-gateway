//go:build integration

package fleet

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/hashicorp/memberlist"
	"github.com/janit/viiwork-gateway/internal/config"
	"github.com/janit/viiwork/v2/mesh"
	"github.com/janit/viiwork/v2/mesh/meshtest"
	"github.com/janit/viiwork/v2/meshapi"
)

// The in-process mesh runs far faster than a real one so a test can watch a
// member leave and be declared dead inside a few seconds.
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

func permissiveAddr(netip.Addr) error { return nil }

// loopback is what every member advertises: the capacity poller makes a real
// HTTP request to Member.APIAddr(), so the address has to be dialable. Members
// are told apart by their API port, and by their gossip port on the in-process
// network.
var loopback = netip.MustParseAddr("127.0.0.1")

// fakeNode is a viiwork node: a real mesh member with role node, and a real
// HTTP server answering /v1/capacity from a value the test controls.
type fakeNode struct {
	name    string
	mesh    *mesh.Mesh
	srv     *httptest.Server
	apiPort int

	mu       sync.Mutex
	models   []meshapi.ModelCapacity
	requests int
}

func (n *fakeNode) setModels(models ...meshapi.ModelCapacity) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.models = models
}

func (n *fakeNode) capacityRequests() int {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.requests
}

// sees reports whether this node currently lists name in the given state.
func (n *fakeNode) sees(name, state string) bool {
	for _, m := range n.mesh.Members() {
		if m.Name == name {
			return m.State == state
		}
	}
	return false
}

func nodeModel(name string, slots, busy int) meshapi.ModelCapacity {
	return meshapi.ModelCapacity{
		Name: name, Engine: "llamacpp",
		Slots: slots, Busy: busy,
		Backends: 1, HealthyBackends: 1,
	}
}

// startFakeNode brings up a node on the in-process network. Its gossip port is
// unique; its API port is whatever its httptest server got.
func startFakeNode(t *testing.T, nw *meshtest.Network, name string, gossipPort int, tune func(*mesh.Options)) *fakeNode {
	t.Helper()
	n := &fakeNode{name: name}

	n.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != meshapi.PathCapacity {
			http.NotFound(w, r)
			return
		}
		n.mu.Lock()
		n.requests++
		models := append([]meshapi.ModelCapacity(nil), n.models...)
		n.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(meshapi.CapacityResponse{
			Node: name, Ver: "v2.0.0-beta1", Models: models,
		})
	}))
	t.Cleanup(n.srv.Close)

	_, portStr, err := net.SplitHostPort(n.srv.Listener.Addr().String())
	if err != nil {
		t.Fatalf("%s: server address: %v", name, err)
	}
	n.apiPort, _ = strconv.Atoi(portStr)

	tr := nw.TransportAt(name, netip.AddrPortFrom(loopback, uint16(gossipPort)))
	o := mesh.Options{
		Name: name, Network: mesh.NetworkTailnet, BindPort: gossipPort,
		Advertise: loopback, APIPort: n.apiPort, Role: meshapi.RoleNode,
		Version: "v2.0.0-beta1", Enforce: mesh.EnforceFull,
		RejoinInterval: 300 * time.Millisecond,
		AddrCheck:      permissiveAddr,
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

// gatewayHarness is a Fleet on the same in-process network, plus a listener on
// its API port so the test can prove nothing ever connects to it (F7).
type gatewayHarness struct {
	*Fleet
	apiConns func() int
}

func startGateway(t *testing.T, nw *meshtest.Network, name string, gossipPort int, seeds []string, tweak func(*config.Config)) *gatewayHarness {
	t.Helper()

	// The gateway's own API port. Fleet never listens on it; this listener is
	// here only to count connections that should never arrive.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("api listener: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	var connMu sync.Mutex
	conns := 0
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			connMu.Lock()
			conns++
			connMu.Unlock()
			_ = c.Close()
		}
	}()

	cfg := config.Config{
		Listen:   ln.Addr().String(),
		NodeName: name,
		Mesh: config.MeshConfig{
			Network: "tailnet", BindPort: gossipPort, Advertise: loopback,
			Open: true, Enforce: "full",
			Seeds:          seeds,
			RejoinInterval: 300 * time.Millisecond,
			CapacityPoll:   200 * time.Millisecond,
			StaleAfter:     time.Second,
		},
		ViewNodes: []string{"node-a", "node-b"},
	}
	if tweak != nil {
		tweak(&cfg)
	}

	tr := nw.TransportAt(name, netip.AddrPortFrom(loopback, uint16(gossipPort)))
	f, err := Start(context.Background(), Options{
		Config:  cfg,
		Version: "test",
		MeshTune: func(o *mesh.Options) {
			o.AddrCheck = permissiveAddr
			o.Tune = fastTimings(tr)
			o.Feeders = []mesh.Feeder{mesh.SeedFeeder(cfg.Mesh.Seeds)}
		},
	})
	if err != nil {
		t.Fatalf("start gateway %s: %v", name, err)
	}
	t.Cleanup(func() { _ = f.Close(time.Second) })

	return &gatewayHarness{Fleet: f, apiConns: func() int {
		connMu.Lock()
		defer connMu.Unlock()
		return conns
	}}
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

// twoNodeMesh is the shape most cases need: node-a and node-b, and a gateway
// seeded from both.
func twoNodeMesh(t *testing.T) (*meshtest.Network, *fakeNode, *fakeNode, *gatewayHarness) {
	t.Helper()
	nw := meshtest.NewNetwork()
	a := startFakeNode(t, nw, "node-a", 17946, nil)
	b := startFakeNode(t, nw, "node-b", 17947, func(o *mesh.Options) {
		o.Seeds = []string{nw.Addr("node-a").String()}
	})
	a.setModels(nodeModel("m", 2, 0)) // 2 free
	b.setModels(nodeModel("m", 2, 2)) // 0 free

	gw := startGateway(t, nw, "gw", 17948, []string{
		nw.Addr("node-a").String(), nw.Addr("node-b").String(),
	}, nil)
	return nw, a, b, gw
}

// F1: the gateway joins, sees both nodes, and routes to the one with slots.
func TestFleetJoinsAndRoutesByFreeSlots(t *testing.T) {
	_, a, _, gw := twoNodeMesh(t)

	within(t, 3*time.Second, "three alive members", func() bool {
		return gw.NumAlive() == 3
	})
	within(t, 3*time.Second, "a routable pick", func() bool {
		_, release, ok := gw.Pick("m", nil)
		if ok {
			release()
		}
		return ok
	})

	target, release, ok := gw.Pick("m", nil)
	if !ok {
		t.Fatal("Pick: no candidate")
	}
	defer release()
	if target.Node != "node-a" {
		t.Errorf("Pick = %q, want node-a (2 free vs 0)", target.Node)
	}
	wantAddr := netip.AddrPortFrom(loopback, uint16(a.apiPort)).String()
	if target.APIAddr != wantAddr {
		t.Errorf("APIAddr = %q, want %q", target.APIAddr, wantAddr)
	}
}

// F2: a node that vanishes stops being routable, and traffic moves to the
// survivor even though the survivor reported no free slots.
func TestFleetRoutesAwayFromAVanishedNode(t *testing.T) {
	nw, _, _, gw := twoNodeMesh(t)

	within(t, 3*time.Second, "node-a routable", func() bool {
		target, release, ok := gw.Pick("m", nil)
		if ok {
			release()
		}
		return ok && target.Node == "node-a"
	})

	nw.Unplug("node-a")

	within(t, gw.staleAfterForTest()+2*time.Second, "node-b to take over", func() bool {
		target, release, ok := gw.Pick("m", nil)
		if ok {
			release()
		}
		return ok && target.Node == "node-b"
	})
}

// F3: a graceful stop leaves the mesh, so every member sees it go at once
// rather than waiting for a failure detector — and the capacity poller stops.
func TestFleetCloseLeavesTheMeshAndStopsPolling(t *testing.T) {
	_, a, b, gw := twoNodeMesh(t)

	within(t, 3*time.Second, "three alive members", func() bool {
		return gw.NumAlive() == 3
	})
	within(t, 3*time.Second, "the gateway to have polled", func() bool {
		return a.capacityRequests() > 0 && b.capacityRequests() > 0
	})

	if err := gw.Close(time.Second); err != nil {
		t.Fatalf("Close: %v", err)
	}
	afterA, afterB := a.capacityRequests(), b.capacityRequests()

	within(t, 2*time.Second, "both nodes to see the gateway leave", func() bool {
		return a.sees("gw", meshapi.MemberLeft) && b.sees("gw", meshapi.MemberLeft)
	})

	// Close returned, so the poller is stopped: several poll intervals later
	// neither node has been asked again.
	time.Sleep(time.Second)
	if got := a.capacityRequests(); got != afterA {
		t.Errorf("node-a was polled %d more times after Close", got-afterA)
	}
	if got := b.capacityRequests(); got != afterB {
		t.Errorf("node-b was polled %d more times after Close", got-afterB)
	}

	if err := gw.Close(time.Second); err != nil {
		t.Errorf("second Close: %v, want nil (Close must be safe to call twice)", err)
	}
}

// F4: two gateways cannot share a name. The second must be told, so its
// process can exit rather than fight over the identity.
func TestFleetDuplicateNameIsFatal(t *testing.T) {
	nw, _, _, gw := twoNodeMesh(t)

	within(t, 3*time.Second, "three alive members", func() bool {
		return gw.NumAlive() == 3
	})

	second := startGateway(t, nw, "gw", 17949, []string{nw.Addr("node-a").String()}, nil)

	select {
	case err := <-second.Fatal():
		if err == nil {
			t.Fatal("Fatal yielded a nil error")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no fatal error for a duplicate name within 5s")
	}
}

// F5: a secured gateway will not join an open mesh. It must end up alone
// rather than half-joined, and route nothing.
func TestFleetSecuredGatewayCannotJoinAnOpenMesh(t *testing.T) {
	nw := meshtest.NewNetwork()
	a := startFakeNode(t, nw, "node-a", 17946, nil)
	a.setModels(nodeModel("m", 2, 0))

	secret := make([]byte, 32)
	gw := startGateway(t, nw, "gw", 17948, []string{nw.Addr("node-a").String()},
		func(c *config.Config) {
			c.Mesh.Open = false
			c.Mesh.SecretKey = secret
		})

	time.Sleep(3 * time.Second)
	if n := gw.NumAlive(); n != 1 {
		t.Errorf("NumAlive = %d, want 1: a secured member must not join an open mesh", n)
	}
	if target, release, ok := gw.Pick("m", nil); ok {
		release()
		t.Errorf("Pick = %q, want no candidate", target.Node)
	}
}

// recordingPayload notes every MergeRemoteState it is handed. A gateway
// carries no payload, and memberlist must therefore never hand a node an empty
// buffer to decode — if it did, every node would log a decode error each time
// the gateway pushed state.
type recordingPayload struct {
	mu     sync.Mutex
	merges [][]byte
}

func (p *recordingPayload) NotifyMsg([]byte) {}

// nodeLocalState is what a node gossips. Every buffer a node merges must be
// exactly this: anything else came from the gateway, which must send nothing.
var nodeLocalState = []byte(`{"aliases":{}}`)

func (p *recordingPayload) LocalState(bool) []byte { return nodeLocalState }

func (p *recordingPayload) MergeRemoteState(buf []byte, _ bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.merges = append(p.merges, append([]byte(nil), buf...))
}

// counts returns how many merges happened and how many handed over an empty
// buffer. The first number is what makes the second meaningful.
func (p *recordingPayload) counts() (merges, empties, foreign int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, b := range p.merges {
		merges++
		switch {
		case len(b) == 0:
			empties++
		case !bytes.Equal(b, nodeLocalState):
			foreign++
		}
	}
	return merges, empties, foreign
}

// F6: the payload-less gateway must not disturb the nodes' alias gossip.
//
// Two nodes carry a payload so the test can tell "the gateway sent nothing"
// apart from "no push/pull happened at all": node-a must see a real, non-empty
// merge from node-b, and never an empty one from anybody. An empty buffer
// would mean every node logged a decode error each time the gateway pushed.
func TestFleetGatewayDoesNotReachNodeState(t *testing.T) {
	nw := meshtest.NewNetwork()
	payloadA := &recordingPayload{}
	a := startFakeNode(t, nw, "node-a", 17946, func(o *mesh.Options) {
		o.Payload = payloadA
	})
	b := startFakeNode(t, nw, "node-b", 17947, func(o *mesh.Options) {
		o.Payload = &recordingPayload{}
		o.Seeds = []string{nw.Addr("node-a").String()}
	})
	a.setModels(nodeModel("m", 2, 0))
	b.setModels(nodeModel("m", 2, 0))

	gw := startGateway(t, nw, "gw", 17948, []string{nw.Addr("node-a").String()}, nil)
	within(t, 3*time.Second, "three alive members", func() bool {
		return gw.NumAlive() == 3
	})

	// Several push/pull intervals, so the gateway has certainly pushed state
	// and the two nodes have certainly exchanged theirs.
	time.Sleep(2500 * time.Millisecond)

	merges, empties, foreign := payloadA.counts()
	if merges == 0 {
		t.Fatal("node-a never merged any remote state, so this test proves " +
			"nothing about what the gateway sends")
	}
	if empties != 0 {
		t.Errorf("MergeRemoteState was handed an empty buffer %d times out of "+
			"%d merges; a payload-less gateway must reach no node's state at all",
			empties, merges)
	}
	if foreign != 0 {
		t.Errorf("%d of %d merged buffers were not another node's state; the "+
			"gateway must gossip no payload at all", foreign, merges)
	}
}

// F7: nodes never poll a gateway for capacity, so nothing should ever connect
// to the gateway's API port from the mesh.
func TestFleetIsNeverPolledByNodes(t *testing.T) {
	_, _, _, gw := twoNodeMesh(t)

	within(t, 3*time.Second, "three alive members", func() bool {
		return gw.NumAlive() == 3
	})
	time.Sleep(2 * time.Second)

	if n := gw.apiConns(); n != 0 {
		t.Errorf("the gateway's API port received %d connections; nodes must "+
			"never poll a gateway", n)
	}
}

// helper so F2 can wait proportionally to the configured staleness window
// without the test hardcoding it twice.
func (g *gatewayHarness) staleAfterForTest() time.Duration { return time.Second }
