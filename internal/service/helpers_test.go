package service

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/thaivanlam/mini-payment-gateway/internal/domain"
	"github.com/thaivanlam/mini-payment-gateway/internal/money"
)

// testTransaction builds an authorized, captured transaction for tests that do
// not need a database.
func testTransaction(t *testing.T) *domain.Transaction {
	t.Helper()
	now := time.Date(2026, 8, 27, 10, 0, 0, 0, time.UTC)

	txn, err := domain.NewTransaction(
		uuid.New(), "ORDER-SVC-1", 150_000, money.VND,
		map[string]string{"customer_id": "cus_123"}, now)
	require.NoError(t, err)

	require.NoError(t, txn.Authorize("auth_ref_1", "4242", "visa", now))
	require.NoError(t, txn.Capture(150_000, now))
	return txn
}
