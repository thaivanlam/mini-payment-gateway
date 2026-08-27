//go:build integration

package integration

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/thaivanlam/mini-payment-gateway/internal/domain"
)

// TestConcurrentCaptureWritesLedgerOnce is the test the whole locking design
// exists for.
//
// Ten goroutines capture the same transaction at the same moment. All ten pass
// the pre-check, all ten call the acquirer, and all ten open a database
// transaction -- but SELECT ... FOR UPDATE serialises them, and each one
// re-applies the domain rules to the row as it actually is. Exactly one can
// find money left to capture.
//
// Run with -race.
func TestConcurrentCaptureWritesLedgerOnce(t *testing.T) {
	e := newEnv(t)
	payment := e.authorize("ORDER-RACE-1", 100_000)
	txID := uuid.MustParse(payment.ID)

	const goroutines = 10

	var (
		start   sync.WaitGroup
		done    sync.WaitGroup
		mu      sync.Mutex
		results []error
	)
	start.Add(1)

	for i := 0; i < goroutines; i++ {
		done.Add(1)
		go func() {
			defer done.Done()
			start.Wait() // release everyone at the same instant

			_, err := shared.Payments.Capture(context.Background(), e.Merchant.ID, txID, 100_000)

			mu.Lock()
			results = append(results, err)
			mu.Unlock()
		}()
	}

	start.Done()
	done.Wait()

	var succeeded int
	for _, err := range results {
		if err == nil {
			succeeded++
			continue
		}
		// Everyone else must lose for a *reasonable* reason: either there was
		// nothing left to capture, or the row moved under them.
		assert.Truef(t,
			errors.Is(err, domain.ErrCaptureExceedsAuthlzd) ||
				errors.Is(err, domain.ErrInvalidStateTransition) ||
				errors.Is(err, domain.ErrConcurrentModification),
			"unexpected loser error: %v", err)
	}

	assert.Equal(t, 1, succeeded, "exactly one capture may succeed")

	// The decisive assertion: one accounting event, not ten.
	groups, err := shared.LedgerRepo.CountEntryGroups(
		context.Background(), shared.DB.Pool, txID, domain.EventCapture)
	require.NoError(t, err)
	assert.Equal(t, 1, groups, "a concurrent double capture must produce one entry group")

	txn := e.transaction(txID)
	assert.Equal(t, domain.StatusCaptured, txn.Status)
	assert.Equal(t, int64(100_000), txn.CapturedAmount.Int64(), "the amount must not be captured twice")

	assert.Equal(t, int64(98_000), balanceOf(t, e))
	e.assertLedgerBalanced()
}

// TestConcurrentPartialCapturesNeverOvercapture lets ten goroutines each take a
// slice of the same authorization: their sum may reach the authorized amount
// and must never exceed it.
func TestConcurrentPartialCapturesNeverOvercapture(t *testing.T) {
	e := newEnv(t)
	payment := e.authorize("ORDER-RACE-2", 100_000)
	txID := uuid.MustParse(payment.ID)

	const goroutines = 10
	const slice = 30_000 // 10 x 30000 = 300000, three times the authorization

	var wg sync.WaitGroup
	var mu sync.Mutex
	succeeded := 0

	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := shared.Payments.Capture(
				context.Background(), e.Merchant.ID, txID, slice); err == nil {
				mu.Lock()
				succeeded++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()

	txn := e.transaction(txID)
	assert.LessOrEqual(t, txn.CapturedAmount.Int64(), int64(100_000),
		"captured_amount must never exceed the authorized amount")
	assert.Equal(t, int64(slice)*int64(succeeded), txn.CapturedAmount.Int64(),
		"captured_amount must equal the sum of the captures that reported success")

	e.assertLedgerBalanced()
}

// TestConcurrentRefundsNeverOverRefund is the same property on the refund side.
func TestConcurrentRefundsNeverOverRefund(t *testing.T) {
	e := newEnv(t)
	payment := e.authorize("ORDER-RACE-3", 100_000)
	txID := uuid.MustParse(payment.ID)

	capture := e.do(http.MethodPost, "/api/v1/payments/"+payment.ID+"/capture", nil,
		requestOpts{IdempotencyKey: uuid.NewString()})
	require.Equal(t, http.StatusOK, capture.Status)

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = shared.Payments.Refund(context.Background(), e.Merchant.ID, txID, 40_000)
		}()
	}
	wg.Wait()

	txn := e.transaction(txID)
	assert.LessOrEqual(t, txn.RefundedAmount, txn.CapturedAmount,
		"refunded_amount must never exceed captured_amount")
	e.assertLedgerBalanced()
}

// TestConcurrentAuthorizeSameReference proves the unique constraint, not the
// application, is what stops a duplicated order.
func TestConcurrentAuthorizeSameReference(t *testing.T) {
	e := newEnv(t)

	const goroutines = 10
	var wg sync.WaitGroup
	var mu sync.Mutex
	created := 0

	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			// A different idempotency key each time, so the requests are not
			// deduplicated by the middleware: only the database can stop them.
			resp := e.createPayment("ORDER-RACE-REF", 10_000, CardNormal, false, uuid.NewString())
			if resp.Status == http.StatusCreated {
				mu.Lock()
				created++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()

	assert.Equal(t, 1, created, "one reference, one transaction")

	var count int
	require.NoError(t, shared.DB.Pool.QueryRow(context.Background(),
		`SELECT COUNT(*) FROM transactions WHERE merchant_id = $1 AND reference = $2`,
		e.Merchant.ID, "ORDER-RACE-REF").Scan(&count))
	assert.Equal(t, 1, count)
}
