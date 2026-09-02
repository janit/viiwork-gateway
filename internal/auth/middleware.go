package auth

import (
	"context"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/janit/viiwork-gateway/internal/clientip"
	"github.com/janit/viiwork-gateway/internal/keys"
)

// CookieName is the browser session cookie. It carries a signed token, never
// the key itself.
const CookieName = "viiwork_gw"

// QueryParam is the bootstrap parameter a browser arrives with once.
const QueryParam = "key"

type contextKey struct{}

// LabelFromContext returns the authenticated key's label, or "" if the request
// was not authenticated.
func LabelFromContext(ctx context.Context) string {
	label, _ := ctx.Value(contextKey{}).(string)
	return label
}

// ContextWithLabel returns ctx carrying label as the authenticated key's
// label. Wrap does this for every real request; it is exported so code
// downstream of authentication (per-key rate limiting, for one) can be tested
// without standing up the whole middleware, and so both sides are guaranteed
// to use the same context key.
func ContextWithLabel(ctx context.Context, label string) context.Context {
	return context.WithValue(ctx, contextKey{}, label)
}

// Middleware authenticates every request. There is no unauthenticated path:
// the mesh behind this gateway has no authentication of its own.
type Middleware struct {
	Keys *keys.Set
	TTL  time.Duration
	// Now is injectable so tests can pin expiry behaviour.
	Now func() time.Time
	// Logger records rejections: source IP and reason, never the attempted
	// key. Defaults to slog.Default() when nil.
	Logger *slog.Logger
}

func (m *Middleware) now() time.Time {
	if m.Now != nil {
		return m.Now()
	}
	return time.Now()
}

func (m *Middleware) logger() *slog.Logger {
	if m.Logger != nil {
		return m.Logger
	}
	return slog.Default()
}

// Wrap authenticates, then hands a credential-free request to next.
func (m *Middleware) Wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 1. Bearer — API clients and OpenAI SDKs.
		if raw := r.Header.Get("Authorization"); raw != "" {
			secret, ok := strings.CutPrefix(raw, "Bearer ")
			if !ok {
				secret, _ = strings.CutPrefix(raw, "bearer ")
			}
			if label, valid := m.Keys.Lookup(strings.TrimSpace(secret)); valid {
				m.forward(w, r, next, label)
				return
			}
			m.reject(w, r, "invalid bearer")
			return
		}

		reason := "no credential presented"

		// 2. Cookie — browsers, and EventSource, which cannot set headers.
		if c, err := r.Cookie(CookieName); err == nil {
			if label, valid := Verify(c.Value, m.Keys.Secret, m.now()); valid {
				m.forward(w, r, next, label)
				return
			}
			reason = "invalid cookie"
		}

		// 3. ?key= — the browser bootstrap. Mint a cookie and redirect to the
		// same URL without the parameter, so the key leaves the address bar
		// and never reaches a Referer header.
		if secret := r.URL.Query().Get(QueryParam); secret != "" {
			if label, valid := m.Keys.Lookup(secret); valid {
				keySecret, _ := m.Keys.Secret(label)
				http.SetCookie(w, &http.Cookie{
					Name:     CookieName,
					Value:    Mint(label, keySecret, m.now().Add(m.TTL)),
					Path:     "/",
					HttpOnly: true,
					Secure:   true,
					SameSite: http.SameSiteLaxMode,
					Expires:  m.now().Add(m.TTL),
				})
				http.Redirect(w, r, cleanURL(r), http.StatusSeeOther)
				return
			}
			reason = "invalid key"
		}

		m.reject(w, r, reason)
	})
}

func (m *Middleware) forward(w http.ResponseWriter, r *http.Request, next http.Handler, label string) {
	next.ServeHTTP(w, stripCredentials(r, label))
}

// stripCredentials removes everything the gateway used to authenticate, so no
// gateway key can reach a viiwork node's logs or prompt history.
func stripCredentials(r *http.Request, label string) *http.Request {
	out := r.Clone(ContextWithLabel(r.Context(), label))
	out.Header.Del("Authorization")
	// The whole Cookie header goes: the gateway's own cookie is in there, and
	// a viiwork node has no use for any of the rest.
	out.Header.Del("Cookie")

	q := out.URL.Query()
	if q.Has(QueryParam) {
		q.Del(QueryParam)
		out.URL.RawQuery = q.Encode()
	}
	return out
}

// cleanURL returns the request-target to redirect back to once the ?key=
// bootstrap parameter is stripped. It always returns a same-origin,
// path-absolute target: a request line beginning "//" (e.g.
// "GET //evil.example.com/x?key=...") parses into a *url.URL whose Path is
// "//evil.example.com/x", and RequestURI() on that is a protocol-relative
// URL — one a browser follows off-site — even though nothing about it names
// a scheme or host. A leading "/" is folded down to one so the redirect can
// only ever target this same server.
func cleanURL(r *http.Request) string {
	u := *r.URL
	q := u.Query()
	q.Del(QueryParam)
	u.RawQuery = q.Encode()
	if u.Path == "" {
		u.Path = "/"
	}
	for strings.HasPrefix(u.Path, "//") {
		u.Path = u.Path[1:]
	}
	return u.RequestURI()
}

const unauthorizedHTML = `<!doctype html>
<title>viiwork-gateway</title>
<style>body{font:16px system-ui;margin:4rem auto;max-width:34rem;padding:0 1rem}code{background:#eee;padding:.15em .4em;border-radius:3px}</style>
<h1>Authentication required</h1>
<p>Append your API key once and the browser will be issued a session cookie:</p>
<p><code>?key=YOUR_API_KEY</code></p>
<p>API clients should send <code>Authorization: Bearer YOUR_API_KEY</code> instead.</p>
`

// reject answers in whichever dialect the caller speaks: OpenAI's error shape
// for an API client, a readable page for a browser. It logs the source IP and
// reason for every rejection — and never the attempted credential, in any
// form, so a brute-force run against this internet-facing gateway still shows
// up in the log stream.
func (m *Middleware) reject(w http.ResponseWriter, r *http.Request, reason string) {
	m.logger().Warn("authentication rejected",
		"method", r.Method,
		"path", r.URL.Path,
		"remote", clientip.From(r),
		"reason", reason,
	)

	if wantsHTML(r) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(unauthorizedHTML))
		return
	}
	WriteError(w, http.StatusUnauthorized, "invalid API key", "invalid_api_key")
}

func wantsHTML(r *http.Request) bool {
	if r.Header.Get("Authorization") != "" {
		return false
	}
	return strings.Contains(r.Header.Get("Accept"), "text/html")
}
