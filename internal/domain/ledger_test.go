package domain

import (
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/thaivanlam/mini-payment-gateway/internal/money"
)

func ledgerTestTransaction(t *testing.T) *Transaction {
	t.Helper()
	txn, err := NewTransaction(uuid.New(), "ORDER-L", 100_000, money.VND, nil, testNow)
	require.NoError(t, err)
	return txn
}

// assertBalanced is the invariant every entry group must satisfy, asserted the
// same way the integration tests assert it over the whole database.
func assertBalanced(t *testing.T, g EntryGroup) {
	t.Helper()
	require.NoError(t, g.Validate())

	debits, credits := g.Totals()
	assert.Equal(t, debits, credits, "debits must equal credits within an entry group")

	for _, e := range g.Entries {
		assert.Equal(t, g.ID, e.EntryGroupID, "every entry carries the group id")
		assert.True(t, e.Amount.IsPositive(), "entry amounts are always positive")
	}
}

func TestMerchantPayableAccount(t *testing.T) {
	id := uuid.New()
	account := MerchantPayableAccount(id)

	assert.Equal(t, "merchant_payable:"+id.String(), account)
	assert.True(t, IsMerchantPayable(account))
	assert.False(t, IsMerchantPayable(AccountAcquirerReceivable))
	assert.False(t, IsMerchantPayable(AccountPlatformFeeRevenue))
}

func TestNewCaptureEntryGroup(t *testing.T) {
	txn := ledgerTestTransaction(t)

	// The worked example from the spec: capture 100000 with a 2% fee.
	group, err := NewCaptureEntryGroup(txn, 100_000, money.Fee(100_000, 200), testNow)
	require.NoError(t, err)
	assertBalanced(t, group)
	require.Len(t, group.Entries, 3)

	byAccount := map[string]LedgerEntry{}
	for _, e := range group.Entries {
		byAccount[e.Account] = e
	}

	receivable := byAccount[AccountAcquirerReceivable]
	assert.Equal(t, Debit, receivable.Direction)
	assert.Equal(t, money.Amount(100_000), receivable.Amount)
	assert.Equal(t, EventCapture, receivable.EventType)

	payable := byAccount[MerchantPayableAccount(txn.MerchantID)]
	assert.Equal(t, Credit, payable.Direction)
	assert.Equal(t, money.Amount(98_000), payable.Amount)

	fee := byAccount[AccountPlatformFeeRevenue]
	assert.Equal(t, Credit, fee.Direction)
	assert.Equal(t, money.Amount(2_000), fee.Amount)
	assert.Equal(t, EventFee, fee.EventType)
}

func TestNewCaptureEntryGroupWithoutFee(t *testing.T) {
	txn := ledgerTestTransaction(t)

	group, err := NewCaptureEntryGroup(txn, 100_000, 0, testNow)
	require.NoError(t, err)
	assertBalanced(t, group)
	assert.Len(t, group.Entries, 2, "a zero fee produces no revenue entry")
}

func TestNewCaptureEntryGroupRejectsBadInput(t *testing.T) {
	txn := ledgerTestTransaction(t)

	_, err := NewCaptureEntryGroup(txn, 0, 0, testNow)
	assert.ErrorIs(t, err, ErrInvalidAmount)

	_, err = NewCaptureEntryGroup(txn, 100_000, 100_001, testNow)
	assert.ErrorIs(t, err, ErrValidation, "the fee cannot exceed the captured amount")

	_, err = NewCaptureEntryGroup(txn, 100_000, -1, testNow)
	assert.ErrorIs(t, err, ErrValidation)
}

func TestNewRefundEntryGroup(t *testing.T) {
	txn := ledgerTestTransaction(t)

	group, err := NewRefundEntryGroup(txn, 40_000, testNow)
	require.NoError(t, err)
	assertBalanced(t, group)
	require.Len(t, group.Entries, 2)

	for _, e := range group.Entries {
		assert.Equal(t, EventRefund, e.EventType)
		assert.Equal(t, money.Amount(40_000), e.Amount)
		if IsMerchantPayable(e.Account) {
			assert.Equal(t, Debit, e.Direction, "a refund reduces what we owe the merchant")
		} else {
			assert.Equal(t, AccountAcquirerReceivable, e.Account)
			assert.Equal(t, Credit, e.Direction)
		}
	}

	_, err = NewRefundEntryGroup(txn, 0, testNow)
	assert.ErrorIs(t, err, ErrInvalidAmount)
}

func TestNewSettlementEntryGroup(t *testing.T) {
	txn := ledgerTestTransaction(t)

	group, err := NewSettlementEntryGroup(txn, 98_000, testNow)
	require.NoError(t, err)
	assertBalanced(t, group)
	require.Len(t, group.Entries, 2)

	for _, e := range group.Entries {
		assert.Equal(t, EventSettlement, e.EventType)
		if IsMerchantPayable(e.Account) {
			assert.Equal(t, Debit, e.Direction, "paying out discharges the liability")
		} else {
			assert.Equal(t, AccountPlatformCash, e.Account)
			assert.Equal(t, Credit, e.Direction, "cash leaves our account")
		}
	}

	_, err = NewSettlementEntryGroup(txn, 0, testNow)
	assert.ErrorIs(t, err, ErrInvalidAmount)
}

func TestEntryGroupValidate(t *testing.T) {
	groupID := uuid.New()
	entry := func(dir Direction, amount money.Amount, currency money.Currency) LedgerEntry {
		return LedgerEntry{
			EntryGroupID: groupID, Account: "a", Direction: dir,
			Amount: amount, Currency: currency, EventType: EventCapture,
		}
	}

	t.Run("unbalanced group is rejected", func(t *testing.T) {
		g := EntryGroup{ID: groupID, Entries: []LedgerEntry{
			entry(Debit, 100, money.VND),
			entry(Credit, 99, money.VND),
		}}
		assert.ErrorIs(t, g.Validate(), ErrUnbalancedEntryGroup)
	})

	t.Run("single-leg group is rejected", func(t *testing.T) {
		g := EntryGroup{ID: groupID, Entries: []LedgerEntry{entry(Debit, 100, money.VND)}}
		assert.ErrorIs(t, g.Validate(), ErrUnbalancedEntryGroup)
	})

	t.Run("empty group is rejected", func(t *testing.T) {
		assert.ErrorIs(t, EntryGroup{ID: groupID}.Validate(), ErrUnbalancedEntryGroup)
	})

	t.Run("mixed currency is rejected", func(t *testing.T) {
		g := EntryGroup{ID: groupID, Entries: []LedgerEntry{
			entry(Debit, 100, money.VND),
			entry(Credit, 100, money.USD),
		}}
		assert.ErrorIs(t, g.Validate(), ErrCurrencyMismatch)
	})

	t.Run("non-positive amount is rejected", func(t *testing.T) {
		g := EntryGroup{ID: groupID, Entries: []LedgerEntry{
			entry(Debit, 0, money.VND),
			entry(Credit, 0, money.VND),
		}}
		assert.ErrorIs(t, g.Validate(), ErrInvalidAmount)
	})

	t.Run("unknown direction is rejected", func(t *testing.T) {
		g := EntryGroup{ID: groupID, Entries: []LedgerEntry{
			entry("sideways", 100, money.VND),
			entry(Credit, 100, money.VND),
		}}
		assert.ErrorIs(t, g.Validate(), ErrValidation)
	})

	t.Run("balanced multi-leg group passes", func(t *testing.T) {
		g := EntryGroup{ID: groupID, Entries: []LedgerEntry{
			entry(Debit, 100, money.VND),
			entry(Credit, 98, money.VND),
			entry(Credit, 2, money.VND),
		}}
		assert.NoError(t, g.Validate())
	})
}

func TestComputeBalance(t *testing.T) {
	merchantID := uuid.New()
	account := MerchantPayableAccount(merchantID)

	entries := []LedgerEntry{
		{Account: account, Direction: Credit, Amount: 98_000, Currency: money.VND},
		{Account: account, Direction: Credit, Amount: 49_000, Currency: money.VND},
		{Account: account, Direction: Debit, Amount: 20_000, Currency: money.VND},
		// Noise that must not be counted:
		{Account: AccountPlatformFeeRevenue, Direction: Credit, Amount: 3_000, Currency: money.VND},
		{Account: account, Direction: Credit, Amount: 500, Currency: money.USD},
	}

	balance := ComputeBalance(account, money.VND, entries)
	assert.Equal(t, money.Amount(147_000), balance.Credits)
	assert.Equal(t, money.Amount(20_000), balance.Debits)
	assert.Equal(t, money.Amount(127_000), balance.Available())
	assert.Equal(t, money.VND, balance.Currency)
}

// TestCaptureRefundLifecycleBalances walks a full lifecycle and asserts that
// the merchant liability ends where arithmetic says it should.
func TestCaptureRefundLifecycleBalances(t *testing.T) {
	txn := ledgerTestTransaction(t)
	account := MerchantPayableAccount(txn.MerchantID)

	var all []LedgerEntry

	capture, err := NewCaptureEntryGroup(txn, 100_000, money.Fee(100_000, 200), testNow)
	require.NoError(t, err)
	assertBalanced(t, capture)
	all = append(all, capture.Entries...)

	refund, err := NewRefundEntryGroup(txn, 30_000, testNow)
	require.NoError(t, err)
	assertBalanced(t, refund)
	all = append(all, refund.Entries...)

	// 98000 credited at capture, 30000 debited at refund.
	balance := ComputeBalance(account, money.VND, all)
	assert.Equal(t, money.Amount(68_000), balance.Available())

	settlement, err := NewSettlementEntryGroup(txn, balance.Available(), testNow)
	require.NoError(t, err)
	assertBalanced(t, settlement)
	all = append(all, settlement.Entries...)

	assert.Equal(t, money.Amount(0), ComputeBalance(account, money.VND, all).Available(),
		"after settlement we owe the merchant nothing")
}
