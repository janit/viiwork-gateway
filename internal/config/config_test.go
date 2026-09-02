package config

import (
	"testing"
	"time"
)

func envFrom(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func TestLoadDefaults(t *testing.T) {
	cfg, err := Load(envFrom(map[string]string{
		"VIIWORK_GW_SEEDS": "100.64.1.10:8080",
	}))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Listen != "127.0.0.1:8090" {
		t.Errorf("Listen = %q, want 127.0.0.1:8090", cfg.Listen)
	}
	if cfg.PollInterval != 10*time.Second {
		t.Errorf("PollInterval = %v, want 10s", cfg.PollInterval)
	}
	if cfg.PollTimeout != 2*time.Second {
		t.Errorf("PollTimeout = %v, want 2s", cfg.PollTimeout)
	}
	if cfg.CookieTTL != 720*time.Hour {
		t.Errorf("CookieTTL = %v, want 720h", cfg.CookieTTL)
	}
	if cfg.MaxBody != 16*1024*1024 {
		t.Errorf("MaxBody = %d, want 16MiB", cfg.MaxBody)
	}
	if cfg.MaxInFlight != 256 {
		t.Errorf("MaxInFlight = %d, want 256", cfg.MaxInFlight)
	}
	if cfg.BodyReadTimeout != 30*time.Second {
		t.Errorf("BodyReadTimeout = %v, want 30s", cfg.BodyReadTimeout)
	}
	if len(cfg.Seeds) != 1 || cfg.Seeds[0] != "100.64.1.10:8080" {
		t.Errorf("Seeds = %v", cfg.Seeds)
	}
	if cfg.AllowPrivatePeers {
		t.Error("AllowPrivatePeers should default to false")
	}
}

func TestLoadAllowPrivatePeers(t *testing.T) {
	cfg, err := Load(envFrom(map[string]string{
		"VIIWORK_GW_SEEDS":               "100.64.1.10:8080",
		"VIIWORK_GW_ALLOW_PRIVATE_PEERS": "true",
	}))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !cfg.AllowPrivatePeers {
		t.Error("AllowPrivatePeers should be true when the env var is set to true")
	}
}

func TestLoadAllowPrivatePeersInvalid(t *testing.T) {
	if _, err := Load(envFrom(map[string]string{
		"VIIWORK_GW_SEEDS":               "100.64.1.10:8080",
		"VIIWORK_GW_ALLOW_PRIVATE_PEERS": "not-a-bool",
	})); err == nil {
		t.Fatal("want error for an unparseable VIIWORK_GW_ALLOW_PRIVATE_PEERS")
	}
}

func TestLoadBodyReadTimeoutOverride(t *testing.T) {
	cfg, err := Load(envFrom(map[string]string{
		"VIIWORK_GW_SEEDS":             "100.64.1.10:8080",
		"VIIWORK_GW_BODY_READ_TIMEOUT": "5s",
	}))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.BodyReadTimeout != 5*time.Second {
		t.Errorf("BodyReadTimeout = %v, want 5s", cfg.BodyReadTimeout)
	}
}

func TestLoadBodyReadTimeoutInvalid(t *testing.T) {
	if _, err := Load(envFrom(map[string]string{
		"VIIWORK_GW_SEEDS":             "100.64.1.10:8080",
		"VIIWORK_GW_BODY_READ_TIMEOUT": "not-a-duration",
	})); err == nil {
		t.Fatal("want error for an unparseable VIIWORK_GW_BODY_READ_TIMEOUT")
	}
}

func TestLoadMaxInFlightOverride(t *testing.T) {
	cfg, err := Load(envFrom(map[string]string{
		"VIIWORK_GW_SEEDS":        "100.64.1.10:8080",
		"VIIWORK_GW_MAX_INFLIGHT": "10",
	}))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.MaxInFlight != 10 {
		t.Errorf("MaxInFlight = %d, want 10", cfg.MaxInFlight)
	}
}

func TestLoadMaxInFlightRejectsNonPositive(t *testing.T) {
	for _, bad := range []string{"0", "-1", "-256"} {
		if _, err := Load(envFrom(map[string]string{
			"VIIWORK_GW_SEEDS":        "100.64.1.10:8080",
			"VIIWORK_GW_MAX_INFLIGHT": bad,
		})); err == nil {
			t.Errorf("VIIWORK_GW_MAX_INFLIGHT=%q: want error, got none", bad)
		}
	}
}

func TestLoadMaxInFlightRejectsGarbage(t *testing.T) {
	if _, err := Load(envFrom(map[string]string{
		"VIIWORK_GW_SEEDS":        "100.64.1.10:8080",
		"VIIWORK_GW_MAX_INFLIGHT": "not-a-number",
	})); err == nil {
		t.Fatal("want error for an unparseable VIIWORK_GW_MAX_INFLIGHT")
	}
}

func TestLoadSeedsRequired(t *testing.T) {
	if _, err := Load(envFrom(map[string]string{})); err == nil {
		t.Fatal("want error when VIIWORK_GW_SEEDS is unset")
	}
}

func TestLoadSeedsSplitAndTrim(t *testing.T) {
	cfg, err := Load(envFrom(map[string]string{
		"VIIWORK_GW_SEEDS": " 100.64.1.10:8080 , 100.64.1.11:8080 ,",
	}))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	want := []string{"100.64.1.10:8080", "100.64.1.11:8080"}
	if len(cfg.Seeds) != len(want) {
		t.Fatalf("Seeds = %v, want %v", cfg.Seeds, want)
	}
	for i := range want {
		if cfg.Seeds[i] != want[i] {
			t.Errorf("Seeds[%d] = %q, want %q", i, cfg.Seeds[i], want[i])
		}
	}
}

func TestParseSize(t *testing.T) {
	cases := []struct {
		in   string
		want int64
	}{
		{"1024", 1024},
		{"16MB", 16 * 1024 * 1024},
		{"16mb", 16 * 1024 * 1024},
		{"2KB", 2 * 1024},
		{"1GB", 1024 * 1024 * 1024},
		{" 8MB ", 8 * 1024 * 1024},
	}
	for _, c := range cases {
		got, err := ParseSize(c.in)
		if err != nil {
			t.Errorf("ParseSize(%q): %v", c.in, err)
			continue
		}
		if got != c.want {
			t.Errorf("ParseSize(%q) = %d, want %d", c.in, got, c.want)
		}
	}
	for _, bad := range []string{"", "MB", "-1", "12TB", "abc"} {
		if _, err := ParseSize(bad); err == nil {
			t.Errorf("ParseSize(%q): want error", bad)
		}
	}
}

// A value whose byte count overflows int64 must fail with a clear error, not
// wrap around into a negative number. A silent wraparound here would make
// maxBody+1 negative downstream, which turns every inference POST into a
// baffling 413 with nothing pointing at the real cause.
func TestParseSizeOverflow(t *testing.T) {
	for _, bad := range []string{
		"9007199254740992KB", // one bit past int64 once multiplied by 1024
		"9223372036854775807GB",
	} {
		got, err := ParseSize(bad)
		if err == nil {
			t.Fatalf("ParseSize(%q) = %d, <nil>; want an overflow error", bad, got)
		}
	}
}

func TestLoadRateLimitDefaults(t *testing.T) {
	cfg, err := Load(envFrom(map[string]string{
		"VIIWORK_GW_SEEDS": "100.64.1.10:8080",
	}))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	// Rate limiting ships on: a deployment that never reads the docs still
	// gets a per-key ceiling.
	if cfg.RatePerMin != 120 {
		t.Errorf("RatePerMin = %v, want 120", cfg.RatePerMin)
	}
	if cfg.RateBurst != 240 {
		t.Errorf("RateBurst = %d, want 240", cfg.RateBurst)
	}
}

func TestLoadRateLimitOverrides(t *testing.T) {
	cfg, err := Load(envFrom(map[string]string{
		"VIIWORK_GW_SEEDS":        "100.64.1.10:8080",
		"VIIWORK_GW_RATE_PER_MIN": "30.5",
		"VIIWORK_GW_RATE_BURST":   "10",
	}))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.RatePerMin != 30.5 {
		t.Errorf("RatePerMin = %v, want 30.5", cfg.RatePerMin)
	}
	if cfg.RateBurst != 10 {
		t.Errorf("RateBurst = %d, want 10", cfg.RateBurst)
	}
}

// TestLoadRateLimitZeroDisables pins the documented escape hatch. This is a
// deliberate difference from VIIWORK_GW_MAX_INFLIGHT, which rejects
// non-positive values: 0 here means "switched off", not "reject everything".
func TestLoadRateLimitZeroDisables(t *testing.T) {
	cfg, err := Load(envFrom(map[string]string{
		"VIIWORK_GW_SEEDS":        "100.64.1.10:8080",
		"VIIWORK_GW_RATE_PER_MIN": "0",
	}))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.RatePerMin != 0 {
		t.Errorf("RatePerMin = %v, want 0 (disabled)", cfg.RatePerMin)
	}
}

func TestLoadRateLimitRejectsNegativeAndUnparseable(t *testing.T) {
	for _, bad := range []string{"-1", "abc", "1/2", ""} {
		env := map[string]string{
			"VIIWORK_GW_SEEDS":        "100.64.1.10:8080",
			"VIIWORK_GW_RATE_PER_MIN": bad,
		}
		if bad == "" {
			// An empty value means "unset", which must fall back to the
			// default rather than erroring.
			cfg, err := Load(envFrom(env))
			if err != nil {
				t.Fatalf("empty VIIWORK_GW_RATE_PER_MIN: unexpected error %v", err)
			}
			if cfg.RatePerMin != 120 {
				t.Errorf("empty value gave RatePerMin = %v, want the 120 default", cfg.RatePerMin)
			}
			continue
		}
		if _, err := Load(envFrom(env)); err == nil {
			t.Errorf("VIIWORK_GW_RATE_PER_MIN=%q: want error, got none", bad)
		}
	}

	for _, bad := range []string{"-1", "abc", "1.5"} {
		if _, err := Load(envFrom(map[string]string{
			"VIIWORK_GW_SEEDS":      "100.64.1.10:8080",
			"VIIWORK_GW_RATE_BURST": bad,
		})); err == nil {
			t.Errorf("VIIWORK_GW_RATE_BURST=%q: want error, got none", bad)
		}
	}
}
