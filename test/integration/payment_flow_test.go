//go:build integration

package integration

import (
	"context"
	"net/http"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/thaivanlam/mini-payment-gateway/internal/domain"
	"github.com/thaivanlam/mini-payment-gateway/internal/service"
	transport "github.com/thaivanlam/mini-payment-gateway/internal/transport/http"
)

// TestAuthorizeCaptureRefundFlow walks the whole lifecycle through the public
// API and checks the journal at every step.
func TestAuthorizeCaptureRefundFlow(t *testing.T) {
	e := newEnv(t)

	// --- authorize ---
	payment := e.authorize("ORDER-FLOW-1", 150_000)
	assert.Equal(t, int64(150_000), payment.Amount)
	assert.Equal(t, int64(0), payment.CapturedAmount)
	assert.Equal(t, "4242", payment.Card.Last4)
	assert.Equal(t, "visa", payment.Card.Brand)
	require.NotNil(t, payment.AuthorizedAt)
	require.NotNil(t, payment.ExpiresAt, "an authorization has a capture deadline")

	txID := uuid.MustParse(payment.ID)
	assert.Empty(t, e.entriesFor(txID), "an authorization moves no money, so it writes no journal lines")

	// --- capture in full ---
	resp := e.do(http.MethodPost, "/api/v1/payments/"+payment.ID+"/capture",
		map[string]any{"amount": 150_000}, requestOpts{IdempotencyKey: uuid.NewString()})
	require.Equalf(t, http.StatusOK, resp.Status, "capture failed: %s", resp.Body)

	var captured transport.PaymentResponse
	resp.JSON(t, &captured)
	assert.Equal(t, string(domain.StatusCaptured), captured.Status)
	assert.Equal(t, int64(150_000), captured.CapturedAmount)

	// 2% of 150000 = 3000, so the merchant is owed 147000.
	entries := e.entriesFor(txID)
	require.Len(t, entries, 3, "capture books receivable, payable and fee")
	assert.Equal(t, int64(147_000), balanceOf(t, e), "balance = credits - debits on merchant_payable")
	e.assertLedgerBalanced()

	// --- partial refund ---
	resp = e.do(http.MethodPost, "/api/v1/payments/"+payment.ID+"/refund",
		map[string]any{"amount": 50_000}, requestOpts{IdempotencyKey: uuid.NewString()})
	require.Equalf(t, http.StatusOK, resp.Status, "refund failed: %s", resp.Body)

	var refunded transport.PaymentResponse
	resp.JSON(t, &refunded)
	assert.Equal(t, string(domain.StatusCaptured), refunded.Status,
		"a partial refund does not reach the terminal refunded state")
	assert.Equal(t, int64(50_000), refunded.RefundedAmount)

	assert.Len(t, e.entriesFor(txID), 5, "the refund adds two more journal lines")
	assert.Equal(t, int64(97_000), balanceOf(t, e), "147000 - 50000")
	e.assertLedgerBalanced()

	// --- refund the rest ---
	resp = e.do(http.MethodPost, "/api/v1/payments/"+payment.ID+"/refund",
		map[string]any{"amount": 100_000}, requestOpts{IdempotencyKey: uuid.NewString()})
	require.Equal(t, http.StatusOK, resp.Status)
	resp.JSON(t, &refunded)
	assert.Equal(t, string(domain.StatusRefunded), refunded.Status, "a full refund is terminal")

	// The fee is not returned: 147000 credited, 150000 debited back out.
	assert.Equal(t, int64(-3_000), balanceOf(t, e),
		"the platform keeps the fee, so the merchant ends up owing it")
	e.assertLedgerBalanced()

	// --- the journal is append-only ---
	_, err := shared.DB.Pool.Exec(context.Background(), `DELETE FROM ledger_entries WHERE transaction_id = $1`, txID)
	assert.Error(t, err, "the database itself refuses to delete journal lines")
}

func TestPartialCaptures(t *testing.T) {
	e := newEnv(t)
	payment := e.authorize("ORDER-PARTIAL-1", 100_000)

	first := e.do(http.MethodPost, "/api/v1/payments/"+payment.ID+"/capture",
		map[string]any{"amount": 40_000}, requestOpts{IdempotencyKey: uuid.NewString()})
	require.Equal(t, http.StatusOK, first.Status)

	second := e.do(http.MethodPost, "/api/v1/payments/"+payment.ID+"/capture",
		map[string]any{"amount": 60_000}, requestOpts{IdempotencyKey: uuid.NewString()})
	require.Equal(t, http.StatusOK, second.Status)

	var captured transport.PaymentResponse
	second.JSON(t, &captured)
	assert.Equal(t, int64(100_000), captured.CapturedAmount)
	assert.Equal(t, string(domain.StatusCaptured), captured.Status)

	// 2% of 40000 = 800; 2% of 60000 = 1200. Merchant is owed 98000.
	assert.Equal(t, int64(98_000), balanceOf(t, e))
	e.assertLedgerBalanced()

	// A third capture has nothing left to take.
	third := e.do(http.MethodPost, "/api/v1/payments/"+payment.ID+"/capture",
		map[string]any{"amount": 1}, requestOpts{IdempotencyKey: uuid.NewString()})
	assert.Equal(t, http.StatusUnprocessableEntity, third.Status)
	assert.Equal(t, "capture_exceeds_authorized", third.ErrorCode())
}

func TestCaptureWithNoBodyCapturesEverything(t *testing.T) {
	e := newEnv(t)
	payment := e.authorize("ORDER-FULL-1", 80_000)

	resp := e.do(http.MethodPost, "/api/v1/payments/"+payment.ID+"/capture", nil,
		requestOpts{IdempotencyKey: uuid.NewString()})
	require.Equalf(t, http.StatusOK, resp.Status, "body: %s", resp.Body)

	var captured transport.PaymentResponse
	resp.JSON(t, &captured)
	assert.Equal(t, int64(80_000), captured.CapturedAmount)
	e.assertLedgerBalanced()
}

func TestAuthorizeAndCaptureInOneCall(t *testing.T) {
	e := newEnv(t)

	resp := e.createPayment("ORDER-AUTOCAP-1", 60_000, CardNormal, true, "")
	require.Equalf(t, http.StatusCreated, resp.Status, "body: %s", resp.Body)

	var payment transport.PaymentResponse
	resp.JSON(t, &payment)
	assert.Equal(t, string(domain.StatusCaptured), payment.Status)
	assert.Equal(t, int64(60_000), payment.CapturedAmount)

	assert.Equal(t, int64(58_800), balanceOf(t, e), "60000 minus a 2% fee")
	e.assertLedgerBalanced()
}

func TestDeclinedCardIsRecordedAsFailed(t *testing.T) {
	e := newEnv(t)

	resp := e.createPayment("ORDER-DECLINE-1", 10_000, CardDecline, false, "")

	assert.Equal(t, http.StatusPaymentRequired, resp.Status)
	assert.Equal(t, "do_not_honor", resp.ErrorCode())

	txn, err := shared.TransactionRepo.GetByReference(
		context.Background(), shared.DB.Pool, e.Merchant.ID, "ORDER-DECLINE-1")
	require.NoError(t, err)
	assert.Equal(t, domain.StatusFailed, txn.Status)
	assert.Equal(t, "do_not_honor", txn.FailureCode)

	assert.Empty(t, e.entriesFor(txn.ID), "a declined payment moves no money")
	e.assertLedgerBalanced()

	// A failed payment cannot be captured.
	capture := e.do(http.MethodPost, "/api/v1/payments/"+txn.ID.String()+"/capture", nil,
		requestOpts{IdempotencyKey: uuid.NewString()})
	assert.Equal(t, http.StatusConflict, capture.Status)
	assert.Equal(t, "invalid_state", capture.ErrorCode())
}

func TestVoidReleasesAuthorization(t *testing.T) {
	e := newEnv(t)
	payment := e.authorize("ORDER-VOID-1", 25_000)

	resp := e.do(http.MethodPost, "/api/v1/payments/"+payment.ID+"/void", nil)
	require.Equalf(t, http.StatusOK, resp.Status, "body: %s", resp.Body)

	var voided transport.PaymentResponse
	resp.JSON(t, &voided)
	assert.Equal(t, string(domain.StatusVoided), voided.Status)

	assert.Empty(t, e.entriesFor(uuid.MustParse(payment.ID)), "a void moves no money")
	assert.Equal(t, int64(0), balanceOf(t, e))

	// Voided is terminal.
	again := e.do(http.MethodPost, "/api/v1/payments/"+payment.ID+"/void", nil)
	assert.Equal(t, http.StatusConflict, again.Status)
}

func TestDuplicateReferenceIsRejected(t *testing.T) {
	e := newEnv(t)

	first := e.createPayment("ORDER-DUP-1", 10_000, CardNormal, false, "")
	require.Equal(t, http.StatusCreated, first.Status)

	// Same reference, new idempotency key: this is a genuinely new request for
	// an order that already exists, and the unique constraint catches it.
	second := e.createPayment("ORDER-DUP-1", 10_000, CardNormal, false, "")

	assert.Equal(t, http.StatusConflict, second.Status)
	assert.Equal(t, "duplicate_reference", second.ErrorCode())
}

func TestMerchantCannotReadAnotherMerchantsPayment(t *testing.T) {
	e := newEnv(t)
	payment := e.authorize("ORDER-ISOLATION-1", 10_000)

	// A second merchant with its own credentials.
	other := newEnvForSecondMerchant(t, e)

	resp := other.do(http.MethodGet, "/api/v1/payments/"+payment.ID, nil)
	assert.Equal(t, http.StatusNotFound, resp.Status,
		"a transaction id from another merchant must look like it does not exist")
}

func TestGetAndListPayments(t *testing.T) {
	e := newEnv(t)

	for i := 1; i <= 5; i++ {
		e.authorize("ORDER-LIST-"+string(rune('0'+i)), int64(10_000*i))
	}

	list := e.do(http.MethodGet, "/api/v1/payments?limit=2", nil)
	require.Equalf(t, http.StatusOK, list.Status, "body: %s", list.Body)

	var page transport.PaymentListResponse
	list.JSON(t, &page)
	require.Len(t, page.Data, 2)
	assert.True(t, page.HasMore)
	require.NotEmpty(t, page.NextCursor)

	// Walk the cursor to the end and check every transaction appears once.
	seen := map[string]bool{}
	for _, p := range page.Data {
		seen[p.ID] = true
	}
	cursor := page.NextCursor
	for cursor != "" {
		next := e.do(http.MethodGet, "/api/v1/payments?limit=2&cursor="+cursor, nil)
		require.Equal(t, http.StatusOK, next.Status)
		var nextPage transport.PaymentListResponse
		next.JSON(t, &nextPage)
		for _, p := range nextPage.Data {
			require.Falsef(t, seen[p.ID], "cursor pagination returned %s twice", p.ID)
			seen[p.ID] = true
		}
		cursor = nextPage.NextCursor
	}
	assert.Len(t, seen, 5, "cursor pagination must visit every row exactly once")

	// Filtering by status.
	filtered := e.do(http.MethodGet, "/api/v1/payments?status=captured", nil)
	require.Equal(t, http.StatusOK, filtered.Status)
	var capturedPage transport.PaymentListResponse
	filtered.JSON(t, &capturedPage)
	assert.Empty(t, capturedPage.Data, "nothing has been captured in this scenario")

	bad := e.do(http.MethodGet, "/api/v1/payments?status=nonsense", nil)
	assert.Equal(t, http.StatusBadRequest, bad.Status)
}

func TestGetUnknownPayment(t *testing.T) {
	e := newEnv(t)

	resp := e.do(http.MethodGet, "/api/v1/payments/"+uuid.NewString(), nil)
	assert.Equal(t, http.StatusNotFound, resp.Status)

	bad := e.do(http.MethodGet, "/api/v1/payments/not-a-uuid", nil)
	assert.Equal(t, http.StatusBadRequest, bad.Status)
}

func TestLedgerEndpoints(t *testing.T) {
	e := newEnv(t)
	payment := e.authorize("ORDER-LEDGER-1", 200_000)

	capture := e.do(http.MethodPost, "/api/v1/payments/"+payment.ID+"/capture", nil,
		requestOpts{IdempotencyKey: uuid.NewString()})
	require.Equal(t, http.StatusOK, capture.Status)

	entries := e.do(http.MethodGet, "/api/v1/ledger/entries", nil)
	require.Equalf(t, http.StatusOK, entries.Status, "body: %s", entries.Body)

	var entryPage transport.LedgerListResponse
	entries.JSON(t, &entryPage)
	require.Len(t, entryPage.Data, 3)
	for _, entry := range entryPage.Data {
		assert.Positive(t, entry.Amount, "journal amounts are always positive")
		assert.Contains(t, []string{"debit", "credit"}, entry.Direction)
	}

	balance := e.do(http.MethodGet, "/api/v1/ledger/balance?currency=VND", nil)
	require.Equal(t, http.StatusOK, balance.Status)

	var balanceBody transport.BalanceResponse
	balance.JSON(t, &balanceBody)
	assert.Equal(t, int64(196_000), balanceBody.Available, "200000 minus a 2% fee")
	assert.Equal(t, balanceBody.Credits-balanceBody.Debits, balanceBody.Available)

	bad := e.do(http.MethodGet, "/api/v1/ledger/balance?currency=XYZ", nil)
	assert.Equal(t, http.StatusBadRequest, bad.Status)
}

// newEnvForSecondMerchant creates a second merchant sharing the same server,
// used to prove that merchant data is isolated.
func newEnvForSecondMerchant(t *testing.T, base *env) *env {
	t.Helper()
	created, err := shared.Merchants.Create(context.Background(), service.CreateMerchantInput{
		Name:  "Other Merchant",
		Email: "other-" + uuid.NewString()[:8] + "@example.com",
	})
	require.NoError(t, err)

	return &env{
		t:        t,
		app:      base.app,
		server:   base.server,
		log:      base.log,
		Merchant: created.Merchant,
		APIKey:   created.Merchant.APIKey,
		Secret:   created.APISecret,
		Received: base.Received,
	}
}

// balanceOf reads the merchant balance through the API, which is the same
// fold over the journal the rest of the system uses.
func balanceOf(t *testing.T, e *env) int64 {
	t.Helper()
	resp := e.do(http.MethodGet, "/api/v1/ledger/balance?currency=VND", nil)
	require.Equalf(t, http.StatusOK, resp.Status, "body: %s", resp.Body)

	var body transport.BalanceResponse
	resp.JSON(t, &body)
	return body.Available
}
