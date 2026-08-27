package domain

import (
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/thaivanlam/mini-payment-gateway/internal/money"
)

var testNow = time.Date(2026, 8, 27, 10, 0, 0, 0, time.UTC)

func newTestTransaction(t *testing.T) *Transaction {
	t.Helper()
	txn, err := NewTransaction(uuid.New(), "ORDER-1", 150_000, money.VND, nil, testNow)
	require.NoError(t, err)
	return txn
}

func authorized(t *testing.T) *Transaction {
	t.Helper()
	txn := newTestTransaction(t)
	require.NoError(t, txn.Authorize("auth_123", "4242", "visa", testNow))
	return txn
}

func TestNewTransaction(t *testing.T) {
	tests := []struct {
		name      string
		reference string
		amount    money.Amount
		currency  money.Currency
		wantField string
	}{
		{name: "valid", reference: "ORDER-1", amount: 1000, currency: money.VND},
		{name: "empty reference", reference: "", amount: 1000, currency: money.VND, wantField: "reference"},
		{name: "zero amount", reference: "ORDER-1", amount: 0, currency: money.VND, wantField: "amount"},
		{name: "negative amount", reference: "ORDER-1", amount: -1, currency: money.VND, wantField: "amount"},
		{name: "unknown currency", reference: "ORDER-1", amount: 1000, currency: "XYZ", wantField: "currency"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			txn, err := NewTransaction(uuid.New(), tc.reference, tc.amount, tc.currency, nil, testNow)
			if tc.wantField == "" {
				require.NoError(t, err)
				assert.Equal(t, StatusCreated, txn.Status)
				assert.NotEqual(t, uuid.Nil, txn.ID)
				return
			}
			require.Error(t, err)
			assert.ErrorIs(t, err, ErrValidation)
			var invalid *ValidationError
			require.True(t, errors.As(err, &invalid))
			assert.Equal(t, tc.wantField, invalid.Field)
		})
	}
}

func TestNewTransactionRejectsLongReference(t *testing.T) {
	long := make([]byte, 129)
	for i := range long {
		long[i] = 'x'
	}
	_, err := NewTransaction(uuid.New(), string(long), 1000, money.VND, nil, testNow)
	assert.ErrorIs(t, err, ErrValidation)
}

// TestAllowedTransitions walks every (from, to) pair in the state machine and
// asserts the matrix, so a change to allowedTransitions cannot pass unnoticed.
func TestAllowedTransitions(t *testing.T) {
	all := []Status{
		StatusCreated, StatusAuthorized, StatusCaptured,
		StatusSettled, StatusRefunded, StatusVoided, StatusFailed,
	}
	allowed := map[Status]map[Status]bool{
		StatusCreated:    {StatusAuthorized: true, StatusFailed: true},
		StatusAuthorized: {StatusCaptured: true, StatusVoided: true, StatusFailed: true},
		StatusCaptured:   {StatusSettled: true, StatusRefunded: true},
		StatusSettled:    {StatusRefunded: true},
		StatusRefunded:   {},
		StatusVoided:     {},
		StatusFailed:     {},
	}

	for _, from := range all {
		for _, to := range all {
			txn := &Transaction{Status: from}
			want := allowed[from][to]

			assert.Equalf(t, want, txn.CanTransitionTo(to), "CanTransitionTo(%s -> %s)", from, to)

			err := txn.TransitionTo(to)
			if want {
				require.NoErrorf(t, err, "TransitionTo(%s -> %s)", from, to)
				assert.Equal(t, to, txn.Status)
			} else {
				require.Errorf(t, err, "TransitionTo(%s -> %s) should fail", from, to)
				assert.ErrorIs(t, err, ErrInvalidStateTransition)
				assert.Equal(t, from, txn.Status, "a rejected transition must not mutate status")
			}
		}
	}
}

func TestStatusHelpers(t *testing.T) {
	assert.True(t, StatusFailed.IsTerminal())
	assert.True(t, StatusVoided.IsTerminal())
	assert.True(t, StatusRefunded.IsTerminal())
	assert.False(t, StatusAuthorized.IsTerminal())
	assert.True(t, StatusCaptured.Valid())
	assert.False(t, Status("nonsense").Valid())
}

func TestAuthorize(t *testing.T) {
	txn := newTestTransaction(t)
	require.NoError(t, txn.Authorize("auth_1", "4242", "visa", testNow))

	assert.Equal(t, StatusAuthorized, txn.Status)
	assert.Equal(t, "auth_1", txn.AcquirerRef)
	assert.Equal(t, "4242", txn.CardLast4)
	assert.Equal(t, "visa", txn.CardBrand)
	require.NotNil(t, txn.AuthorizedAt)
	assert.Equal(t, testNow, *txn.AuthorizedAt)

	// Authorizing twice is not a thing.
	assert.ErrorIs(t, txn.Authorize("auth_2", "4242", "visa", testNow), ErrInvalidStateTransition)
}

func TestFail(t *testing.T) {
	txn := newTestTransaction(t)
	require.NoError(t, txn.Fail("insufficient_funds", testNow))
	assert.Equal(t, StatusFailed, txn.Status)
	assert.Equal(t, "insufficient_funds", txn.FailureCode)
	assert.ErrorIs(t, txn.Fail("do_not_honor", testNow), ErrInvalidStateTransition)
}

func TestVoid(t *testing.T) {
	t.Run("authorized transaction", func(t *testing.T) {
		txn := authorized(t)
		require.NoError(t, txn.Void(testNow))
		assert.Equal(t, StatusVoided, txn.Status)
	})

	t.Run("captured transaction cannot be voided", func(t *testing.T) {
		txn := authorized(t)
		require.NoError(t, txn.Capture(50_000, testNow))
		err := txn.Void(testNow)
		assert.ErrorIs(t, err, ErrInvalidStateTransition)
		assert.Equal(t, StatusCaptured, txn.Status)
	})

	t.Run("created transaction cannot be voided", func(t *testing.T) {
		txn := newTestTransaction(t)
		assert.ErrorIs(t, txn.Void(testNow), ErrInvalidStateTransition)
	})
}

func TestCaptureFull(t *testing.T) {
	txn := authorized(t)
	require.NoError(t, txn.Capture(150_000, testNow))

	assert.Equal(t, StatusCaptured, txn.Status)
	assert.Equal(t, money.Amount(150_000), txn.CapturedAmount)
	assert.Equal(t, money.Amount(0), txn.RemainingCapturable())
	require.NotNil(t, txn.CapturedAt)
}

func TestCapturePartialThenRest(t *testing.T) {
	txn := authorized(t)

	require.NoError(t, txn.Capture(50_000, testNow))
	assert.Equal(t, StatusCaptured, txn.Status)
	assert.Equal(t, money.Amount(100_000), txn.RemainingCapturable())
	firstCapturedAt := *txn.CapturedAt

	later := testNow.Add(time.Hour)
	require.NoError(t, txn.Capture(100_000, later))
	assert.Equal(t, money.Amount(150_000), txn.CapturedAmount)
	assert.Equal(t, StatusCaptured, txn.Status)
	assert.Equal(t, firstCapturedAt, *txn.CapturedAt, "captured_at marks the first capture")
}

func TestCaptureRules(t *testing.T) {
	t.Run("cannot exceed authorized amount", func(t *testing.T) {
		txn := authorized(t)
		err := txn.Capture(150_001, testNow)
		assert.ErrorIs(t, err, ErrCaptureExceedsAuthlzd)
		assert.Equal(t, money.Amount(0), txn.CapturedAmount)
	})

	t.Run("cannot exceed across two captures", func(t *testing.T) {
		txn := authorized(t)
		require.NoError(t, txn.Capture(100_000, testNow))
		assert.ErrorIs(t, txn.Capture(50_001, testNow), ErrCaptureExceedsAuthlzd)
		assert.Equal(t, money.Amount(100_000), txn.CapturedAmount)
	})

	t.Run("amount must be positive", func(t *testing.T) {
		txn := authorized(t)
		assert.ErrorIs(t, txn.Capture(0, testNow), ErrValidation)
		assert.ErrorIs(t, txn.Capture(-1, testNow), ErrValidation)
	})

	t.Run("cannot capture from created", func(t *testing.T) {
		txn := newTestTransaction(t)
		assert.ErrorIs(t, txn.Capture(1000, testNow), ErrInvalidStateTransition)
	})

	t.Run("cannot capture a voided transaction", func(t *testing.T) {
		txn := authorized(t)
		require.NoError(t, txn.Void(testNow))
		assert.ErrorIs(t, txn.Capture(1000, testNow), ErrInvalidStateTransition)
	})
}

func TestAuthorizationExpiry(t *testing.T) {
	txn := authorized(t)

	expires := txn.AuthorizationExpiresAt()
	require.NotNil(t, expires)
	assert.Equal(t, testNow.Add(AuthorizationValidity), *expires)

	assert.False(t, txn.AuthorizationExpired(testNow.Add(6*24*time.Hour)))
	assert.True(t, txn.AuthorizationExpired(testNow.Add(8*24*time.Hour)))

	err := txn.Capture(1000, testNow.Add(8*24*time.Hour))
	assert.ErrorIs(t, err, ErrAuthorizationExpired)

	// A transaction that was never authorized has no expiry.
	fresh := newTestTransaction(t)
	assert.Nil(t, fresh.AuthorizationExpiresAt())
	assert.False(t, fresh.AuthorizationExpired(testNow.Add(100*24*time.Hour)))
}

func TestRefundFull(t *testing.T) {
	txn := authorized(t)
	require.NoError(t, txn.Capture(150_000, testNow))

	require.NoError(t, txn.Refund(150_000, testNow))
	assert.Equal(t, StatusRefunded, txn.Status, "a full refund is terminal")
	assert.Equal(t, money.Amount(0), txn.RemainingRefundable())
}

func TestRefundPartialKeepsStatus(t *testing.T) {
	txn := authorized(t)
	require.NoError(t, txn.Capture(150_000, testNow))

	require.NoError(t, txn.Refund(50_000, testNow))
	assert.Equal(t, StatusCaptured, txn.Status, "a partial refund is not terminal")
	assert.Equal(t, money.Amount(100_000), txn.RemainingRefundable())

	require.NoError(t, txn.Refund(100_000, testNow))
	assert.Equal(t, StatusRefunded, txn.Status)
}

func TestRefundRules(t *testing.T) {
	t.Run("cannot exceed captured", func(t *testing.T) {
		txn := authorized(t)
		require.NoError(t, txn.Capture(100_000, testNow))
		assert.ErrorIs(t, txn.Refund(100_001, testNow), ErrRefundExceedsCaptured)
		assert.Equal(t, money.Amount(0), txn.RefundedAmount)
	})

	t.Run("cannot refund an authorization", func(t *testing.T) {
		txn := authorized(t)
		assert.ErrorIs(t, txn.Refund(1000, testNow), ErrInvalidStateTransition)
	})

	t.Run("cannot refund twice past the captured amount", func(t *testing.T) {
		txn := authorized(t)
		require.NoError(t, txn.Capture(100_000, testNow))
		require.NoError(t, txn.Refund(60_000, testNow))
		assert.ErrorIs(t, txn.Refund(60_000, testNow), ErrRefundExceedsCaptured)
	})

	t.Run("amount must be positive", func(t *testing.T) {
		txn := authorized(t)
		require.NoError(t, txn.Capture(100_000, testNow))
		assert.ErrorIs(t, txn.Refund(0, testNow), ErrValidation)
		assert.ErrorIs(t, txn.Refund(-5, testNow), ErrValidation)
	})

	t.Run("a settled transaction can still be refunded", func(t *testing.T) {
		txn := authorized(t)
		require.NoError(t, txn.Capture(100_000, testNow))
		require.NoError(t, txn.Settle(testNow))
		require.NoError(t, txn.Refund(100_000, testNow))
		assert.Equal(t, StatusRefunded, txn.Status)
	})
}

func TestSettle(t *testing.T) {
	txn := authorized(t)
	require.NoError(t, txn.Capture(150_000, testNow))

	require.NoError(t, txn.Settle(testNow))
	assert.Equal(t, StatusSettled, txn.Status)
	require.NotNil(t, txn.SettledAt)

	assert.ErrorIs(t, txn.Settle(testNow), ErrInvalidStateTransition)
}

func TestNetPayable(t *testing.T) {
	txn := authorized(t)
	require.NoError(t, txn.Capture(100_000, testNow))

	// 2% of 100000 = 2000.
	assert.Equal(t, money.Amount(98_000), txn.NetPayable(200))

	require.NoError(t, txn.Refund(30_000, testNow))
	assert.Equal(t, money.Amount(68_000), txn.NetPayable(200))

	assert.Equal(t, money.Amount(70_000), txn.NetPayable(0), "no fee means no deduction")
}
