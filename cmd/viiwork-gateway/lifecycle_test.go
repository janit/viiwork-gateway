package main

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeLifecycle records when Close was called and with what timeout, so the
// tests can prove the drain finished first.
type fakeLifecycle struct {
	mu           sync.Mutex
	closedAt     time.Time
	closeTimeout time.Duration
	closes       int
	fatal        chan error
}

func newFakeLifecycle() *fakeLifecycle {
	return &fakeLifecycle{fatal: make(chan error, 1)}
}

func (f *fakeLifecycle) Close(leaveTimeout time.Duration) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closes++
	f.closedAt = time.Now()
	f.closeTimeout = leaveTimeout
	return nil
}

func (f *fakeLifecycle) Fatal() <-chan error { return f.fatal }

func (f *fakeLifecycle) snapshot() (at time.Time, timeout time.Duration, n int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.closedAt, f.closeTimeout, f.closes
}

// serveHarness starts serve on a loopback listener with the given handler.
func serveHarness(t *testing.T, h http.Handler) (*http.Server, net.Listener, *fakeLifecycle) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	srv := &http.Server{Handler: h}
	return srv, ln, newFakeLifecycle()
}

// M1: a signalled shutdown drains HTTP first and only then leaves the mesh.
// Draining needs the mesh — a request still in flight may yet be forwarded —
// so leaving first would sever exactly the requests the drain exists to
// protect.
func TestServeDrainsBeforeLeavingTheMesh(t *testing.T) {
	var handlerDone time.Time
	srv, ln, fl := serveHarness(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		handlerDone = time.Now()
	}))

	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() { errCh <- serve(ctx, srv, ln, fl, slog.Default()) }()

	// One real request, so the server is genuinely up before we stop it.
	resp, err := http.Get("http://" + ln.Addr().String() + "/")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	resp.Body.Close()

	cancel()
	if err := <-errCh; err != nil {
		t.Fatalf("serve returned %v, want nil for a cancelled context", err)
	}

	closedAt, timeout, n := fl.snapshot()
	if n != 1 {
		t.Fatalf("Close was called %d times, want 1", n)
	}
	if closedAt.Before(handlerDone) {
		t.Error("the mesh was left before the in-flight request finished")
	}
	if timeout != 5*time.Second {
		t.Errorf("Close got %v, want 5s", timeout)
	}
}

// M2: a duplicate node name is fatal. The process must stop — another member
// already holds this identity — but it stops the same orderly way.
func TestServeReturnsFatalMeshError(t *testing.T) {
	srv, ln, fl := serveHarness(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))

	wantErr := errors.New("duplicate name")
	fl.fatal <- wantErr

	err := serve(context.Background(), srv, ln, fl, slog.Default())
	if !errors.Is(err, wantErr) {
		t.Fatalf("serve returned %v, want %v", err, wantErr)
	}
	if _, timeout, n := fl.snapshot(); n != 1 || timeout != 5*time.Second {
		t.Errorf("Close called %d times with %v, want once with 5s", n, timeout)
	}
}

// M3: a request already being served is allowed to finish. An inference
// request can be seconds or minutes from its last token when a deploy lands.
func TestServeLetsAnInFlightRequestFinish(t *testing.T) {
	started := make(chan struct{})
	var handlerDone time.Time
	srv, ln, fl := serveHarness(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		time.Sleep(300 * time.Millisecond)
		w.WriteHeader(http.StatusOK)
		handlerDone = time.Now()
	}))

	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() { errCh <- serve(ctx, srv, ln, fl, slog.Default()) }()

	respCh := make(chan *http.Response, 1)
	go func() {
		resp, err := http.Get("http://" + ln.Addr().String() + "/slow")
		if err != nil {
			respCh <- nil
			return
		}
		respCh <- resp
	}()

	<-started
	cancel() // shut down while the request is mid-flight

	resp := <-respCh
	if resp == nil {
		t.Fatal("the in-flight request was severed by the shutdown")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	if err := <-errCh; err != nil {
		t.Fatalf("serve returned %v, want nil", err)
	}
	if closedAt, _, _ := fl.snapshot(); closedAt.Before(handlerDone) {
		t.Error("the mesh was left before the in-flight request finished")
	}
}

// M5: the alias-write deny holds through the whole chain, not just in the
// router in isolation.
func TestFullChainRefusesAliasWrites(t *testing.T) {
	h := testHandler(t)

	for _, c := range []struct{ method, path string }{
		{"PUT", "/v1/aliases/stable-coder"},
		{"POST", "/v1/aliases/stable-coder/revert"},
	} {
		req := httptest.NewRequest(c.method, c.path, strings.NewReader(`{"target":"x"}`))
		req.Header.Set("Authorization", "Bearer "+key)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)

		if rec.Code != http.StatusForbidden {
			t.Errorf("%s %s: status = %d, want 403", c.method, c.path, rec.Code)
		}
		if !strings.Contains(rec.Body.String(), "forbidden_path") {
			t.Errorf("%s %s: body %q should carry forbidden_path", c.method, c.path, rec.Body.String())
		}
	}
}
