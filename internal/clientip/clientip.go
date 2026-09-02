// Package clientip determines the client address a request should be
// attributed to.
//
// It lives apart from internal/accesslog so internal/auth can use it too:
// accesslog already imports auth for LabelFromContext, so auth importing
// accesslog would be an import cycle. Keeping one copy of this logic here
// stops the two call sites drifting apart on a security-relevant detail.
package clientip

import (
	"net"
	"net/http"
	"strings"
)

// From returns the client address a request should be attributed to.
func From(r *http.Request) string {
	// Caddy appends the peer it observed to the X-Forwarded-For header rather than
	// replacing it. The client can send its own X-Forwarded-For, and Caddy appends
	// to that, so the format is "<client-claimed>, <client-claimed>, <caddy-observed>".
	// Only the rightmost entry is trustworthy; leftmost entries are attacker-controlled.
	if fwd := r.Header.Get("X-Forwarded-For"); fwd != "" {
		// Split on comma and take the rightmost non-empty entry.
		entries := strings.Split(fwd, ",")
		for i := len(entries) - 1; i >= 0; i-- {
			ip := strings.TrimSpace(entries[i])
			if ip != "" {
				// Try to strip port if present; if it fails, use the value as-is
				// since XFF entries are usually bare IPs.
				if host, _, err := net.SplitHostPort(ip); err == nil {
					return host
				}
				return ip
			}
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}
