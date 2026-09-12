// Package config loads the gateway's settings from the environment.
package config

import (
	"encoding/base64"
	"fmt"
	"math"
	"net/netip"
	"strconv"
	"strings"
	"time"
)

// Config is the gateway's full configuration. Every field has a default
// except the mesh mode, which the operator must choose explicitly.
type Config struct {
	Listen   string
	NodeName string
	Mesh     MeshConfig
	// ViewNodes names the nodes allowed to serve dashboards, /v1/models and
	// GET /v1/aliases. Empty means "any alive node with a fresh capacity
	// report", which is only safe in a secured mesh: membership is then
	// authenticated by the shared secret. In an open mesh any viiwork node
	// that can reach the gossip port joins, and the view node's HTML runs at
	// this gateway's cookie-authenticated origin — so an open mesh requires
	// this list.
	ViewNodes []string
	CookieTTL time.Duration
	MaxBody   int64
	// MaxInFlight bounds concurrent in-flight proxied requests (the
	// model-routed inference path and the view forwarding path — see
	// internal/proxy.Router). It exists to cap worst-case memory: each
	// in-flight inference request can buffer up to MaxBody bytes before it is
	// forwarded, so MaxInFlight * MaxBody is (roughly) the memory ceiling a
	// full-tilt attacker can force — at the defaults, 256 * 16MB ≈ 4GB, well
	// under a typical host's RAM but generous for real concurrent traffic.
	// Lower it on a memory-constrained host; requests over the cap fail fast
	// with 503 rather than queuing.
	MaxInFlight int
	// BodyReadTimeout bounds how long the router may take to read a request
	// body (see internal/proxy.Router). It is a per-request read deadline set
	// only for the duration of the body read — not the http.Server-wide
	// ReadTimeout, which would also fire while a long inference response or
	// SSE stream is still being written. A slow or stalled body upload is
	// severed after this long; a normal request clears the deadline before
	// the response begins so it never affects streaming. See
	// docs/security/adversarial-resource.md finding R2.
	BodyReadTimeout time.Duration
	// RatePerMin is the sustained requests-per-minute allowance for each API
	// key on the model-routed inference path (see internal/ratelimit). It
	// meters what MaxInFlight does not: MaxInFlight bounds concurrency, and
	// so memory, but leaves a single key free to issue unlimited *sequential*
	// requests. Zero switches rate limiting off entirely — deliberately
	// unlike MaxInFlight, where a non-positive value is an error, because
	// "no limit" is a coherent (if unwise) choice for a closed deployment.
	RatePerMin float64
	// RateBurst is the token-bucket capacity: the most requests a key may
	// spend at once after an idle spell. It should exceed RatePerMin's
	// per-second rate comfortably, or normal bursty client behaviour (an SDK
	// firing a handful of parallel calls) trips the limit.
	RateBurst int
}

// MeshConfig is what the gateway needs to join a viiwork 2 mesh and keep its
// members' capacity fresh. It mirrors the plain values mesh.Options takes:
// the gateway imports viiwork's mesh package from another module and has no
// viiwork config of its own.
type MeshConfig struct {
	Network   string     // "tailnet" | "lan"
	BindPort  int        // gossip, tcp+udp
	Advertise netip.Addr // zero = resolve from the network
	Open      bool
	SecretKey []byte // nil = open mesh
	// PreviousKey lets a secret rotate without a flag day: members still on
	// the old key can be decrypted while they catch up.
	PreviousKey    []byte
	Enforce        string // "full" | "outgoing" | "none"
	Tailnet        bool   // resolved from auto
	TailnetSocket  string
	MDNS           bool     // resolved from auto
	Seeds          []string // "ip:port"
	RejoinInterval time.Duration
	// CapacityPoll is how often each member's /v1/capacity is read, and
	// StaleAfter is how old a report may be before the node stops being a
	// routing candidate. StaleAfter must comfortably exceed CapacityPoll or
	// every report is stale on arrival.
	CapacityPoll time.Duration
	StaleAfter   time.Duration
}

// Mode reports which mesh the gateway is configured to join, for logs and
// error messages.
func (c Config) Mode() string {
	if c.Mesh.SecretKey != nil {
		return "secured"
	}
	return "open"
}

// removed maps each v1 variable to the error a deployment still carrying it
// gets. They are checked before anything else: an old VIIWORK_GW_SEEDS holds
// API addresses (:8080), and reusing it as a gossip seed list would fail
// quietly at join time rather than loudly at startup.
var removed = []struct{ env, msg string }{
	{"VIIWORK_GW_SEEDS", "VIIWORK_GW_SEEDS is removed: use VIIWORK_GW_MESH_SEEDS with gossip addresses (ip:7946); seeds are optional on the tailnet"},
	{"VIIWORK_GW_POLL_INTERVAL", "VIIWORK_GW_POLL_INTERVAL is removed: capacity is polled every VIIWORK_GW_CAPACITY_POLL"},
	{"VIIWORK_GW_POLL_TIMEOUT", "VIIWORK_GW_POLL_TIMEOUT is removed: capacity is polled every VIIWORK_GW_CAPACITY_POLL"},
	{"VIIWORK_GW_ALLOW_PRIVATE_PEERS", "VIIWORK_GW_ALLOW_PRIVATE_PEERS is removed: set VIIWORK_GW_MESH_NETWORK=lan for a LAN mesh"},
	{"VIIWORK_GW_VIEW_PREFER", "VIIWORK_GW_VIEW_PREFER is removed: use VIIWORK_GW_VIEW_NODES (node names)"},
}

// Load reads configuration through getenv. It takes functions rather than
// reading os.Getenv and os.Hostname directly so tests need not mutate process
// state.
func Load(getenv func(string) string, hostname func() (string, error)) (Config, error) {
	for _, r := range removed {
		if strings.TrimSpace(getenv(r.env)) != "" {
			return Config{}, fmt.Errorf("%s", r.msg)
		}
	}

	cfg := Config{
		Listen:          "127.0.0.1:8090",
		CookieTTL:       720 * time.Hour,
		MaxBody:         16 * 1024 * 1024,
		MaxInFlight:     256,
		BodyReadTimeout: 30 * time.Second,
		RatePerMin:      120,
		RateBurst:       240,
		Mesh: MeshConfig{
			Network:        "tailnet",
			BindPort:       7946,
			Enforce:        "full",
			TailnetSocket:  "/var/run/tailscale/tailscaled.sock",
			RejoinInterval: 60 * time.Second,
			CapacityPoll:   time.Second,
			StaleAfter:     3 * time.Second,
		},
	}

	if v := strings.TrimSpace(getenv("VIIWORK_GW_LISTEN")); v != "" {
		cfg.Listen = v
	}

	name := strings.TrimSpace(getenv("VIIWORK_GW_NODE_NAME"))
	if name == "" {
		h, err := hostname()
		if err != nil {
			return Config{}, fmt.Errorf("VIIWORK_GW_NODE_NAME is not set and the hostname cannot be read: %w", err)
		}
		name = strings.TrimSpace(h)
	}
	if name == "" {
		return Config{}, fmt.Errorf("VIIWORK_GW_NODE_NAME is empty: the mesh identifies members by name")
	}
	if strings.ContainsFunc(name, func(r rune) bool { return r == ' ' || r == '\t' || r == '\n' || r == '\r' }) {
		return Config{}, fmt.Errorf("VIIWORK_GW_NODE_NAME must not contain whitespace, got %q", name)
	}
	cfg.NodeName = name

	if err := loadMesh(getenv, &cfg); err != nil {
		return Config{}, err
	}

	nodes, err := splitList(getenv("VIIWORK_GW_VIEW_NODES"), "VIIWORK_GW_VIEW_NODES")
	if err != nil {
		return Config{}, err
	}
	cfg.ViewNodes = nodes
	if cfg.Mesh.Open && len(cfg.ViewNodes) == 0 {
		return Config{}, fmt.Errorf("VIIWORK_GW_VIEW_NODES is required in an open mesh: name the nodes allowed to serve dashboards")
	}

	durations := []struct {
		env string
		dst *time.Duration
	}{
		{"VIIWORK_GW_COOKIE_TTL", &cfg.CookieTTL},
		{"VIIWORK_GW_BODY_READ_TIMEOUT", &cfg.BodyReadTimeout},
		{"VIIWORK_GW_MESH_REJOIN_INTERVAL", &cfg.Mesh.RejoinInterval},
		{"VIIWORK_GW_CAPACITY_POLL", &cfg.Mesh.CapacityPoll},
		{"VIIWORK_GW_STALE_AFTER", &cfg.Mesh.StaleAfter},
	}
	for _, d := range durations {
		v := strings.TrimSpace(getenv(d.env))
		if v == "" {
			continue
		}
		parsed, err := time.ParseDuration(v)
		if err != nil {
			return Config{}, fmt.Errorf("%s: %w", d.env, err)
		}
		if parsed <= 0 {
			return Config{}, fmt.Errorf("%s must be positive, got %v", d.env, parsed)
		}
		*d.dst = parsed
	}

	if v := strings.TrimSpace(getenv("VIIWORK_GW_MAX_BODY")); v != "" {
		size, err := ParseSize(v)
		if err != nil {
			return Config{}, fmt.Errorf("VIIWORK_GW_MAX_BODY: %w", err)
		}
		cfg.MaxBody = size
	}

	if v := strings.TrimSpace(getenv("VIIWORK_GW_MAX_INFLIGHT")); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			return Config{}, fmt.Errorf("VIIWORK_GW_MAX_INFLIGHT: not an integer: %q", v)
		}
		if n <= 0 {
			return Config{}, fmt.Errorf("VIIWORK_GW_MAX_INFLIGHT must be positive, got %d", n)
		}
		cfg.MaxInFlight = n
	}

	// Parsed as a float so an operator can express a slow allowance ("0.5"
	// = one request every two minutes) without a second unit.
	if v := strings.TrimSpace(getenv("VIIWORK_GW_RATE_PER_MIN")); v != "" {
		n, err := strconv.ParseFloat(v, 64)
		if err != nil {
			return Config{}, fmt.Errorf("VIIWORK_GW_RATE_PER_MIN: not a number: %q", v)
		}
		if n < 0 {
			return Config{}, fmt.Errorf("VIIWORK_GW_RATE_PER_MIN must not be negative, got %v (use 0 to disable rate limiting)", n)
		}
		cfg.RatePerMin = n
	}

	if v := strings.TrimSpace(getenv("VIIWORK_GW_RATE_BURST")); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			return Config{}, fmt.Errorf("VIIWORK_GW_RATE_BURST: not an integer: %q", v)
		}
		if n <= 0 {
			return Config{}, fmt.Errorf("VIIWORK_GW_RATE_BURST must be positive, got %d (use VIIWORK_GW_RATE_PER_MIN=0 to disable rate limiting)", n)
		}
		cfg.RateBurst = n
	}

	return cfg, nil
}

// loadMesh fills cfg.Mesh, and is where the secured-or-open choice is made.
func loadMesh(getenv func(string) string, cfg *Config) error {
	m := &cfg.Mesh

	switch v := strings.TrimSpace(getenv("VIIWORK_GW_MESH_NETWORK")); v {
	case "":
	case "tailnet", "lan":
		m.Network = v
	default:
		return fmt.Errorf("VIIWORK_GW_MESH_NETWORK must be tailnet or lan, got %q", v)
	}

	if v := strings.TrimSpace(getenv("VIIWORK_GW_MESH_BIND_PORT")); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			return fmt.Errorf("VIIWORK_GW_MESH_BIND_PORT: not an integer: %q", v)
		}
		if n < 1 || n > 65535 {
			return fmt.Errorf("VIIWORK_GW_MESH_BIND_PORT must be 1-65535, got %d", n)
		}
		m.BindPort = n
	}

	if v := strings.TrimSpace(getenv("VIIWORK_GW_MESH_ADVERTISE")); v != "" {
		addr, err := netip.ParseAddr(v)
		if err != nil {
			return fmt.Errorf("VIIWORK_GW_MESH_ADVERTISE: not an IP address: %q", v)
		}
		m.Advertise = addr
	}

	if v := strings.TrimSpace(getenv("VIIWORK_GW_MESH_OPEN")); v != "" {
		b, err := strconv.ParseBool(v)
		if err != nil {
			return fmt.Errorf("VIIWORK_GW_MESH_OPEN: %w", err)
		}
		m.Open = b
	}

	secret := strings.TrimSpace(getenv("VIIWORK_MESH_SECRET"))
	prev := strings.TrimSpace(getenv("VIIWORK_MESH_SECRET_PREV"))

	if m.Open && secret != "" {
		return fmt.Errorf("VIIWORK_GW_MESH_OPEN is true but VIIWORK_MESH_SECRET is set: choose a secured mesh or an open one, not both")
	}
	if !m.Open && secret == "" {
		return fmt.Errorf("VIIWORK_MESH_SECRET is not set and VIIWORK_GW_MESH_OPEN is not true: set VIIWORK_MESH_SECRET (base64 of 32 bytes, openssl rand -base64 32) or declare VIIWORK_GW_MESH_OPEN=true")
	}

	if secret != "" {
		key, err := decodeKey(secret, "VIIWORK_MESH_SECRET")
		if err != nil {
			return err
		}
		m.SecretKey = key
	}
	if prev != "" {
		if m.SecretKey == nil {
			return fmt.Errorf("VIIWORK_MESH_SECRET_PREV is set without VIIWORK_MESH_SECRET: a previous key is only meaningful while rotating to a current one")
		}
		key, err := decodeKey(prev, "VIIWORK_MESH_SECRET_PREV")
		if err != nil {
			return err
		}
		m.PreviousKey = key
	}

	switch v := strings.TrimSpace(getenv("VIIWORK_GW_MESH_ENFORCE")); v {
	case "":
	case "full", "outgoing", "none":
		m.Enforce = v
	default:
		return fmt.Errorf("VIIWORK_GW_MESH_ENFORCE must be full, outgoing or none, got %q", v)
	}
	if m.Enforce != "full" && m.SecretKey == nil {
		return fmt.Errorf("VIIWORK_GW_MESH_ENFORCE is %q without VIIWORK_MESH_SECRET: there is nothing to enforce in an open mesh", m.Enforce)
	}

	tailnet, err := parseAuto(getenv("VIIWORK_GW_MESH_TAILNET"), "VIIWORK_GW_MESH_TAILNET", m.Network == "tailnet")
	if err != nil {
		return err
	}
	m.Tailnet = tailnet

	mdns, err := parseAuto(getenv("VIIWORK_GW_MESH_MDNS"), "VIIWORK_GW_MESH_MDNS", m.Network == "lan")
	if err != nil {
		return err
	}
	m.MDNS = mdns

	if v := strings.TrimSpace(getenv("VIIWORK_GW_TAILSCALE_SOCKET")); v != "" {
		m.TailnetSocket = v
	}

	seeds, err := splitList(getenv("VIIWORK_GW_MESH_SEEDS"), "VIIWORK_GW_MESH_SEEDS")
	if err != nil {
		return err
	}
	for _, s := range seeds {
		// memberlist dials a literal; a hostname here would resolve at join
		// time or not at all, and the failure would look like an unreachable
		// peer rather than a typo.
		if _, err := netip.ParseAddrPort(s); err != nil {
			return fmt.Errorf("VIIWORK_GW_MESH_SEEDS: %q is not an ip:port gossip address (for example 100.64.0.2:%d)", s, m.BindPort)
		}
	}
	m.Seeds = seeds

	return nil
}

// parseAuto reads a three-state flag: "auto" (or unset) takes the default the
// network implies, and true/false override it.
func parseAuto(raw, env string, auto bool) (bool, error) {
	v := strings.TrimSpace(raw)
	if v == "" || v == "auto" {
		return auto, nil
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		return false, fmt.Errorf("%s must be auto, true or false, got %q", env, v)
	}
	return b, nil
}

// decodeKey reads a mesh secret. Its error names the variable and never the
// value, so a secret cannot leak into a log through a startup failure.
func decodeKey(v, env string) ([]byte, error) {
	key, err := base64.StdEncoding.DecodeString(v)
	if err != nil {
		return nil, fmt.Errorf("%s is not standard base64 (generate one with: openssl rand -base64 32)", env)
	}
	if len(key) != 32 {
		return nil, fmt.Errorf("%s must decode to exactly 32 bytes, got %d", env, len(key))
	}
	return key, nil
}

// splitList parses a comma-separated list, rejecting empty and repeated
// entries: both are typos that a silent skip would hide.
func splitList(raw, env string) ([]string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	seen := make(map[string]bool, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			return nil, fmt.Errorf("%s has an empty entry: %q", env, raw)
		}
		if seen[p] {
			return nil, fmt.Errorf("%s lists %q twice", env, p)
		}
		seen[p] = true
		out = append(out, p)
	}
	return out, nil
}

// ParseSize accepts a plain byte count or a KB/MB/GB suffix, case-insensitive.
// Suffixes are binary multiples, which is what an operator sizing a request
// body against RAM actually means.
func ParseSize(s string) (int64, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, fmt.Errorf("empty size")
	}
	original := s
	mult := int64(1)
	upper := strings.ToUpper(s)
	for _, suf := range []struct {
		name string
		mult int64
	}{
		{"KB", 1024},
		{"MB", 1024 * 1024},
		{"GB", 1024 * 1024 * 1024},
	} {
		if strings.HasSuffix(upper, suf.name) {
			mult = suf.mult
			s = strings.TrimSpace(s[:len(s)-len(suf.name)])
			break
		}
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("not a size: %q", s)
	}
	if n <= 0 {
		return 0, fmt.Errorf("size must be positive, got %d", n)
	}
	if n > math.MaxInt64/mult {
		return 0, fmt.Errorf("size %q overflows: too large to represent in bytes", original)
	}
	return n * mult, nil
}
