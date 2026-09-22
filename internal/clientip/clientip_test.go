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

// Header lines repeat the way comma-separated entries do (RFC 9110 §5.3), so
// with more than one X-Forwarded-For line the trustworthy entry is the last
// one of the last line — not the first line, which a client wrote itself.
func TestClientIPUsesLastForwardedHeaderLine(t *testing.T) {
	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Add("X-Forwarded-For", "6.6.6.6")
	req.Header.Add("X-Forwarded-For", "1.2.3.4, 10.0.0.9")
	if got := From(req); got != "10.0.0.9" {
		t.Errorf("From() = %v, want 10.0.0.9 from the last header line", got)
	}
}

func TestClientIPFallsBackToRemoteAddr(t *testing.T) {
	cases := []struct {
		name, xff, remote, want string
	}{
		{"no header", "", "203.0.113.7:51234", "203.0.113.7"},
		{"only separators", ",,,", "203.0.113.7:51234", "203.0.113.7"},
		{"only whitespace", " ,  , ", "203.0.113.7:51234", "203.0.113.7"},
		{"IPv6 peer", "", "[2001:db8::1]:443", "2001:db8::1"},
		{"no port", "", "203.0.113.7", "203.0.113.7"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			req := httptest.NewRequest("GET", "/", nil)
			req.RemoteAddr = c.remote
			if c.xff != "" {
				req.Header.Set("X-Forwarded-For", c.xff)
			}
			if got := From(req); got != c.want {
				t.Errorf("From() = %q, want %q", got, c.want)
			}
		})
	}
}

// Trailing empty entries are skipped rather than returned, and a port on a
// forwarded entry is stripped, bracketed IPv6 included.
func TestClientIPForwardedEntryForms(t *testing.T) {
	cases := []struct {
		name, xff, want string
	}{
		{"trailing comma", "1.2.3.4, 10.0.0.9,", "10.0.0.9"},
		{"with port", "10.0.0.9:4711", "10.0.0.9"},
		{"bare IPv6", "2001:db8::9", "2001:db8::9"},
		{"bracketed IPv6 with port", "[2001:db8::9]:4711", "2001:db8::9"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			req := httptest.NewRequest("GET", "/", nil)
			req.Header.Set("X-Forwarded-For", c.xff)
			if got := From(req); got != c.want {
				t.Errorf("From() = %q, want %q", got, c.want)
			}
		})
	}
}
