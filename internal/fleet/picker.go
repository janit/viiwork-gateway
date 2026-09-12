// Package fleet is everything mesh-shaped in the gateway: joining a viiwork 2
// mesh as a zero-model member, keeping members' capacity fresh, and the two
// decisions the router needs — which node serves an inference request, and
// which node serves the dashboards.
package fleet

import (
	"sort"
	"sync"
	"time"

	"github.com/janit/viiwork/v2/mesh"
	"github.com/janit/viiwork/v2/mesh/capacity"
	"github.com/janit/viiwork/v2/meshapi"
)

// MemberSource is the mesh, seen as the member table. *mesh.Mesh satisfies it.
type MemberSource interface {
	Members() []mesh.Member
}

// ReportSource is the capacity poller. *capacity.Poller satisfies it.
type ReportSource interface {
	Report(node string) (capacity.Report, bool)
}

// Target is a node to forward to.
type Target struct {
	Node    string
	APIAddr string
}

// reservation is one forward this gateway has started but not finished.
//
// It exists because capacity reports are a snapshot: between two polls a node
// reports the same free-slot count no matter how many requests this gateway
// has already sent it. Without reservations a burst arriving inside one poll
// interval would all be routed at the same single free slot.
type reservation struct {
	node  string
	model string
	at    time.Time
}

// Picker holds the routing and view decisions over a member table and the
// capacity reports for it. All methods are safe for concurrent use: one Picker
// is shared by every in-flight request.
type Picker struct {
	members    MemberSource
	reports    ReportSource
	staleAfter time.Duration
	viewNodes  []string
	now        func() time.Time

	mu sync.Mutex
	// held is keyed by an id that only has to be unique, so the release func
	// can remove exactly its own reservation.
	held   map[uint64]reservation
	nextID uint64
	// rotate breaks a full tie: same free slots and same RTT. Per model, so
	// traffic for one model does not shift another model's rotation.
	rotate map[string]int
	// view is the sticky choice, kept while it stays eligible so an SSE
	// stream is not moved from under a browser for a cosmetic reason.
	view string
}

// NewPicker builds a Picker. viewNodes, when non-empty, is the only set of
// names allowed to serve the dashboards, in order of preference. now is
// injectable so tests need no real clock.
func NewPicker(members MemberSource, reports ReportSource, staleAfter time.Duration, viewNodes []string, now func() time.Time) *Picker {
	if now == nil {
		now = time.Now
	}
	return &Picker{
		members:    members,
		reports:    reports,
		staleAfter: staleAfter,
		viewNodes:  append([]string(nil), viewNodes...),
		now:        now,
		held:       map[uint64]reservation{},
		rotate:     map[string]int{},
	}
}

// candidate is one routable node for a model, with the numbers the ordering
// rule needs.
type candidate struct {
	target Target
	free   int
	rtt    time.Duration
}

// Pick returns the node that should serve model, and a release func that must
// be called exactly once when the forward ends. Extra calls are no-ops.
//
// ok is false when no member can serve the model at all: an alias, a pipeline,
// a model still loading or an unknown name. The router forwards those to the
// view node, which resolves it or answers 404 or 503 — the gateway holds no
// alias table of its own.
//
// A candidate with zero free slots is still picked. The node queues and spills
// from there, which it can do with knowledge this gateway does not have.
func (p *Picker) Pick(model string, exclude map[string]bool) (Target, func(), bool) {
	now := p.now()
	members := p.members.Members()

	p.mu.Lock()
	defer p.mu.Unlock()

	var cands []candidate
	for _, m := range members {
		if !routable(m) || exclude[m.Name] {
			continue
		}
		rep, ok := p.reports.Report(m.Name)
		if !ok || !capacity.Fresh(rep, now, p.staleAfter) {
			continue
		}
		mc, ok := rep.Model(model)
		if !ok || mc.HealthyBackends <= 0 {
			continue
		}
		free := mc.Free() - p.reservedLocked(m.Name, model, rep.Received)
		if free < 0 {
			free = 0
		}
		cands = append(cands, candidate{
			target: Target{Node: m.Name, APIAddr: rep.APIAddr},
			free:   free,
			rtt:    rep.RTT,
		})
	}
	if len(cands) == 0 {
		return Target{}, func() {}, false
	}

	// Most free slots, then the lower round-trip time, then by name so the
	// rotation below is over a stable order.
	sort.Slice(cands, func(i, j int) bool {
		a, b := cands[i], cands[j]
		if a.free != b.free {
			return a.free > b.free
		}
		if a.rtt != b.rtt {
			return a.rtt < b.rtt
		}
		return a.target.Node < b.target.Node
	})

	// Rotate only among those tied with the leader on both free and RTT.
	tied := 1
	for tied < len(cands) && cands[tied].free == cands[0].free && cands[tied].rtt == cands[0].rtt {
		tied++
	}
	i := 0
	if tied > 1 {
		i = p.rotate[model] % tied
		p.rotate[model] = (p.rotate[model] + 1) % tied
	}
	chosen := cands[i].target

	id := p.nextID
	p.nextID++
	p.held[id] = reservation{node: chosen.Node, model: model, at: now}

	var once sync.Once
	release := func() {
		once.Do(func() {
			p.mu.Lock()
			delete(p.held, id)
			p.mu.Unlock()
		})
	}
	return chosen, release, true
}

// reservedLocked counts this gateway's in-flight forwards to (node, model)
// that the given report does not already account for.
//
// A reservation started before the report was received is already counted in
// that report's Busy, so counting it again would charge the same request
// twice and make a node look fuller than it is.
func (p *Picker) reservedLocked(node, model string, received time.Time) int {
	n := 0
	for _, r := range p.held {
		if r.node == node && r.model == model && r.at.After(received) {
			n++
		}
	}
	return n
}

// View returns the node that serves the dashboards, /v1/models, GET
// /v1/aliases, prompts and streams.
//
// The view node's HTML runs at this gateway's cookie-authenticated origin, so
// which node it is matters for more than latency. In a secured mesh membership
// is authenticated by the shared secret, and any member is a safe choice. In
// an open mesh any viiwork node that can reach the gossip port joins, so the
// operator must name the eligible nodes — config refuses to start otherwise.
func (p *Picker) View() (Target, bool) {
	now := p.now()
	members := p.members.Members()

	p.mu.Lock()
	defer p.mu.Unlock()

	eligible := map[string]Target{}
	for _, m := range members {
		if !routable(m) {
			continue
		}
		rep, ok := p.reports.Report(m.Name)
		if !ok || !capacity.Fresh(rep, now, p.staleAfter) {
			continue
		}
		// Any fresh report qualifies, whatever models it lists: serving the
		// dashboards needs no model at all.
		eligible[m.Name] = Target{Node: m.Name, APIAddr: rep.APIAddr}
	}
	if len(eligible) == 0 {
		p.view = ""
		return Target{}, false
	}

	// An allowlist is authoritative and ordered: the first name in it that is
	// eligible wins, so the operator's preference survives a node coming back.
	if len(p.viewNodes) > 0 {
		for _, name := range p.viewNodes {
			if t, ok := eligible[name]; ok {
				p.view = name
				return t, true
			}
		}
		p.view = ""
		return Target{}, false
	}

	// Otherwise keep the current choice while it stands, so a browser's SSE
	// stream is not moved to another node because a lower name appeared.
	if t, ok := eligible[p.view]; ok {
		return t, true
	}
	names := make([]string, 0, len(eligible))
	for name := range eligible {
		names = append(names, name)
	}
	sort.Strings(names)
	p.view = names[0]
	return eligible[names[0]], true
}

// routable reports whether a member can be sent a request at all: alive, a
// node rather than a gateway, and not this process.
func routable(m mesh.Member) bool {
	return m.State == meshapi.MemberAlive &&
		!m.Local &&
		m.Meta.Role == meshapi.RoleNode
}
