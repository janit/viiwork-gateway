package auth

import (
	"strings"
	"testing"
	"time"
)

func secretsFor(m map[string]string) func(string) (string, bool) {
	return func(label string) (string, bool) {
		v, ok := m[label]
		return v, ok
	}
}

var testSecrets = secretsFor(map[string]string{
	"janit": "aaaaaaaaaaaaaaaaaaaaaaaaaaaa",
	"other": "bbbbbbbbbbbbbbbbbbbbbbbbbbbb",
})

func TestMintVerifyRoundTrip(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	tok := Mint("janit", "aaaaaaaaaaaaaaaaaaaaaaaaaaaa", now.Add(time.Hour))
	label, ok := Verify(tok, testSecrets, now)
	if !ok || label != "janit" {
		t.Fatalf("Verify = %q, %v; want janit, true", label, ok)
	}
}

func TestVerifyRejectsExpired(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	tok := Mint("janit", "aaaaaaaaaaaaaaaaaaaaaaaaaaaa", now.Add(time.Hour))
	if _, ok := Verify(tok, testSecrets, now.Add(2*time.Hour)); ok {
		t.Fatal("Verify accepted an expired token")
	}
}

func TestVerifyRejectsTampered(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	tok := Mint("janit", "aaaaaaaaaaaaaaaaaaaaaaaaaaaa", now.Add(time.Hour))
	parts := strings.Split(tok, ".")
	if len(parts) != 3 {
		t.Fatalf("token should have three parts, got %d", len(parts))
	}
	// Move the expiry out without re-signing.
	forged := parts[0] + ".9999999999." + parts[2]
	if _, ok := Verify(forged, testSecrets, now); ok {
		t.Fatal("Verify accepted a token with a rewritten expiry")
	}
}

// This is the property that removes the need for a session store: deleting a
// key from .env must invalidate every cookie ever minted from it.
func TestVerifyRejectsRevokedKey(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	tok := Mint("gone", "cccccccccccccccccccccccccccc", now.Add(time.Hour))
	if _, ok := Verify(tok, testSecrets, now); ok {
		t.Fatal("Verify accepted a token whose label no longer has a key")
	}
}

// A token signed with a different key must not verify, even for a label that
// still exists.
func TestVerifyRejectsWrongSecret(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	tok := Mint("janit", "bbbbbbbbbbbbbbbbbbbbbbbbbbbb", now.Add(time.Hour))
	if _, ok := Verify(tok, testSecrets, now); ok {
		t.Fatal("Verify accepted a token signed with another key's secret")
	}
}

func TestVerifyRejectsMalformed(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	for _, bad := range []string{"", "a.b", "a.b.c.d", "!!!.1.!!!", "a.notanumber.c"} {
		if _, ok := Verify(bad, testSecrets, now); ok {
			t.Errorf("Verify accepted malformed token %q", bad)
		}
	}
}
