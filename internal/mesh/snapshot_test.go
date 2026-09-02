package mesh

import (
	"testing"
)

// node builds a healthy-by-default test node that is a Seed: view election
// (electView) now restricts its candidate pool to Seed nodes, and most of
// this file's tests are about ordering among nodes that are meant to be
// view-eligible. Tests that specifically exercise a discovered (non-seed)
// node set n.Seed = false explicitly after construction.
func node(addr string, healthy bool, inFlight int64, known bool, models ...string) *Node {
	return &Node{
		Addr:            addr,
		Models:          models,
		Healthy:         healthy,
		HealthyBackends: 1,
		InFlight:        inFlight,
		InFlightKnown:   known,
		FullUI:          true,
		Seed:            true,
	}
}

func addrsOf(nodes []*Node) []string {
	out := make([]string, 0, len(nodes))
	for _, n := range nodes {
		out = append(out, n.Addr)
	}
	return out
}

func TestCandidatesPicksModelOwners(t *testing.T) {
	s := BuildSnapshot(map[string]*Node{
		"a:8080": node("a:8080", true, 0, true, "gemma"),
		"b:8080": node("b:8080", true, 0, true, "qwen"),
	}, "")

	got := addrsOf(s.CandidatesForModel("qwen"))
	if len(got) != 1 || got[0] != "b:8080" {
		t.Fatalf("candidates = %v, want [b:8080]", got)
	}
	if len(s.CandidatesForModel("nonexistent")) != 0 {
		t.Error("unknown model should have no candidates")
	}
}

func TestCandidatesExcludeUnhealthy(t *testing.T) {
	s := BuildSnapshot(map[string]*Node{
		"a:8080": node("a:8080", false, 0, true, "gemma"),
		"b:8080": node("b:8080", true, 5, true, "gemma"),
	}, "")

	got := addrsOf(s.CandidatesForModel("gemma"))
	if len(got) != 1 || got[0] != "b:8080" {
		t.Fatalf("candidates = %v, want [b:8080]", got)
	}
}

func TestCandidatesExcludeNodesWithNoHealthyBackends(t *testing.T) {
	n := node("a:8080", true, 0, true, "gemma")
	n.HealthyBackends = 0
	s := BuildSnapshot(map[string]*Node{
		"a:8080": n,
		"b:8080": node("b:8080", true, 9, true, "gemma"),
	}, "")

	got := addrsOf(s.CandidatesForModel("gemma"))
	if len(got) != 1 || got[0] != "b:8080" {
		t.Fatalf("candidates = %v, want [b:8080]: a node with no healthy backends is not routable", got)
	}
}

func TestCandidatesPreferLowestInFlight(t *testing.T) {
	s := BuildSnapshot(map[string]*Node{
		"a:8080": node("a:8080", true, 7, true, "gemma"),
		"b:8080": node("b:8080", true, 2, true, "gemma"),
		"c:8080": node("c:8080", true, 9, true, "gemma"),
	}, "")

	got := addrsOf(s.CandidatesForModel("gemma"))
	if len(got) != 1 || got[0] != "b:8080" {
		t.Fatalf("candidates = %v, want [b:8080]", got)
	}
}

func TestCandidatesReturnAllTiedAtMinimum(t *testing.T) {
	s := BuildSnapshot(map[string]*Node{
		"c:8080": node("c:8080", true, 2, true, "gemma"),
		"a:8080": node("a:8080", true, 2, true, "gemma"),
		"b:8080": node("b:8080", true, 8, true, "gemma"),
	}, "")

	got := addrsOf(s.CandidatesForModel("gemma"))
	if len(got) != 2 || got[0] != "a:8080" || got[1] != "c:8080" {
		t.Fatalf("candidates = %v, want [a:8080 c:8080] sorted for determinism", got)
	}
}

// meshapi's "absent is not zero" rule. A node whose in-flight count is unknown
// must never be treated as idle, which would make it the most attractive
// target in the fleet and pin all traffic to it.
func TestUnknownInFlightNeverOutranksKnown(t *testing.T) {
	s := BuildSnapshot(map[string]*Node{
		"unknown:8080": node("unknown:8080", true, 0, false, "gemma"),
		"known:8080":   node("known:8080", true, 4, true, "gemma"),
	}, "")

	got := addrsOf(s.CandidatesForModel("gemma"))
	if len(got) != 1 || got[0] != "known:8080" {
		t.Fatalf("candidates = %v, want [known:8080]: an unknown count must not read as idle", got)
	}
}

func TestAllUnknownInFlightFallsBackToEveryOwner(t *testing.T) {
	s := BuildSnapshot(map[string]*Node{
		"a:8080": node("a:8080", true, 0, false, "gemma"),
		"b:8080": node("b:8080", true, 0, false, "gemma"),
	}, "")

	got := addrsOf(s.CandidatesForModel("gemma"))
	if len(got) != 2 {
		t.Fatalf("candidates = %v, want both owners when no load figure is known", got)
	}
}

func TestModelIDsAreUnionSortedAndDeduped(t *testing.T) {
	s := BuildSnapshot(map[string]*Node{
		"a:8080": node("a:8080", true, 0, true, "qwen", "gemma"),
		"b:8080": node("b:8080", true, 0, true, "gemma", "granite"),
		"c:8080": node("c:8080", false, 0, true, "offline-only"),
	}, "")

	got := s.ModelIDs()
	want := []string{"gemma", "granite", "qwen"}
	if len(got) != len(want) {
		t.Fatalf("ModelIDs = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("ModelIDs[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestViewPrefersFullUIThenWidestPeerView(t *testing.T) {
	nvidia := node("nvidia:8080", true, 0, true, "llama")
	nvidia.FullUI = false
	nvidia.PeerCount = 9

	small := node("small:8080", true, 0, true, "gemma")
	small.PeerCount = 1

	s := BuildSnapshot(map[string]*Node{
		"nvidia:8080": nvidia,
		"small:8080":  small,
	}, "")

	if s.View() != "small:8080" {
		t.Fatalf("View = %q, want small:8080: a node without /chat must not be elected while a full-surface node is healthy", s.View())
	}
}

func TestViewFallsBackWhenNoFullUINodeIsHealthy(t *testing.T) {
	nvidia := node("nvidia:8080", true, 0, true, "llama")
	nvidia.FullUI = false

	s := BuildSnapshot(map[string]*Node{"nvidia:8080": nvidia}, "")

	if s.View() != "nvidia:8080" {
		t.Fatalf("View = %q, want nvidia:8080 as a degraded fallback", s.View())
	}
	if !s.ViewDegraded() {
		t.Error("ViewDegraded should report that the elected view node lacks the full UI")
	}
}

func TestViewTieBreaksOnLowestAddr(t *testing.T) {
	a := node("a:8080", true, 0, true, "gemma")
	a.PeerCount = 3
	b := node("b:8080", true, 0, true, "gemma")
	b.PeerCount = 3

	s := BuildSnapshot(map[string]*Node{"b:8080": b, "a:8080": a}, "")
	if s.View() != "a:8080" {
		t.Fatalf("View = %q, want a:8080: ties must break deterministically", s.View())
	}
}

func TestViewPreferOverrides(t *testing.T) {
	a := node("a:8080", true, 0, true, "gemma")
	a.PeerCount = 9
	// b is deliberately a discovered (non-seed) node: an explicit operator
	// pin is a judgement call the operator made, not a self-report, so it
	// must still be honoured even for a node that would otherwise be
	// ineligible for view election. See TestViewPreferCanPointAtDiscoveredNode
	// for the same claim made explicit as its own test.
	b := node("b:8080", true, 0, true, "gemma")
	b.Seed = false

	s := BuildSnapshot(map[string]*Node{"a:8080": a, "b:8080": b}, "b:8080")
	if s.View() != "b:8080" {
		t.Fatalf("View = %q, want the pinned b:8080", s.View())
	}
}

// TestViewPreferCanPointAtDiscoveredNode is F3's explicit-override case: an
// operator setting VIIWORK_GW_VIEW_PREFER at a node the mesh only discovered
// (never a seed) is a deliberate operator choice, not a self-report, and must
// still win — seed-only eligibility applies only to the unpinned election
// path.
func TestViewPreferCanPointAtDiscoveredNode(t *testing.T) {
	seed := node("seed:8080", true, 0, true, "gemma")
	discovered := node("discovered:8080", true, 0, true, "gemma")
	discovered.Seed = false

	s := BuildSnapshot(map[string]*Node{
		"seed:8080":       seed,
		"discovered:8080": discovered,
	}, "discovered:8080")

	if s.View() != "discovered:8080" {
		t.Fatalf("View = %q, want the explicitly pinned discovered node discovered:8080", s.View())
	}
	if s.ViewDegraded() {
		t.Error("ViewDegraded should be false: the pinned discovered node serves the full UI")
	}
}

// TestViewNeverElectsDiscoveredNodeOverHealthySeed is F3's core fix: a
// discovered node cannot buy its way into view election by self-reporting an
// inflated PeerCount and answering the /chat probe. A seed wins even lacking
// FullUI and with a far lower PeerCount, over a discovered node that has
// both full UI and a huge self-reported PeerCount.
func TestViewNeverElectsDiscoveredNodeOverHealthySeed(t *testing.T) {
	seed := node("seed:8080", true, 0, true, "gemma")
	seed.FullUI = false
	seed.PeerCount = 0

	evil := node("evil:8080", true, 0, true, "llama")
	evil.Seed = false
	evil.FullUI = true
	evil.PeerCount = 999

	s := BuildSnapshot(map[string]*Node{
		"seed:8080": seed,
		"evil:8080": evil,
	}, "")

	if s.View() == "evil:8080" {
		t.Fatalf("View = %q: a discovered node won view election via self-reported FullUI/PeerCount", s.View())
	}
	if s.View() != "seed:8080" {
		t.Fatalf("View = %q, want the seed node seed:8080 even though it lacks FullUI and has a lower PeerCount", s.View())
	}
	if !s.ViewDegraded() {
		t.Error("ViewDegraded should be true: the elected seed lacks the full UI")
	}
}

// TestViewEmptyWhenNoHealthySeedEvenWithHealthyDiscoveredFullUINode is F3's
// no-fallback guarantee: with no eligible seed at all, View() must be empty
// rather than falling back to a discovered node for the browser-facing
// surface, even a perfectly healthy, full-UI one. The router 503s the sticky
// paths on an empty view; it is not this test's job to re-verify that, only
// that the mesh package itself never hands out a discovered node as View().
func TestViewEmptyWhenNoHealthySeedEvenWithHealthyDiscoveredFullUINode(t *testing.T) {
	discovered := node("discovered:8080", true, 0, true, "gemma")
	discovered.Seed = false

	s := BuildSnapshot(map[string]*Node{"discovered:8080": discovered}, "")

	if s.View() != "" {
		t.Fatalf("View = %q, want empty: no seed is healthy, so the gateway must not fall back to a discovered node", s.View())
	}
}

func TestViewPreferIgnoredWhenUnhealthy(t *testing.T) {
	a := node("a:8080", true, 0, true, "gemma")
	b := node("b:8080", false, 0, true, "gemma")

	s := BuildSnapshot(map[string]*Node{"a:8080": a, "b:8080": b}, "b:8080")
	if s.View() != "a:8080" {
		t.Fatalf("View = %q, want a:8080: a pinned node that is down cannot serve views", s.View())
	}
}

func TestViewEmptyWhenNothingHealthy(t *testing.T) {
	s := BuildSnapshot(map[string]*Node{
		"a:8080": node("a:8080", false, 0, true, "gemma"),
	}, "")
	if s.View() != "" {
		t.Fatalf("View = %q, want empty when no node is healthy", s.View())
	}
}

func TestViewPreferPinnedNodeWithoutFullUIIsDegraded(t *testing.T) {
	noUI := node("noui:8080", true, 0, true, "gemma")
	noUI.FullUI = false

	withUI := node("withui:8080", true, 0, true, "gemma")
	withUI.FullUI = true

	s := BuildSnapshot(map[string]*Node{
		"noui:8080":   noUI,
		"withui:8080": withUI,
	}, "noui:8080")

	if s.View() != "noui:8080" {
		t.Fatalf("View = %q, want noui:8080: the pin should win even without full UI", s.View())
	}
	if !s.ViewDegraded() {
		t.Error("ViewDegraded should report true when the pinned node lacks full UI")
	}
}
