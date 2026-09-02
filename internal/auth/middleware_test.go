package auth

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/janit/viiwork-gateway/internal/keys"
)

const testKey = "aaaaaaaaaaaaaaaaaaaaaaaaaaaa"

func testMiddleware(t *testing.T) *Middleware {
	t.Helper()
	set, err := keys.Load([]string{"VIIWORK_KEY_janit=" + testKey})
	if err != nil {
		t.Fatalf("keys.Load: %v", err)
	}
	return &Middleware{
		Keys: set,
		TTL:  time.Hour,
		Now:  func() time.Time { return time.Unix(1_000_000, 0) },
	}
}

// echoHandler records what the wrapped handler actually received.
type echoHandler struct {
	called bool
	label  string
	auth   string
	cookie string
	query  string
}

func (e *echoHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	e.called = true
	e.label = LabelFromContext(r.Context())
	e.auth = r.Header.Get("Authorization")
	e.cookie = r.Header.Get("Cookie")
	e.query = r.URL.RawQuery
	w.WriteHeader(http.StatusOK)
}

func TestBearerAccepted(t *testing.T) {
	m := testMiddleware(t)
	next := &echoHandler{}
	req := httptest.NewRequest("GET", "/v1/models", nil)
	req.Header.Set("Authorization", "Bearer "+testKey)
	rec := httptest.NewRecorder()

	m.Wrap(next).ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if !next.called {
		t.Fatal("handler was not called")
	}
	if next.label != "janit" {
		t.Errorf("label in context = %q, want janit", next.label)
	}
}

func TestBearerRejected(t *testing.T) {
	m := testMiddleware(t)
	next := &echoHandler{}
	req := httptest.NewRequest("GET", "/v1/models", nil)
	req.Header.Set("Authorization", "Bearer wrong-key-wrong-key-wrong")
	rec := httptest.NewRecorder()

	m.Wrap(next).ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
	if next.called {
		t.Fatal("handler ran for an invalid key")
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "application/json") {
		t.Errorf("Content-Type = %q, want JSON for an API caller", ct)
	}
	if !strings.Contains(rec.Body.String(), "invalid_api_key") {
		t.Errorf("body should use the OpenAI error shape, got: %s", rec.Body.String())
	}
}

func TestNoCredentialHTMLGetsHTML(t *testing.T) {
	m := testMiddleware(t)
	req := httptest.NewRequest("GET", "/mesh", nil)
	req.Header.Set("Accept", "text/html,application/xhtml+xml")
	rec := httptest.NewRecorder()

	m.Wrap(&echoHandler{}).ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "text/html") {
		t.Errorf("Content-Type = %q, want HTML for a browser", ct)
	}
	if !strings.Contains(rec.Body.String(), "?key=") {
		t.Error("the HTML page should tell the operator how to authenticate")
	}
}

func TestQueryKeyBootstrapsCookie(t *testing.T) {
	m := testMiddleware(t)
	next := &echoHandler{}
	req := httptest.NewRequest("GET", "/mesh?key="+testKey+"&tab=models", nil)
	rec := httptest.NewRecorder()

	m.Wrap(next).ServeHTTP(rec, req)

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303", rec.Code)
	}
	if next.called {
		t.Fatal("the bootstrap request should redirect, not reach the handler")
	}

	loc := rec.Header().Get("Location")
	if strings.Contains(loc, "key=") {
		t.Errorf("Location %q still carries the key", loc)
	}
	if !strings.Contains(loc, "tab=models") {
		t.Errorf("Location %q dropped unrelated query parameters", loc)
	}

	cookies := rec.Result().Cookies()
	if len(cookies) != 1 {
		t.Fatalf("got %d cookies, want 1", len(cookies))
	}
	c := cookies[0]
	if c.Name != CookieName {
		t.Errorf("cookie name = %q, want %q", c.Name, CookieName)
	}
	if !c.HttpOnly || !c.Secure || c.SameSite != http.SameSiteLaxMode || c.Path != "/" {
		t.Errorf("cookie flags wrong: HttpOnly=%v Secure=%v SameSite=%v Path=%q",
			c.HttpOnly, c.Secure, c.SameSite, c.Path)
	}
	if label, ok := Verify(c.Value, m.Keys.Secret, m.Now()); !ok || label != "janit" {
		t.Errorf("cookie does not verify: label=%q ok=%v", label, ok)
	}
}

// A request target beginning "//" parses into a *url.URL whose Path is
// itself "//host/path" — a protocol-relative URL. Unless cleanURL guards
// against it, the bootstrap redirect would carry a valid key straight into
// an open redirect: a browser follows Location: //evil.example.com/x
// off-site.
func TestQueryKeyBootstrapNeverRedirectsOffOrigin(t *testing.T) {
	m := testMiddleware(t)
	next := &echoHandler{}
	req := httptest.NewRequest("GET", "//evil.example.com/x?key="+testKey, nil)
	rec := httptest.NewRecorder()

	m.Wrap(next).ServeHTTP(rec, req)

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303", rec.Code)
	}
	loc := rec.Header().Get("Location")
	if strings.HasPrefix(loc, "//") {
		t.Fatalf("Location %q is protocol-relative: a browser would follow it off-site", loc)
	}
	if strings.Contains(loc, "key=") {
		t.Errorf("Location %q still carries the key", loc)
	}
}

func TestQueryKeyInvalidDoesNotSetCookie(t *testing.T) {
	m := testMiddleware(t)
	req := httptest.NewRequest("GET", "/mesh?key=nope-nope-nope-nope-nope-nope", nil)
	rec := httptest.NewRecorder()

	m.Wrap(&echoHandler{}).ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
	if len(rec.Result().Cookies()) != 0 {
		t.Fatal("a rejected key must not mint a cookie")
	}
}

func TestCookieAccepted(t *testing.T) {
	m := testMiddleware(t)
	next := &echoHandler{}
	req := httptest.NewRequest("GET", "/v1/mesh/stream", nil)
	req.AddCookie(&http.Cookie{
		Name:  CookieName,
		Value: Mint("janit", testKey, m.Now().Add(time.Hour)),
	})
	rec := httptest.NewRecorder()

	m.Wrap(next).ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if next.label != "janit" {
		t.Errorf("label = %q, want janit", next.label)
	}
}

// Gateway credentials must never reach a viiwork node, where they would land
// in its logs and prompt history.
func TestCredentialsStrippedBeforeForwarding(t *testing.T) {
	m := testMiddleware(t)
	next := &echoHandler{}
	req := httptest.NewRequest("GET", "/v1/models?key="+testKey+"&keep=1", nil)
	req.Header.Set("Authorization", "Bearer "+testKey)
	req.AddCookie(&http.Cookie{Name: CookieName, Value: "whatever"})
	req.AddCookie(&http.Cookie{Name: "unrelated", Value: "x"})
	rec := httptest.NewRecorder()

	m.Wrap(next).ServeHTTP(rec, req)

	if !next.called {
		t.Fatalf("handler not called, status %d", rec.Code)
	}
	if next.auth != "" {
		t.Errorf("Authorization forwarded upstream: %q", next.auth)
	}
	if next.cookie != "" {
		t.Errorf("Cookie forwarded upstream: %q", next.cookie)
	}
	if strings.Contains(next.query, "key=") && !strings.Contains(next.query, "keep=") {
		t.Errorf("query not cleaned: %q", next.query)
	}
	if strings.Contains(next.query, testKey) {
		t.Errorf("the key survived in the query: %q", next.query)
	}
	if !strings.Contains(next.query, "keep=1") {
		t.Errorf("unrelated query parameters were dropped: %q", next.query)
	}
}

// Authentication failures must log source IP and reason, and must never log
// the attempted credential in any form — not the raw key, a prefix, a
// length, or a hash — even when that credential is presented twice, once as
// a bearer token and once as the ?key= bootstrap parameter.
func TestRejectionLogsIPAndReasonNeverTheCredential(t *testing.T) {
	var buf bytes.Buffer
	m := testMiddleware(t)
	m.Logger = slog.New(slog.NewJSONHandler(&buf, nil))

	const secret = "super-secret-attempted-key-value"
	req := httptest.NewRequest("GET", "/v1/models?key="+secret, nil)
	req.Header.Set("Authorization", "Bearer "+secret)
	req.RemoteAddr = "203.0.113.7:54321"
	rec := httptest.NewRecorder()

	m.Wrap(&echoHandler{}).ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
	if bytes.Contains(buf.Bytes(), []byte(secret)) {
		t.Fatalf("log line contains the attempted credential: %s", buf.String())
	}

	var entry map[string]any
	if err := json.Unmarshal(buf.Bytes(), &entry); err != nil {
		t.Fatalf("log line is not JSON: %v (%s)", err, buf.String())
	}
	if entry["path"] != "/v1/models" {
		t.Errorf("path = %v, want /v1/models", entry["path"])
	}
	if entry["remote"] != "203.0.113.7" {
		t.Errorf("remote = %v, want 203.0.113.7", entry["remote"])
	}
	if entry["reason"] == nil || entry["reason"] == "" {
		t.Error("reason missing from rejection log line")
	}
}

// Rejections for an invalid bearer and for no credential at all must be
// distinguishable in the log.
func TestRejectionReasonDistinguishesBearerFromMissing(t *testing.T) {
	var bearerBuf, missingBuf bytes.Buffer

	m := testMiddleware(t)
	m.Logger = slog.New(slog.NewJSONHandler(&bearerBuf, nil))
	req := httptest.NewRequest("GET", "/v1/models", nil)
	req.Header.Set("Authorization", "Bearer wrong-key-wrong-key-wrong")
	m.Wrap(&echoHandler{}).ServeHTTP(httptest.NewRecorder(), req)

	m2 := testMiddleware(t)
	m2.Logger = slog.New(slog.NewJSONHandler(&missingBuf, nil))
	m2.Wrap(&echoHandler{}).ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/v1/models", nil))

	var bearerEntry, missingEntry map[string]any
	if err := json.Unmarshal(bearerBuf.Bytes(), &bearerEntry); err != nil {
		t.Fatalf("bearer log line is not JSON: %v (%s)", err, bearerBuf.String())
	}
	if err := json.Unmarshal(missingBuf.Bytes(), &missingEntry); err != nil {
		t.Fatalf("missing-credential log line is not JSON: %v (%s)", err, missingBuf.String())
	}
	if bearerEntry["reason"] != "invalid bearer" {
		t.Errorf("bearer reason = %v, want %q", bearerEntry["reason"], "invalid bearer")
	}
	if missingEntry["reason"] != "no credential presented" {
		t.Errorf("missing-credential reason = %v, want %q", missingEntry["reason"], "no credential presented")
	}
	if bearerEntry["reason"] == missingEntry["reason"] {
		t.Error("invalid bearer and no-credential rejections must be distinguishable")
	}
}

// TestContextWithLabelRoundTrips covers the setter the proxy layer needs to
// read the authenticated label back out. Without an exported setter, code
// outside this package can only obtain a labelled context by running the real
// middleware, which makes downstream unit tests (rate limiting, for one) far
// heavier than they need to be.
func TestContextWithLabelRoundTrips(t *testing.T) {
	ctx := ContextWithLabel(context.Background(), "alice")

	if got := LabelFromContext(ctx); got != "alice" {
		t.Fatalf("LabelFromContext = %q, want %q", got, "alice")
	}
}

// TestContextWithLabelUsesTheSameKeyAsTheMiddleware guards against the setter
// drifting onto a different context key from the one Wrap uses — which would
// leave it round-tripping in its own tests while reading nothing in
// production.
func TestContextWithLabelUsesTheSameKeyAsTheMiddleware(t *testing.T) {
	var seen string
	mw := testMiddleware(t)
	h := mw.Wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = LabelFromContext(r.Context())
	}))

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Authorization", "Bearer "+testKey)
	h.ServeHTTP(httptest.NewRecorder(), req)

	if seen == "" {
		t.Fatal("the middleware set no readable label")
	}
	if got := LabelFromContext(ContextWithLabel(context.Background(), seen)); got != seen {
		t.Fatalf("setter and middleware disagree: %q vs %q", got, seen)
	}
}
