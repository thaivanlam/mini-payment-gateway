package domain

import (
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/thaivanlam/mini-payment-gateway/internal/money"
)

// Status is a position in the transaction lifecycle.
type Status string

const (
	StatusCreated    Status = "created"
	StatusAuthorized Status = "authorized"
	StatusCaptured   Status = "captured"
	StatusSettled    Status = "settled"
	StatusRefunded   Status = "refunded"
	StatusVoided     Status = "voided"
	StatusFailed     Status = "failed"
)

// AuthorizationValidity is how long an authorization may be captured for.
// Card networks expire holds after roughly a week; we mirror that.
const AuthorizationValidity = 7 * 24 * time.Hour

// allowedTransitions is the single source of truth for the state machine.
//
//	created    -> authorized | failed
//	authorized -> captured | voided | failed
//	captured   -> settled | refunded
//	settled    -> refunded
//	failed, voided, refunded are terminal.
var allowedTransitions = map[Status][]Status{
	StatusCreated:    {StatusAuthorized, StatusFailed},
	StatusAuthorized: {StatusCaptured, StatusVoided, StatusFailed},
	StatusCaptured:   {StatusSettled, StatusRefunded},
	StatusSettled:    {StatusRefunded},
	StatusFailed:     {},
	StatusVoided:     {},
	StatusRefunded:   {},
}

// IsTerminal reports whether no further transition is possible.
func (s Status) IsTerminal() bool { return len(allowedTransitions[s]) == 0 }

// Valid reports whether s is a known status.
func (s Status) Valid() bool {
	_, ok := allowedTransitions[s]
	return ok
}

// Transaction is a single payment through the gateway.
//
// Card data is deliberately absent: only the brand and last four digits are
// kept. The PAN, expiry and CVV live in the request struct, are handed to the
// acquirer, and are dropped -- the database is never in PCI-DSS scope.
type Transaction struct {
	ID             uuid.UUID
	MerchantID     uuid.UUID
	Reference      string
	Amount         money.Amount
	Currency       money.Currency
	Status         Status
	CapturedAmount money.Amount
	RefundedAmount money.Amount
	CardLast4      string
	CardBrand      string
	AcquirerRef    string
	FailureCode    string
	Metadata       map[string]string
	Version        int
	AuthorizedAt   *time.Time
	CapturedAt     *time.Time
	SettledAt      *time.Time
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

// NewTransaction builds a transaction in the created state after validating
// the merchant-supplied fields.
func NewTransaction(
	merchantID uuid.UUID,
	reference string,
	amount money.Amount,
	currency money.Currency,
	metadata map[string]string,
	now time.Time,
) (*Transaction, error) {
	if reference == "" {
		return nil, Invalid("reference", "must not be empty")
	}
	if len(reference) > 128 {
		return nil, Invalid("reference", "must be at most 128 characters")
	}
	if !amount.IsPositive() {
		return nil, Invalid("amount", "must be greater than zero")
	}
	if !currency.Valid() {
		return nil, Invalid("currency", "must be one of VND, USD")
	}
	return &Transaction{
		ID:         uuid.New(),
		MerchantID: merchantID,
		Reference:  reference,
		Amount:     amount,
		Currency:   currency,
		Status:     StatusCreated,
		Metadata:   metadata,
		CreatedAt:  now,
		UpdatedAt:  now,
	}, nil
}

// CanTransitionTo reports whether next is reachable from the current status.
func (t *Transaction) CanTransitionTo(next Status) bool {
	for _, s := range allowedTransitions[t.Status] {
		if s == next {
			return true
		}
	}
	return false
}

// TransitionTo moves the transaction to next, or fails.
//
// Every status change in the codebase goes through this method: there is no
// direct assignment to t.Status anywhere outside this file.
func (t *Transaction) TransitionTo(next Status) error {
	if !t.CanTransitionTo(next) {
		return fmt.Errorf("%w: %s -> %s", ErrInvalidStateTransition, t.Status, next)
	}
	t.Status = next
	return nil
}

// RemainingCapturable is how much of the authorization is still uncaptured.
func (t *Transaction) RemainingCapturable() money.Amount {
	return t.Amount - t.CapturedAmount
}

// RemainingRefundable is how much of the captured money can still be returned.
func (t *Transaction) RemainingRefundable() money.Amount {
	return t.CapturedAmount - t.RefundedAmount
}

// AuthorizationExpiresAt is the deadline for capturing, or nil if the
// transaction was never authorized.
func (t *Transaction) AuthorizationExpiresAt() *time.Time {
	if t.AuthorizedAt == nil {
		return nil
	}
	exp := t.AuthorizedAt.Add(AuthorizationValidity)
	return &exp
}

// AuthorizationExpired reports whether the capture window has closed.
func (t *Transaction) AuthorizationExpired(now time.Time) bool {
	exp := t.AuthorizationExpiresAt()
	return exp != nil && now.After(*exp)
}

// Authorize records a successful acquirer authorization.
func (t *Transaction) Authorize(acquirerRef, cardLast4, cardBrand string, now time.Time) error {
	if err := t.TransitionTo(StatusAuthorized); err != nil {
		return err
	}
	t.AcquirerRef = acquirerRef
	t.CardLast4 = cardLast4
	t.CardBrand = cardBrand
	t.AuthorizedAt = &now
	t.UpdatedAt = now
	return nil
}

// Fail marks the transaction as failed with the acquirer reason code.
func (t *Transaction) Fail(code string, now time.Time) error {
	if err := t.TransitionTo(StatusFailed); err != nil {
		return err
	}
	t.FailureCode = code
	t.UpdatedAt = now
	return nil
}

// Void releases an authorization that will never be captured.
func (t *Transaction) Void(now time.Time) error {
	if t.CapturedAmount > 0 {
		return fmt.Errorf("%w: cannot void a partially captured transaction", ErrInvalidStateTransition)
	}
	if err := t.TransitionTo(StatusVoided); err != nil {
		return err
	}
	t.UpdatedAt = now
	return nil
}

// Capture moves money for part or all of the authorization.
//
// Partial capture is supported: the first capture moves authorized -> captured,
// subsequent ones only grow CapturedAmount while the status stays captured.
func (t *Transaction) Capture(amount money.Amount, now time.Time) error {
	if !amount.IsPositive() {
		return Invalid("amount", "must be greater than zero")
	}
	if t.Status != StatusAuthorized && t.Status != StatusCaptured {
		return fmt.Errorf("%w: cannot capture from %s", ErrInvalidStateTransition, t.Status)
	}
	if t.AuthorizationExpired(now) {
		return ErrAuthorizationExpired
	}
	if amount > t.RemainingCapturable() {
		return fmt.Errorf("%w: %d requested, %d remaining",
			ErrCaptureExceedsAuthlzd, amount, t.RemainingCapturable())
	}
	if t.Status == StatusAuthorized {
		if err := t.TransitionTo(StatusCaptured); err != nil {
			return err
		}
	}
	t.CapturedAmount += amount
	if t.CapturedAt == nil {
		t.CapturedAt = &now
	}
	t.UpdatedAt = now
	return nil
}

// Refund returns part or all of the captured money.
//
// A partial refund leaves the status untouched (captured or settled); only a
// refund that brings RefundedAmount up to CapturedAmount moves the transaction
// into the terminal refunded state. Anything else would make "refunded" mean
// two different things.
func (t *Transaction) Refund(amount money.Amount, now time.Time) error {
	if !amount.IsPositive() {
		return Invalid("amount", "must be greater than zero")
	}
	if t.Status != StatusCaptured && t.Status != StatusSettled {
		return fmt.Errorf("%w: cannot refund from %s", ErrInvalidStateTransition, t.Status)
	}
	if amount > t.RemainingRefundable() {
		return fmt.Errorf("%w: %d requested, %d remaining",
			ErrRefundExceedsCaptured, amount, t.RemainingRefundable())
	}
	t.RefundedAmount += amount
	if t.RefundedAmount == t.CapturedAmount {
		if err := t.TransitionTo(StatusRefunded); err != nil {
			return err
		}
	}
	t.UpdatedAt = now
	return nil
}

// Settle marks a captured transaction as paid out, after reconciliation proved
// the ledger and the transaction table agree.
func (t *Transaction) Settle(now time.Time) error {
	if err := t.TransitionTo(StatusSettled); err != nil {
		return err
	}
	t.SettledAt = &now
	t.UpdatedAt = now
	return nil
}

// NetPayable is what the merchant is owed for this transaction at fee rate
// bps: captured minus the fee on the captured amount, minus refunds.
func (t *Transaction) NetPayable(feeBPS int) money.Amount {
	return t.CapturedAmount - money.Fee(t.CapturedAmount, feeBPS) - t.RefundedAmount
}
