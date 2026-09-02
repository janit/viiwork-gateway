package mesh

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode"

	"github.com/janit/viiwork/meshapi"
)

// FailuresBeforeUnhealthy is how many consecutive failed status polls take a
// node out of rotation. It stays in the set afterwards, so it rejoins by
// itself without any operator action.
const FailuresBeforeUnhealthy = 3

// MaxKnownNodes bounds how many distinct peer addresses the registry will
// ever track. known never shrinks — a node that stops responding stays in
// the set as unhealthy rather than being evicted — so without a cap, a node
// advertising fresh addresses every poll grows it without bound. Each entry
// costs one goroutine and one TCP dial per poll interval, forever, and a
// phantom address is never routable or electable: it contributes nothing but
// resource use. A few hundred is generous for any real fleet; a misconfigured
// or hostile node advertising thousands would otherwise be able to exhaust
// the process's file descriptors and starve the proxy's own connections.
const MaxKnownNodes = 500

// maxPollResponseBytes bounds how much of a single /v1/status or /v1/cluster
// response getJSON will read before giving up. It is a few times larger than
// any real payload a viiwork node produces (a status/cluster response is a
// handful of KB even with a generous model and peer list), so it never
// trips on legitimate traffic, but it stops a node — compromised, buggy, or
// just misbehaving — from making one poll balloon the gateway's memory: a
// response over this bound fails the poll instead of being decoded, partially
// or otherwise.
const maxPollResponseBytes = 4 * 1024 * 1024

// Options configures a Registry.
type Options struct {
	Seeds      []string
	Interval   time.Duration
	Timeout    time.Duration
	ViewPrefer string
	// DiscoveryEvery is how many rounds pass between full-mesh cluster polls.
	// The view node is cluster-polled every round regardless; this controls
	// how often every other node is asked, which is what keeps PeerCount
	// current and finds peers the view node does not know. 0 means 6.
	DiscoveryEvery int
	// AllowPrivatePeers opts a discovered peer address into RFC1918/ULA
	// private ranges (10/8, 172.16/12, 192.168/16, fc00::/7), which
	// validPeerAddr otherwise rejects by default. It never affects loopback,
	// link-local, or unspecified addresses — those are always rejected — and
	// it never affects seeds, which are operator-chosen and exempt from
	// validation entirely. The tailnet's own CGNAT range (100.64.0.0/10) is
	// not RFC1918 and is always allowed regardless of this setting: real
	// viiwork nodes live there.
	AllowPrivatePeers bool
	Client            *http.Client
	Logger            *slog.Logger
}

// Registry keeps the live mesh view. Readers take no lock: each round builds a
// fresh Snapshot and swaps the pointer.
type Registry struct {
	opts   Options
	client *http.Client
	log    *slog.Logger

	mu    sync.Mutex
	known map[string]*Node

	snap  atomic.Pointer[Snapshot]
	rr    atomic.Uint64
	round atomic.Uint64

	degradedLogged atomic.Bool
	knownCapLogged atomic.Bool

	// testSkipPeerValidation disables validPeerAddr for discovered peers. It
	// exists only for tests in this package that exercise discovery
	// mechanics with httptest servers, which necessarily bind to loopback —
	// set directly from a _test.go file in this package (see
	// testRegistryPermissive in registry_test.go), never exposed as a public
	// method: nothing outside this package, and nothing in a production
	// build, has any way to disable peer-address validation. Never set
	// outside a test, and never after the first Round.
	testSkipPeerValidation bool
}

// New creates a Registry seeded with the configured addresses. It does not
// poll; call Round or Run.
func New(opts Options) *Registry {
	if opts.Timeout <= 0 {
		opts.Timeout = 2 * time.Second
	}
	if opts.Interval <= 0 {
		opts.Interval = 10 * time.Second
	}
	if opts.DiscoveryEvery <= 0 {
		opts.DiscoveryEvery = 6
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	client := opts.Client
	if client == nil {
		client = &http.Client{Timeout: opts.Timeout}
	}

	r := &Registry{
		opts:   opts,
		client: client,
		log:    opts.Logger,
		known:  make(map[string]*Node),
	}
	for _, addr := range opts.Seeds {
		// Seed: true marks this an operator-configured address, never a
		// peer's self-report — clusterRound's discovered-node branch below
		// must never set it. electView restricts view eligibility to Seed
		// nodes; see snapshot.go and F3 in docs/security/adversarial-mesh.md.
		r.known[addr] = &Node{Addr: addr, Seed: true}
	}
	r.snap.Store(BuildSnapshot(map[string]*Node{}, opts.ViewPrefer))
	return r
}

// Snapshot returns the current view. Never nil.
func (r *Registry) Snapshot() *Snapshot { return r.snap.Load() }

// Run polls until ctx is cancelled.
func (r *Registry) Run(ctx context.Context) {
	r.Round(ctx)
	t := time.NewTicker(r.opts.Interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			r.Round(ctx)
		}
	}
}

// Round performs one discovery pass: status-poll every known node, then
// cluster-poll for topology.
func (r *Registry) Round(ctx context.Context) {
	n := r.round.Add(1)

	r.mu.Lock()
	addrs := make([]string, 0, len(r.known))
	for addr := range r.known {
		addrs = append(addrs, addr)
	}
	r.mu.Unlock()

	r.statusRound(ctx, addrs)

	// Topology. The view node every round, everyone else on the slower
	// cadence: PeerCount drives view election and can only be read here.
	targets := []string{}
	if view := r.Snapshot().View(); view != "" {
		targets = append(targets, view)
	}
	if uint64(r.opts.DiscoveryEvery) <= 1 || n%uint64(r.opts.DiscoveryEvery) == 1 {
		targets = addrs
	}
	r.clusterRound(ctx, targets)

	r.rebuild()
}

func (r *Registry) statusRound(ctx context.Context, addrs []string) {
	type result struct {
		addr   string
		status *meshapi.StatusResponse
	}
	results := make(chan result, len(addrs))

	var wg sync.WaitGroup
	for _, addr := range addrs {
		wg.Add(1)
		go func(addr string) {
			defer wg.Done()
			s, err := r.pollStatus(ctx, addr)
			if err != nil {
				results <- result{addr: addr}
				return
			}
			results <- result{addr: addr, status: s}
		}(addr)
	}
	wg.Wait()
	close(results)

	// Apply status updates under the lock, but never make a network call
	// while holding it: probeChat does an HTTP round-trip with a timeout, and
	// doing that here would stall every other registry writer, serially, for
	// up to that timeout each time a node transitions to healthy. Instead,
	// collect the addresses that need probing while locked, release the
	// lock, probe concurrently, then take the lock again just to record the
	// results.
	var toProbe []string
	r.mu.Lock()
	for res := range results {
		n := r.known[res.addr]
		if n == nil {
			continue
		}
		if res.status == nil {
			n.Fails++
			if n.Fails >= FailuresBeforeUnhealthy {
				n.Healthy = false
			}
			continue
		}
		wasHealthy := n.Healthy
		n.Fails = 0
		n.Healthy = true
		n.Hostname = res.status.Hostname
		n.Models = res.status.Models
		if res.status.TotalInFlight < 0 {
			// A negative total_in_flight is implausible on its face — no
			// genuine load counter goes negative — and CandidatesForModel
			// narrows to whichever nodes tie at the minimum InFlightKnown
			// value, so trusting it would let a single lying node forge
			// itself into the unique lowest-load slot and capture every
			// pick for a model. Treat it exactly like a poll that never
			// reported a figure at all: unknown, not a measured value.
			n.InFlight = 0
			n.InFlightKnown = false
		} else {
			n.InFlight = res.status.TotalInFlight
			// StatusResponse.TotalInFlight is not omitempty, so a successful
			// poll always carries a real figure. This flag is what stops a
			// node we have only heard about from being treated as idle.
			n.InFlightKnown = true
		}
		n.HealthyBackends = res.status.HealthyBackends

		if !wasHealthy {
			toProbe = append(toProbe, res.addr)
		}
	}
	r.mu.Unlock()

	if len(toProbe) == 0 {
		return
	}

	type probeResult struct {
		addr   string
		fullUI bool
	}
	probeResults := make(chan probeResult, len(toProbe))
	var pwg sync.WaitGroup
	for _, addr := range toProbe {
		pwg.Add(1)
		go func(addr string) {
			defer pwg.Done()
			probeResults <- probeResult{addr: addr, fullUI: r.probeChat(ctx, addr)}
		}(addr)
	}
	pwg.Wait()
	close(probeResults)

	r.mu.Lock()
	defer r.mu.Unlock()
	for res := range probeResults {
		if n := r.known[res.addr]; n != nil {
			n.FullUI = res.fullUI
		}
	}
}

func (r *Registry) clusterRound(ctx context.Context, addrs []string) {
	type result struct {
		addr    string
		cluster *meshapi.ClusterResponse
	}
	results := make(chan result, len(addrs))

	var wg sync.WaitGroup
	for _, addr := range addrs {
		wg.Add(1)
		go func(addr string) {
			defer wg.Done()
			c, err := r.pollCluster(ctx, addr)
			if err != nil {
				results <- result{addr: addr}
				return
			}
			results <- result{addr: addr, cluster: c}
		}(addr)
	}
	wg.Wait()
	close(results)

	r.mu.Lock()
	defer r.mu.Unlock()
	var rejected int
	for res := range results {
		if res.cluster == nil {
			continue
		}
		if n := r.known[res.addr]; n != nil {
			n.PeerCount = len(res.cluster.Peers)
		}
		for _, p := range res.cluster.Peers {
			if p.Addr == "" || r.known[p.Addr] != nil {
				continue
			}
			// A discovered address is another node's self-report, not
			// operator input — unlike a seed, it must earn its way into
			// known and onto the dial path. See validPeerAddr's doc comment
			// for exactly what this closes (SSRF via advertised peers).
			if !r.testSkipPeerValidation {
				if err := validPeerAddr(p.Addr, r.opts.AllowPrivatePeers); err != nil {
					rejected++
					continue
				}
			}
			if len(r.known) >= MaxKnownNodes {
				// One line, however many addresses overflow this round: a
				// flood of advertised addresses must not become a flood of
				// log lines.
				if !r.knownCapLogged.Swap(true) {
					r.log.Warn("discovery intake capped; further advertised peer addresses are ignored",
						"cap", MaxKnownNodes)
				}
				continue
			}
			// Discovered, but not yet trusted for anything. It becomes
			// routable only once its own status poll succeeds — a peer's
			// report is hearsay, and its load figures are omitempty. Seed is
			// left false (the zero value): a discovered node must never be
			// eligible for view election, however it inflates its own
			// self-reported PeerCount or answers the /chat probe.
			r.known[p.Addr] = &Node{Addr: p.Addr, Hostname: p.Hostname}
		}
	}
	if rejected > 0 {
		// One aggregated line per round, however many addresses were
		// rejected: a hostile node flooding bad advertisements must not
		// become a log flood in its own right.
		r.log.Warn("discovered peer address(es) rejected by validation; dropped, never dialed",
			"count", rejected)
	}
}

// tailscaleCGNAT is the tailnet's own address space (RFC6598 shared/CGNAT
// space, not RFC1918): real viiwork nodes live here, and it must always be
// reachable regardless of AllowPrivatePeers.
var tailscaleCGNAT = netip.MustParsePrefix("100.64.0.0/10")

// privatePeerPrefixes are the RFC1918/ULA ranges a discovered peer address
// may additionally use, but only once an operator opts in via
// AllowPrivatePeers — some deployments legitimately peer over a plain LAN.
var privatePeerPrefixes = []netip.Prefix{
	netip.MustParsePrefix("10.0.0.0/8"),
	netip.MustParsePrefix("172.16.0.0/12"),
	netip.MustParsePrefix("192.168.0.0/16"),
	netip.MustParsePrefix("fc00::/7"),
}

// validPeerAddr reports whether addr is safe to add to known and dial as a
// discovered mesh peer. Unlike a seed — operator-chosen, exempt from this
// check entirely — a discovered address is another node's self-report:
// clusterRound is about to insert it into known, and the next Round will
// dial "http://"+addr+path against it, so trusting it blindly hands whoever
// controls that self-report the gateway's own outbound requests, aimed
// wherever they like.
//
// addr must be a strict "host:port" pair with nothing else in it. Any of
// '/', '?', '#', '@', or whitespace is rejected outright: each is exactly
// what would let an advertised value carry a path, query, fragment, or
// userinfo component into the URL that gets dialed, rather than just a host
// and port (e.g. "internal-host/latest/meta-data#" turns the appended
// "/v1/status" into a URL fragment, so the real path requested is whatever
// follows the '#'). The port must be canonical decimal — no leading '+', no
// leading zero beyond "0" itself — so a value like "host:+80" can't slip a
// non-numeric-looking but Atoi-parseable string into the address that
// actually gets dialed.
//
// The host must be an IP literal (net/netip). A bare hostname is rejected
// rather than resolved here — deliberately, and this is the one judgement
// call in this function worth calling out: resolving DNS inside the poll's
// hot path would add a network round trip to every discovery round, and a
// name that resolves safely today can resolve to a loopback or metadata
// address tomorrow (classic DNS-rebinding), reopening exactly the hole this
// function exists to close. Real viiwork nodes report the interface address
// they're listening on, which is always an IP literal, so this loses no
// legitimate functionality.
//
// This is an ALLOW-list, not a deny-list: a discovered address must resolve
// (after Unmap, so IPv4-mapped IPv6 collapses to its IPv4 form) into one of
// two buckets, or it is rejected outright. The first bucket, always allowed,
// is the tailnet's own 100.64.0.0/10 CGNAT range — the exact range real
// viiwork nodes live in (100.64.0.0 and 100.127.255.255 are in;
// 100.63.255.255 and 100.128.0.0 are not). The second, allowed only when
// allowPrivate is set, is RFC1918/ULA private space (10/8, 172.16/12,
// 192.168/16, fc00::/7). Everything else — every public IPv4 or IPv6
// address, loopback, link-local, unspecified, and the NAT64 well-known
// prefix 64:ff9b::/96 (which embeds an arbitrary IPv4 address, including
// 169.254.169.254, inside what otherwise looks like an ordinary global IPv6
// address) — falls outside both buckets and is rejected by default-deny,
// with no need to enumerate it explicitly.
func validPeerAddr(addr string, allowPrivate bool) error {
	if addr == "" {
		return fmt.Errorf("empty address")
	}
	if strings.ContainsAny(addr, "/?#@") {
		return fmt.Errorf("%q contains a path, query, fragment, or userinfo character", addr)
	}
	for _, r := range addr {
		if unicode.IsSpace(r) {
			return fmt.Errorf("%q contains whitespace", addr)
		}
	}
	host, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("%q is not host:port: %w", addr, err)
	}
	if host == "" {
		return fmt.Errorf("%q has an empty host", addr)
	}
	if _, err := strictDecimalPort(portStr); err != nil {
		return fmt.Errorf("%q has an invalid port: %w", addr, err)
	}
	ip, err := netip.ParseAddr(host)
	if err != nil {
		return fmt.Errorf("%q host %q is not an IP literal", addr, host)
	}
	ip = ip.Unmap()

	if tailscaleCGNAT.Contains(ip) {
		return nil
	}
	if allowPrivate {
		for _, p := range privatePeerPrefixes {
			if p.Contains(ip) {
				return nil
			}
		}
	}
	return fmt.Errorf("%q (%s) is not in an allowed discovered-peer range", addr, ip)
}

// strictDecimalPort parses a port string the way validPeerAddr requires:
// canonical decimal only. strconv.Atoi accepts a leading '+' and leading
// zeros ("+80", "0080"), which would let an advertised address carry a
// non-canonical port string, unchanged, into known and onto the dial path —
// harmless for the dial itself, but an unnecessary crack for anything else
// downstream that might compare or log the raw address string.
func strictDecimalPort(s string) (int, error) {
	if s == "" {
		return 0, fmt.Errorf("empty port")
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0, fmt.Errorf("%q is not decimal", s)
		}
	}
	if len(s) > 1 && s[0] == '0' {
		return 0, fmt.Errorf("%q has a leading zero", s)
	}
	n, err := strconv.ParseUint(s, 10, 32)
	if err != nil {
		return 0, err
	}
	if n < 1 || n > 65535 {
		return 0, fmt.Errorf("%d is out of range", n)
	}
	return int(n), nil
}

func (r *Registry) rebuild() {
	r.mu.Lock()
	nodes := make(map[string]*Node, len(r.known))
	for addr, n := range r.known {
		clone := *n
		// *n copies every plain field by value, Seed included — view
		// election's seed-only eligibility (electView) depends on that
		// carrying over unchanged into each snapshot. It does not copy the
		// slice header's backing array: clone.Models would otherwise alias
		// n.Models, which is safe only because statusRound always replaces
		// the slice wholesale rather than mutating it in place — an
		// invariant that lives in a different function and that a future
		// edit there could silently break into a data race between this
		// reader-side goroutine and the poller.
		clone.Models = slices.Clone(n.Models)
		nodes[addr] = &clone
	}
	r.mu.Unlock()

	snap := BuildSnapshot(nodes, r.opts.ViewPrefer)
	r.snap.Store(snap)

	if snap.ViewDegraded() && !r.degradedLogged.Swap(true) {
		r.log.Warn("view node does not serve the full UI surface; /chat will 404",
			"view", snap.View())
	} else if !snap.ViewDegraded() {
		r.degradedLogged.Store(false)
	}
}

func (r *Registry) pollStatus(ctx context.Context, addr string) (*meshapi.StatusResponse, error) {
	var out meshapi.StatusResponse
	if err := r.getJSON(ctx, addr, meshapi.PathStatus, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func (r *Registry) pollCluster(ctx context.Context, addr string) (*meshapi.ClusterResponse, error) {
	var out meshapi.ClusterResponse
	if err := r.getJSON(ctx, addr, meshapi.PathCluster, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func (r *Registry) getJSON(ctx context.Context, addr, path string, dst any) error {
	ctx, cancel := context.WithTimeout(ctx, r.opts.Timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+addr+path, nil)
	if err != nil {
		return err
	}
	resp, err := r.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, resp.Body)
		return fmt.Errorf("%s%s: status %d", addr, path, resp.StatusCode)
	}

	// Read into a capped buffer rather than decoding the body directly: a
	// hostile or merely buggy node can otherwise return an arbitrarily large
	// body and the decoder will happily allocate for all of it. Reading one
	// byte past the bound lets a body that is exactly at the limit succeed
	// while anything larger is detected before it is ever handed to
	// json.Unmarshal — a response over the bound fails the whole poll (the
	// node is treated as unreachable this round) rather than being decoded
	// partially or truncated.
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxPollResponseBytes+1))
	if err != nil {
		return fmt.Errorf("%s%s: reading response: %w", addr, path, err)
	}
	if len(body) > maxPollResponseBytes {
		return fmt.Errorf("%s%s: response exceeds %d byte limit", addr, path, maxPollResponseBytes)
	}
	return json.Unmarshal(body, dst)
}

// probeChat records whether a node serves the full dashboard surface.
// viiwork does; viiwork-nvidia, the mesh's second implementation, does not.
func (r *Registry) probeChat(ctx context.Context, addr string) bool {
	ctx, cancel := context.WithTimeout(ctx, r.opts.Timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+addr+"/chat", nil)
	if err != nil {
		return false
	}
	resp, err := r.client.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.StatusCode == http.StatusOK
}

// PickForModel returns the node that should serve a model, round-robining
// among those tied at the lowest reported load.
func (r *Registry) PickForModel(model string) (string, bool) {
	candidates := r.Snapshot().CandidatesForModel(model)
	if len(candidates) == 0 {
		return "", false
	}
	i := r.rr.Add(1) - 1
	return candidates[int(i%uint64(len(candidates)))].Addr, true
}

// ViewAddr returns the node serving dashboards and per-node views.
func (r *Registry) ViewAddr() (string, bool) {
	view := r.Snapshot().View()
	return view, view != ""
}

// SetSnapshotForTest replaces the current snapshot. It exists so tests in
// other packages can exercise routing against a fixed mesh view without
// standing up pollable nodes.
func (r *Registry) SetSnapshotForTest(s *Snapshot) { r.snap.Store(s) }
