package http

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/thaivanlam/mini-payment-gateway/internal/acquirer"
	"github.com/thaivanlam/mini-payment-gateway/internal/domain"
	"github.com/thaivanlam/mini-payment-gateway/internal/money"
	"github.com/thaivanlam/mini-payment-gateway/internal/service"
)

// maxBodyBytes caps request bodies. Payment payloads are small; anything larger
// is a mistake or an attack, and reading it would be free memory for the caller.
const maxBodyBytes = 64 << 10

// decodeJSON reads and validates a JSON body.
func decodeJSON(w http.ResponseWriter, r *http.Request, dst any) error {
	r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()

	if err := dec.Decode(dst); err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			return domain.Invalid("body", "request body is too large")
		}
		var syntaxErr *json.SyntaxError
		if errors.As(err, &syntaxErr) {
			return domain.Invalid("body", fmt.Sprintf("malformed JSON at byte %d", syntaxErr.Offset))
		}
		var typeErr *json.UnmarshalTypeError
		if errors.As(err, &typeErr) {
			return domain.Invalid(typeErr.Field, "has the wrong type")
		}
		if errors.Is(err, io.EOF) {
			return domain.Invalid("body", "request body is empty")
		}
		return domain.Invalid("body", strings.TrimPrefix(err.Error(), "json: "))
	}
	if dec.More() {
		return domain.Invalid("body", "request body must contain a single JSON object")
	}
	return nil
}

// CreateMerchantRequest is the admin payload for POST /merchants.
type CreateMerchantRequest struct {
	Name       string `json:"name"`
	Email      string `json:"email"`
	WebhookURL string `json:"webhook_url"`
}

// CardRequest is card data supplied by the merchant. It exists only for the
// lifetime of the request.
type CardRequest struct {
	Number   string `json:"number"`
	ExpMonth int    `json:"exp_month"`
	ExpYear  int    `json:"exp_year"`
	CVV      string `json:"cvv"`
}

// CreatePaymentRequest is the payload of POST /payments.
type CreatePaymentRequest struct {
	Reference string            `json:"reference"`
	Amount    int64             `json:"amount"`
	Currency  string            `json:"currency"`
	Card      CardRequest       `json:"card"`
	Capture   bool              `json:"capture"`
	Metadata  map[string]string `json:"metadata"`
}

// ToInput validates and converts the request into a service input.
func (r CreatePaymentRequest) ToInput(merchantID uuid.UUID) (service.AuthorizeInput, error) {
	if r.Amount <= 0 {
		return service.AuthorizeInput{}, domain.Invalid("amount", "must be greater than zero")
	}
	currency := money.Currency(strings.ToUpper(r.Currency))
	if !currency.Valid() {
		return service.AuthorizeInput{}, domain.Invalid("currency", "must be one of VND, USD")
	}
	if r.Card.Number == "" {
		return service.AuthorizeInput{}, domain.Invalid("card.number", "must not be empty")
	}
	for k, v := range r.Metadata {
		if len(k) > 40 || len(v) > 500 {
			return service.AuthorizeInput{}, domain.Invalid("metadata", "keys are limited to 40 and values to 500 characters")
		}
	}
	return service.AuthorizeInput{
		MerchantID: merchantID,
		Reference:  strings.TrimSpace(r.Reference),
		Amount:     money.Amount(r.Amount),
		Currency:   currency,
		Card: acquirer.Card{
			Number:   strings.TrimSpace(r.Card.Number),
			ExpMonth: r.Card.ExpMonth,
			ExpYear:  r.Card.ExpYear,
			CVV:      r.Card.CVV,
		},
		Capture:  r.Capture,
		Metadata: r.Metadata,
	}, nil
}

// AmountRequest is the payload of capture and refund, where an absent or zero
// amount means "the whole remaining amount".
type AmountRequest struct {
	Amount int64 `json:"amount"`
}

// Validate checks the amount is not negative.
func (r AmountRequest) Validate() (money.Amount, error) {
	if r.Amount < 0 {
		return 0, domain.Invalid("amount", "must not be negative")
	}
	return money.Amount(r.Amount), nil
}

// decodeOptionalJSON accepts an empty body as a zero-valued struct, so
// `POST /payments/{id}/capture` with no body means "capture everything".
func decodeOptionalJSON(w http.ResponseWriter, r *http.Request, dst any) error {
	if r.ContentLength == 0 {
		return nil
	}
	if err := decodeJSON(w, r, dst); err != nil {
		var invalid *domain.ValidationError
		if errors.As(err, &invalid) && invalid.Message == "request body is empty" {
			return nil
		}
		return err
	}
	return nil
}

// pathUUID parses a UUID path parameter.
func pathUUID(raw, field string) (uuid.UUID, error) {
	id, err := uuid.Parse(raw)
	if err != nil {
		return uuid.Nil, domain.Invalid(field, "must be a UUID")
	}
	return id, nil
}

// queryTime parses an RFC3339 query parameter.
func queryTime(raw, field string) (*time.Time, error) {
	if raw == "" {
		return nil, nil
	}
	t, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		return nil, domain.Invalid(field, "must be an RFC3339 timestamp")
	}
	return &t, nil
}

// queryLimit parses a page size, clamped to a sane maximum.
func queryLimit(raw string, def, max int) (int, error) {
	if raw == "" {
		return def, nil
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 1 {
		return 0, domain.Invalid("limit", "must be a positive integer")
	}
	if n > max {
		n = max
	}
	return n, nil
}

// Cursor pagination helpers.
//
// The cursor is opaque to clients on purpose: it is base64 of "<rfc3339nano>|<uuid>",
// so the server can change the sort key later without breaking anyone who
// stored a cursor.

func encodeCursor(ts time.Time, id uuid.UUID) string {
	return base64.RawURLEncoding.EncodeToString([]byte(ts.UTC().Format(time.RFC3339Nano) + "|" + id.String()))
}

func decodeCursor(raw string) (*time.Time, *uuid.UUID, error) {
	if raw == "" {
		return nil, nil, nil
	}
	decoded, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil {
		return nil, nil, domain.Invalid("cursor", "is not a valid cursor")
	}
	tsRaw, idRaw, found := strings.Cut(string(decoded), "|")
	if !found {
		return nil, nil, domain.Invalid("cursor", "is not a valid cursor")
	}
	ts, err := time.Parse(time.RFC3339Nano, tsRaw)
	if err != nil {
		return nil, nil, domain.Invalid("cursor", "is not a valid cursor")
	}
	id, err := uuid.Parse(idRaw)
	if err != nil {
		return nil, nil, domain.Invalid("cursor", "is not a valid cursor")
	}
	return &ts, &id, nil
}

func encodeLedgerCursor(id int64) string {
	return base64.RawURLEncoding.EncodeToString([]byte(strconv.FormatInt(id, 10)))
}

func decodeLedgerCursor(raw string) (*int64, error) {
	if raw == "" {
		return nil, nil
	}
	decoded, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil {
		return nil, domain.Invalid("cursor", "is not a valid cursor")
	}
	id, err := strconv.ParseInt(string(decoded), 10, 64)
	if err != nil {
		return nil, domain.Invalid("cursor", "is not a valid cursor")
	}
	return &id, nil
}
