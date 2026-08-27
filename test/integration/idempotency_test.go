//go:build integration

package integration

import (
	"context"
	"net/http"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/thaivanlam/mini-payment-gateway/internal/domain"
	"github.com/thaivanlam/mini-payment-gateway/internal/idempotency"
	transport "github.com/thaivanlam/mini-payment-gateway/internal/transport/http"
)

// Case 1: the retry that a flaky network causes. Same key, same body: the
// merchant gets the original response and is charged once.
func TestIdempotentRetryReplaysAndChargesOnce(t *testing.T) {
	e := newEnv(t)
	key := uuid.NewString()

	first := e.createPayment("ORDER-IDEM-1", 150_000, CardNormal, false, key)
	require.Equalf(t, http.StatusCreated, first.Status, "body: %s", first.Body)

	second := e.createPayment("ORDER-IDEM-1", 150_000, CardNormal, false, key)

	assert.Equal(t, http.StatusCreated, second.Status)
	assert.Equal(t, "true", second.Header.Get(transport.ReplayHeader))
	assert.JSONEq(t, string(first.Body), string(second.Body),
		"the replay must be the original response, byte for byte")

	var count int
	require.NoError(t, shared.DB.Pool.QueryRow(context.Background(),
		`SELECT COUNT(*) FROM transactions WHERE merchant_id = $1`, e.Merchant.ID).Scan(&count))
	assert.Equal(t, 1, count, "a retry must not create a second transaction")
}

// Case 2: the same key with a different body is a client bug, and pretending
// otherwise would either duplicate a charge or silently ignore the new request.
func TestIdempotencyKeyReuseWithDifferentBody(t *testing.T) {
	e := newEnv(t)
	key := uuid.NewString()

	first := e.createPayment("ORDER-IDEM-2", 150_000, CardNormal, false, key)
	require.Equal(t, http.StatusCreated, first.Status)

	second := e.createPayment("ORDER-IDEM-2-OTHER", 999_999, CardNormal, false, key)

	assert.Equal(t, http.StatusUnprocessableEntity, second.Status)
	assert.Equal(t, "idempotency_key_reuse", second.ErrorCode())

	var count int
	require.NoError(t, shared.DB.Pool.QueryRow(context.Background(),
		`SELECT COUNT(*) FROM transactions WHERE merchant_id = $1`, e.Merchant.ID).Scan(&count))
	assert.Equal(t, 1, count)
}

// Case 3: a second request while the first is still in flight.
func TestIdempotencyInFlightRequestIsRejected(t *testing.T) {
	e := newEnv(t)
	key := uuid.NewString()

	// Claim the key exactly as the middleware does for a request that has not
	// finished yet.
	body := map[string]any{
		"reference": "ORDER-IDEM-3",
		"amount":    150_000,
		"currency":  "VND",
		"card": map[string]any{
			"number": CardNormal, "exp_month": 12, "exp_year": 2030, "cvv": "123",
		},
		"capture":  false,
		"metadata": map[string]string{"suite": "integration"},
	}
	fingerprint := idempotency.Fingerprint(mustJSON(t, body))
	_, err := shared.Idempotency.Begin(context.Background(),
		idempotency.Key(e.Merchant.ID.String(), key), fingerprint)
	require.NoError(t, err)

	resp := e.createPayment("ORDER-IDEM-3", 150_000, CardNormal, false, key)

	assert.Equal(t, http.StatusConflict, resp.Status)
	assert.Equal(t, "request_in_progress", resp.ErrorCode())
}

// Case 4: the server failed, so the key is released and the retry gets a real
// answer instead of a cached error.
// A decline is a definite answer about the card, so it is cached and replayed
// like any other completed response.
func TestIdempotencyCachesDefiniteFailures(t *testing.T) {
	e := newEnv(t)
	key := uuid.NewString()
	redisKey := idempotency.Key(e.Merchant.ID.String(), key)

	declined := e.createPayment("ORDER-IDEM-4", 10_000, CardDecline, false, key)
	require.Equal(t, http.StatusPaymentRequired, declined.Status)

	record, err := shared.Idempotency.Get(context.Background(), redisKey)
	require.NoError(t, err)
	require.NotNil(t, record, "a 4xx answer is stored")
	assert.Equal(t, idempotency.StateCompleted, record.State)

	replay := e.createPayment("ORDER-IDEM-4", 10_000, CardDecline, false, key)
	assert.Equal(t, http.StatusPaymentRequired, replay.Status)
	assert.Equal(t, "true", replay.Header.Get(transport.ReplayHeader))
	assert.JSONEq(t, string(declined.Body), string(replay.Body))
}

// Case 4: the acquirer never answered, so the outcome is unknown. Caching that
// for 24 hours would leave the merchant permanently unable to complete the
// payment, so the key is released and the retry gets a real attempt.
func TestIdempotencyReleasesKeyAfterServerError(t *testing.T) {
	e := newEnv(t)
	key := uuid.NewString()
	redisKey := idempotency.Key(e.Merchant.ID.String(), key)

	first := e.createPayment("ORDER-IDEM-4", 10_000, CardTimeout, false, key)
	require.Equalf(t, http.StatusServiceUnavailable, first.Status, "body: %s", first.Body)
	assert.Equal(t, "acquirer_unavailable", first.ErrorCode())

	record, err := shared.Idempotency.Get(context.Background(), redisKey)
	require.NoError(t, err)
	assert.Nil(t, record, "a 5xx must leave no cached answer behind")

	// The retry actually re-runs the handler instead of replaying a failure.
	retry := e.createPayment("ORDER-IDEM-4", 10_000, CardTimeout, false, key)
	assert.Equal(t, http.StatusServiceUnavailable, retry.Status)
	assert.Empty(t, retry.Header.Get(transport.ReplayHeader), "the handler ran again")

	// And it resumed the unfinished transaction rather than creating a second
	// one: the row is still `created`, because a timeout says nothing about
	// whether the hold exists.
	var count int
	require.NoError(t, shared.DB.Pool.QueryRow(context.Background(),
		`SELECT COUNT(*) FROM transactions WHERE merchant_id = $1`, e.Merchant.ID).Scan(&count))
	assert.Equal(t, 1, count, "a retried authorization must not duplicate the order")

	txn, err := shared.TransactionRepo.GetByReference(
		context.Background(), shared.DB.Pool, e.Merchant.ID, "ORDER-IDEM-4")
	require.NoError(t, err)
	assert.Equal(t, domain.StatusCreated, txn.Status,
		"an unknown outcome must not be recorded as a definite failure")
	assert.Empty(t, e.entriesFor(txn.ID), "nothing may be booked for an unknown outcome")
	e.assertLedgerBalanced()
}

// The header is not optional on money-moving endpoints.
func TestIdempotencyKeyIsRequired(t *testing.T) {
	e := newEnv(t)

	resp := e.do(http.MethodPost, "/api/v1/payments", map[string]any{
		"reference": "ORDER-IDEM-5",
		"amount":    1000,
		"currency":  "VND",
		"card":      map[string]any{"number": CardNormal, "exp_month": 12, "exp_year": 2030, "cvv": "123"},
	})

	assert.Equal(t, http.StatusBadRequest, resp.Status)
	assert.Equal(t, "invalid_request", resp.ErrorCode())
}

// TestConcurrentIdenticalRequestsChargeOnce is the realistic version of the
// double-click: N identical requests, one charge.
func TestConcurrentIdenticalRequestsChargeOnce(t *testing.T) {
	e := newEnv(t)
	key := uuid.NewString()

	const attempts = 8
	var wg sync.WaitGroup
	statuses := make([]int, attempts)

	for i := 0; i < attempts; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			statuses[i] = e.createPayment("ORDER-IDEM-RACE", 75_000, CardNormal, false, key).Status
		}(i)
	}
	wg.Wait()

	created, replayedOrConflicted := 0, 0
	for _, status := range statuses {
		switch status {
		case http.StatusCreated:
			created++
		case http.StatusConflict:
			replayedOrConflicted++
		default:
			t.Fatalf("unexpected status %d", status)
		}
	}
	assert.GreaterOrEqual(t, created, 1)
	assert.Equal(t, attempts, created+replayedOrConflicted)

	var count int
	require.NoError(t, shared.DB.Pool.QueryRow(context.Background(),
		`SELECT COUNT(*) FROM transactions WHERE merchant_id = $1`, e.Merchant.ID).Scan(&count))
	assert.Equal(t, 1, count, "one transaction, no matter how many times the button was clicked")

	e.assertLedgerBalanced()
}

// A capture retried with the same key must not capture twice.
func TestIdempotentCaptureIsNotDoubleCharged(t *testing.T) {
	e := newEnv(t)
	payment := e.authorize("ORDER-IDEM-CAP", 100_000)
	key := uuid.NewString()

	first := e.do(http.MethodPost, "/api/v1/payments/"+payment.ID+"/capture",
		map[string]any{"amount": 40_000}, requestOpts{IdempotencyKey: key})
	require.Equal(t, http.StatusOK, first.Status)

	second := e.do(http.MethodPost, "/api/v1/payments/"+payment.ID+"/capture",
		map[string]any{"amount": 40_000}, requestOpts{IdempotencyKey: key})

	assert.Equal(t, http.StatusOK, second.Status)
	assert.Equal(t, "true", second.Header.Get(transport.ReplayHeader))

	txn := e.transaction(uuid.MustParse(payment.ID))
	assert.Equal(t, int64(40_000), txn.CapturedAmount.Int64(), "the retry must not capture again")

	groups, err := shared.LedgerRepo.CountEntryGroups(
		context.Background(), shared.DB.Pool, txn.ID, domain.EventCapture)
	require.NoError(t, err)
	assert.Equal(t, 1, groups)
	e.assertLedgerBalanced()
}
