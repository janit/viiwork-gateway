// Package keys holds the gateway's API keys, each carrying a label.
package keys

import (
	"crypto/subtle"
	"fmt"
	"sort"
	"strings"
)

// EnvPrefix is the environment variable prefix. Everything after it is the
// key's label, which is what appears in the access log.
const EnvPrefix = "VIIWORK_KEY_"

// MinKeyLength is the shortest key the gateway will start with. This service
// is reachable from the internet and stands in front of hardware that has no
// authentication of its own, so a weak key is not the operator's own risk to
// take quietly.
const MinKeyLength = 24

// Set is an immutable collection of labelled keys.
type Set struct {
	byLabel map[string]string
	// labels is sorted, so Lookup always scans in the same order regardless
	// of map iteration randomness.
	labels []string
}

// Load parses keys from environ, which is in os.Environ() form. It reads the
// whole environment rather than a single variable because the label lives in
// the variable's name.
func Load(environ []string) (*Set, error) {
	set := &Set{byLabel: make(map[string]string)}
	seen := make(map[string]string) // secret -> label, for duplicate detection

	for _, kv := range environ {
		if !strings.HasPrefix(kv, EnvPrefix) {
			continue
		}
		name, value, ok := strings.Cut(kv, "=")
		if !ok {
			continue
		}
		label := strings.TrimPrefix(name, EnvPrefix)
		if label == "" {
			return nil, fmt.Errorf("%s with no label: the text after %s is the key's name in the access log", name, EnvPrefix)
		}
		if len(value) < MinKeyLength {
			return nil, fmt.Errorf("key %q is %d characters, minimum is %d: generate one with `openssl rand -base64 32`", label, len(value), MinKeyLength)
		}
		if other, dup := seen[value]; dup {
			return nil, fmt.Errorf("keys %q and %q have the same secret: revoking one would silently leave the other working", other, label)
		}
		seen[value] = label
		set.byLabel[label] = value
	}

	if len(set.byLabel) == 0 {
		return nil, fmt.Errorf("no API keys configured: set at least one %s<label> variable", EnvPrefix)
	}

	for label := range set.byLabel {
		set.labels = append(set.labels, label)
	}
	sort.Strings(set.labels)
	return set, nil
}

// Lookup returns the label for a secret. It compares against every key in
// constant time and never returns early, so response timing does not reveal
// how much of a guessed key was correct.
func (s *Set) Lookup(secret string) (string, bool) {
	var found string
	match := 0
	for _, label := range s.labels {
		if subtle.ConstantTimeCompare([]byte(secret), []byte(s.byLabel[label])) == 1 {
			found = label
			match = 1
		}
	}
	if match == 0 {
		return "", false
	}
	return found, true
}

// Secret returns the key for a label. It is how a cookie signature is
// verified, and how deleting a key from .env invalidates every cookie minted
// from it: the label stops resolving.
func (s *Set) Secret(label string) (string, bool) {
	v, ok := s.byLabel[label]
	return v, ok
}

// Len reports how many keys are configured.
func (s *Set) Len() int { return len(s.byLabel) }
