// Package auth authenticates gateway callers against labelled API keys.
package auth

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"strconv"
	"strings"
	"time"
)

// Mint builds a cookie token for a key. The token is
// base64url(label).unixExpiry.base64url(signature), signed with that key's own
// secret — which is what makes deleting the key invalidate the cookie, with no
// session store and no revocation list.
//
// The label is encoded rather than embedded raw so that a label containing a
// dot cannot forge the delimiter structure.
func Mint(label, secret string, exp time.Time) string {
	enc := base64.RawURLEncoding.EncodeToString([]byte(label))
	expStr := strconv.FormatInt(exp.Unix(), 10)
	return enc + "." + expStr + "." + sign(enc, expStr, secret)
}

// Verify checks a token and returns the label it belongs to. secretOf resolves
// a label to its current key; a label it does not know fails verification,
// which is how a revoked key takes its cookies with it.
func Verify(token string, secretOf func(string) (string, bool), now time.Time) (string, bool) {
	encLabel, rest, ok := strings.Cut(token, ".")
	if !ok {
		return "", false
	}
	expStr, sig, ok := strings.Cut(rest, ".")
	if !ok || strings.Contains(sig, ".") {
		return "", false
	}

	raw, err := base64.RawURLEncoding.DecodeString(encLabel)
	if err != nil {
		return "", false
	}
	label := string(raw)

	secret, known := secretOf(label)
	if !known {
		return "", false
	}

	// Compare the signature before trusting any other field.
	if !hmac.Equal([]byte(sig), []byte(sign(encLabel, expStr, secret))) {
		return "", false
	}

	exp, err := strconv.ParseInt(expStr, 10, 64)
	if err != nil {
		return "", false
	}
	if !now.Before(time.Unix(exp, 0)) {
		return "", false
	}
	return label, true
}

func sign(encLabel, expStr, secret string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(encLabel))
	mac.Write([]byte("."))
	mac.Write([]byte(expStr))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}
