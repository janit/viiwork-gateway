// Package proxy forwards authenticated requests into the viiwork mesh.
package proxy

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"strings"
	"time"
)

type targetKey struct{}

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
			logger.Error("upstream request failed", "node", addr, "path", r.URL.Path, "err", err)
			secureError(w, http.StatusBadGateway,
				"mesh node "+addr+" is not reachable", "upstream_unavailable")
		},
	}
	return f
}

// To forwards r to addr.
func (f *Forwarder) To(w http.ResponseWriter, r *http.Request, addr string) {
	f.proxy.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), targetKey{}, addr)))
}
