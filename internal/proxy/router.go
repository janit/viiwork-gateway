package proxy

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"math"
	"net"
	"net/http"
	"os"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/janit/viiwork-gateway/internal/auth"
	"github.com/janit/viiwork-gateway/internal/mesh"
	"github.com/janit/viiwork-gateway/internal/ratelimit"
	"github.com/janit/viiwork/meshapi"
)

// modelEntry and modelList are the OpenAI catalogue shape. They are declared
// here rather than taken from meshapi because they are OpenAI's contract, not
// the mesh's — meshapi covers what nodes say to each other.
type modelEntry struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	OwnedBy string `json:"owned_by"`
}

type modelList struct {
	Object string       `json:"object"`
	Data   []modelEntry `json:"data"`
}

// defaultMaxInFlight is the fallback used when NewRouter is given a
// non-positive maxInFlight (defensive: config.Load's own default is 256,
// so this only matters for a caller — e.g. a test — that passes 0).
const defaultMaxInFlight = 256

// landingPath is the gateway's own front door, and meshPagePath is the
// fleet-wide mesh view on a viiwork node. Neither is in meshapi: that package
// covers what nodes say to each other, and these are the node's HTML pages.
// A GET of the first is served by the second — see ServeHTTP.
const (
	landingPath  = "/"
	meshPagePath = "/mesh"
)

// defaultBodyReadTimeout is the fallback used when NewRouter is given a
// non-positive bodyReadTimeout (defensive: config.Load's own default is
// 30s, so this only matters for a caller — e.g. a test — that passes 0).
const defaultBodyReadTimeout = 30 * time.Second

// Router decides which node answers a request.
type Router struct {
	reg     *mesh.Registry
	fwd     *Forwarder
	maxBody int64
	log     *slog.Logger

	// bodyReadTimeout bounds how long readBody may take to read a request
	// body, whether called from routeByModel or routeToView. It is applied
	// as a per-request connection read
	// deadline (via http.NewResponseController), set immediately before the
	// read and cleared immediately after — never as http.Server's
	// ReadTimeout, which would also apply while a long inference response
	// or SSE stream is still being written. See docs/security/
	// adversarial-resource.md finding R2 and cmd/viiwork-gateway/main.go's
	// WriteTimeout: 0 / ResponseHeaderTimeout: 0.
	bodyReadTimeout time.Duration

	// tokens is a buffered channel used as a counting semaphore: its buffer
	// size is the cap on concurrent in-flight proxied requests (the
	// model-routed and sticky/view forwarding paths, which buffer bodies or
	// hold upstream connections open). A request takes a token before doing
	// that work and returns it when done; acquisition is non-blocking (see
	// tryAcquire) so a request over the cap fails fast with 503 instead of
	// queuing behind slow or malicious in-flight requests.
	tokens chan struct{}

	// limiter meters the model-routed path per API key, which the in-flight
	// cap above deliberately does not: that cap bounds concurrency (and so
	// memory), leaving one key free to issue unlimited *sequential* requests.
	// A nil limiter means rate limiting is switched off
	// (VIIWORK_GW_RATE_PER_MIN=0); ratelimit.Limiter handles the nil receiver
	// so there is no nil check at the call site.
	limiter *ratelimit.Limiter
}

// NewRouter wires the routing table. maxInFlight caps concurrent in-flight
// proxied requests (see Router.tokens); a non-positive value falls back to
// defaultMaxInFlight. bodyReadTimeout bounds how long a request body may
// take to arrive (see Router.bodyReadTimeout); a non-positive value falls
// back to defaultBodyReadTimeout. limiter meters the model-routed path per
// API key and may be nil, meaning unmetered.
func NewRouter(reg *mesh.Registry, fwd *Forwarder, maxBody int64, maxInFlight int, bodyReadTimeout time.Duration, limiter *ratelimit.Limiter, logger *slog.Logger) *Router {
	if logger == nil {
		logger = slog.Default()
	}
	if maxInFlight <= 0 {
		maxInFlight = defaultMaxInFlight
	}
	if bodyReadTimeout <= 0 {
		bodyReadTimeout = defaultBodyReadTimeout
	}
	return &Router{
		reg:             reg,
		fwd:             fwd,
		maxBody:         maxBody,
		log:             logger,
		tokens:          make(chan struct{}, maxInFlight),
		bodyReadTimeout: bodyReadTimeout,
		limiter:         limiter,
	}
}

// tryAcquire attempts to take a concurrency token without blocking. It
// returns a release func to call exactly once (via defer) when the caller
// is done, and false if the cap is currently exhausted — in which case no
// token was taken and there is nothing to release.
func (rt *Router) tryAcquire() (release func(), ok bool) {
	select {
	case rt.tokens <- struct{}{}:
		return func() { <-rt.tokens }, true
	default:
		return nil, false
	}
}

// writeOverCapacity answers a request that arrived while the in-flight cap
// was exhausted: 503 in the OpenAI error shape, plus Retry-After so a
// well-behaved client backs off briefly instead of retrying in a tight loop.
func writeOverCapacity(w http.ResponseWriter) {
	w.Header().Set("Retry-After", "1")
	secureError(w, http.StatusServiceUnavailable,
		"gateway is at its concurrent in-flight request limit; retry shortly",
		"too_many_inflight_requests")
}

// writeRateLimited answers a request from a key that has spent its
// allowance: 429 in the OpenAI error shape, plus Retry-After so an SDK backs
// off for the right amount of time instead of hammering. retryAfter is
// rounded UP to whole seconds — rounding down would invite a client back
// before a token exists and produce a second 429 — with a floor of 1, since
// "Retry-After: 0" reads as "immediately".
func writeRateLimited(w http.ResponseWriter, retryAfter time.Duration) {
	seconds := int(math.Ceil(retryAfter.Seconds()))
	if seconds < 1 {
		seconds = 1
	}
	w.Header().Set("Retry-After", strconv.Itoa(seconds))
	secureError(w, http.StatusTooManyRequests,
		"rate limit exceeded for this API key; retry after the interval in the Retry-After header",
		"rate_limit_exceeded")
}

func (rt *Router) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch {
	// Denied. Chassis power control, not GPU wattage caps: /v1/power acts
	// in-band on the node's host and /v1/mesh/power falls back to the BMC,
	// which is the only path that reaches a host that is already off.
	// Switching machines off stays a tailnet-only capability.
	case isPowerPath(r.URL.Path):
		secureError(w, http.StatusForbidden,
			"chassis power control is not available through the gateway; use the tailnet",
			"forbidden_path")

	// Aggregated. Answered here, because a node's own catalogue is only as
	// complete as that node's peer configuration.
	case r.URL.Path == meshapi.PathModels && r.Method == http.MethodGet:
		rt.serveModels(w)

	// Model-routed.
	case r.Method == http.MethodPost && isInferencePath(r.URL.Path):
		rt.routeByModel(w, r)

	// Landing page. The bare hostname serves the fleet-wide mesh view, not
	// the single-node dashboard a viiwork node answers "/" with — through
	// this gateway the mesh is the thing worth looking at, and a dashboard
	// scoped to whichever node happened to be the view node is a confusing
	// front door. The rewrite is upstream-only: the browser keeps "/" in its
	// address bar, and because rewritePath leaves the inbound request alone,
	// the access log still records the path the client actually asked for.
	//
	// GET only, matching the node's own handler, so nothing that would have
	// 404ed upstream starts succeeding here. The node's single-node
	// dashboard is deliberately not reachable through the gateway any more;
	// it stays available on the tailnet.
	case r.Method == http.MethodGet && r.URL.Path == landingPath:
		rt.routeToView(w, rewritePath(r, meshPagePath))

	// Sticky. Everything per-node: request ids are per-node and
	// /v1/mesh/prompt?addr= is validated against that node's own peer list.
	default:
		rt.routeToView(w, r)
	}
}

// isPowerPath decides the power-path deny on a normalised form of p, not the
// raw string. The gateway forwards whatever path it does not deny byte-for-
// byte to the node, and a node's own HTTP stack may resolve "//v1/power",
// "/v1/./power", "/v1/power/", or "/v1/x/../power" down to "/v1/power"
// before routing — even though net/http's ServeMux (what this gateway uses)
// does not collapse those itself. So the deny must be evaluated on the same
// canonical form a node could resolve to, or a single API key can walk a
// path variant straight past this check to a node that then actuates
// chassis power. path.Clean collapses duplicate slashes and resolves "."/
// ".." segments; a trailing slash surviving Clean (only for non-root paths)
// is stripped separately. The comparison is case-insensitive as further
// defence in depth, since a node's router could itself be case-insensitive.
//
// This function is deliberately exact-match only, never prefix or substring:
// "/v1/powerful" and "/v1/power/status" must keep working normally.
func isPowerPath(p string) bool {
	cleaned := path.Clean(p)
	if cleaned != "/" {
		cleaned = strings.TrimSuffix(cleaned, "/")
	}
	return strings.EqualFold(cleaned, meshapi.PathPower) || strings.EqualFold(cleaned, meshapi.PathMeshPower)
}

func isInferencePath(p string) bool {
	return p == meshapi.PathChatCompletions ||
		p == meshapi.PathCompletions ||
		p == meshapi.PathEmbeddings
}

// rewritePath returns a request that will be forwarded to the node as path p,
// leaving r itself untouched. The copy matters: the access log wraps this
// router and reads r.URL.Path *after* the handler returns, and r.WithContext
// down the forwarding path shares the same *url.URL pointer — so mutating the
// path in place would silently rewrite the log line too, and the operator
// would never see which URL the client actually requested. Clone deep-copies
// the URL (and the headers), so the only thing the node sees differently is
// the path.
func rewritePath(r *http.Request, p string) *http.Request {
	out := r.Clone(r.Context())
	out.URL.Path = p
	// RawPath is only consulted when it is a *different* encoding of Path;
	// carrying the old one over would have the proxy send the pre-rewrite
	// path on the wire.
	out.URL.RawPath = ""
	return out
}

func (rt *Router) serveModels(w http.ResponseWriter) {
	ids := rt.reg.Snapshot().ModelIDs()
	out := modelList{Object: "list", Data: make([]modelEntry, 0, len(ids))}
	for _, id := range ids {
		out.Data = append(out.Data, modelEntry{ID: id, Object: "model", OwnedBy: "viiwork"})
	}
	setSecurityHeaders(w.Header())
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
}

func (rt *Router) routeByModel(w http.ResponseWriter, r *http.Request) {
	// Checked first, before a concurrency token is taken and before the body
	// is read: a throttled request must cost a map lookup and nothing else.
	// Taking a token here and returning early would risk leaking it, and
	// reading the body first would let a key past its limit keep forcing the
	// gateway to buffer up to maxBody per request — the very memory the cap
	// exists to bound.
	//
	// The label is the authenticated key's, set by auth.Middleware. An empty
	// label cannot occur in production (nothing reaches the router
	// unauthenticated); if it somehow did, those requests would share one
	// bucket, which fails closed rather than open.
	//
	// Nothing is logged here on purpose. The access log already emits one
	// line per request carrying the status and the key's label, so a warning
	// here would only double the log volume produced by a key hammering past
	// its limit — turning a defence into a log-amplification vector. The
	// in-flight cap's 503 stays silent for the same reason.
	if allowed, retryAfter := rt.limiter.Allow(auth.LabelFromContext(r.Context())); !allowed {
		writeRateLimited(w, retryAfter)
		return
	}

	// Acquired before readBody: that call allocates up to maxBody bytes, and
	// the whole point of the cap is to bound how many of those buffers can
	// exist at once. Acquiring any later would leave memory unbounded.
	release, ok := rt.tryAcquire()
	if !ok {
		writeOverCapacity(w)
		return
	}
	defer release()

	body, err := rt.readBody(w, r)
	if err != nil {
		// readBody returns three distinct kinds of error: errBodyTooLarge for
		// a body that exceeded the configured limit, a read-deadline timeout
		// for a body that arrived too slowly (or not at all — see R2/H5),
		// and any other error from io.ReadAll — a client disconnecting
		// mid-upload, for instance. Reporting 413 for a timeout, or vice
		// versa, would send the operator hunting the wrong configuration
		// setting.
		if errors.Is(err, errBodyTooLarge) {
			secureError(w, http.StatusRequestEntityTooLarge,
				"request body exceeds the configured limit", "body_too_large")
			return
		}
		if isReadTimeout(err) {
			secureError(w, http.StatusRequestTimeout,
				"request body did not arrive within the allotted time", "body_read_timeout")
			return
		}
		secureError(w, http.StatusBadRequest,
			"request body could not be read in full", "invalid_request_body")
		return
	}

	// A body that will not parse, or carries no model, is not the gateway's
	// to reject: forward it and let the node answer.
	var probe struct {
		Model string `json:"model"`
	}
	_ = json.Unmarshal(body, &probe)

	addr, ok := rt.reg.PickForModel(probe.Model)
	if !ok {
		view, haveView := rt.reg.ViewAddr()
		if !haveView {
			rt.log.Warn("no healthy mesh node available", "path", r.URL.Path, "model", probe.Model)
			secureError(w, http.StatusServiceUnavailable,
				"no mesh node is currently reachable", "no_healthy_node")
			return
		}
		addr = view
	}

	if info := RequestInfoFromContext(r.Context()); info != nil {
		info.Model = probe.Model
		info.Node = addr
	}
	rt.fwd.To(w, r, addr)
}

func (rt *Router) routeToView(w http.ResponseWriter, r *http.Request) {
	// Sticky/view requests hold a real upstream connection, potentially for
	// the life of a stream, so they count against the same cap.
	release, acquired := rt.tryAcquire()
	if !acquired {
		writeOverCapacity(w)
		return
	}
	defer release()

	addr, ok := rt.reg.ViewAddr()
	if !ok {
		rt.log.Warn("no healthy mesh node available", "path", r.URL.Path)
		secureError(w, http.StatusServiceUnavailable,
			"no mesh node is currently reachable", "no_healthy_node")
		return
	}

	// Bound the body read the same way routeByModel does (see the
	// final-review sticky-path finding at docs/security/
	// adversarial-resource.md R2): the gateway forwards ANY method/path
	// through this default case, so a slow-body request here — a POST to a
	// sticky path such as /v1/mesh/prompt, or any unmatched path — would
	// otherwise be read with no deadline while holding an in-flight token
	// (H4), two FDs, and an upstream connection. readBody buffers the body
	// (there is no model to parse here, only the buffering-under-deadline
	// matters), then clears the deadline before returning so the
	// (possibly long-lived, streaming) response phase below is unbounded on
	// the write side, exactly as for the model-routed path. A request with
	// no body (every sticky GET) hits EOF immediately and is unaffected.
	if _, err := rt.readBody(w, r); err != nil {
		if errors.Is(err, errBodyTooLarge) {
			secureError(w, http.StatusRequestEntityTooLarge,
				"request body exceeds the configured limit", "body_too_large")
			return
		}
		if isReadTimeout(err) {
			secureError(w, http.StatusRequestTimeout,
				"request body did not arrive within the allotted time", "body_read_timeout")
			return
		}
		secureError(w, http.StatusBadRequest,
			"request body could not be read in full", "invalid_request_body")
		return
	}

	if info := RequestInfoFromContext(r.Context()); info != nil {
		info.Node = addr
	}
	rt.fwd.To(w, r, addr)
}

// readBody buffers the request body — so routeByModel can read the model out
// of it, or so routeToView can simply bound the read — then restores it for
// forwarding.
//
// It bounds the read with a per-request connection read deadline (R2/H5): a
// client that completes headers and then trickles or withholds the body
// forever would otherwise pin this goroutine and its FD in io.ReadAll
// indefinitely, since http.Server's ReadHeaderTimeout only covers headers
// and WriteTimeout is deliberately 0 (long inference generations and SSE
// streams must not be cut off). The deadline is set on the connection just
// before the read via http.NewResponseController — NOT http.Server's
// ReadTimeout, which would remain armed while the response is written and
// would sever exactly those long-lived streams — and is cleared immediately
// after so it cannot linger into the response/streaming phase.
//
// If the ResponseWriter given to us does not support SetReadDeadline (it
// returns http.ErrNotSupported, e.g. an httptest.ResponseRecorder in a
// test), this degrades gracefully: the read proceeds unbounded rather than
// failing the request. See internal/accesslog's recorder.Unwrap, which is
// what lets http.NewResponseController see through the access-log wrapper
// to the real connection in production.
func (rt *Router) readBody(w http.ResponseWriter, r *http.Request) ([]byte, error) {
	if r.Body == nil {
		return nil, nil
	}

	rc := http.NewResponseController(w)
	if err := rc.SetReadDeadline(time.Now().Add(rt.bodyReadTimeout)); err != nil && !errors.Is(err, http.ErrNotSupported) {
		rt.log.Warn("could not set body-read deadline", "err", err)
	}
	defer func() {
		// Zero time.Time clears the deadline. This must happen before
		// returning to routeByModel, which then forwards the request and
		// may hold this connection open for a long-running response.
		if err := rc.SetReadDeadline(time.Time{}); err != nil && !errors.Is(err, http.ErrNotSupported) {
			rt.log.Warn("could not clear body-read deadline", "err", err)
		}
	}()

	body, err := io.ReadAll(io.LimitReader(r.Body, rt.maxBody+1))
	if err != nil {
		return nil, err
	}
	if int64(len(body)) > rt.maxBody {
		return nil, errBodyTooLarge
	}

	r.Body = io.NopCloser(bytes.NewReader(body))
	r.ContentLength = int64(len(body))
	r.GetBody = func() (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(body)), nil
	}
	return body, nil
}

// isReadTimeout reports whether err is the result of the body-read deadline
// set in readBody firing, as opposed to some other read failure (a client
// disconnecting mid-upload, for instance). A fired deadline surfaces as a
// net.Error with Timeout() true wrapping os.ErrDeadlineExceeded; both checks
// are made since the exact wrapping can depend on the underlying
// net.Conn/ResponseWriter implementation.
func isReadTimeout(err error) bool {
	if errors.Is(err, os.ErrDeadlineExceeded) {
		return true
	}
	var netErr net.Error
	return errors.As(err, &netErr) && netErr.Timeout()
}

type bodyTooLargeError struct{}

func (bodyTooLargeError) Error() string { return "request body too large" }

var errBodyTooLarge = bodyTooLargeError{}
