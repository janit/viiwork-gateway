// Package accesslog records one structured line per request.
package accesslog

import (
	"log/slog"
	"net/http"
	"time"

	"github.com/janit/viiwork-gateway/internal/auth"
	"github.com/janit/viiwork-gateway/internal/clientip"
	"github.com/janit/viiwork-gateway/internal/proxy"
)

// recorder captures status and size without taking the response away from the
// handler. It forwards Flush, because every SSE endpoint depends on it.
type recorder struct {
	http.ResponseWriter
	status int
	bytes  int
}

func (rec *recorder) WriteHeader(code int) {
	if rec.status == 0 {
		rec.status = code
	}
	rec.ResponseWriter.WriteHeader(code)
}

func (rec *recorder) Write(b []byte) (int, error) {
	if rec.status == 0 {
		rec.status = http.StatusOK
	}
	n, err := rec.ResponseWriter.Write(b)
	rec.bytes += n
	return n, err
}

func (rec *recorder) Flush() {
	if f, ok := rec.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Unwrap exposes the wrapped ResponseWriter so http.ResponseController can
// see through this recorder to the underlying connection's rwc/deadline
// setters (SetReadDeadline, SetWriteDeadline). Without this, a
// ResponseController built on a *recorder fails every method with
// http.ErrNotSupported — including the per-request body-read deadline
// internal/proxy.Router sets around readBody (see R2/H5) — because
// ResponseController only follows an Unwrap() http.ResponseWriter chain, and
// http.ResponseWriter itself has no SetReadDeadline method to call directly.
func (rec *recorder) Unwrap() http.ResponseWriter {
	return rec.ResponseWriter
}

// Wrap logs each request. It records the key's label, never the key: this is
// what makes labelled keys worth having, since the log is where a misbehaving
// key becomes visible and therefore revocable.
func Wrap(next http.Handler, logger *slog.Logger) http.Handler {
	if logger == nil {
		logger = slog.Default()
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &recorder{ResponseWriter: w}

		// Placed into the context BEFORE next runs, so that Router — several
		// layers downstream — can fill in the model and node it decided on.
		// Router derives its own request via r.WithContext() to pass to the
		// forwarder, and context values propagate downward only, so a plain
		// context.WithValue set here could never be read back by this
		// handler once next returns. A pointer shared through the context
		// and mutated in place sidesteps that: both sides see the same
		// *proxy.RequestInfo regardless of which derived request holds it.
		info := &proxy.RequestInfo{}
		r = r.WithContext(proxy.ContextWithRequestInfo(r.Context(), info))

		// Logged from a defer, because a stream that fails after its first
		// byte does not return: ReverseProxy aborts it by panicking with
		// http.ErrAbortHandler. A flag rather than recover() tells the two
		// apart, so a panic carries on to net/http with its own stack intact.
		completed := false
		defer func() {
			logRequest(logger, r, rec, info, start, !completed)
		}()

		next.ServeHTTP(rec, r)
		completed = true
	})
}

// logRequest writes the one line for a finished request. aborted marks a
// request whose handler never returned — a client that hung up mid-stream, or
// a node that died mid-generation — so status and bytes are what was sent
// before the connection went.
func logRequest(logger *slog.Logger, r *http.Request, rec *recorder, info *proxy.RequestInfo, start time.Time, aborted bool) {
	if rec.status == 0 {
		rec.status = http.StatusOK
	}
	attrs := []any{
		"method", r.Method,
		"path", r.URL.Path,
		"status", rec.status,
		"bytes", rec.bytes,
		"duration_ms", time.Since(start).Milliseconds(),
		"remote", clientip.From(r),
	}
	if label := auth.LabelFromContext(r.Context()); label != "" {
		attrs = append(attrs, "key", label)
	}
	if info.Model != "" {
		attrs = append(attrs, "model", info.Model)
	}
	if info.Node != "" {
		attrs = append(attrs, "node", info.Node)
	}
	if aborted {
		attrs = append(attrs, "aborted", true)
	}
	logger.Info("request", attrs...)
}
