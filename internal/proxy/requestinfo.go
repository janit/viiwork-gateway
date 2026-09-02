package proxy

import "context"

// RequestInfo is a mutable holder that accesslog.Wrap places into the request
// context before calling the wrapped handler. Router fills in Model and Node
// once it decides them; accesslog reads the fields back after the handler
// returns.
//
// This exists because context values propagate downward only: Router builds
// a request via r.WithContext(ctx) and passes that derived request to the
// forwarder, but accesslog sits OUTSIDE Router in the handler chain
// (auth -> accesslog -> router), so it never sees that derived request or its
// context — only the one it was itself called with. A pointer placed into
// the shared context before Router runs, and mutated in place, crosses that
// boundary without either package needing the other's derived request.
//
// The type lives here, in proxy, rather than in accesslog: accesslog already
// imports proxy and auth, so a holder living in accesslog would create an
// import cycle the moment proxy needed to reference it.
type RequestInfo struct {
	Model string
	Node  string
}

type requestInfoKey struct{}

// ContextWithRequestInfo returns a context carrying info, retrievable with
// RequestInfoFromContext.
func ContextWithRequestInfo(ctx context.Context, info *RequestInfo) context.Context {
	return context.WithValue(ctx, requestInfoKey{}, info)
}

// RequestInfoFromContext returns the *RequestInfo placed by
// ContextWithRequestInfo, or nil if none is present.
func RequestInfoFromContext(ctx context.Context) *RequestInfo {
	info, _ := ctx.Value(requestInfoKey{}).(*RequestInfo)
	return info
}
