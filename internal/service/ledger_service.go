package service

import (
	"context"

	"github.com/google/uuid"

	"github.com/thaivanlam/mini-payment-gateway/internal/domain"
	"github.com/thaivanlam/mini-payment-gateway/internal/money"
	"github.com/thaivanlam/mini-payment-gateway/internal/repository"
)

// LedgerService exposes the journal to merchants.
type LedgerService struct {
	db     *repository.DB
	ledger *repository.LedgerRepo
}

// NewLedgerService builds a LedgerService.
func NewLedgerService(db *repository.DB, ledger *repository.LedgerRepo) *LedgerService {
	return &LedgerService{db: db, ledger: ledger}
}

// Balance returns what the platform owes a merchant, derived from the journal.
func (s *LedgerService) Balance(ctx context.Context, merchantID uuid.UUID, currency money.Currency) (domain.Balance, error) {
	if currency == "" {
		currency = money.VND
	}
	if !currency.Valid() {
		return domain.Balance{}, domain.Invalid("currency", "must be one of VND, USD")
	}
	return s.ledger.Balance(ctx, s.db.Pool, merchantID, currency)
}

// Entries returns a page of a merchant's journal lines.
func (s *LedgerService) Entries(ctx context.Context, f repository.EntryFilter) ([]domain.LedgerEntry, error) {
	return s.ledger.ListEntries(ctx, s.db.Pool, f)
}

// CheckInvariant returns every entry group whose debits and credits disagree.
// An empty result is the property the whole ledger design exists to guarantee.
func (s *LedgerService) CheckInvariant(ctx context.Context) ([]repository.UnbalancedGroup, error) {
	return s.ledger.FindUnbalancedGroups(ctx, s.db.Pool)
}
