package proxy

import (
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/janit/viiwork-gateway/internal/fleet"
)

// fakeFleet is a scripted Fleet. It answers Pick from a list of targets —
// either repeating the first forever, or handing them out in order so a test
// can drive the retry — and counts the releases it handed back, which is how
// the tests prove a reservation is never leaked.
type fakeFleet struct {
	mu       sync.Mutex
	targets  []fleet.Target
	repeat   bool
	pickIdx  int
	view     fleet.Target
	hasView  bool
	releases int
	excludes []map[string]bool
}

func (f *fakeFleet) Pick(model string, exclude map[string]bool) (fleet.Target, func(), bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.excludes = append(f.excludes, exclude)

	var t fleet.Target
	switch {
	case len(f.targets) == 0:
		return fleet.Target{}, func() {}, false
	case f.repeat:
		t = f.targets[0]
	case f.pickIdx < len(f.targets):
		t = f.targets[f.pickIdx]
		f.pickIdx++
	default:
		return fleet.Target{}, func() {}, false
	}

	var once sync.Once
	return t, func() {
		once.Do(func() {
			f.mu.Lock()
			f.releases++
			f.mu.Unlock()
		})
	}, true
}

func (f *fakeFleet) View() (fleet.Target, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.view, f.hasView
}

func (f *fakeFleet) releaseCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.releases
}

func (f *fakeFleet) pickCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.excludes)
}

// excludeAt returns the exclude map passed to the nth Pick call.
func (f *fakeFleet) excludeAt(n int) map[string]bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	if n >= len(f.excludes) {
		return nil
	}
	return f.excludes[n]
}

// oneNodeFleet routes everything — picks and the view alike — to one upstream,
// as often as asked. It is what the tests that are not about routing use.
func oneNodeFleet(node, addr string) *fakeFleet {
	t := fleet.Target{Node: node, APIAddr: addr}
	return &fakeFleet{targets: []fleet.Target{t}, repeat: true, view: t, hasView: true}
}

func upstreamFleet(upstream *httptest.Server) *fakeFleet {
	return oneNodeFleet("node-a", addrOf(upstream))
}

// emptyFleet can serve nothing: no candidate for any model, and no view node.
func emptyFleet() *fakeFleet { return &fakeFleet{} }

// viewOnlyFleet has a view node but no routable model, which is what the
// gateway sees for an alias, a pipeline, or a model still loading.
func viewOnlyFleet(node, addr string) *fakeFleet {
	return &fakeFleet{view: fleet.Target{Node: node, APIAddr: addr}, hasView: true}
}

// closedPort returns an address nothing is listening on, for the unreachable
// cases. The listener is opened and closed so the port is real but dead.
func closedPort(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(nil)
	addr := addrOf(srv)
	srv.Close()
	return addr
}
