//go:build integration

package integration

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/thaivanlam/mini-payment-gateway/internal/config"
	"github.com/thaivanlam/mini-payment-gateway/internal/domain"
	"github.com/thaivanlam/mini-payment-gateway/internal/webhook"
)

func webhookConfig() config.WebhookConfig {
	return config.WebhookConfig{
		Workers:         3,
		MaxAttempts:     6,
		PollInterval:    50 * time.Millisecond,
		RequestTimeout:  2 * time.Second,
		ShutdownTimeout: 5 * time.Second,
	}
}

func startDispatcher(t *testing.T, cfg config.WebhookConfig) (context.CancelFunc, <-chan error) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())

	dispatcher := webhook.NewDispatcher(
		shared.DB, shared.WebhookRepo, shared.MerchantRepo, shared.Cipher, cfg, shared.Log)

	done := make(chan error, 1)
	go func() { done <- dispatcher.Run(ctx) }()

	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Error("dispatcher did not stop")
		}
	})
	return cancel, done
}

// TestWebhookIsDeliveredAndVerifiable is the end-to-end path: capture a
// payment, and the merchant's endpoint receives a signed callback it can
// verify with its own webhook_secret.
func TestWebhookIsDeliveredAndVerifiable(t *testing.T) {
	e := newEnv(t)
	startDispatcher(t, webhookConfig())

	payment := e.authorize("ORDER-HOOK-1", 120_000)
	capture := e.do(http.MethodPost, "/api/v1/payments/"+payment.ID+"/capture", nil,
		requestOpts{IdempotencyKey: uuid.NewString()})
	require.Equal(t, http.StatusOK, capture.Status)

	received, ok := e.Received.WaitFor(string(domain.EventPaymentCaptured), 10*time.Second)
	require.True(t, ok, "payment.captured webhook was never delivered")

	assert.True(t, received.SignatureValid, "the merchant must be able to verify the signature")
	assert.Contains(t, received.SignatureHeader, "t=")
	assert.Contains(t, received.SignatureHeader, "v1=")
	assert.Equal(t, payment.ID, received.Payload.Data.ID)
	assert.Equal(t, string(domain.StatusCaptured), received.Payload.Data.Status)
	assert.Equal(t, int64(120_000), received.Payload.Data.CapturedAmount)
	assert.NotEmpty(t, received.Payload.ID, "every event carries its own id")

	// The card number never leaves the gateway.
	assert.NotContains(t, string(received.Body), CardNormal)
	assert.NotContains(t, string(received.Body), "cvv")

	// The outbox row ends up delivered.
	require.Eventually(t, func() bool {
		for _, d := range e.deliveries(uuid.MustParse(payment.ID)) {
			if d.EventType == domain.EventPaymentCaptured && d.Status == domain.DeliveryDelivered {
				return true
			}
		}
		return false
	}, 10*time.Second, 100*time.Millisecond)
}

// A webhook is queued in the same database transaction as the money movement,
// so it exists the instant the API answers -- before any worker runs.
func TestWebhookIsQueuedTransactionally(t *testing.T) {
	e := newEnv(t)
	// No dispatcher: nothing can have delivered anything yet.

	payment := e.authorize("ORDER-HOOK-2", 50_000)

	deliveries := e.deliveries(uuid.MustParse(payment.ID))
	require.Len(t, deliveries, 1)
	assert.Equal(t, domain.EventPaymentAuthorized, deliveries[0].EventType)
	assert.Equal(t, domain.DeliveryPending, deliveries[0].Status)
	assert.Equal(t, 0, e.Received.Count(), "nothing is delivered without a worker")
}

// A failing endpoint is retried with exponential backoff rather than dropped.
func TestWebhookRetriesOnFailure(t *testing.T) {
	e := newEnv(t)
	e.Received.FailNext(2)
	startDispatcher(t, webhookConfig())

	payment := e.authorize("ORDER-HOOK-3", 30_000)
	txID := uuid.MustParse(payment.ID)

	// The first two attempts fail; the delivery must be rescheduled, not lost.
	require.Eventually(t, func() bool {
		for _, d := range e.deliveries(txID) {
			if d.AttemptCount >= 1 && d.Status == domain.DeliveryFailed {
				return true
			}
		}
		return false
	}, 10*time.Second, 100*time.Millisecond, "a failed delivery must be scheduled for retry")

	for _, d := range e.deliveries(txID) {
		if d.AttemptCount >= 1 {
			assert.True(t, d.NextAttemptAt.After(d.CreatedAt), "the retry is in the future")
			assert.NotEmpty(t, d.LastError)
		}
	}
}

// TestWebhookSurvivesWorkerRestart is the "kill the worker, lose no job" case:
// a delivery claimed by a worker that dies is picked up again.
func TestWebhookSurvivesWorkerRestart(t *testing.T) {
	e := newEnv(t)
	e.Received.RejectAll(true)

	// First worker: takes the job, fails to deliver, then is killed.
	cfg := webhookConfig()
	ctx, cancel := context.WithCancel(context.Background())
	dispatcher := webhook.NewDispatcher(
		shared.DB, shared.WebhookRepo, shared.MerchantRepo, shared.Cipher, cfg, shared.Log)
	done := make(chan error, 1)
	go func() { done <- dispatcher.Run(ctx) }()

	payment := e.authorize("ORDER-HOOK-4", 40_000)
	txID := uuid.MustParse(payment.ID)

	require.Eventually(t, func() bool { return e.Received.Count() > 0 },
		10*time.Second, 50*time.Millisecond, "the first worker never attempted delivery")

	cancel()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the first dispatcher did not stop")
	}

	// No delivery may be stranded in 'delivering'.
	for _, d := range e.deliveries(txID) {
		assert.NotEqual(t, domain.DeliveryDelivering, d.Status,
			"a stopped worker must not leave a job claimed")
	}

	// Second worker: the endpoint recovers and the job completes.
	e.Received.RejectAll(false)
	startDispatcher(t, cfg)

	require.Eventually(t, func() bool {
		for _, d := range e.deliveries(txID) {
			if d.Status == domain.DeliveryDelivered {
				return true
			}
		}
		return false
	}, 30*time.Second, 200*time.Millisecond, "the restarted worker must finish the job")
}

// The demo receiver inside the API verifies signatures the same way a merchant
// would, which is what makes it a usable example.
func TestInternalWebhookReceiverVerifiesSignature(t *testing.T) {
	e := newEnv(t)

	secret, err := shared.Merchants.WebhookSecret(context.Background(), e.Merchant.ID)
	require.NoError(t, err)

	payload := mustJSON(t, map[string]any{
		"id":   "evt_test",
		"type": "payment.captured",
		"data": map[string]any{"id": uuid.NewString(), "merchant_id": e.Merchant.ID.String()},
	})

	t.Run("valid signature is accepted", func(t *testing.T) {
		req, err := http.NewRequest(http.MethodPost,
			e.server.URL+"/internal/webhook-receiver", bytesReader(payload))
		require.NoError(t, err)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set(webhook.SignatureHeader, webhook.Sign(secret, time.Now(), payload))

		resp, err := e.server.Client().Do(req)
		require.NoError(t, err)
		defer resp.Body.Close()
		assert.Equal(t, http.StatusOK, resp.StatusCode)
	})

	t.Run("wrong signature is rejected", func(t *testing.T) {
		req, err := http.NewRequest(http.MethodPost,
			e.server.URL+"/internal/webhook-receiver", bytesReader(payload))
		require.NoError(t, err)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set(webhook.SignatureHeader, webhook.Sign("whsec_wrong", time.Now(), payload))

		resp, err := e.server.Client().Do(req)
		require.NoError(t, err)
		defer resp.Body.Close()
		assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)
	})

	t.Run("stale signature is rejected", func(t *testing.T) {
		req, err := http.NewRequest(http.MethodPost,
			e.server.URL+"/internal/webhook-receiver", bytesReader(payload))
		require.NoError(t, err)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set(webhook.SignatureHeader,
			webhook.Sign(secret, time.Now().Add(-10*time.Minute), payload))

		resp, err := e.server.Client().Do(req)
		require.NoError(t, err)
		defer resp.Body.Close()
		assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)
	})
}
