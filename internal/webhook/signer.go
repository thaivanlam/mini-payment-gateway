// Package webhook signs and delivers merchant callbacks.
package webhook

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// SignatureHeader is the header carrying the payload signature.
const SignatureHeader = "X-Signature"

// Errors returned when verifying an inbound signature.
var (
	ErrMalformedSignature = errors.New("malformed signature header")
	ErrSignatureMismatch  = errors.New("signature does not match payload")
	ErrSignatureExpired   = errors.New("signature timestamp outside tolerance")
)

// Sign builds the header value for a payload:
//
//	X-Signature: t=<unix>,v1=<hex hmac_sha256(secret, "<t>.<body>")>
//
// The timestamp is inside the signed string, not just next to it. If it were
// only a header field, an attacker could replay a captured payload forever by
// rewriting t; signing it binds the payload to a moment in time.
func Sign(secret string, ts time.Time, body []byte) string {
	t := ts.Unix()
	return fmt.Sprintf("t=%d,v1=%s", t, computeSignature(secret, t, body))
}

// Verify checks a signature header against the payload.
//
// tolerance bounds replay: a payload signed longer ago than that is refused
// even if the signature itself is valid.
func Verify(secret, header string, body []byte, now time.Time, tolerance time.Duration) error {
	ts, sig, err := parseHeader(header)
	if err != nil {
		return err
	}
	signedAt := time.Unix(ts, 0)
	drift := now.Sub(signedAt)
	if drift < 0 {
		drift = -drift
	}
	if drift > tolerance {
		return fmt.Errorf("%w: drift %s", ErrSignatureExpired, drift)
	}

	expected := computeSignature(secret, ts, body)
	// hmac.Equal is constant time: a byte-by-byte == would leak how much of the
	// signature is correct through timing, which is enough to forge one.
	if !hmac.Equal([]byte(expected), []byte(sig)) {
		return ErrSignatureMismatch
	}
	return nil
}

func computeSignature(secret string, ts int64, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(strconv.FormatInt(ts, 10)))
	mac.Write([]byte("."))
	mac.Write(body)
	return hex.EncodeToString(mac.Sum(nil))
}

func parseHeader(header string) (int64, string, error) {
	var tsRaw, sig string
	for _, part := range strings.Split(header, ",") {
		key, value, found := strings.Cut(strings.TrimSpace(part), "=")
		if !found {
			continue
		}
		switch key {
		case "t":
			tsRaw = value
		case "v1":
			sig = value
		}
	}
	if tsRaw == "" || sig == "" {
		return 0, "", ErrMalformedSignature
	}
	ts, err := strconv.ParseInt(tsRaw, 10, 64)
	if err != nil {
		return 0, "", fmt.Errorf("%w: %v", ErrMalformedSignature, err)
	}
	return ts, sig, nil
}
