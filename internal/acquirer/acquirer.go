// Package acquirer adapts the gateway to a card processor.
//
// The real thing would be an ISO 8583 or REST link to a bank. Here it is a
// simulator, but the seam is the same: the service layer only knows this
// interface, so swapping in a real processor changes nothing above it.
package acquirer

import (
	"context"

	"github.com/thaivanlam/mini-payment-gateway/internal/money"
)

// Card is raw card data. It never reaches the repository layer, is never
// logged, and is never persisted: only Last4 and Brand survive the call.
type Card struct {
	Number   string
	ExpMonth int
	ExpYear  int
	CVV      string
}

// AuthorizeRequest asks the processor to place a hold on the card.
type AuthorizeRequest struct {
	TransactionID string
	Amount        money.Amount
	Currency      money.Currency
	Card          Card
}

// AuthorizeResponse is the processor answer to a hold.
type AuthorizeResponse struct {
	Ref       string
	CardLast4 string
	CardBrand string
}

// CaptureResponse is the processor answer to a capture.
type CaptureResponse struct {
	Ref string
}

// RefundResponse is the processor answer to a refund.
type RefundResponse struct {
	Ref string
}

// VoidResponse is the processor answer to releasing a hold.
type VoidResponse struct {
	Ref string
}

// Acquirer is the card processing port.
//
// A decline is reported as a *domain.DeclinedError; an infrastructure problem
// (timeout, open circuit) as domain.ErrAcquirerUnavailable. Callers must be
// able to tell those apart: the first is a final answer about the card, the
// second says nothing about whether money moved.
type Acquirer interface {
	Authorize(ctx context.Context, req AuthorizeRequest) (AuthorizeResponse, error)
	Capture(ctx context.Context, ref string, amount money.Amount) (CaptureResponse, error)
	Refund(ctx context.Context, ref string, amount money.Amount) (RefundResponse, error)
	Void(ctx context.Context, ref string) (VoidResponse, error)
}
