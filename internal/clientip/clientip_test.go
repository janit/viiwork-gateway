package clientip

import (
	"net/http/httptest"
	"testing"
)

// X-Forwarded-For with multiple entries must use the rightmost (Caddy-observed) entry.
func TestClientIPUsesRightmostForwardedEntry(t *testing.T) {
	// Multi-entry header: rightmost must be used.
	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set("X-Forwarded-For", "1.2.3.4, 10.0.0.9")
	if got := From(req); got != "10.0.0.9" {
		t.Errorf("From() = %v, want 10.0.0.9", got)
	}

	// Single-entry header: must also work.
	req = httptest.NewRequest("GET", "/", nil)
	req.Header.Set("X-Forwarded-For", "192.168.1.1")
	if got := From(req); got != "192.168.1.1" {
		t.Errorf("From() = %v, want 192.168.1.1", got)
	}
}
