// Package config loads the gateway's settings from the environment.
package config

import (
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"
)

// Config is the gateway's full configuration. Every field has a default
// except Seeds, which has no sensible one.
type Config struct {
	Listen       string
	Seeds        []string
	PollInterval time.Duration
	PollTimeout  time.Duration
	CookieTTL    time.Duration
	MaxBody      int64
	// MaxInFlight bounds concurrent in-flight proxied requests (the
	// model-routed inference path and the sticky/view forwarding path — see
	// internal/proxy.Router). It exists to cap worst-case memory: each
	// in-flight inference request can buffer up to MaxBody bytes before it is
	// forwarded, so MaxInFlight * MaxBody is (roughly) the memory ceiling a
	// full-tilt attacker can force — at the defaults, 256 * 16MB ≈ 4GB, well
	// under a typical host's RAM but generous for real concurrent traffic.
	// Lower it on a memory-constrained host; requests over the cap fail fast
	// with 503 rather than queuing.
	MaxInFlight int
	// BodyReadTimeout bounds how long routeByModel or routeToView may take
	// to read a request body (see internal/proxy.Router). It is a per-request read
	// deadline set only for the duration of the body read — not the
	// http.Server-wide ReadTimeout, which would also fire while a long
	// inference response or SSE stream is still being written. A slow or
	// stalled body upload is severed after this long; a normal request
	// clears the deadline before the response begins so it never affects
	// streaming. See docs/security/adversarial-resource.md finding R2.
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
	RateBurst  int
	ViewPrefer string
	// AllowPrivatePeers opts discovered mesh peer addresses into RFC1918/ULA
	// private ranges, which internal/mesh's peer-address validation rejects
	// by default. It never affects loopback/link-local/unspecified addresses
	// (always rejected) or the tailnet's own 100.64.0.0/10 CGNAT range
	// (always allowed). See mesh.Options.AllowPrivatePeers.
	AllowPrivatePeers bool
}

// Load reads configuration through getenv. It takes a function rather than
// reading os.Getenv directly so tests need not mutate process state.
func Load(getenv func(string) string) (Config, error) {
	cfg := Config{
		Listen:          "127.0.0.1:8090",
		PollInterval:    10 * time.Second,
		PollTimeout:     2 * time.Second,
		CookieTTL:       720 * time.Hour,
		MaxBody:         16 * 1024 * 1024,
		MaxInFlight:     256,
		BodyReadTimeout: 30 * time.Second,
		RatePerMin:      120,
		RateBurst:       240,
	}

	if v := strings.TrimSpace(getenv("VIIWORK_GW_LISTEN")); v != "" {
		cfg.Listen = v
	}
	cfg.ViewPrefer = strings.TrimSpace(getenv("VIIWORK_GW_VIEW_PREFER"))

	for _, s := range strings.Split(getenv("VIIWORK_GW_SEEDS"), ",") {
		if s = strings.TrimSpace(s); s != "" {
			cfg.Seeds = append(cfg.Seeds, s)
		}
	}
	if len(cfg.Seeds) == 0 {
		return Config{}, fmt.Errorf("VIIWORK_GW_SEEDS is required: at least one mesh node address, e.g. 100.64.1.10:8080")
	}

	durations := []struct {
		env string
		dst *time.Duration
	}{
		{"VIIWORK_GW_POLL_INTERVAL", &cfg.PollInterval},
		{"VIIWORK_GW_POLL_TIMEOUT", &cfg.PollTimeout},
		{"VIIWORK_GW_COOKIE_TTL", &cfg.CookieTTL},
		{"VIIWORK_GW_BODY_READ_TIMEOUT", &cfg.BodyReadTimeout},
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

	if v := strings.TrimSpace(getenv("VIIWORK_GW_ALLOW_PRIVATE_PEERS")); v != "" {
		b, err := strconv.ParseBool(v)
		if err != nil {
			return Config{}, fmt.Errorf("VIIWORK_GW_ALLOW_PRIVATE_PEERS: %w", err)
		}
		cfg.AllowPrivatePeers = b
	}

	return cfg, nil
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
