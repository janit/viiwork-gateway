package fleet

import (
	"net/netip"
	"testing"
	"time"

	"github.com/janit/viiwork-gateway/internal/config"
	"github.com/janit/viiwork/v2/meshapi"
)

func baseConfig() config.Config {
	return config.Config{
		Listen:   "127.0.0.1:8090",
		NodeName: "gw",
		Mesh: config.MeshConfig{
			Network: "tailnet", BindPort: 7946, Enforce: "full",
			Tailnet: true, TailnetSocket: "/var/run/tailscale/tailscaled.sock",
			RejoinInterval: time.Minute, CapacityPoll: time.Second,
			StaleAfter: 3 * time.Second,
		},
	}
}

// The gateway joins as a member that serves no models, and says so: a node
// that sees role gateway never polls it for capacity.
func TestMeshOptionsJoinsAsAPayloadlessGateway(t *testing.T) {
	mo := meshOptions(baseConfig(), "v1.0.0", 8090)

	if mo.Role != meshapi.RoleGateway {
		t.Errorf("Role = %q, want %q", mo.Role, meshapi.RoleGateway)
	}
	if mo.Payload != nil {
		t.Error("a gateway must gossip no payload: an empty push/pull state is " +
			"what keeps a node's alias table from being handed a buffer to merge")
	}
	if mo.APIPort != 8090 {
		t.Errorf("APIPort = %d, want the port the gateway listens on", mo.APIPort)
	}
	if mo.Version != "v1.0.0" {
		t.Errorf("Version = %q, want the build's version", mo.Version)
	}
	if mo.Name != "gw" {
		t.Errorf("Name = %q, want gw", mo.Name)
	}
}

// LocalAPISocket resolves this member's own tailnet address and is needed
// whether or not the tailnet feeder is running; TailnetSocket is what turns
// that feeder on. Setting them together would mean a gateway with discovery
// off silently used the default socket path.
func TestMeshOptionsSocketsHaveDifferentJobs(t *testing.T) {
	const custom = "/run/tailscale/custom.sock"

	t.Run("feeder on", func(t *testing.T) {
		cfg := baseConfig()
		cfg.Mesh.TailnetSocket = custom
		mo := meshOptions(cfg, "v1.0.0", 8090)

		if mo.TailnetSocket != custom {
			t.Errorf("TailnetSocket = %q, want %q: the feeder is on", mo.TailnetSocket, custom)
		}
		if mo.LocalAPISocket != custom {
			t.Errorf("LocalAPISocket = %q, want %q", mo.LocalAPISocket, custom)
		}
	})

	t.Run("feeder off", func(t *testing.T) {
		cfg := baseConfig()
		cfg.Mesh.TailnetSocket = custom
		cfg.Mesh.Tailnet = false
		mo := meshOptions(cfg, "v1.0.0", 8090)

		if mo.TailnetSocket != "" {
			t.Errorf("TailnetSocket = %q, want empty: the feeder is off", mo.TailnetSocket)
		}
		if mo.LocalAPISocket != custom {
			t.Errorf("LocalAPISocket = %q, want %q: a tailnet member still has to "+
				"resolve its own address to know what to advertise", mo.LocalAPISocket, custom)
		}
	})
}

// The mesh mode and its keys pass through untouched: this is what decides
// whether membership is authenticated at all.
func TestMeshOptionsCarriesTheMeshMode(t *testing.T) {
	cfg := baseConfig()
	cfg.Mesh.SecretKey = make([]byte, 32)
	cfg.Mesh.PreviousKey = make([]byte, 32)
	cfg.Mesh.Advertise = netip.MustParseAddr("100.64.0.9")
	cfg.Mesh.Seeds = []string{"100.64.0.2:7946"}

	mo := meshOptions(cfg, "v1.0.0", 8090)

	if len(mo.SecretKey) != 32 || len(mo.PreviousKey) != 32 {
		t.Errorf("keys did not reach the mesh: %d and %d bytes", len(mo.SecretKey), len(mo.PreviousKey))
	}
	if mo.Advertise != netip.MustParseAddr("100.64.0.9") {
		t.Errorf("Advertise = %v", mo.Advertise)
	}
	if len(mo.Seeds) != 1 || mo.Seeds[0] != "100.64.0.2:7946" {
		t.Errorf("Seeds = %v", mo.Seeds)
	}
	if mo.Enforce != "full" {
		t.Errorf("Enforce = %q, want full", mo.Enforce)
	}
}
