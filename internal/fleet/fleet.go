package fleet

import (
	"context"
	"fmt"
	"io"
	"log"
	"net"
	"net/netip"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/janit/viiwork-gateway/internal/config"
	"github.com/janit/viiwork/v2/mesh"
	"github.com/janit/viiwork/v2/mesh/capacity"
	"github.com/janit/viiwork/v2/meshapi"
)

// Options configures a Fleet.
type Options struct {
	Config  config.Config
	Version string
	Log     io.Writer            // nil = os.Stdout
	Logf    func(string, ...any) // nil = log.Printf
	// MeshTune is a test seam, applied last to the mesh options. Production
	// leaves it nil.
	MeshTune func(*mesh.Options)
}

// Fleet is the gateway's membership of a viiwork 2 mesh: the mesh itself, the
// capacity poller that keeps members' free-slot counts current, and the Picker
// that turns both into the two decisions the router needs.
type Fleet struct {
	mesh   *mesh.Mesh
	poller *capacity.Poller
	picker *Picker

	stopPoller context.CancelFunc
	pollerDone chan struct{}

	closeOnce sync.Once
	closeErr  error
}

// Start joins the mesh and begins polling capacity. The gateway joins as a
// zero-model member with role gateway and no gossip payload: nodes never poll
// a gateway for capacity, and an empty push/pull state means a node's alias
// table is never handed a buffer to merge.
func Start(ctx context.Context, o Options) (*Fleet, error) {
	cfg := o.Config

	apiPort, err := portOf(cfg.Listen)
	if err != nil {
		return nil, fmt.Errorf("mesh: %w", err)
	}

	logf := o.Logf
	if logf == nil {
		logf = log.Printf
	}

	// The poller cannot exist before the mesh — it follows the mesh's member
	// list — but mesh.Start delivers events while it is still bootstrapping
	// (memberlist notifies about the local node as it comes up), and those
	// arrive on another goroutine. The handoff is therefore atomic, and an
	// event that lands before the poller exists is simply not forwarded: the
	// only such events are joins, and the poller re-reads the whole member
	// list on its next tick anyway.
	var pollerRef atomic.Pointer[capacity.Poller]

	mo := meshOptions(cfg, o.Version, apiPort)
	mo.Log = o.Log
	mo.OnChange = func(ev mesh.MemberEvent) {
		if p := pollerRef.Load(); p != nil {
			p.HandleMemberEvent(ev)
		}
		logf("member %s %s", ev.Member.Name, eventWord(ev.Kind))
	}
	if o.MeshTune != nil {
		o.MeshTune(&mo)
	}

	m, err := mesh.Start(ctx, mo)
	if err != nil {
		return nil, err
	}

	poller := capacity.NewPoller(capacity.Config{
		Self:     cfg.NodeName,
		Members:  m,
		Interval: cfg.Mesh.CapacityPoll,
		Logf:     logf,
	})
	pollerRef.Store(poller)

	pollCtx, stop := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		poller.Run(pollCtx)
	}()

	return &Fleet{
		mesh:       m,
		poller:     poller,
		picker:     NewPicker(m, poller, cfg.Mesh.StaleAfter, cfg.ViewNodes, time.Now),
		stopPoller: stop,
		pollerDone: done,
	}, nil
}

// meshOptions maps the gateway's configuration onto viiwork's membership
// options. It is separate from Start so the mapping can be tested without
// joining anything.
func meshOptions(cfg config.Config, version string, apiPort int) mesh.Options {
	mo := mesh.Options{
		Name:           cfg.NodeName,
		Network:        cfg.Mesh.Network,
		BindPort:       cfg.Mesh.BindPort,
		Advertise:      cfg.Mesh.Advertise,
		APIPort:        apiPort,
		Role:           meshapi.RoleGateway,
		Version:        version,
		SecretKey:      cfg.Mesh.SecretKey,
		PreviousKey:    cfg.Mesh.PreviousKey,
		Enforce:        cfg.Mesh.Enforce,
		MDNS:           cfg.Mesh.MDNS,
		Seeds:          cfg.Mesh.Seeds,
		RejoinInterval: cfg.Mesh.RejoinInterval,
		Payload:        nil, // a gateway carries no state
	}

	// Two different jobs share one configured path, and viiwork's node makes
	// the same distinction:
	//
	//   - LocalAPISocket resolves THIS member's own tailnet address, which a
	//     tailnet member needs whether or not it discovers peers that way. It
	//     is always set, or a gateway with the feeder switched off and a
	//     non-default socket path would fall back to the default path and
	//     fail to work out what to advertise.
	//   - TailnetSocket additionally turns on the tailnet discovery feeder,
	//     so it is set only when that feeder is wanted.
	mo.LocalAPISocket = cfg.Mesh.TailnetSocket
	if cfg.Mesh.Tailnet {
		mo.TailnetSocket = cfg.Mesh.TailnetSocket
	}
	return mo
}

// Pick chooses the node to serve an inference request for model. See
// Picker.Pick.
func (f *Fleet) Pick(model string, exclude map[string]bool) (Target, func(), bool) {
	return f.picker.Pick(model, exclude)
}

// View chooses the node that serves the dashboards and the catalogue. See
// Picker.View.
func (f *Fleet) View() (Target, bool) { return f.picker.View() }

// Mode is "secured" or "open", as the mesh actually started.
func (f *Fleet) Mode() string { return f.mesh.Mode() }

// Name is this member's name on the mesh.
func (f *Fleet) Name() string { return f.mesh.Name() }

// Advertise is the address gossip is bound to.
func (f *Fleet) Advertise() netip.Addr { return f.mesh.Advertise() }

// NumAlive counts alive members, this gateway included.
func (f *Fleet) NumAlive() int { return f.mesh.NumAlive() }

// Fatal yields an error the process must exit on — a duplicate node name,
// which means another member already holds this identity.
func (f *Fleet) Fatal() <-chan error { return f.mesh.Fatal() }

// Close leaves the mesh, stops the capacity poller and shuts the membership
// down, in that order.
//
// Leaving first is what makes the gateway disappear from every member at once
// instead of being declared dead a failure-detector interval later. It is safe
// to call twice: the second call returns the first call's error.
func (f *Fleet) Close(leaveTimeout time.Duration) error {
	f.closeOnce.Do(func() {
		var firstErr error
		if err := f.mesh.Leave(leaveTimeout); err != nil {
			firstErr = fmt.Errorf("leave: %w", err)
		}

		f.stopPoller()
		<-f.pollerDone

		if err := f.mesh.Shutdown(); err != nil && firstErr == nil {
			firstErr = fmt.Errorf("shutdown: %w", err)
		}
		f.closeErr = firstErr
	})
	return f.closeErr
}

// portOf extracts the port the gateway's API listens on, which is what it
// advertises to the mesh as its API port.
func portOf(listen string) (int, error) {
	_, portStr, err := net.SplitHostPort(listen)
	if err != nil {
		return 0, fmt.Errorf("listen address %q: %w", listen, err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil || port < 1 || port > 65535 {
		return 0, fmt.Errorf("listen address %q: %q is not a port", listen, portStr)
	}
	return port, nil
}

// eventWord renders a membership change for the log.
func eventWord(k mesh.EventKind) string {
	switch k {
	case mesh.EventJoin:
		return "joined"
	case mesh.EventUpdate:
		return "updated"
	case mesh.EventLeave:
		return "left"
	case mesh.EventFail:
		return "failed"
	default:
		return "changed"
	}
}
