package domain

import (
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/thaivanlam/mini-payment-gateway/internal/money"
)

// Direction is the side of a journal entry.
type Direction string

const (
	Debit  Direction = "debit"
	Credit Direction = "credit"
)

// EventType is the business event an entry group records.
type EventType string

const (
	EventCapture    EventType = "capture"
	EventRefund     EventType = "refund"
	EventFee        EventType = "fee"
	EventSettlement EventType = "settlement"
)

// Chart of accounts (deliberately minimal).
//
//	acquirer_receivable      asset     -- money the acquirer owes us
//	merchant_payable:<id>    liability -- money we owe a merchant
//	platform_fee_revenue     revenue   -- fees we earned
//	platform_cash            asset     -- our bank account, credited on payout
const (
	AccountAcquirerReceivable = "acquirer_receivable"
	AccountPlatformFeeRevenue = "platform_fee_revenue"
	AccountPlatformCash       = "platform_cash"

	merchantPayablePrefix = "merchant_payable:"
)

// MerchantPayableAccount is the per-merchant liability account.
func MerchantPayableAccount(merchantID uuid.UUID) string {
	return merchantPayablePrefix + merchantID.String()
}

// IsMerchantPayable reports whether account is a merchant liability account.
func IsMerchantPayable(account string) bool {
	return strings.HasPrefix(account, merchantPayablePrefix)
}

// LedgerEntry is one line of the append-only journal. Amount is always
// positive; the sign lives in Direction.
type LedgerEntry struct {
	ID            int64
	EntryGroupID  uuid.UUID
	TransactionID uuid.UUID
	Account       string
	MerchantID    *uuid.UUID
	Direction     Direction
	Amount        money.Amount
	Currency      money.Currency
	EventType     EventType
	CreatedAt     time.Time
}

// EntryGroup is the set of entries produced by one business event. It is the
// unit of the double-entry invariant: within a group, debits equal credits.
type EntryGroup struct {
	ID      uuid.UUID
	Entries []LedgerEntry
}

// Validate enforces the invariant every group must satisfy before it is
// written: at least two legs, positive amounts, one currency, and balance.
func (g EntryGroup) Validate() error {
	if len(g.Entries) < 2 {
		return fmt.Errorf("%w: group %s has %d entries", ErrUnbalancedEntryGroup, g.ID, len(g.Entries))
	}
	var debits, credits money.Amount
	currency := g.Entries[0].Currency
	for _, e := range g.Entries {
		if !e.Amount.IsPositive() {
			return fmt.Errorf("%w: entry on %s has non-positive amount", ErrInvalidAmount, e.Account)
		}
		if e.Currency != currency {
			return fmt.Errorf("%w: group %s mixes %s and %s", ErrCurrencyMismatch, g.ID, currency, e.Currency)
		}
		switch e.Direction {
		case Debit:
			debits += e.Amount
		case Credit:
			credits += e.Amount
		default:
			return fmt.Errorf("%w: unknown direction %q", ErrValidation, e.Direction)
		}
	}
	if debits != credits {
		return fmt.Errorf("%w: group %s debits=%d credits=%d", ErrUnbalancedEntryGroup, g.ID, debits, credits)
	}
	return nil
}

// Totals returns the summed debits and credits of the group.
func (g EntryGroup) Totals() (debits, credits money.Amount) {
	for _, e := range g.Entries {
		if e.Direction == Debit {
			debits += e.Amount
		} else {
			credits += e.Amount
		}
	}
	return debits, credits
}

// NewCaptureEntryGroup books a capture of amount with a platform fee.
//
//	debit  acquirer_receivable    amount
//	credit merchant_payable:<id>  amount - fee
//	credit platform_fee_revenue   fee
func NewCaptureEntryGroup(t *Transaction, amount, fee money.Amount, now time.Time) (EntryGroup, error) {
	if !amount.IsPositive() {
		return EntryGroup{}, fmt.Errorf("%w: capture amount", ErrInvalidAmount)
	}
	if fee < 0 || fee > amount {
		return EntryGroup{}, fmt.Errorf("%w: fee %d out of range for amount %d", ErrValidation, fee, amount)
	}
	groupID := uuid.New()
	merchantID := t.MerchantID
	entries := []LedgerEntry{
		{
			EntryGroupID:  groupID,
			TransactionID: t.ID,
			Account:       AccountAcquirerReceivable,
			MerchantID:    &merchantID,
			Direction:     Debit,
			Amount:        amount,
			Currency:      t.Currency,
			EventType:     EventCapture,
			CreatedAt:     now,
		},
		{
			EntryGroupID:  groupID,
			TransactionID: t.ID,
			Account:       MerchantPayableAccount(merchantID),
			MerchantID:    &merchantID,
			Direction:     Credit,
			Amount:        amount - fee,
			Currency:      t.Currency,
			EventType:     EventCapture,
			CreatedAt:     now,
		},
	}
	if fee > 0 {
		entries = append(entries, LedgerEntry{
			EntryGroupID:  groupID,
			TransactionID: t.ID,
			Account:       AccountPlatformFeeRevenue,
			MerchantID:    &merchantID,
			Direction:     Credit,
			Amount:        fee,
			Currency:      t.Currency,
			EventType:     EventFee,
			CreatedAt:     now,
		})
	}
	group := EntryGroup{ID: groupID, Entries: entries}
	if err := group.Validate(); err != nil {
		return EntryGroup{}, err
	}
	return group, nil
}

// NewRefundEntryGroup books a refund of amount.
//
//	debit  merchant_payable:<id>  amount
//	credit acquirer_receivable    amount
//
// The platform fee is not returned: the processing already happened. That is a
// deliberate policy choice, and the reason a refund is not a mirror image of a
// capture.
func NewRefundEntryGroup(t *Transaction, amount money.Amount, now time.Time) (EntryGroup, error) {
	if !amount.IsPositive() {
		return EntryGroup{}, fmt.Errorf("%w: refund amount", ErrInvalidAmount)
	}
	groupID := uuid.New()
	merchantID := t.MerchantID
	group := EntryGroup{
		ID: groupID,
		Entries: []LedgerEntry{
			{
				EntryGroupID:  groupID,
				TransactionID: t.ID,
				Account:       MerchantPayableAccount(merchantID),
				MerchantID:    &merchantID,
				Direction:     Debit,
				Amount:        amount,
				Currency:      t.Currency,
				EventType:     EventRefund,
				CreatedAt:     now,
			},
			{
				EntryGroupID:  groupID,
				TransactionID: t.ID,
				Account:       AccountAcquirerReceivable,
				MerchantID:    &merchantID,
				Direction:     Credit,
				Amount:        amount,
				Currency:      t.Currency,
				EventType:     EventRefund,
				CreatedAt:     now,
			},
		},
	}
	if err := group.Validate(); err != nil {
		return EntryGroup{}, err
	}
	return group, nil
}

// NewSettlementEntryGroup books the payout of net to the merchant.
//
//	debit  merchant_payable:<id>  net   (the liability is discharged)
//	credit platform_cash          net   (cash leaves our account)
func NewSettlementEntryGroup(t *Transaction, net money.Amount, now time.Time) (EntryGroup, error) {
	if !net.IsPositive() {
		return EntryGroup{}, fmt.Errorf("%w: settlement amount", ErrInvalidAmount)
	}
	groupID := uuid.New()
	merchantID := t.MerchantID
	group := EntryGroup{
		ID: groupID,
		Entries: []LedgerEntry{
			{
				EntryGroupID:  groupID,
				TransactionID: t.ID,
				Account:       MerchantPayableAccount(merchantID),
				MerchantID:    &merchantID,
				Direction:     Debit,
				Amount:        net,
				Currency:      t.Currency,
				EventType:     EventSettlement,
				CreatedAt:     now,
			},
			{
				EntryGroupID:  groupID,
				TransactionID: t.ID,
				Account:       AccountPlatformCash,
				MerchantID:    &merchantID,
				Direction:     Credit,
				Amount:        net,
				Currency:      t.Currency,
				EventType:     EventSettlement,
				CreatedAt:     now,
			},
		},
	}
	if err := group.Validate(); err != nil {
		return EntryGroup{}, err
	}
	return group, nil
}

// Balance is a merchant balance derived from the journal. There is no stored
// balance column anywhere: the number is always a fold over the entries.
type Balance struct {
	Account  string
	Currency money.Currency
	Debits   money.Amount
	Credits  money.Amount
}

// Available is the liability we still owe the merchant: credits minus debits.
func (b Balance) Available() money.Amount { return b.Credits - b.Debits }

// ComputeBalance folds entries into a balance for one account and currency.
func ComputeBalance(account string, currency money.Currency, entries []LedgerEntry) Balance {
	b := Balance{Account: account, Currency: currency}
	for _, e := range entries {
		if e.Account != account || e.Currency != currency {
			continue
		}
		if e.Direction == Debit {
			b.Debits += e.Amount
		} else {
			b.Credits += e.Amount
		}
	}
	return b
}
