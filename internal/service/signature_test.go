package service

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestComputeRequestSignatureIsDeterministic(t *testing.T) {
	body := []byte(`{"amount":150000}`)

	first := ComputeRequestSignature("sk_test_secret", "1756288800", "POST", "/api/v1/payments", body)
	second := ComputeRequestSignature("sk_test_secret", "1756288800", "POST", "/api/v1/payments", body)

	assert.Equal(t, first, second)
	assert.Len(t, first, 64, "hex-encoded SHA-256 is 64 characters")
}

// Every component of the signed string must actually change the signature,
// otherwise it is not protecting what it claims to protect.
func TestComputeRequestSignatureCoversEveryComponent(t *testing.T) {
	const (
		secret = "sk_test_secret"
		ts     = "1756288800"
		method = "POST"
		path   = "/api/v1/payments"
	)
	body := []byte(`{"amount":150000}`)
	base := ComputeRequestSignature(secret, ts, method, path, body)

	tests := []struct {
		name string
		got  string
	}{
		{"secret", ComputeRequestSignature("sk_test_other", ts, method, path, body)},
		{"timestamp", ComputeRequestSignature(secret, "1756288801", method, path, body)},
		{"method", ComputeRequestSignature(secret, ts, "GET", path, body)},
		{"path", ComputeRequestSignature(secret, ts, method, "/api/v1/payments/x/refund", body)},
		{"body", ComputeRequestSignature(secret, ts, method, path, []byte(`{"amount":150001}`))},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.NotEqualf(t, base, tc.got, "changing the %s must change the signature", tc.name)
		})
	}
}

// The concatenation must not be ambiguous: two different requests must never
// produce the same signed string.
func TestComputeRequestSignatureIsUnambiguous(t *testing.T) {
	a := ComputeRequestSignature("s", "123", "GET", "/a", []byte("b"))
	b := ComputeRequestSignature("s", "123", "GET", "/ab", nil)

	assert.NotEqual(t, a, b,
		"a path/body split must not be reinterpretable as a different split")
}

func TestParseUnixSeconds(t *testing.T) {
	ts, err := parseUnixSeconds("1756288800")
	require.NoError(t, err)
	assert.Equal(t, int64(1756288800), ts.Unix())

	for _, bad := range []string{"", "not-a-number", "17562888000x", "1756288800.5"} {
		_, err := parseUnixSeconds(bad)
		assert.Errorf(t, err, "input %q should be rejected", bad)
	}
}

func TestBuildEventPayloadOmitsCardData(t *testing.T) {
	txn := testTransaction(t)

	payload, err := BuildEventPayload("payment.captured", txn, time.Now())
	require.NoError(t, err)

	body := string(payload)
	assert.Contains(t, body, `"type":"payment.captured"`)
	assert.Contains(t, body, txn.ID.String())
	assert.Contains(t, body, `"card_last4":"4242"`)
	assert.NotContains(t, body, "4242424242424242", "the full PAN must never leave the gateway")
	assert.NotContains(t, body, "cvv")
	assert.Contains(t, body, `"id":"evt_`, "every event carries its own identifier")
}
