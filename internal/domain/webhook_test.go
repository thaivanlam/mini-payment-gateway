package domain

import (
	"math/rand"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNewWebhookDelivery(t *testing.T) {
	merchantID, txID := uuid.New(), uuid.New()

	d := NewWebhookDelivery(merchantID, txID, EventPaymentCaptured, []byte(`{"a":1}`), testNow)

	assert.Equal(t, DeliveryPending, d.Status)
	assert.Equal(t, testNow, d.NextAttemptAt, "a new delivery is due immediately")
	assert.Equal(t, 0, d.AttemptCount)
	assert.Equal(t, merchantID, d.MerchantID)
	assert.Equal(t, txID, d.TransactionID)
}

func TestBackoffDelayGrowsExponentially(t *testing.T) {
	// With jitter disabled the delay is exactly 2^attempt seconds.
	assert.Equal(t, 2*time.Second, BackoffDelay(1, nil))
	assert.Equal(t, 4*time.Second, BackoffDelay(2, nil))
	assert.Equal(t, 8*time.Second, BackoffDelay(3, nil))
	assert.Equal(t, 64*time.Second, BackoffDelay(6, nil))

	assert.Equal(t, 2*time.Second, BackoffDelay(0, nil), "attempt is clamped to at least 1")
	assert.Equal(t, BackoffDelay(16, nil), BackoffDelay(99, nil), "attempt is clamped at the top")
}

func TestBackoffDelayJitterStaysInBand(t *testing.T) {
	rnd := rand.New(rand.NewSource(7))
	base := 8 * time.Second // attempt 3

	for i := 0; i < 200; i++ {
		d := BackoffDelay(3, rnd)
		assert.GreaterOrEqual(t, d, time.Duration(float64(base)*0.8))
		assert.LessOrEqual(t, d, time.Duration(float64(base)*1.2))
	}
}

func TestRecordFailureRetriesThenDies(t *testing.T) {
	d := NewWebhookDelivery(uuid.New(), uuid.New(), EventPaymentCaptured, nil, testNow)
	const maxAttempts = 6

	for attempt := 1; attempt < maxAttempts; attempt++ {
		d.RecordFailure("connection refused", maxAttempts, testNow, nil)
		require.Equal(t, DeliveryFailed, d.Status)
		assert.Equal(t, attempt, d.AttemptCount)
		assert.True(t, d.NextAttemptAt.After(testNow), "a retry is scheduled in the future")
		assert.Equal(t, "connection refused", d.LastError)
	}

	d.RecordFailure("connection refused", maxAttempts, testNow, nil)
	assert.Equal(t, DeliveryDead, d.Status, "the last attempt parks the delivery")
	assert.Equal(t, maxAttempts, d.AttemptCount)
}

func TestRecordSuccess(t *testing.T) {
	d := NewWebhookDelivery(uuid.New(), uuid.New(), EventPaymentCaptured, nil, testNow)
	d.RecordFailure("boom", 6, testNow, nil)

	d.RecordSuccess(testNow.Add(time.Minute))

	assert.Equal(t, DeliveryDelivered, d.Status)
	assert.Equal(t, 2, d.AttemptCount)
	assert.Empty(t, d.LastError, "a success clears the last error")
}
