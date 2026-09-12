package config

import (
	"strings"
	"testing"
	"time"
)

// testSecret is the standard base64 of 32 zero bytes: a valid mesh secret for
// tests, and never a value any deployment would use.
const testSecret = "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA="

func envFrom(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func hostnameOK() (string, error) { return "gw-test", nil }

func hostnameFails() (string, error) { return "", errHostname }

var errHostname = &hostnameError{}

type hostnameError struct{}

func (*hostnameError) Error() string { return "hostname unavailable" }

// securedEnv is the smallest environment that loads: a secured mesh, plus any
// extras the case needs.
func securedEnv(extra map[string]string) func(string) string {
	m := map[string]string{"VIIWORK_MESH_SECRET": testSecret}
	for k, v := range extra {
		m[k] = v
	}
	return envFrom(m)
}

// openEnv is the smallest open-mesh environment: no secret, and the view nodes
// an open mesh requires.
func openEnv(extra map[string]string) func(string) string {
	m := map[string]string{
		"VIIWORK_GW_MESH_OPEN":  "true",
		"VIIWORK_GW_VIEW_NODES": "node-a",
	}
	for k, v := range extra {
		m[k] = v
	}
	return envFrom(m)
}

func mustLoad(t *testing.T, getenv func(string) string) Config {
	t.Helper()
	cfg, err := Load(getenv, hostnameOK)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	return cfg
}

func wantErr(t *testing.T, getenv func(string) string, substrings ...string) error {
	t.Helper()
	_, err := Load(getenv, hostnameOK)
	if err == nil {
		t.Fatal("want an error, got nil")
	}
	for _, s := range substrings {
		if !strings.Contains(err.Error(), s) {
			t.Errorf("error %q does not contain %q", err, s)
		}
	}
	return err
}

// CG1: a secret is the only thing a secured mesh needs; everything else has a
// default.
func TestLoadDefaults(t *testing.T) {
	cfg := mustLoad(t, securedEnv(nil))

	if cfg.Listen != "127.0.0.1:8090" {
		t.Errorf("Listen = %q, want 127.0.0.1:8090", cfg.Listen)
	}
	if cfg.NodeName != "gw-test" {
		t.Errorf("NodeName = %q, want gw-test", cfg.NodeName)
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
	if cfg.RatePerMin != 120 {
		t.Errorf("RatePerMin = %v, want 120", cfg.RatePerMin)
	}
	if cfg.RateBurst != 240 {
		t.Errorf("RateBurst = %d, want 240", cfg.RateBurst)
	}
	if len(cfg.ViewNodes) != 0 {
		t.Errorf("ViewNodes = %v, want empty", cfg.ViewNodes)
	}

	if cfg.Mode() != "secured" {
		t.Errorf("Mode() = %q, want secured", cfg.Mode())
	}
	m := cfg.Mesh
	if m.Network != "tailnet" {
		t.Errorf("Mesh.Network = %q, want tailnet", m.Network)
	}
	if m.BindPort != 7946 {
		t.Errorf("Mesh.BindPort = %d, want 7946", m.BindPort)
	}
	if m.Advertise.IsValid() {
		t.Errorf("Mesh.Advertise = %v, want the zero address", m.Advertise)
	}
	if m.Open {
		t.Error("Mesh.Open should default to false")
	}
	if len(m.SecretKey) != 32 {
		t.Errorf("Mesh.SecretKey is %d bytes, want 32", len(m.SecretKey))
	}
	if m.PreviousKey != nil {
		t.Errorf("Mesh.PreviousKey = %v, want nil", m.PreviousKey)
	}
	if m.Enforce != "full" {
		t.Errorf("Mesh.Enforce = %q, want full", m.Enforce)
	}
	if !m.Tailnet {
		t.Error("Mesh.Tailnet should be true when the network is tailnet")
	}
	if m.TailnetSocket != "/var/run/tailscale/tailscaled.sock" {
		t.Errorf("Mesh.TailnetSocket = %q", m.TailnetSocket)
	}
	if m.MDNS {
		t.Error("Mesh.MDNS should be false when the network is tailnet")
	}
	if len(m.Seeds) != 0 {
		t.Errorf("Mesh.Seeds = %v, want empty", m.Seeds)
	}
	if m.RejoinInterval != 60*time.Second {
		t.Errorf("Mesh.RejoinInterval = %v, want 60s", m.RejoinInterval)
	}
	if m.CapacityPoll != time.Second {
		t.Errorf("Mesh.CapacityPoll = %v, want 1s", m.CapacityPoll)
	}
	if m.StaleAfter != 3*time.Second {
		t.Errorf("Mesh.StaleAfter = %v, want 3s", m.StaleAfter)
	}
}

// CG2: an open mesh names its view nodes.
func TestLoadOpenMesh(t *testing.T) {
	cfg := mustLoad(t, envFrom(map[string]string{
		"VIIWORK_GW_MESH_OPEN":  "true",
		"VIIWORK_GW_VIEW_NODES": "node-a, node-b",
	}))
	if cfg.Mode() != "open" {
		t.Errorf("Mode() = %q, want open", cfg.Mode())
	}
	if !cfg.Mesh.Open {
		t.Error("Mesh.Open should be true")
	}
	if cfg.Mesh.SecretKey != nil {
		t.Errorf("Mesh.SecretKey = %v, want nil in an open mesh", cfg.Mesh.SecretKey)
	}
	want := []string{"node-a", "node-b"}
	if len(cfg.ViewNodes) != len(want) {
		t.Fatalf("ViewNodes = %v, want %v", cfg.ViewNodes, want)
	}
	for i := range want {
		if cfg.ViewNodes[i] != want[i] {
			t.Errorf("ViewNodes[%d] = %q, want %q", i, cfg.ViewNodes[i], want[i])
		}
	}
}

// CG3: an open mesh admits any viiwork node that can reach the gossip port, so
// the operator must say which of them may serve dashboards.
func TestLoadOpenMeshRequiresViewNodes(t *testing.T) {
	wantErr(t, envFrom(map[string]string{"VIIWORK_GW_MESH_OPEN": "true"}),
		"VIIWORK_GW_VIEW_NODES")
}

// CG4: secured or open, not both.
func TestLoadOpenAndSecretConflict(t *testing.T) {
	wantErr(t, envFrom(map[string]string{
		"VIIWORK_GW_MESH_OPEN":  "true",
		"VIIWORK_GW_VIEW_NODES": "node-a",
		"VIIWORK_MESH_SECRET":   testSecret,
	}), "not both")
}

// CG5: neither is a choice the operator has to make explicitly.
func TestLoadRequiresMeshMode(t *testing.T) {
	wantErr(t, envFrom(map[string]string{}),
		"VIIWORK_MESH_SECRET", "VIIWORK_GW_MESH_OPEN")
}

// CG6: a bad secret names its variable and never echoes its value.
func TestLoadSecretMustBe32Bytes(t *testing.T) {
	err := wantErr(t, envFrom(map[string]string{"VIIWORK_MESH_SECRET": "AAAA"}),
		"VIIWORK_MESH_SECRET")
	if strings.Contains(err.Error(), "AAAA") {
		t.Errorf("error %q echoes the secret value", err)
	}
}

// CG7: a previous key is meaningless without a current one.
func TestLoadPreviousSecretRequiresSecret(t *testing.T) {
	wantErr(t, openEnv(map[string]string{"VIIWORK_MESH_SECRET_PREV": testSecret}),
		"VIIWORK_MESH_SECRET_PREV")
}

// CG8: there is nothing to enforce without a secret.
func TestLoadEnforceRequiresSecret(t *testing.T) {
	wantErr(t, openEnv(map[string]string{"VIIWORK_GW_MESH_ENFORCE": "outgoing"}),
		"VIIWORK_GW_MESH_ENFORCE")
}

// CG9: a v1 variable left in place fails loudly and names its replacement,
// rather than being read as something it no longer means.
func TestLoadRemovedVariables(t *testing.T) {
	cases := []struct {
		env  string
		val  string
		want string
	}{
		{"VIIWORK_GW_SEEDS", "100.64.1.10:8080",
			"VIIWORK_GW_SEEDS is removed: use VIIWORK_GW_MESH_SEEDS with gossip addresses (ip:7946); seeds are optional on the tailnet"},
		{"VIIWORK_GW_POLL_INTERVAL", "10s",
			"VIIWORK_GW_POLL_INTERVAL is removed: capacity is polled every VIIWORK_GW_CAPACITY_POLL"},
		{"VIIWORK_GW_POLL_TIMEOUT", "2s",
			"VIIWORK_GW_POLL_TIMEOUT is removed: capacity is polled every VIIWORK_GW_CAPACITY_POLL"},
		{"VIIWORK_GW_ALLOW_PRIVATE_PEERS", "true",
			"VIIWORK_GW_ALLOW_PRIVATE_PEERS is removed: set VIIWORK_GW_MESH_NETWORK=lan for a LAN mesh"},
		{"VIIWORK_GW_VIEW_PREFER", "node-a",
			"VIIWORK_GW_VIEW_PREFER is removed: use VIIWORK_GW_VIEW_NODES (node names)"},
	}
	for _, c := range cases {
		t.Run(c.env, func(t *testing.T) {
			_, err := Load(securedEnv(map[string]string{c.env: c.val}), hostnameOK)
			if err == nil {
				t.Fatalf("want an error when %s is set", c.env)
			}
			if err.Error() != c.want {
				t.Errorf("error = %q,\n want %q", err, c.want)
			}
		})
	}
}

// CG10: seeds are gossip address:port pairs, IPv6 included.
func TestLoadMeshSeeds(t *testing.T) {
	cfg := mustLoad(t, securedEnv(map[string]string{
		"VIIWORK_GW_MESH_SEEDS": "100.64.0.2:7946, [fd7a::1]:7946",
	}))
	want := []string{"100.64.0.2:7946", "[fd7a::1]:7946"}
	if len(cfg.Mesh.Seeds) != len(want) {
		t.Fatalf("Seeds = %v, want %v", cfg.Mesh.Seeds, want)
	}
	for i := range want {
		if cfg.Mesh.Seeds[i] != want[i] {
			t.Errorf("Seeds[%d] = %q, want %q", i, cfg.Mesh.Seeds[i], want[i])
		}
	}
}

// CG11: a hostname is not a gossip address; memberlist needs a literal.
func TestLoadMeshSeedsRejectsHostname(t *testing.T) {
	wantErr(t, securedEnv(map[string]string{"VIIWORK_GW_MESH_SEEDS": "node-a:7946"}),
		"VIIWORK_GW_MESH_SEEDS", "node-a:7946")
}

// CG12: a LAN mesh discovers over mDNS, not the tailnet.
func TestLoadLANNetwork(t *testing.T) {
	cfg := mustLoad(t, securedEnv(map[string]string{"VIIWORK_GW_MESH_NETWORK": "lan"}))
	if cfg.Mesh.Network != "lan" {
		t.Errorf("Network = %q, want lan", cfg.Mesh.Network)
	}
	if cfg.Mesh.Tailnet {
		t.Error("Tailnet should be false on a LAN mesh")
	}
	if !cfg.Mesh.MDNS {
		t.Error("MDNS should be true on a LAN mesh")
	}
}

// CG13: auto is a default, not a rule; either feeder can be forced off.
func TestLoadTailnetFeederOverride(t *testing.T) {
	cfg := mustLoad(t, securedEnv(map[string]string{"VIIWORK_GW_MESH_TAILNET": "false"}))
	if cfg.Mesh.Network != "tailnet" {
		t.Errorf("Network = %q, want tailnet", cfg.Mesh.Network)
	}
	if cfg.Mesh.Tailnet {
		t.Error("Tailnet should be false when explicitly disabled")
	}
}

// CG14: a node name is a mesh identity; whitespace in it breaks the wire.
func TestLoadRejectsNodeNameWithWhitespace(t *testing.T) {
	wantErr(t, securedEnv(map[string]string{"VIIWORK_GW_NODE_NAME": "bad name"}),
		"VIIWORK_GW_NODE_NAME")
}

// CG15: with no hostname and no override there is no identity to join under.
func TestLoadHostnameFailureNamesOverride(t *testing.T) {
	_, err := Load(securedEnv(nil), hostnameFails)
	if err == nil {
		t.Fatal("want an error when the hostname cannot be read")
	}
	if !strings.Contains(err.Error(), "VIIWORK_GW_NODE_NAME") {
		t.Errorf("error %q does not name VIIWORK_GW_NODE_NAME", err)
	}
}

// CG16: an empty or repeated view node is a typo, and the view election would
// silently absorb it.
func TestLoadRejectsMalformedViewNodes(t *testing.T) {
	wantErr(t, securedEnv(map[string]string{"VIIWORK_GW_VIEW_NODES": "node-a,,node-a"}),
		"VIIWORK_GW_VIEW_NODES")
}

// CG17: a zero staleness window would make every report stale on arrival.
func TestLoadRejectsZeroStaleAfter(t *testing.T) {
	wantErr(t, securedEnv(map[string]string{"VIIWORK_GW_STALE_AFTER": "0s"}),
		"VIIWORK_GW_STALE_AFTER")
}

func TestLoadBodyReadTimeoutOverride(t *testing.T) {
	cfg := mustLoad(t, securedEnv(map[string]string{"VIIWORK_GW_BODY_READ_TIMEOUT": "5s"}))
	if cfg.BodyReadTimeout != 5*time.Second {
		t.Errorf("BodyReadTimeout = %v, want 5s", cfg.BodyReadTimeout)
	}
}

func TestLoadBodyReadTimeoutInvalid(t *testing.T) {
	wantErr(t, securedEnv(map[string]string{"VIIWORK_GW_BODY_READ_TIMEOUT": "0s"}),
		"VIIWORK_GW_BODY_READ_TIMEOUT")
}

func TestLoadMaxInFlightOverride(t *testing.T) {
	cfg := mustLoad(t, securedEnv(map[string]string{"VIIWORK_GW_MAX_INFLIGHT": "8"}))
	if cfg.MaxInFlight != 8 {
		t.Errorf("MaxInFlight = %d, want 8", cfg.MaxInFlight)
	}
}

func TestLoadMaxInFlightRejectsNonPositive(t *testing.T) {
	wantErr(t, securedEnv(map[string]string{"VIIWORK_GW_MAX_INFLIGHT": "0"}),
		"VIIWORK_GW_MAX_INFLIGHT")
}

func TestLoadMaxInFlightRejectsGarbage(t *testing.T) {
	wantErr(t, securedEnv(map[string]string{"VIIWORK_GW_MAX_INFLIGHT": "lots"}),
		"VIIWORK_GW_MAX_INFLIGHT")
}

func TestLoadListenOverride(t *testing.T) {
	cfg := mustLoad(t, securedEnv(map[string]string{"VIIWORK_GW_LISTEN": "127.0.0.1:9999"}))
	if cfg.Listen != "127.0.0.1:9999" {
		t.Errorf("Listen = %q, want 127.0.0.1:9999", cfg.Listen)
	}
}

func TestLoadMaxBodyOverride(t *testing.T) {
	cfg := mustLoad(t, securedEnv(map[string]string{"VIIWORK_GW_MAX_BODY": "2MB"}))
	if cfg.MaxBody != 2*1024*1024 {
		t.Errorf("MaxBody = %d, want 2MiB", cfg.MaxBody)
	}
}

func TestLoadRateLimitDefaults(t *testing.T) {
	cfg := mustLoad(t, securedEnv(nil))
	if cfg.RatePerMin != 120 {
		t.Errorf("RatePerMin = %v, want 120", cfg.RatePerMin)
	}
	if cfg.RateBurst != 240 {
		t.Errorf("RateBurst = %d, want 240", cfg.RateBurst)
	}
}

func TestLoadRateLimitOverrides(t *testing.T) {
	cfg := mustLoad(t, securedEnv(map[string]string{
		"VIIWORK_GW_RATE_PER_MIN": "30",
		"VIIWORK_GW_RATE_BURST":   "45",
	}))
	if cfg.RatePerMin != 30 {
		t.Errorf("RatePerMin = %v, want 30", cfg.RatePerMin)
	}
	if cfg.RateBurst != 45 {
		t.Errorf("RateBurst = %d, want 45", cfg.RateBurst)
	}
}

func TestLoadRateLimitZeroDisables(t *testing.T) {
	cfg := mustLoad(t, securedEnv(map[string]string{"VIIWORK_GW_RATE_PER_MIN": "0"}))
	if cfg.RatePerMin != 0 {
		t.Errorf("RatePerMin = %v, want 0", cfg.RatePerMin)
	}
}

func TestLoadRateLimitRejectsNegativeAndUnparseable(t *testing.T) {
	wantErr(t, securedEnv(map[string]string{"VIIWORK_GW_RATE_PER_MIN": "-1"}),
		"VIIWORK_GW_RATE_PER_MIN")
	wantErr(t, securedEnv(map[string]string{"VIIWORK_GW_RATE_PER_MIN": "fast"}),
		"VIIWORK_GW_RATE_PER_MIN")
	wantErr(t, securedEnv(map[string]string{"VIIWORK_GW_RATE_BURST": "0"}),
		"VIIWORK_GW_RATE_BURST")
}

func TestParseSize(t *testing.T) {
	cases := []struct {
		in   string
		want int64
		ok   bool
	}{
		{"1024", 1024, true},
		{"1KB", 1024, true},
		{"2mb", 2 * 1024 * 1024, true},
		{" 3 GB ", 3 * 1024 * 1024 * 1024, true},
		{"0", 0, false},
		{"-1", 0, false},
		{"", 0, false},
		{"big", 0, false},
	}
	for _, c := range cases {
		got, err := ParseSize(c.in)
		if c.ok && err != nil {
			t.Errorf("ParseSize(%q): %v", c.in, err)
			continue
		}
		if !c.ok {
			if err == nil {
				t.Errorf("ParseSize(%q) = %d, want an error", c.in, got)
			}
			continue
		}
		if got != c.want {
			t.Errorf("ParseSize(%q) = %d, want %d", c.in, got, c.want)
		}
	}
}

func TestParseSizeOverflow(t *testing.T) {
	if _, err := ParseSize("9223372036854775807GB"); err == nil {
		t.Fatal("want an overflow error")
	}
}
