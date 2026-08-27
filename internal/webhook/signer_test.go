package webhook

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const testSecret = "whsec_test_secret"

var signTime = time.Date(2026, 8, 27, 10, 0, 0, 0, time.UTC)

func TestSignAndVerify(t *testing.T) {
	body := []byte(`{"type":"payment.captured","data":{"id":"txn_1"}}`)

	header := Sign(testSecret, signTime, body)

	assert.True(t, strings.HasPrefix(header, "t="))
	assert.Contains(t, header, ",v1=")
	assert.NoError(t, Verify(testSecret, header, body, signTime, 5*time.Minute))
}

func TestVerifyRejectsTamperedPayload(t *testing.T) {
	body := []byte(`{"amount":100}`)
	header := Sign(testSecret, signTime, body)

	tampered := []byte(`{"amount":1000000}`)
	assert.ErrorIs(t, Verify(testSecret, header, tampered, signTime, 5*time.Minute), ErrSignatureMismatch)
}

func TestVerifyRejectsWrongSecret(t *testing.T) {
	body := []byte(`{"amount":100}`)
	header := Sign(testSecret, signTime, body)

	assert.ErrorIs(t, Verify("whsec_other", header, body, signTime, 5*time.Minute), ErrSignatureMismatch)
}

// TestVerifyRejectsReplay is the reason the timestamp is inside the signed
// string: rewriting t in the header invalidates the signature, and an old
// signature falls outside the tolerance.
func TestVerifyRejectsReplay(t *testing.T) {
	body := []byte(`{"amount":100}`)
	header := Sign(testSecret, signTime, body)

	later := signTime.Add(10 * time.Minute)
	assert.ErrorIs(t, Verify(testSecret, header, body, later, 5*time.Minute), ErrSignatureExpired)

	earlier := signTime.Add(-10 * time.Minute)
	assert.ErrorIs(t, Verify(testSecret, header, body, earlier, 5*time.Minute), ErrSignatureExpired,
		"a signature from the future is refused too")

	// Rewriting the timestamp breaks the signature rather than extending it.
	parts := strings.SplitN(header, ",", 2)
	rewritten := "t=" + timestampOf(later) + "," + parts[1]
	assert.ErrorIs(t, Verify(testSecret, rewritten, body, later, 5*time.Minute), ErrSignatureMismatch)
}

func TestVerifyRejectsMalformedHeader(t *testing.T) {
	body := []byte(`{}`)
	for _, header := range []string{
		"",
		"nonsense",
		"t=123",
		"v1=abc",
		"t=notanumber,v1=abc",
	} {
		assert.ErrorIsf(t, Verify(testSecret, header, body, signTime, time.Minute),
			ErrMalformedSignature, "header %q", header)
	}
}

func TestSignIsDeterministic(t *testing.T) {
	body := []byte(`{"a":1}`)
	require.Equal(t, Sign(testSecret, signTime, body), Sign(testSecret, signTime, body))
	assert.NotEqual(t, Sign(testSecret, signTime, body), Sign(testSecret, signTime.Add(time.Second), body),
		"a different timestamp yields a different signature")
}

func timestampOf(ts time.Time) string {
	return strings.TrimSpace(strings.Split(Sign(testSecret, ts, nil), ",")[0][2:])
}
