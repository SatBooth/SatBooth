// Package webhook signs, verifies and delivers SatBooth's webhook events.
//
// A webhook is an at-least-once hint, not the record: the receiver verifies the signature, then
// re-reads the order from the API. The same event can arrive more than once (a retry after a lost
// answer, a restart between the 2xx and its record), so receivers must treat it as idempotent.
//
// The header is
//
//	SatBooth-Signature: t=<unix seconds>,v1=<64 lowercase hex>
//
// where v1 is HMAC-SHA256(secret, t + "." + body), t is the decimal Unix time the attempt was
// signed (each retry is signed afresh) and body is the exact bytes of the request body.
package webhook

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"strconv"
	"strings"
	"time"
)

// HeaderName is the request header that carries the signature.
const HeaderName = "SatBooth-Signature"

// RecommendedWindow is the timestamp window the recipe in docs/webhooks.md recommends to receivers: reject a
// header whose t differs from the receiver's clock by more than 5 minutes either way.
const RecommendedWindow = 5 * time.Minute

// Verify's failures. A caller can tell them apart; none carries the secret, the header or the body.
var (
	ErrMalformed = errors.New("webhook: malformed signature header")
	ErrSignature = errors.New("webhook: signature does not match")
	ErrTimestamp = errors.New("webhook: timestamp outside the window")
)

const maxTimestampDigits = 18 // fits int64

// Sign returns the header value for body signed at t.
func Sign(secret []byte, t time.Time, body []byte) string {
	ts := strconv.FormatInt(t.Unix(), 10)
	return "t=" + ts + ",v1=" + hex.EncodeToString(mac(secret, ts, body))
}

func mac(secret []byte, ts string, body []byte) []byte {
	h := hmac.New(sha256.New, secret)
	h.Write([]byte(ts))
	h.Write([]byte{'.'})
	h.Write(body)
	return h.Sum(nil)
}

// Verify checks header against body and the secret, at the receiver's time now.
//
// The parser is strict: the header is exactly "t=<decimal>,v1=<64 lowercase hex>", with no other
// field, no repeated field, no space, no sign and no leading zero in t. The MAC is compared in
// constant time (crypto/subtle). The timestamp must be within window either way of now: the
// recommended window is RecommendedWindow (5 minutes). A negative window is refused.
//
// The result is nil, or ErrMalformed, ErrSignature or ErrTimestamp. A signature mismatch is
// reported before a timestamp problem, so a forged header learns nothing about the clock.
func Verify(secret []byte, header string, body []byte, now time.Time, window time.Duration) error {
	if window < 0 {
		return errors.New("webhook: negative verification window")
	}
	rest, ok := strings.CutPrefix(header, "t=")
	if !ok {
		return ErrMalformed
	}
	ts, rest, ok := strings.Cut(rest, ",v1=")
	if !ok || !canonicalTimestamp(ts) || len(rest) != 2*sha256.Size {
		return ErrMalformed
	}
	for i := 0; i < len(rest); i++ {
		if c := rest[i]; (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return ErrMalformed
		}
	}
	got, err := hex.DecodeString(rest)
	if err != nil {
		return ErrMalformed
	}
	t, err := strconv.ParseInt(ts, 10, 64)
	if err != nil {
		return ErrMalformed
	}
	if subtle.ConstantTimeCompare(got, mac(secret, ts, body)) != 1 {
		return ErrSignature
	}
	d := now.Sub(time.Unix(t, 0))
	if d < 0 {
		d = -d
	}
	if d < 0 || d > window { // d < 0: the negation of the most negative Duration overflows
		return ErrTimestamp
	}
	return nil
}

// canonicalTimestamp reports whether s is what strconv.FormatInt gives for a non-negative number.
func canonicalTimestamp(s string) bool {
	if len(s) == 0 || len(s) > maxTimestampDigits || (len(s) > 1 && s[0] == '0') {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}
