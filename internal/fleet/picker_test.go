package fleet

import (
	"net/netip"
	"sync"
	"testing"
	"time"

	"github.com/janit/viiwork/v2/mesh"
	"github.com/janit/viiwork/v2/mesh/capacity"
	"github.com/janit/viiwork/v2/meshapi"
)

// The clock is fixed: reports are stamped one second before now, and
// staleAfter is three, so a report is fresh unless a case ages it deliberately.
var (
	testNow        = time.Date(2026, 9, 12, 12, 0, 0, 0, time.UTC)
	testStaleAfter = 3 * time.Second
)

func now() time.Time { return testNow }

// fakeMembers and fakeReports stand in for the mesh and the capacity poller.
type fakeMembers struct {
	mu sync.Mutex
	m  []mesh.Member
}

func (f *fakeMembers) Members() []mesh.Member {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]mesh.Member(nil), f.m...)
}

func (f *fakeMembers) set(m ...mesh.Member) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.m = m
}

type fakeReports struct {
	mu sync.Mutex
	r  map[string]capacity.Report
}

func (f *fakeReports) Report(node string) (capacity.Report, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	r, ok := f.r[node]
	return r, ok
}

func (f *fakeReports) set(r capacity.Report) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.r == nil {
		f.r = map[string]capacity.Report{}
	}
	f.r[r.Node] = r
}

func (f *fakeReports) drop(node string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.r, node)
}

// node builds an alive role-node member.
func node(name string, lastOctet byte) mesh.Member {
	return mesh.Member{
		Name:  name,
		Addr:  netip.AddrFrom4([4]byte{100, 64, 0, lastOctet}),
		Port:  7946,
		Meta:  mesh.NodeMeta{V: mesh.MetaVersion, API: 8086, Ver: "v2.0.0-beta1", Role: meshapi.RoleNode},
		State: meshapi.MemberAlive,
	}
}

func gateway(name string) mesh.Member {
	m := node(name, 250)
	m.Meta.Role = meshapi.RoleGateway
	return m
}

// report builds a report received one second ago (fresh under testStaleAfter).
func report(nodeName string, rtt time.Duration, models ...meshapi.ModelCapacity) capacity.Report {
	return capacity.Report{
		Node:     nodeName,
		APIAddr:  nodeName + ":8086",
		Received: testNow.Add(-time.Second),
		RTT:      rtt,
		Models:   models,
	}
}

func model(name string, slots, busy, healthy int) meshapi.ModelCapacity {
	return meshapi.ModelCapacity{
		Name: name, Engine: "llamacpp",
		Slots: slots, Busy: busy,
		Backends: healthy, HealthyBackends: healthy,
	}
}

// newTestPicker wires a picker over the two fakes.
func newTestPicker(m *fakeMembers, r *fakeReports, viewNodes ...string) *Picker {
	return NewPicker(m, r, testStaleAfter, viewNodes, now)
}

// pickName picks and reports the chosen node, failing if nothing is routable.
func pickName(t *testing.T, p *Picker, model string) (string, func()) {
	t.Helper()
	target, release, ok := p.Pick(model, nil)
	if !ok {
		t.Fatalf("Pick(%q): no candidate", model)
	}
	return target.Node, release
}

// P1: the node with more free slots wins.
func TestPickPrefersMostFreeSlots(t *testing.T) {
	m := &fakeMembers{}
	r := &fakeReports{}
	m.set(node("a", 1), node("b", 2))
	r.set(report("a", 5*time.Millisecond, model("m", 2, 0, 1)))
	r.set(report("b", 5*time.Millisecond, model("m", 2, 1, 1)))

	p := newTestPicker(m, r)
	got, release := pickName(t, p, "m")
	defer release()
	if got != "a" {
		t.Errorf("Pick = %q, want a (2 free vs 1)", got)
	}
}

// P2: equal free slots break on the lower round-trip time.
func TestPickBreaksTieOnRTT(t *testing.T) {
	m := &fakeMembers{}
	r := &fakeReports{}
	m.set(node("a", 1), node("b", 2))
	r.set(report("a", 5*time.Millisecond, model("m", 2, 0, 1)))
	r.set(report("b", 2*time.Millisecond, model("m", 2, 0, 1)))

	p := newTestPicker(m, r)
	got, release := pickName(t, p, "m")
	defer release()
	if got != "b" {
		t.Errorf("Pick = %q, want b (same free, lower RTT)", got)
	}
}

// P3: with nothing to separate them, picks rotate rather than always landing
// on the same node.
func TestPickRotatesOnFullTie(t *testing.T) {
	m := &fakeMembers{}
	r := &fakeReports{}
	m.set(node("a", 1), node("b", 2))
	r.set(report("a", 5*time.Millisecond, model("m", 2, 0, 1)))
	r.set(report("b", 5*time.Millisecond, model("m", 2, 0, 1)))

	p := newTestPicker(m, r)
	want := []string{"a", "b", "a", "b"}
	for i, w := range want {
		got, release := pickName(t, p, "m")
		release() // released each time, so reservations never accumulate
		if got != w {
			t.Errorf("pick %d = %q, want %q", i, got, w)
		}
	}
}

// P4: reservations count, so a burst spreads instead of piling onto one
// reported free slot.
func TestPickReservationsSpreadABurst(t *testing.T) {
	m := &fakeMembers{}
	r := &fakeReports{}
	m.set(node("a", 1), node("b", 2))
	r.set(report("a", 1*time.Millisecond, model("m", 2, 0, 1))) // 2 free
	r.set(report("b", 9*time.Millisecond, model("m", 1, 0, 1))) // 1 free

	p := newTestPicker(m, r)
	var releases []func()
	var got []string
	for range 3 {
		name, release := pickName(t, p, "m")
		got = append(got, name)
		releases = append(releases, release)
	}
	for _, rel := range releases {
		defer rel()
	}

	want := []string{"a", "a", "b"}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("pick %d = %q, want %q (held: %v)", i, got[i], want[i], got)
		}
	}
}

// P5: a newer report already counts the in-flight request as busy, so the
// reservation must stop counting or the same request is charged twice.
//
// The numbers are chosen so that double-counting changes the winner: after
// the refresh a and b are level on free slots, and a takes it on RTT. Charge
// a's three reservations a second time and a drops to zero, handing it to b.
func TestPickReservationsExpireWithANewerReport(t *testing.T) {
	m := &fakeMembers{}
	r := &fakeReports{}
	m.set(node("a", 1), node("b", 2))
	r.set(report("a", 1*time.Millisecond, model("m", 4, 0, 1))) // 4 free, fast
	r.set(report("b", 9*time.Millisecond, model("m", 1, 0, 1))) // 1 free, slow

	p := newTestPicker(m, r)
	var held []func()
	for range 3 {
		name, release := pickName(t, p, "m")
		held = append(held, release)
		if name != "a" {
			t.Fatalf("setup pick = %q, want a", name)
		}
	}
	for _, rel := range held {
		defer rel()
	}

	// a's report is refreshed after those picks, and now reports all three as
	// busy: 4 slots, 3 busy, so one genuinely free.
	fresh := report("a", 1*time.Millisecond, model("m", 4, 3, 1))
	fresh.Received = testNow // after every pick time
	r.set(fresh)

	got, release := pickName(t, p, "m")
	defer release()
	if got != "a" {
		t.Errorf("Pick = %q, want a: the three reservations predate the refreshed "+
			"report, which already counts them as busy, so they must not be "+
			"subtracted again", got)
	}
}

// P6: zero free is still routable — the node queues or spills from there.
func TestPickChoosesAFullNode(t *testing.T) {
	m := &fakeMembers{}
	r := &fakeReports{}
	m.set(node("a", 1))
	r.set(report("a", 5*time.Millisecond, model("m", 2, 2, 1)))

	p := newTestPicker(m, r)
	got, release := pickName(t, p, "m")
	defer release()
	if got != "a" {
		t.Errorf("Pick = %q, want a", got)
	}
}

// P7: a stale report is not routable — the node may be gone.
func TestPickSkipsStaleReports(t *testing.T) {
	m := &fakeMembers{}
	r := &fakeReports{}
	m.set(node("a", 1))
	stale := report("a", 5*time.Millisecond, model("m", 2, 0, 1))
	stale.Received = testNow.Add(-4 * time.Second)
	r.set(stale)

	p := newTestPicker(m, r)
	if _, _, ok := p.Pick("m", nil); ok {
		t.Error("Pick succeeded on a stale report")
	}
}

// P8: a model with no healthy backend cannot serve.
func TestPickSkipsUnhealthyModel(t *testing.T) {
	m := &fakeMembers{}
	r := &fakeReports{}
	m.set(node("a", 1))
	r.set(report("a", 5*time.Millisecond, model("m", 2, 0, 0)))

	p := newTestPicker(m, r)
	if _, _, ok := p.Pick("m", nil); ok {
		t.Error("Pick succeeded on a model with no healthy backends")
	}
}

// P9: only alive, remote, role-node members are routable.
func TestPickSkipsDeadGatewayAndLocal(t *testing.T) {
	dead := node("dead", 1)
	dead.State = meshapi.MemberDead
	local := node("self", 3)
	local.Local = true

	m := &fakeMembers{}
	r := &fakeReports{}
	m.set(dead, gateway("gw"), local)
	for _, n := range []string{"dead", "gw", "self"} {
		r.set(report(n, 5*time.Millisecond, model("m", 4, 0, 1)))
	}

	p := newTestPicker(m, r)
	if target, _, ok := p.Pick("m", nil); ok {
		t.Errorf("Pick = %q, want no candidate", target.Node)
	}
}

// P10: a node already tried is excluded from the retry.
func TestPickHonoursExclude(t *testing.T) {
	m := &fakeMembers{}
	r := &fakeReports{}
	m.set(node("a", 1), node("b", 2))
	r.set(report("a", 1*time.Millisecond, model("m", 4, 0, 1)))
	r.set(report("b", 9*time.Millisecond, model("m", 4, 0, 1)))

	p := newTestPicker(m, r)
	target, release, ok := p.Pick("m", map[string]bool{"a": true})
	if !ok {
		t.Fatal("want b, got no candidate")
	}
	defer release()
	if target.Node != "b" {
		t.Errorf("Pick = %q, want b", target.Node)
	}
}

// P11: a double release must not free a slot twice.
func TestReleaseIsIdempotent(t *testing.T) {
	m := &fakeMembers{}
	r := &fakeReports{}
	m.set(node("a", 1), node("b", 2))
	r.set(report("a", 1*time.Millisecond, model("m", 2, 0, 1))) // 2 free
	r.set(report("b", 9*time.Millisecond, model("m", 1, 0, 1))) // 1 free

	p := newTestPicker(m, r)
	_, rel1 := pickName(t, p, "m") // a, 1 free left
	rel1()
	rel1() // no-op

	// a is back to 2 free, so it wins again. A double release that credited
	// twice would show up as a going to 3 free, which still picks a — so
	// hold two and check b takes the third, exactly as in P4.
	_, relA1 := pickName(t, p, "m")
	_, relA2 := pickName(t, p, "m")
	defer relA1()
	defer relA2()
	got, release := pickName(t, p, "m")
	defer release()
	if got != "b" {
		t.Errorf("Pick = %q, want b: a's two reservations should exhaust it", got)
	}
}

// V1: without an allowlist the view is the lowest eligible name to begin
// with, and then sticks until its node stops being eligible — so a browser's
// SSE stream is not moved to another node because a lower name appeared.
func TestViewIsStickyOncePicked(t *testing.T) {
	// From a clean slate, the lowest eligible name wins.
	m := &fakeMembers{}
	r := &fakeReports{}
	m.set(node("b", 2), node("a", 1))
	r.set(report("a", time.Millisecond))
	r.set(report("b", time.Millisecond))
	if target, ok := newTestPicker(m, r).View(); !ok || target.Node != "a" {
		t.Fatalf("View = %q (ok=%v), want a", target.Node, ok)
	}

	// Now the discriminating case: b is chosen while it is the only eligible
	// member, and must keep the view when the lower name a joins later.
	m2 := &fakeMembers{}
	r2 := &fakeReports{}
	m2.set(node("a", 1), node("b", 2))
	r2.set(report("b", time.Millisecond)) // a has no report yet
	p := newTestPicker(m2, r2)
	if target, ok := p.View(); !ok || target.Node != "b" {
		t.Fatalf("View = %q (ok=%v), want b as the only eligible member", target.Node, ok)
	}

	r2.set(report("a", time.Millisecond))
	if target, _ := p.View(); target.Node != "b" {
		t.Errorf("View = %q, want b to keep it: a lower name appearing is not a "+
			"reason to move a live stream", target.Node)
	}

	// Only when b stops being eligible does the view move, and then to the
	// lowest eligible name.
	stale := report("b", time.Millisecond)
	stale.Received = testNow.Add(-4 * time.Second)
	r2.set(stale)
	if target, _ := p.View(); target.Node != "a" {
		t.Errorf("View = %q, want a once b went stale", target.Node)
	}
}

// V2: an allowlist is an order of preference, and it is authoritative.
func TestViewFollowsAllowlistOrder(t *testing.T) {
	m := &fakeMembers{}
	r := &fakeReports{}
	m.set(node("node-a", 1), node("node-b", 2))
	r.set(report("node-a", time.Millisecond))
	r.set(report("node-b", time.Millisecond))

	p := newTestPicker(m, r, "node-b", "node-a")
	if target, _ := p.View(); target.Node != "node-b" {
		t.Fatalf("View = %q, want node-b (first in the list)", target.Node)
	}

	stale := report("node-b", time.Millisecond)
	stale.Received = testNow.Add(-4 * time.Second)
	r.set(stale)
	if target, _ := p.View(); target.Node != "node-a" {
		t.Errorf("View = %q, want node-a", target.Node)
	}

	r.set(report("node-b", time.Millisecond))
	if target, _ := p.View(); target.Node != "node-b" {
		t.Errorf("View = %q, want node-b to take it back", target.Node)
	}
}

// V3: a node not in the mesh cannot serve the dashboards, allowlisted or not.
func TestViewAllowlistMissesEveryMember(t *testing.T) {
	m := &fakeMembers{}
	r := &fakeReports{}
	m.set(node("a", 1))
	r.set(report("a", time.Millisecond))

	p := newTestPicker(m, r, "node-z")
	if target, ok := p.View(); ok {
		t.Errorf("View = %q, want no view node", target.Node)
	}
}

// V4: a gateway never serves the dashboards, and neither does a dead node.
func TestViewSkipsGatewayAndDead(t *testing.T) {
	dead := node("dead", 1)
	dead.State = meshapi.MemberDead

	m := &fakeMembers{}
	r := &fakeReports{}
	m.set(dead, gateway("gw"))
	r.set(report("dead", time.Millisecond))
	r.set(report("gw", time.Millisecond))

	p := newTestPicker(m, r)
	if target, ok := p.View(); ok {
		t.Errorf("View = %q, want no view node", target.Node)
	}
}

// C1: the picker is shared by every in-flight request, so it must be safe
// under -race, and every reservation must be given back.
func TestPickerIsConcurrencySafe(t *testing.T) {
	m := &fakeMembers{}
	r := &fakeReports{}
	m.set(node("a", 1), node("b", 2))
	r.set(report("a", time.Millisecond, model("m", 50, 0, 1)))
	r.set(report("b", time.Millisecond, model("m", 50, 0, 1)))

	p := newTestPicker(m, r)

	var wg sync.WaitGroup
	for i := range 100 {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if _, release, ok := p.Pick("m", nil); ok {
				release()
			}
			p.View()
			if i%10 == 0 {
				m.set(node("a", 1), node("b", 2))
			}
		}(i)
	}
	wg.Wait()

	// Every reservation was released, so the picker must now behave exactly
	// as a fresh one. Re-report a at 2 free and b at 1, keeping Received in
	// the past so a leaked reservation would still be counted against its
	// node — and check the P4 burst pattern holds.
	r.set(report("a", 1*time.Millisecond, model("m", 2, 0, 1)))
	r.set(report("b", 9*time.Millisecond, model("m", 1, 0, 1)))

	var got []string
	for range 3 {
		name, release := pickName(t, p, "m")
		defer release()
		got = append(got, name)
	}
	want := []string{"a", "a", "b"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("picks after the concurrent phase = %v, want %v: a reservation leaked", got, want)
		}
	}
}
