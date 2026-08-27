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

	"github.com/thaivanlam/mini-payment-gateway/internal/domain"
	transport "github.com/thaivanlam/mini-payment-gateway/internal/transport/http"
)

// TestReconciliationSettlesACleanDay is the daily close: the transaction table
// and the journal agree, so every captured payment is settled and the payout
// entries are booked.
func TestReconciliationSettlesACleanDay(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()

	// Three captured payments, one of them partially refunded, plus one
	// authorization that was never captured and must be ignored.
	captured := []int64{100_000, 200_000, 50_000}
	var ids []uuid.UUID
	for i, amount := range captured {
		payment := e.authorize(referenceFor("RECON", i), amount)
		resp := e.do(http.MethodPost, "/api/v1/payments/"+payment.ID+"/capture", nil,
			requestOpts{IdempotencyKey: uuid.NewString()})
		require.Equalf(t, http.StatusOK, resp.Status, "body: %s", resp.Body)
		ids = append(ids, uuid.MustParse(payment.ID))
	}
	e.authorize("RECON-UNCAPTURED", 999_000)

	refunded := e.do(http.MethodPost, "/api/v1/payments/"+ids[0].String()+"/refund",
		map[string]any{"amount": 20_000}, requestOpts{IdempotencyKey: uuid.NewString()})
	require.Equal(t, http.StatusOK, refunded.Status)

	report, err := shared.Recon.Run(ctx, time.Now().UTC())
	require.NoError(t, err)

	require.Truef(t, report.OK(), "the day should reconcile: %+v", report.Discrepancies)
	assert.Empty(t, report.UnbalancedGroups)
	assert.Equal(t, 3, report.TransactionCount, "only captured transactions are in scope")
	assert.Equal(t, 3, report.SettledCount)
	require.Len(t, report.Merchants, 1)

	summary := report.Merchants[0]
	assert.Equal(t, e.Merchant.ID, summary.MerchantID)
	assert.Equal(t, int64(350_000), summary.CapturedTotal.Int64())
	assert.Equal(t, int64(20_000), summary.RefundedTotal.Int64())
	assert.Equal(t, int64(7_000), summary.FeeTotal.Int64(), "2% of 350000")
	assert.Equal(t, int64(323_000), summary.NetPayout.Int64(), "350000 - 7000 - 20000")
	assert.True(t, summary.Balanced)

	// Both sides of the comparison must actually agree, not merely be empty.
	assert.Equal(t, summary.CapturedTotal, summary.LedgerCaptured)
	assert.Equal(t, summary.RefundedTotal, summary.LedgerRefunded)
	assert.Equal(t, summary.FeeTotal, summary.LedgerFees)

	// Settlement moved every captured transaction on and booked the payout.
	for _, id := range ids {
		txn := e.transaction(id)
		assert.Equal(t, domain.StatusSettled, txn.Status)
		require.NotNil(t, txn.SettledAt)
	}
	assert.Equal(t, int64(0), balanceOf(t, e),
		"after payout the platform owes the merchant nothing")
	e.assertLedgerBalanced()
}

// A second run of the same day is a no-op rather than a second payout.
func TestReconciliationIsRepeatable(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()

	payment := e.authorize("RECON-REPEAT-1", 100_000)
	resp := e.do(http.MethodPost, "/api/v1/payments/"+payment.ID+"/capture", nil,
		requestOpts{IdempotencyKey: uuid.NewString()})
	require.Equal(t, http.StatusOK, resp.Status)

	first, err := shared.Recon.Run(ctx, time.Now().UTC())
	require.NoError(t, err)
	require.True(t, first.OK())
	assert.Equal(t, 1, first.SettledCount)

	second, err := shared.Recon.Run(ctx, time.Now().UTC())
	require.NoError(t, err)
	assert.True(t, second.OK())
	assert.Equal(t, 0, second.SettledCount, "an already settled transaction is not paid twice")

	assert.Equal(t, int64(0), balanceOf(t, e))
	e.assertLedgerBalanced()
}

// TestReconciliationDetectsATamperedLedger is the whole point of the exercise:
// if the two records ever disagree, the job must refuse to settle and say
// exactly what is wrong.
func TestReconciliationDetectsATamperedLedger(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()

	payment := e.authorize("RECON-BREAK-1", 100_000)
	resp := e.do(http.MethodPost, "/api/v1/payments/"+payment.ID+"/capture", nil,
		requestOpts{IdempotencyKey: uuid.NewString()})
	require.Equal(t, http.StatusOK, resp.Status)

	// Forge a capture in the journal that no transaction accounts for. The
	// journal is append-only, so the only way to break it is to append.
	txID := uuid.MustParse(payment.ID)
	groupID := uuid.New()
	for _, leg := range []struct {
		account   string
		direction string
	}{
		{domain.AccountAcquirerReceivable, "debit"},
		{domain.MerchantPayableAccount(e.Merchant.ID), "credit"},
	} {
		_, err := shared.DB.Pool.Exec(ctx, `
			INSERT INTO ledger_entries (
				entry_group_id, transaction_id, account, merchant_id,
				direction, amount, currency, event_type, created_at)
			VALUES ($1, $2, $3, $4, $5, $6, 'VND', 'capture', now())`,
			groupID, txID, leg.account, e.Merchant.ID, leg.direction, 12_345)
		require.NoError(t, err)
	}

	report, err := shared.Recon.Run(ctx, time.Now().UTC())
	require.NoError(t, err)

	assert.False(t, report.OK(), "a tampered journal must not reconcile")
	assert.True(t, report.SettlementSkipped, "settlement must not run on a broken day")
	assert.Equal(t, 0, report.SettledCount)
	require.NotEmpty(t, report.Discrepancies)
	assert.Equal(t, "captured_total", report.Discrepancies[0].Field)
	assert.Equal(t, int64(100_000), report.Discrepancies[0].Expected)
	assert.Equal(t, int64(112_345), report.Discrepancies[0].Actual)

	// The transaction is left alone for a human to look at.
	assert.Equal(t, domain.StatusCaptured, e.transaction(txID).Status)
}

// An unbalanced entry group blocks settlement even if the totals happen to add
// up, because the invariant itself is broken.
func TestReconciliationDetectsAnUnbalancedGroup(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()

	payment := e.authorize("RECON-BREAK-2", 100_000)
	resp := e.do(http.MethodPost, "/api/v1/payments/"+payment.ID+"/capture", nil,
		requestOpts{IdempotencyKey: uuid.NewString()})
	require.Equal(t, http.StatusOK, resp.Status)

	// A single-legged entry group: a debit with no matching credit.
	_, err := shared.DB.Pool.Exec(ctx, `
		INSERT INTO ledger_entries (
			entry_group_id, transaction_id, account, merchant_id,
			direction, amount, currency, event_type, created_at)
		VALUES ($1, $2, $3, $4, 'debit', 1, 'VND', 'settlement', now())`,
		uuid.New(), uuid.MustParse(payment.ID), domain.AccountPlatformCash, e.Merchant.ID)
	require.NoError(t, err)

	report, err := shared.Recon.Run(ctx, time.Now().UTC())
	require.NoError(t, err)

	assert.False(t, report.OK())
	assert.NotEmpty(t, report.UnbalancedGroups)
	assert.Equal(t, 0, report.SettledCount)

	unbalanced, err := shared.Ledger.CheckInvariant(ctx)
	require.NoError(t, err)
	assert.Len(t, unbalanced, 1)
}

// The per-merchant settlement report is the same arithmetic, exposed over HTTP.
func TestSettlementReportEndpoint(t *testing.T) {
	e := newEnv(t)

	payment := e.authorize("RECON-REPORT-1", 250_000)
	capture := e.do(http.MethodPost, "/api/v1/payments/"+payment.ID+"/capture", nil,
		requestOpts{IdempotencyKey: uuid.NewString()})
	require.Equal(t, http.StatusOK, capture.Status)

	today := time.Now().UTC().Format("2006-01-02")
	resp := e.do(http.MethodGet, "/api/v1/reports/settlement?date="+today, nil)
	require.Equalf(t, http.StatusOK, resp.Status, "body: %s", resp.Body)

	var report transport.SettlementReportResponse
	resp.JSON(t, &report)

	assert.Equal(t, today, report.Date)
	assert.Equal(t, e.Merchant.ID.String(), report.MerchantID)
	assert.Equal(t, 1, report.TransactionCount)
	assert.Equal(t, int64(250_000), report.CapturedTotal)
	assert.Equal(t, int64(5_000), report.FeeTotal)
	assert.Equal(t, int64(245_000), report.NetPayout)
	assert.True(t, report.Balanced)

	bad := e.do(http.MethodGet, "/api/v1/reports/settlement?date=27-08-2026", nil)
	assert.Equal(t, http.StatusBadRequest, bad.Status)
}

func referenceFor(prefix string, i int) string {
	return prefix + "-" + string(rune('A'+i))
}
