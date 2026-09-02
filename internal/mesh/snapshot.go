// Package mesh keeps a live view of the viiwork cluster: which nodes exist,
// which models they serve, and which one should answer a given request.
package mesh

import "sort"

// Node is one mesh node as the gateway currently understands it. Everything
// here comes from that node's own /v1/status poll, never from a peer's
// second-hand report — see Registry.
type Node struct {
	Addr     string
	Hostname string
	Models   []string

	// InFlight is the node's own reported load. InFlightKnown distinguishes
	// "reported zero" from "never told us", which meshapi's "absent is not
	// zero" rule requires and which routing depends on.
	InFlight      int64
	InFlightKnown bool

	HealthyBackends int
	Healthy         bool

	// FullUI records whether the node serves /chat. viiwork does;
	// viiwork-nvidia, a second implementation of the same mesh, does not.
	FullUI bool

	// PeerCount is how many peers this node advertises. The node that sees
	// most of the mesh makes the best dashboard host. It is only ever
	// consulted for view election among Seed nodes (see electView) — a
	// discovered node inflating it now wins nothing.
	PeerCount int

	// Fails counts consecutive failed status polls.
	Fails int

	// Seed reports whether this node was an operator-configured seed address
	// (Options.Seeds), as opposed to one learned transitively from a peer's
	// self-report (see Registry.clusterRound). Only seed nodes are eligible
	// for view election (electView): a discovered node's self-reported
	// PeerCount and FullUI are both unverifiable, so letting one become the
	// node that serves dashboards and raw HTML to the operator's browser
	// would let a rogue/compromised mesh node win that role outright. See
	// docs/security/adversarial-mesh.md finding F3.
	Seed bool
}

// Routable reports whether the node can serve inference right now.
func (n *Node) Routable() bool {
	return n.Healthy && n.HealthyBackends > 0
}

// Snapshot is an immutable view of the mesh, rebuilt each poll round and
// swapped in atomically so request handling never takes a lock.
type Snapshot struct {
	nodes    map[string]*Node
	byModel  map[string][]*Node
	models   []string
	view     string
	degraded bool
}

// BuildSnapshot indexes nodes for routing and elects the view node. The
// returned snapshot takes ownership of the passed map and Node pointers; the
// caller must not mutate the map or any Node in place afterwards. Allocating
// fresh nodes each poll round is what makes concurrent reads of older
// snapshots safe while a newer one is swapped in atomically.
func BuildSnapshot(nodes map[string]*Node, viewPrefer string) *Snapshot {
	s := &Snapshot{
		nodes:   nodes,
		byModel: make(map[string][]*Node),
	}

	seen := make(map[string]bool)
	for _, n := range nodes {
		if !n.Routable() {
			continue
		}
		for _, m := range n.Models {
			s.byModel[m] = append(s.byModel[m], n)
			if !seen[m] {
				seen[m] = true
				s.models = append(s.models, m)
			}
		}
	}
	sort.Strings(s.models)
	for m := range s.byModel {
		sort.Slice(s.byModel[m], func(i, j int) bool {
			return s.byModel[m][i].Addr < s.byModel[m][j].Addr
		})
	}

	s.view, s.degraded = electView(nodes, viewPrefer)
	return s
}

// electView chooses the node that serves dashboards and every per-node view.
//
// It must prefer a node serving the full UI surface. The mesh is
// heterogeneous: viiwork-nvidia joins the same cluster and renders the same
// dashboard but serves no /chat, so electing it would 404 the chat UI for no
// visible reason.
//
// Only Seed nodes (operator-configured addresses) are eligible, aside from
// viewPrefer below — a discovered node's self-reported PeerCount and FullUI
// are both unverifiable, and this is the node that serves raw HTML/JS and
// headers to the operator's browser at the gateway's cookie-authenticated
// origin. See F3 in docs/security/adversarial-mesh.md.
func electView(nodes map[string]*Node, viewPrefer string) (addr string, degraded bool) {
	// viewPrefer is an explicit operator pin, not a self-report: it overrides
	// seed-only eligibility by design, the same way a seed address itself is
	// operator-chosen and exempt from discovery validation.
	if viewPrefer != "" {
		if n, ok := nodes[viewPrefer]; ok && n.Healthy {
			return viewPrefer, !n.FullUI
		}
	}

	best := func(requireFullUI bool) *Node {
		var chosen *Node
		for _, n := range nodes {
			if !n.Healthy || !n.Seed || (requireFullUI && !n.FullUI) {
				continue
			}
			// PeerCount only ever breaks ties here among nodes that already
			// passed the n.Seed check above, i.e. trusted, operator-chosen
			// nodes — so a rogue discovered node inflating its own
			// self-reported PeerCount has no candidate pool to win.
			switch {
			case chosen == nil:
			case n.PeerCount > chosen.PeerCount:
			case n.PeerCount == chosen.PeerCount && n.Addr < chosen.Addr:
			default:
				continue
			}
			chosen = n
		}
		return chosen
	}

	if n := best(true); n != nil {
		return n.Addr, false
	}
	if n := best(false); n != nil {
		return n.Addr, true
	}
	return "", false
}

// CandidatesForModel returns the nodes that should be considered for a model,
// already narrowed to the best tier and sorted by address so the caller's
// round-robin is deterministic.
//
// Nodes whose in-flight count is unknown are excluded whenever any candidate
// has reported one. A missing count means "unknown", and treating it as a
// measured zero would make that node look permanently idle and attract every
// request in the fleet.
func (s *Snapshot) CandidatesForModel(model string) []*Node {
	owners := s.byModel[model]
	if len(owners) == 0 {
		return nil
	}

	var known []*Node
	for _, n := range owners {
		if n.InFlightKnown {
			known = append(known, n)
		}
	}
	if len(known) == 0 {
		out := make([]*Node, len(owners))
		copy(out, owners)
		return out
	}

	min := known[0].InFlight
	for _, n := range known[1:] {
		if n.InFlight < min {
			min = n.InFlight
		}
	}
	var tied []*Node
	for _, n := range known {
		if n.InFlight == min {
			tied = append(tied, n)
		}
	}
	return tied
}

// ModelIDs is the union of models across every routable node, deduped and
// sorted. This is what /v1/models answers with.
func (s *Snapshot) ModelIDs() []string {
	out := make([]string, len(s.models))
	copy(out, s.models)
	return out
}

// View is the node serving dashboards and per-node lookups, or "" if no node
// is healthy.
func (s *Snapshot) View() string { return s.view }

// ViewDegraded reports that the elected view node does not serve the full UI
// surface, so parts of the dashboard will 404.
func (s *Snapshot) ViewDegraded() bool { return s.degraded }

// Node returns one node by address, or nil.
func (s *Snapshot) Node(addr string) *Node { return s.nodes[addr] }

// Nodes returns every known node, sorted by address.
func (s *Snapshot) Nodes() []*Node {
	out := make([]*Node, 0, len(s.nodes))
	for _, n := range s.nodes {
		out = append(out, n)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Addr < out[j].Addr })
	return out
}
