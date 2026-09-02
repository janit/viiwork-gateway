package proxy

import (
	"net/http"

	"github.com/janit/viiwork-gateway/internal/auth"
)

// contentSecurityPolicy is applied to every response the gateway returns —
// proxied and locally generated alike (see setSecurityHeaders).
//
// It is deliberately not `default-src 'none'`: the real viiwork dashboards
// (`/`, `/mesh`, `/chat`, `/prompt`) are served BY mesh nodes through this
// gateway and are known to rely on inline <script>/<style>, same-origin
// `fetch` to `/v1/*`, and same-origin `EventSource` (SSE). A same-origin-only
// policy that also allows inline script/style keeps those working while still
// closing the vectors an attacker actually needs:
//   - no cross-origin script/style/image/connect/frame loads
//   - no framing of the gateway's origin at all (frame-ancestors 'none')
//   - no form submission or <base> retargeting to another origin
//   - no plugin/object embeds
//
// IMPORTANT LIMITATION: `script-src 'self' 'unsafe-inline'` does NOT stop an
// injected inline <script> tag from executing — that is precisely what a
// hostile view node's HTML response would contain (see
// docs/security/adversarial-mesh.md finding F3). This CSP is defense-in-depth
// against exfiltration, cross-origin pivoting, and framing/clickjacking once
// script is already running; it does not by itself prevent that script from
// running in the first place. The primary control against that is the
// seed-only view-node election landed for F3 — a compromised-but-untrusted
// node can no longer become the view node at all. Removing 'unsafe-inline'
// would break the dashboards' legitimate inline script/style, so it stays;
// tightening it further would require the dashboards to adopt nonces/hashes,
// which is out of scope for this gateway-side change.
const contentSecurityPolicy = "default-src 'self'; " +
	"script-src 'self' 'unsafe-inline'; " +
	"style-src 'self' 'unsafe-inline'; " +
	"img-src 'self' data:; " +
	"connect-src 'self'; " +
	"frame-ancestors 'none'; " +
	"base-uri 'none'; " +
	"form-action 'self'; " +
	"object-src 'none'"

// setSecurityHeaders sets the belt-and-braces response headers (F3
// defense-in-depth): anti-MIME-sniffing, anti-framing, and the restrictive
// CSP above. It uses Header.Set, not Header.Add, so any value already present
// — including one an upstream mesh node put there itself — is replaced, not
// appended to. That matters: a hostile (or merely compromised) view node
// could otherwise ship its own `X-Frame-Options: ALLOWALL` or a permissive
// `Content-Security-Policy` and have it survive alongside, or instead of,
// the gateway's; browsers that honour multiple CSP headers intersect them,
// but X-Frame-Options and other single-value headers are not safe to leave
// to chance, so the gateway always wins.
//
// Applied uniformly to every response the gateway emits — proxied or
// locally generated, JSON or HTML or SSE. Response headers only constrain
// browsers, not the API clients hitting /v1/chat/completions and friends, so
// there is no downside to setting them on JSON/streamed responses too; the
// upside is that any HTML a hostile node manages to smuggle back through a
// path this gateway didn't anticipate is covered as well.
func setSecurityHeaders(h http.Header) {
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("X-Frame-Options", "DENY")
	h.Set("Content-Security-Policy", contentSecurityPolicy)
}

// secureError wraps auth.WriteError so the gateway's own locally-generated
// error responses carry the same security headers as everything else it
// returns. Headers must be set before auth.WriteError calls WriteHeader.
func secureError(w http.ResponseWriter, status int, message, code string) {
	setSecurityHeaders(w.Header())
	auth.WriteError(w, status, message, code)
}
