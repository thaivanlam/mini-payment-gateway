package http

import (
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/thaivanlam/mini-payment-gateway/internal/domain"
	"github.com/thaivanlam/mini-payment-gateway/internal/repository"
	"github.com/thaivanlam/mini-payment-gateway/internal/service"
)

// PaymentHandler serves the payment endpoints.
type PaymentHandler struct {
	payments *service.PaymentService
}

// NewPaymentHandler builds a PaymentHandler.
func NewPaymentHandler(payments *service.PaymentService) *PaymentHandler {
	return &PaymentHandler{payments: payments}
}

// CardResponse is all the card data that survives a request.
type CardResponse struct {
	Last4 string `json:"last4,omitempty"`
	Brand string `json:"brand,omitempty"`
}

// PaymentResponse is the public view of a transaction.
type PaymentResponse struct {
	ID             string            `json:"id"`
	Reference      string            `json:"reference"`
	Amount         int64             `json:"amount"`
	Currency       string            `json:"currency"`
	Status         string            `json:"status"`
	CapturedAmount int64             `json:"captured_amount"`
	RefundedAmount int64             `json:"refunded_amount"`
	Card           CardResponse      `json:"card"`
	FailureCode    string            `json:"failure_code,omitempty"`
	Metadata       map[string]string `json:"metadata,omitempty"`
	AuthorizedAt   *time.Time        `json:"authorized_at,omitempty"`
	CapturedAt     *time.Time        `json:"captured_at,omitempty"`
	SettledAt      *time.Time        `json:"settled_at,omitempty"`
	ExpiresAt      *time.Time        `json:"authorization_expires_at,omitempty"`
	CreatedAt      time.Time         `json:"created_at"`
}

// PaymentListResponse is one page of transactions.
type PaymentListResponse struct {
	Data       []PaymentResponse `json:"data"`
	HasMore    bool              `json:"has_more"`
	NextCursor string            `json:"next_cursor,omitempty"`
}

func toPaymentResponse(t *domain.Transaction) PaymentResponse {
	return PaymentResponse{
		ID:             t.ID.String(),
		Reference:      t.Reference,
		Amount:         t.Amount.Int64(),
		Currency:       t.Currency.String(),
		Status:         string(t.Status),
		CapturedAmount: t.CapturedAmount.Int64(),
		RefundedAmount: t.RefundedAmount.Int64(),
		Card:           CardResponse{Last4: t.CardLast4, Brand: t.CardBrand},
		FailureCode:    t.FailureCode,
		Metadata:       t.Metadata,
		AuthorizedAt:   t.AuthorizedAt,
		CapturedAt:     t.CapturedAt,
		SettledAt:      t.SettledAt,
		ExpiresAt:      t.AuthorizationExpiresAt(),
		CreatedAt:      t.CreatedAt,
	}
}

// Create handles POST /api/v1/payments.
func (h *PaymentHandler) Create(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	merchant := MerchantFrom(ctx)

	var req CreatePaymentRequest
	if err := decodeJSON(w, r, &req); err != nil {
		writeError(ctx, w, err)
		return
	}
	input, err := req.ToInput(merchant.ID)
	if err != nil {
		writeError(ctx, w, err)
		return
	}

	txn, err := h.payments.Authorize(ctx, input)
	if err != nil {
		writeError(ctx, w, err)
		return
	}
	writeJSON(w, http.StatusCreated, toPaymentResponse(txn))
}

// Get handles GET /api/v1/payments/{id}.
func (h *PaymentHandler) Get(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	merchant := MerchantFrom(ctx)

	id, err := pathUUID(chi.URLParam(r, "id"), "id")
	if err != nil {
		writeError(ctx, w, err)
		return
	}
	txn, err := h.payments.Get(ctx, merchant.ID, id)
	if err != nil {
		writeError(ctx, w, err)
		return
	}
	writeJSON(w, http.StatusOK, toPaymentResponse(txn))
}

// List handles GET /api/v1/payments.
func (h *PaymentHandler) List(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	merchant := MerchantFrom(ctx)
	q := r.URL.Query()

	filter := repository.ListFilter{MerchantID: merchant.ID}

	if raw := q.Get("status"); raw != "" {
		status := domain.Status(raw)
		if !status.Valid() {
			writeError(ctx, w, domain.Invalid("status", "is not a known transaction status"))
			return
		}
		filter.Status = &status
	}

	var err error
	if filter.From, err = queryTime(q.Get("from"), "from"); err != nil {
		writeError(ctx, w, err)
		return
	}
	if filter.To, err = queryTime(q.Get("to"), "to"); err != nil {
		writeError(ctx, w, err)
		return
	}
	if filter.CursorTime, filter.CursorID, err = decodeCursor(q.Get("cursor")); err != nil {
		writeError(ctx, w, err)
		return
	}
	limit, err := queryLimit(q.Get("limit"), 25, 100)
	if err != nil {
		writeError(ctx, w, err)
		return
	}
	// Fetch one extra row to learn whether another page exists, without a
	// second COUNT query over the whole table.
	filter.Limit = limit + 1

	txns, err := h.payments.List(ctx, filter)
	if err != nil {
		writeError(ctx, w, err)
		return
	}

	resp := PaymentListResponse{Data: []PaymentResponse{}}
	if len(txns) > limit {
		resp.HasMore = true
		txns = txns[:limit]
	}
	for _, t := range txns {
		resp.Data = append(resp.Data, toPaymentResponse(t))
	}
	if resp.HasMore && len(txns) > 0 {
		last := txns[len(txns)-1]
		resp.NextCursor = encodeCursor(last.CreatedAt, last.ID)
	}
	writeJSON(w, http.StatusOK, resp)
}

// Capture handles POST /api/v1/payments/{id}/capture.
func (h *PaymentHandler) Capture(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	merchant := MerchantFrom(ctx)

	id, err := pathUUID(chi.URLParam(r, "id"), "id")
	if err != nil {
		writeError(ctx, w, err)
		return
	}
	var req AmountRequest
	if err := decodeOptionalJSON(w, r, &req); err != nil {
		writeError(ctx, w, err)
		return
	}
	amount, err := req.Validate()
	if err != nil {
		writeError(ctx, w, err)
		return
	}

	txn, err := h.payments.Capture(ctx, merchant.ID, id, amount)
	if err != nil {
		writeError(ctx, w, err)
		return
	}
	writeJSON(w, http.StatusOK, toPaymentResponse(txn))
}

// Refund handles POST /api/v1/payments/{id}/refund.
func (h *PaymentHandler) Refund(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	merchant := MerchantFrom(ctx)

	id, err := pathUUID(chi.URLParam(r, "id"), "id")
	if err != nil {
		writeError(ctx, w, err)
		return
	}
	var req AmountRequest
	if err := decodeOptionalJSON(w, r, &req); err != nil {
		writeError(ctx, w, err)
		return
	}
	amount, err := req.Validate()
	if err != nil {
		writeError(ctx, w, err)
		return
	}

	txn, err := h.payments.Refund(ctx, merchant.ID, id, amount)
	if err != nil {
		writeError(ctx, w, err)
		return
	}
	writeJSON(w, http.StatusOK, toPaymentResponse(txn))
}

// Void handles POST /api/v1/payments/{id}/void.
func (h *PaymentHandler) Void(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	merchant := MerchantFrom(ctx)

	id, err := pathUUID(chi.URLParam(r, "id"), "id")
	if err != nil {
		writeError(ctx, w, err)
		return
	}
	txn, err := h.payments.Void(ctx, merchant.ID, id)
	if err != nil {
		writeError(ctx, w, err)
		return
	}
	writeJSON(w, http.StatusOK, toPaymentResponse(txn))
}
