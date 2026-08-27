package service

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/thaivanlam/mini-payment-gateway/internal/domain"
	"github.com/thaivanlam/mini-payment-gateway/internal/money"
	"github.com/thaivanlam/mini-payment-gateway/internal/repository"
)

// MerchantSummary is one merchant's line in a settlement report.
type MerchantSummary struct {
	MerchantID       uuid.UUID      `json:"merchant_id"`
	Currency         money.Currency `json:"currency"`
	TransactionCount int            `json:"transaction_count"`
	CapturedTotal    money.Amount   `json:"captured_total"`
	RefundedTotal    money.Amount   `json:"refunded_total"`
	FeeTotal         money.Amount   `json:"fee_total"`
	NetPayout        money.Amount   `json:"net_payout"`

	// Ledger side of the comparison.
	LedgerCaptured money.Amount `json:"ledger_captured"`
	LedgerRefunded money.Amount `json:"ledger_refunded"`
	LedgerFees     money.Amount `json:"ledger_fees"`
	Balanced       bool         `json:"balanced"`
}

// Discrepancy is one mismatch found during reconciliation.
type Discrepancy struct {
	MerchantID uuid.UUID `json:"merchant_id"`
	Field      string    `json:"field"`
	Expected   int64     `json:"expected_from_transactions"`
	Actual     int64     `json:"actual_from_ledger"`
}

// Report is the result of one reconciliation run.
type Report struct {
	Date              string            `json:"date"`
	GeneratedAt       time.Time         `json:"generated_at"`
	TransactionCount  int               `json:"transaction_count"`
	Merchants         []MerchantSummary `json:"merchants"`
	Discrepancies     []Discrepancy     `json:"discrepancies"`
	UnbalancedGroups  []uuid.UUID       `json:"unbalanced_entry_groups"`
	SettledCount      int               `json:"settled_count"`
	SettlementSkipped bool              `json:"settlement_skipped"`
}

// OK reports whether the day reconciled cleanly.
func (r *Report) OK() bool { return len(r.Discrepancies) == 0 && len(r.UnbalancedGroups) == 0 }

// ReconciliationService compares the transaction table against the journal and,
// when they agree, settles the day.
type ReconciliationService struct {
	db           *repository.DB
	transactions *repository.TransactionRepo
	ledger       *repository.LedgerRepo
	webhooks     *repository.WebhookRepo
	feeBPS       int
	reportDir    string
	log          *slog.Logger
	now          func() time.Time
}

// NewReconciliationService builds a ReconciliationService.
func NewReconciliationService(
	db *repository.DB,
	transactions *repository.TransactionRepo,
	ledger *repository.LedgerRepo,
	webhooks *repository.WebhookRepo,
	feeBPS int,
	reportDir string,
	log *slog.Logger,
) *ReconciliationService {
	if log == nil {
		log = slog.Default()
	}
	return &ReconciliationService{
		db:           db,
		transactions: transactions,
		ledger:       ledger,
		webhooks:     webhooks,
		feeBPS:       feeBPS,
		reportDir:    reportDir,
		log:          log,
		now:          time.Now,
	}
}

// Run reconciles one UTC day.
//
// The point of the exercise: the transaction table and the journal are written
// by the same code path, so they should agree -- and a system that never checks
// is a system that finds out months later that they do not. Settlement is only
// allowed to proceed when they do.
func (s *ReconciliationService) Run(ctx context.Context, date time.Time) (*Report, error) {
	from := time.Date(date.Year(), date.Month(), date.Day(), 0, 0, 0, 0, time.UTC)
	to := from.AddDate(0, 0, 1)

	report := &Report{
		Date:        from.Format("2006-01-02"),
		GeneratedAt: s.now().UTC(),
	}

	// 1. The transaction side.
	txns, err := s.transactions.ListCapturedBetween(ctx, s.db.Pool, from, to)
	if err != nil {
		return nil, err
	}
	report.TransactionCount = len(txns)

	expected := map[reconKey]*MerchantSummary{}
	byMerchant := map[reconKey][]*domain.Transaction{}

	for _, t := range txns {
		k := reconKey{t.MerchantID, t.Currency}
		sum, ok := expected[k]
		if !ok {
			sum = &MerchantSummary{MerchantID: t.MerchantID, Currency: t.Currency}
			expected[k] = sum
		}
		fee := money.Fee(t.CapturedAmount, s.feeBPS)
		sum.TransactionCount++
		sum.CapturedTotal += t.CapturedAmount
		sum.RefundedTotal += t.RefundedAmount
		sum.FeeTotal += fee
		sum.NetPayout += t.CapturedAmount - fee - t.RefundedAmount
		byMerchant[k] = append(byMerchant[k], t)
	}

	// 2. The journal side.
	aggregates, err := s.ledger.AggregateByMerchant(ctx, s.db.Pool, from, to)
	if err != nil {
		return nil, err
	}
	for _, a := range aggregates {
		k := reconKey{a.MerchantID, a.Currency}
		sum, ok := expected[k]
		if !ok {
			// Journal activity with no matching captured transaction: a
			// discrepancy in its own right.
			sum = &MerchantSummary{MerchantID: a.MerchantID, Currency: a.Currency}
			expected[k] = sum
		}
		sum.LedgerCaptured = a.Captured
		sum.LedgerRefunded = a.Refunded
		sum.LedgerFees = a.Fees
	}

	// 3. Compare.
	for _, sum := range expected {
		sum.Balanced = true
		checks := []struct {
			field    string
			expected money.Amount
			actual   money.Amount
		}{
			{"captured_total", sum.CapturedTotal, sum.LedgerCaptured},
			{"refunded_total", sum.RefundedTotal, sum.LedgerRefunded},
			{"fee_total", sum.FeeTotal, sum.LedgerFees},
		}
		for _, c := range checks {
			if c.expected != c.actual {
				sum.Balanced = false
				report.Discrepancies = append(report.Discrepancies, Discrepancy{
					MerchantID: sum.MerchantID,
					Field:      c.field,
					Expected:   c.expected.Int64(),
					Actual:     c.actual.Int64(),
				})
			}
		}
		report.Merchants = append(report.Merchants, *sum)
	}
	sort.Slice(report.Merchants, func(i, j int) bool {
		return report.Merchants[i].MerchantID.String() < report.Merchants[j].MerchantID.String()
	})

	// 4. The invariant, over the whole journal and not just today.
	unbalanced, err := s.ledger.FindUnbalancedGroups(ctx, s.db.Pool)
	if err != nil {
		return nil, err
	}
	for _, g := range unbalanced {
		report.UnbalancedGroups = append(report.UnbalancedGroups, g.EntryGroupID)
		s.log.ErrorContext(ctx, "unbalanced ledger entry group",
			"entry_group_id", g.EntryGroupID.String(),
			"debits", g.Debits.Int64(), "credits", g.Credits.Int64())
	}

	if !report.OK() {
		report.SettlementSkipped = true
		for _, d := range report.Discrepancies {
			s.log.ErrorContext(ctx, "reconciliation discrepancy",
				"merchant_id", d.MerchantID.String(),
				"field", d.Field,
				"expected_from_transactions", d.Expected,
				"actual_from_ledger", d.Actual)
		}
		s.logOffendingTransactions(ctx, report, byMerchant)
		if err := s.write(report); err != nil {
			return report, err
		}
		return report, nil
	}

	// 5. Everything agrees: settle.
	settled, err := s.settle(ctx, txns)
	if err != nil {
		return report, err
	}
	report.SettledCount = settled

	if err := s.write(report); err != nil {
		return report, err
	}
	s.log.InfoContext(ctx, "reconciliation complete",
		"date", report.Date,
		"transactions", report.TransactionCount,
		"settled", report.SettledCount)
	return report, nil
}

// settle moves captured transactions to settled and books the payout entries.
func (s *ReconciliationService) settle(ctx context.Context, txns []*domain.Transaction) (int, error) {
	settled := 0
	for _, t := range txns {
		if t.Status != domain.StatusCaptured {
			continue // already settled, or fully refunded
		}
		err := s.db.WithTx(ctx, func(tx pgx.Tx) error {
			fresh, err := s.transactions.GetForUpdate(ctx, tx, t.ID)
			if err != nil {
				return err
			}
			if fresh.Status != domain.StatusCaptured {
				return nil // changed since the scan; leave it for tomorrow
			}
			now := s.now().UTC()
			if err := fresh.Settle(now); err != nil {
				return err
			}
			if err := s.transactions.Update(ctx, tx, fresh); err != nil {
				return err
			}
			net := fresh.NetPayable(s.feeBPS)
			if net > 0 {
				group, err := domain.NewSettlementEntryGroup(fresh, net, now)
				if err != nil {
					return err
				}
				if err := s.ledger.InsertGroup(ctx, tx, group); err != nil {
					return err
				}
			}
			payload, err := BuildEventPayload(domain.EventPaymentSettled, fresh, now)
			if err != nil {
				return err
			}
			return s.webhooks.Create(ctx, tx, domain.NewWebhookDelivery(
				fresh.MerchantID, fresh.ID, domain.EventPaymentSettled, payload, now))
		})
		if err != nil {
			return settled, fmt.Errorf("settle transaction %s: %w", t.ID, err)
		}
		settled++
	}
	return settled, nil
}

// reconKey groups a day's activity by merchant and currency.
type reconKey struct {
	merchant uuid.UUID
	currency money.Currency
}

func (s *ReconciliationService) logOffendingTransactions(ctx context.Context, report *Report, byMerchant map[reconKey][]*domain.Transaction) {
	broken := map[uuid.UUID]bool{}
	for _, d := range report.Discrepancies {
		broken[d.MerchantID] = true
	}
	for k, txns := range byMerchant {
		if !broken[k.merchant] {
			continue
		}
		for _, t := range txns {
			s.log.ErrorContext(ctx, "transaction in unreconciled merchant",
				"transaction_id", t.ID.String(),
				"merchant_id", t.MerchantID.String(),
				"status", string(t.Status),
				"captured_amount", t.CapturedAmount.Int64(),
				"refunded_amount", t.RefundedAmount.Int64())
		}
	}
}

// write exports the report as JSON and CSV.
func (s *ReconciliationService) write(r *Report) error {
	if err := os.MkdirAll(s.reportDir, 0o755); err != nil {
		return fmt.Errorf("create report dir: %w", err)
	}

	jsonPath := filepath.Join(s.reportDir, "settlement-"+r.Date+".json")
	buf, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal report: %w", err)
	}
	if err := os.WriteFile(jsonPath, buf, 0o644); err != nil {
		return fmt.Errorf("write json report: %w", err)
	}

	csvPath := filepath.Join(s.reportDir, "settlement-"+r.Date+".csv")
	f, err := os.Create(csvPath)
	if err != nil {
		return fmt.Errorf("create csv report: %w", err)
	}
	defer f.Close()

	w := csv.NewWriter(f)
	defer w.Flush()

	header := []string{
		"merchant_id", "currency", "transaction_count",
		"captured_total", "refunded_total", "fee_total", "net_payout",
		"ledger_captured", "ledger_refunded", "ledger_fees", "balanced",
	}
	if err := w.Write(header); err != nil {
		return fmt.Errorf("write csv header: %w", err)
	}
	for _, m := range r.Merchants {
		row := []string{
			m.MerchantID.String(),
			m.Currency.String(),
			strconv.Itoa(m.TransactionCount),
			m.CapturedTotal.String(),
			m.RefundedTotal.String(),
			m.FeeTotal.String(),
			m.NetPayout.String(),
			m.LedgerCaptured.String(),
			m.LedgerRefunded.String(),
			m.LedgerFees.String(),
			strconv.FormatBool(m.Balanced),
		}
		if err := w.Write(row); err != nil {
			return fmt.Errorf("write csv row: %w", err)
		}
	}
	s.log.Info("settlement report written", "json", jsonPath, "csv", csvPath)
	return nil
}

// SettlementReport reads back a previously generated report, backing
// GET /reports/settlement.
func (s *ReconciliationService) SettlementReport(ctx context.Context, date time.Time, merchantID uuid.UUID) (*MerchantSummary, error) {
	from := time.Date(date.Year(), date.Month(), date.Day(), 0, 0, 0, 0, time.UTC)
	to := from.AddDate(0, 0, 1)

	txns, err := s.transactions.ListCapturedBetween(ctx, s.db.Pool, from, to)
	if err != nil {
		return nil, err
	}

	summary := &MerchantSummary{MerchantID: merchantID, Balanced: true}
	for _, t := range txns {
		if t.MerchantID != merchantID {
			continue
		}
		if summary.Currency == "" {
			summary.Currency = t.Currency
		}
		fee := money.Fee(t.CapturedAmount, s.feeBPS)
		summary.TransactionCount++
		summary.CapturedTotal += t.CapturedAmount
		summary.RefundedTotal += t.RefundedAmount
		summary.FeeTotal += fee
		summary.NetPayout += t.CapturedAmount - fee - t.RefundedAmount
	}

	aggregates, err := s.ledger.AggregateByMerchant(ctx, s.db.Pool, from, to)
	if err != nil {
		return nil, err
	}
	for _, a := range aggregates {
		if a.MerchantID != merchantID {
			continue
		}
		summary.LedgerCaptured = a.Captured
		summary.LedgerRefunded = a.Refunded
		summary.LedgerFees = a.Fees
	}
	summary.Balanced = summary.CapturedTotal == summary.LedgerCaptured &&
		summary.RefundedTotal == summary.LedgerRefunded &&
		summary.FeeTotal == summary.LedgerFees
	return summary, nil
}
