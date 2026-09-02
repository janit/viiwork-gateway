package accesslog

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/janit/viiwork-gateway/internal/proxy"
)

func TestLogsRequestDetails(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, nil))

	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte("hello"))
	})

	rec := httptest.NewRecorder()
	Wrap(next, logger).ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", nil))

	var entry map[string]any
	if err := json.Unmarshal(buf.Bytes(), &entry); err != nil {
		t.Fatalf("log line is not JSON: %v (%s)", err, buf.String())
	}
	if entry["method"] != "POST" {
		t.Errorf("method = %v", entry["method"])
	}
	if entry["path"] != "/v1/chat/completions" {
		t.Errorf("path = %v", entry["path"])
	}
	if entry["status"] != float64(http.StatusCreated) {
		t.Errorf("status = %v, want 201", entry["status"])
	}
	if entry["bytes"] != float64(5) {
		t.Errorf("bytes = %v, want 5", entry["bytes"])
	}
	if _, ok := entry["duration_ms"]; !ok {
		t.Error("duration_ms missing")
	}
}

// A handler that never calls WriteHeader still returns 200.
func TestDefaultStatusIs200(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, nil))

	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("ok"))
	})
	Wrap(next, logger).ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/", nil))

	var entry map[string]any
	_ = json.Unmarshal(buf.Bytes(), &entry)
	if entry["status"] != float64(200) {
		t.Errorf("status = %v, want 200", entry["status"])
	}
}

// SSE responses must still stream through the wrapper.
func TestWrapperPreservesFlusher(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, nil))

	flushed := false
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f, ok := w.(http.Flusher)
		if !ok {
			t.Fatal("wrapped ResponseWriter lost http.Flusher: SSE would buffer")
		}
		_, _ = w.Write([]byte("data: x\n\n"))
		f.Flush()
		flushed = true
	})
	Wrap(next, logger).ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/v1/mesh/stream", nil))

	if !flushed {
		t.Fatal("handler did not run")
	}
}

// A rejected key must never be written to the log.
func TestNeverLogsCredentials(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, nil))

	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	})
	req := httptest.NewRequest("GET", "/v1/models?key=super-secret-key-value-here", nil)
	req.Header.Set("Authorization", "Bearer super-secret-key-value-here")
	Wrap(next, logger).ServeHTTP(httptest.NewRecorder(), req)

	if bytes.Contains(buf.Bytes(), []byte("super-secret-key-value-here")) {
		t.Fatalf("the log line contains a credential: %s", buf.String())
	}

	// Verify the log line was actually emitted and is well-formed.
	var entry map[string]any
	if err := json.Unmarshal(buf.Bytes(), &entry); err != nil {
		t.Fatalf("log line is not JSON: %v (%s)", err, buf.String())
	}
	if entry["method"] != "GET" {
		t.Errorf("method = %v, want GET", entry["method"])
	}
	if entry["path"] != "/v1/models" {
		t.Errorf("path = %v, want /v1/models", entry["path"])
	}
}

// The whole reason labelled keys are worth having, per the design spec, is
// that the access log shows which key sent how much traffic to which node
// serving which model. Router — several layers downstream of Wrap, past the
// point Wrap itself can see — is what actually decides the model and node;
// this proves Wrap makes that decision visible in its own log line rather
// than only in a context value nothing downstream of it can read back.
func TestLogsModelAndNodeRecordedDownstream(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, nil))

	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Stand-in for what Router does: read the holder Wrap placed in the
		// context, and fill in the fields once the routing decision is made.
		if info := proxy.RequestInfoFromContext(r.Context()); info != nil {
			info.Model = "gemma"
			info.Node = "10.0.0.5:8080"
		}
		w.WriteHeader(http.StatusOK)
	})

	Wrap(next, logger).ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("POST", "/v1/chat/completions", nil))

	var entry map[string]any
	if err := json.Unmarshal(buf.Bytes(), &entry); err != nil {
		t.Fatalf("log line is not JSON: %v (%s)", err, buf.String())
	}
	if entry["model"] != "gemma" {
		t.Errorf("model = %v, want gemma", entry["model"])
	}
	if entry["node"] != "10.0.0.5:8080" {
		t.Errorf("node = %v, want 10.0.0.5:8080", entry["node"])
	}
}

// When nothing downstream records a model or node — a sticky, non-inference
// path, say — the log line must simply omit them, not emit empty strings.
func TestLogsOmitModelAndNodeWhenUnset(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, nil))

	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	Wrap(next, logger).ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/mesh", nil))

	var entry map[string]any
	if err := json.Unmarshal(buf.Bytes(), &entry); err != nil {
		t.Fatalf("log line is not JSON: %v (%s)", err, buf.String())
	}
	if _, ok := entry["model"]; ok {
		t.Errorf("model present with no route decision made: %v", entry["model"])
	}
	if _, ok := entry["node"]; ok {
		t.Errorf("node present with no route decision made: %v", entry["node"])
	}
}

// TestRecorderUnwrapReachesUnderlyingDeadlineSetter proves the fix for R2/H5's
// per-request body-read deadline: internal/proxy.Router sets that deadline
// via http.NewResponseController(w), and ResponseController can only see
// through *recorder to the real connection if *recorder implements
// Unwrap() http.ResponseWriter. Without Unwrap, SetReadDeadline would fail
// with http.ErrNotSupported on every request that passes through this
// package's Wrap — which is every request the gateway serves — silently
// defeating the fix.
//
// A real network connection is required: httptest.ResponseRecorder does not
// implement the deadline setters ResponseController looks for, so this must
// run over an actual httptest.Server rather than calling Wrap's handler
// in-process.
func TestRecorderUnwrapReachesUnderlyingDeadlineSetter(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec := &recorder{ResponseWriter: w}
		rc := http.NewResponseController(rec)
		if err := rc.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
			http.Error(w, "SetReadDeadline through recorder.Unwrap failed: "+err.Error(),
				http.StatusInternalServerError)
			return
		}
		if err := rc.SetReadDeadline(time.Time{}); err != nil {
			http.Error(w, "clearing the deadline through recorder.Unwrap failed: "+err.Error(),
				http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", resp.StatusCode, body)
	}
}
