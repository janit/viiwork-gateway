// Package proxy forwards authenticated requests into the viiwork mesh.
package proxy

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"strings"
	"sync"
	"time"
)

type targetKey struct{}

// unreachableKey carries a per-request holder the ErrorHandler records a dial
// failure into, so To can report it to the router instead of the handler
// writing a 502 the router might still want to retry past.
type unreachableKey struct{}

// ErrUpstreamUnreachable reports that a forward failed to dial its node and
// nothing was written to the client, so the request may safely be sent
// somewhere else. Any failure after the first byte is not this error: the
// generation may already have started, and re-running it would double the
// work and could double a side effect.
var ErrUpstreamUnreachable = errors.New("proxy: upstream unreachable")

// unreachableHolder is written by the ErrorHandler goroutine and read by To
// after ServeHTTP returns. ReverseProxy calls ErrorHandler synchronously, but
// the mutex costs nothing and makes the handoff obviously safe.
type unreachableHolder struct {
	mu  sync.Mutex
	err error
}

func (h *unreachableHolder) set(err error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.err = err
}

func (h *unreachableHolder) get() error {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.err
}

// isDialError reports whether err is a failure to reach the node at all, as
// opposed to a failure once connected. Only the former is safe to retry.
func isDialError(err error) bool {
	var opErr *net.OpError
	return errors.As(err, &opErr) && opErr.Op == "dial"
}

// Forwarder proxies a request to one mesh node.
type Forwarder struct {
	proxy *httputil.ReverseProxy
}

// NewForwarder builds the reverse proxy. Its timeouts are sized for LLM
// inference rather than for a web application: a cold prefill on a large model
// can run for many minutes, so no response-header deadline is imposed and
// requests end when the client goes away.
func NewForwarder(logger *slog.Logger) *Forwarder {
	if logger == nil {
		logger = slog.Default()
	}
	f := &Forwarder{}

	f.proxy = &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			addr, _ := pr.In.Context().Value(targetKey{}).(string)
			pr.Out.URL.Scheme = "http"
			pr.Out.URL.Host = addr
			pr.Out.Host = addr
			pr.SetXForwarded()
		},

		// Immediate flush. Without it, token-by-token completions and all
		// three SSE endpoints buffer and arrive in lumps.
		FlushInterval: -1,

		Transport: &http.Transport{
			DialContext: (&net.Dialer{
				Timeout:   5 * time.Second,
				KeepAlive: 30 * time.Second,
			}).DialContext,
			MaxIdleConns:        100,
			MaxIdleConnsPerHost: 16,
			IdleConnTimeout:     90 * time.Second,
			// Deliberately zero: a cold prefill can legitimately take minutes
			// before the first byte of the response header.
			ResponseHeaderTimeout: 0,
		},

		ModifyResponse: func(resp *http.Response) error {
			// Everything is same-origin through the gateway, so upstream CORS
			// headers grant nothing the page needs and would let a
			// third-party origin drive the fleet from a visitor's browser.
			for k := range resp.Header {
				if strings.HasPrefix(strings.ToLower(k), "access-control-") {
					resp.Header.Del(k)
				}
			}

			// A compromised (or merely misbehaving) seed view node must not
			// be able to plant a cookie at the gateway's authenticated
			// origin: strip any Set-Cookie the upstream response carries.
			// The gateway sets its OWN session cookie in the auth
			// middleware, on the gateway's own 303 response — a different
			// layer, entirely outside this proxy path — so stripping
			// upstream Set-Cookie here cannot interfere with gateway auth.
			resp.Header.Del("Set-Cookie")

			// F3 defense-in-depth: the view node is now seed-only, but even a
			// trusted node could be compromised, and any node's HTML response
			// (sticky/view paths) is otherwise forwarded to the browser
			// verbatim. Overwrite, don't append: an attacker-controlled
			// upstream must not be able to ship its own X-Frame-Options or
			// CSP and have it stand alongside or instead of the gateway's.
			setSecurityHeaders(resp.Header)
			return nil
		},

		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			// A client that hung up is not an error worth reporting, and the
			// response is already gone.
			if errors.Is(err, context.Canceled) || errors.Is(r.Context().Err(), context.Canceled) {
				return
			}
			addr, _ := r.Context().Value(targetKey{}).(string)

			// A node that cannot be dialled may simply have left between two
			// capacity polls. Nothing has been written yet, so hand the
			// failure back to the router, which can pick another node. The
			// router writes the 502 itself once the retry is spent.
			if holder, ok := r.Context().Value(unreachableKey{}).(*unreachableHolder); ok && isDialError(err) {
				logger.Warn("upstream not reachable", "node", addr, "path", r.URL.Path, "err", err)
				holder.set(err)
				return
			}

			logger.Error("upstream request failed", "node", addr, "path", r.URL.Path, "err", err)
			secureError(w, http.StatusBadGateway,
				"mesh node "+addr+" is not reachable", "upstream_unavailable")
		},
	}
	return f
}

// To forwards r to addr.
//
// It returns an error wrapping ErrUpstreamUnreachable when the node could not
// be dialled and nothing was written to w, which lets the router try another
// node. Every other outcome — including a 502 for a connection lost after the
// request was sent — is written to w as before, and To returns nil.
func (f *Forwarder) To(w http.ResponseWriter, r *http.Request, addr string) error {
	holder := &unreachableHolder{}
	ctx := context.WithValue(r.Context(), targetKey{}, addr)
	ctx = context.WithValue(ctx, unreachableKey{}, holder)
	f.proxy.ServeHTTP(w, r.WithContext(ctx))
	if err := holder.get(); err != nil {
		return fmt.Errorf("%w: %s: %w", ErrUpstreamUnreachable, addr, err)
	}
	return nil
}

// writeUnreachable answers a request whose node (or nodes, once the retry is
// spent) could not be reached at all.
func writeUnreachable(w http.ResponseWriter, node string) {
	secureError(w, http.StatusBadGateway,
		"mesh node "+node+" is not reachable", "upstream_unavailable")
}
