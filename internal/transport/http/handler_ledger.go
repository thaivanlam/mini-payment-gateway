package http

import (
	"net/http"
	"time"

	"github.com/thaivanlam/mini-payment-gateway/internal/domain"
	"github.com/thaivanlam/mini-payment-gateway/internal/money"
	"github.com/thaivanlam/mini-payment-gateway/internal/repository"
	"github.com/thaivanlam/mini-payment-gateway/internal/service"
)

// LedgerHandler serves the ledger and reporting endpoints.
type LedgerHandler struct {
	ledger *service.LedgerService
	recon  *service.ReconciliationService
}

// NewLedgerHandler builds a LedgerHandler.
func NewLedgerHandler(ledger *service.LedgerService, recon *service.ReconciliationService) *LedgerHandler {
	return &LedgerHandler{ledger: ledger, recon: recon}
}

// LedgerEntryResponse is the public view of one journal line.
type LedgerEntryResponse struct {
	ID            int64     `json:"id"`
	EntryGroupID  string    `json:"entry_group_id"`
	TransactionID string    `json:"transaction_id"`
	Account       string    `json:"account"`
	Direction     string    `json:"direction"`
	Amount        int64     `json:"amount"`
	Currency      string    `json:"currency"`
	EventType     string    `json:"event_type"`
	CreatedAt     time.Time `json:"created_at"`
}

// LedgerListResponse is one page of journal lines.
type LedgerListResponse struct {
	Data       []LedgerEntryResponse `json:"data"`
	HasMore    bool                  `json:"has_more"`
	NextCursor string                `json:"next_cursor,omitempty"`
}

// BalanceResponse is a merchant balance derived from the journal.
type BalanceResponse struct {
	Account   string `json:"account"`
	Currency  string `json:"currency"`
	Debits    int64  `json:"total_debits"`
	Credits   int64  `json:"total_credits"`
	Available int64  `json:"available"`
}

// Entries handles GET /api/v1/ledger/entries.
func (h *LedgerHandler) Entries(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	merchant := MerchantFrom(ctx)
	q := r.URL.Query()

	filter := repository.EntryFilter{
		MerchantID: merchant.ID,
		Account:    q.Get("account"),
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
	if filter.CursorID, err = decodeLedgerCursor(q.Get("cursor")); err != nil {
		writeError(ctx, w, err)
		return
	}
	limit, err := queryLimit(q.Get("limit"), 50, 200)
	if err != nil {
		writeError(ctx, w, err)
		return
	}
	filter.Limit = limit + 1

	entries, err := h.ledger.Entries(ctx, filter)
	if err != nil {
		writeError(ctx, w, err)
		return
	}

	resp := LedgerListResponse{Data: []LedgerEntryResponse{}}
	if len(entries) > limit {
		resp.HasMore = true
		entries = entries[:limit]
	}
	for _, e := range entries {
		resp.Data = append(resp.Data, LedgerEntryResponse{
			ID:            e.ID,
			EntryGroupID:  e.EntryGroupID.String(),
			TransactionID: e.TransactionID.String(),
			Account:       e.Account,
			Direction:     string(e.Direction),
			Amount:        e.Amount.Int64(),
			Currency:      e.Currency.String(),
			EventType:     string(e.EventType),
			CreatedAt:     e.CreatedAt,
		})
	}
	if resp.HasMore && len(entries) > 0 {
		resp.NextCursor = encodeLedgerCursor(entries[len(entries)-1].ID)
	}
	writeJSON(w, http.StatusOK, resp)
}

// Balance handles GET /api/v1/ledger/balance.
func (h *LedgerHandler) Balance(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	merchant := MerchantFrom(ctx)

	currency := money.Currency(r.URL.Query().Get("currency"))
	balance, err := h.ledger.Balance(ctx, merchant.ID, currency)
	if err != nil {
		writeError(ctx, w, err)
		return
	}
	writeJSON(w, http.StatusOK, BalanceResponse{
		Account:   balance.Account,
		Currency:  balance.Currency.String(),
		Debits:    balance.Debits.Int64(),
		Credits:   balance.Credits.Int64(),
		Available: balance.Available().Int64(),
	})
}

// SettlementReportResponse is the day's settlement summary for one merchant.
type SettlementReportResponse struct {
	Date             string `json:"date"`
	MerchantID       string `json:"merchant_id"`
	Currency         string `json:"currency"`
	TransactionCount int    `json:"transaction_count"`
	CapturedTotal    int64  `json:"captured_total"`
	RefundedTotal    int64  `json:"refunded_total"`
	FeeTotal         int64  `json:"fee_total"`
	NetPayout        int64  `json:"net_payout"`
	LedgerCaptured   int64  `json:"ledger_captured"`
	LedgerRefunded   int64  `json:"ledger_refunded"`
	LedgerFees       int64  `json:"ledger_fees"`
	Balanced         bool   `json:"balanced"`
}

// Settlement handles GET /api/v1/reports/settlement?date=YYYY-MM-DD.
func (h *LedgerHandler) Settlement(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	merchant := MerchantFrom(ctx)

	raw := r.URL.Query().Get("date")
	if raw == "" {
		raw = time.Now().UTC().Format("2006-01-02")
	}
	date, err := time.Parse("2006-01-02", raw)
	if err != nil {
		writeError(ctx, w, domain.Invalid("date", "must be YYYY-MM-DD"))
		return
	}

	summary, err := h.recon.SettlementReport(ctx, date, merchant.ID)
	if err != nil {
		writeError(ctx, w, err)
		return
	}
	writeJSON(w, http.StatusOK, SettlementReportResponse{
		Date:             date.Format("2006-01-02"),
		MerchantID:       merchant.ID.String(),
		Currency:         summary.Currency.String(),
		TransactionCount: summary.TransactionCount,
		CapturedTotal:    summary.CapturedTotal.Int64(),
		RefundedTotal:    summary.RefundedTotal.Int64(),
		FeeTotal:         summary.FeeTotal.Int64(),
		NetPayout:        summary.NetPayout.Int64(),
		LedgerCaptured:   summary.LedgerCaptured.Int64(),
		LedgerRefunded:   summary.LedgerRefunded.Int64(),
		LedgerFees:       summary.LedgerFees.Int64(),
		Balanced:         summary.Balanced,
	})
}
