package domain

import (
	"errors"
	"fmt"
)

// Sentinel errors of the domain. Everything above the domain layer inspects
// these with errors.Is / errors.As; the HTTP layer owns the single place where
// they are mapped to status codes (transport/http/response.go).
var (
	ErrNotFound               = errors.New("resource not found")
	ErrValidation             = errors.New("validation failed")
	ErrInvalidAmount          = errors.New("amount must be positive")
	ErrCurrencyMismatch       = errors.New("currency mismatch")
	ErrUnsupportedCurrency    = errors.New("unsupported currency")
	ErrInvalidStateTransition = errors.New("invalid state transition")
	ErrCaptureExceedsAuthlzd  = errors.New("capture exceeds authorized amount")
	ErrRefundExceedsCaptured  = errors.New("refund exceeds captured amount")
	ErrAuthorizationExpired   = errors.New("authorization has expired")
	ErrConcurrentModification = errors.New("transaction was modified concurrently")
	ErrDuplicateReference     = errors.New("reference already used by this merchant")
	ErrMerchantSuspended      = errors.New("merchant is suspended")
	ErrUnauthenticated        = errors.New("authentication failed")
	ErrUnbalancedEntryGroup   = errors.New("ledger entry group does not balance")
	ErrAcquirerUnavailable    = errors.New("acquirer unavailable")
	ErrRateLimited            = errors.New("rate limit exceeded")
)

// DeclineCode is the reason an acquirer refused a card operation.
type DeclineCode string

const (
	DeclineInsufficientFunds DeclineCode = "insufficient_funds"
	DeclineCardExpired       DeclineCode = "card_expired"
	DeclineDoNotHonor        DeclineCode = "do_not_honor"
	DeclineFraudSuspected    DeclineCode = "fraud_suspected"
)

// DeclinedError is returned when the acquirer refused the operation. It is a
// card error, not a bug: it maps to HTTP 402 and is surfaced to the merchant
// with the acquirer's reason code.
type DeclinedError struct {
	Code    DeclineCode
	Message string
}

func (e *DeclinedError) Error() string {
	return fmt.Sprintf("card declined: %s", e.Code)
}

// NewDeclinedError builds a DeclinedError with the human message that belongs
// to the code.
func NewDeclinedError(code DeclineCode) *DeclinedError {
	msg, ok := declineMessages[code]
	if !ok {
		msg = "The card was declined."
	}
	return &DeclinedError{Code: code, Message: msg}
}

var declineMessages = map[DeclineCode]string{
	DeclineInsufficientFunds: "The card has insufficient funds.",
	DeclineCardExpired:       "The card has expired.",
	DeclineDoNotHonor:        "The issuer declined the transaction (do not honor).",
	DeclineFraudSuspected:    "The transaction was declined as suspected fraud.",
}

// ValidationError carries the field that failed validation so the API can
// answer with something more useful than "bad request".
type ValidationError struct {
	Field   string
	Message string
}

func (e *ValidationError) Error() string {
	return fmt.Sprintf("%s: %s", e.Field, e.Message)
}

// Unwrap lets errors.Is(err, ErrValidation) match any ValidationError.
func (e *ValidationError) Unwrap() error { return ErrValidation }

// Invalid is a shorthand constructor for ValidationError.
func Invalid(field, message string) error {
	return &ValidationError{Field: field, Message: message}
}
