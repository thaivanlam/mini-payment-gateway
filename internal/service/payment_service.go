package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/thaivanlam/mini-payment-gateway/internal/acquirer"
	"github.com/thaivanlam/mini-payment-gateway/internal/domain"
	"github.com/thaivanlam/mini-payment-gateway/internal/money"
	"github.com/thaivanlam/mini-payment-gateway/internal/repository"
)

// PaymentService drives the authorize -> capture -> refund lifecycle.
type PaymentService struct {
	db           *repository.DB
	transactions *repository.TransactionRepo
	ledger       *repository.LedgerRepo
	webhooks     *repository.WebhookRepo
	acq          acquirer.Acquirer
	feeBPS       int
	log          *slog.Logger
	now          func() time.Time
}

// NewPaymentService builds a PaymentService.
func NewPaymentService(
	db *repository.DB,
	transactions *repository.TransactionRepo,
	ledger *repository.LedgerRepo,
	webhooks *repository.WebhookRepo,
	acq acquirer.Acquirer,
	feeBPS int,
	log *slog.Logger,
) *PaymentService {
	if log == nil {
		log = slog.Default()
	}
	return &PaymentService{
		db:           db,
		transactions: transactions,
		ledger:       ledger,
		webhooks:     webhooks,
		acq:          acq,
		feeBPS:       feeBPS,
		log:          log,
		now:          time.Now,
	}
}

// AuthorizeInput is a create-payment request.
type AuthorizeInput struct {
	MerchantID uuid.UUID
	Reference  string
	Amount     money.Amount
	Currency   money.Currency
	Card       acquirer.Card
	Capture    bool
	Metadata   map[string]string
}

// Authorize creates a transaction and places a hold on the card.
//
// Order of operations, and why: the row is inserted first so the unique
// (merchant_id, reference) constraint rejects a duplicate order before any
// money is touched; the acquirer is then called with no lock and no open
// database transaction; only the write-back takes a transaction.
func (s *PaymentService) Authorize(ctx context.Context, in AuthorizeInput) (*domain.Transaction, error) {
	now := s.now().UTC()

	txn, err := domain.NewTransaction(in.MerchantID, in.Reference, in.Amount, in.Currency, in.Metadata, now)
	if err != nil {
		return nil, err
	}

	if err := s.transactions.Create(ctx, s.db.Pool, txn); err != nil {
		if !errors.Is(err, domain.ErrDuplicateReference) {
			return nil, err
		}
		// The reference exists. If the previous attempt never got an answer
		// from the acquirer it is still `created`, and this is a retry of an
		// unfinished authorization rather than a duplicate order: resume it.
		existing, getErr := s.transactions.GetByReference(ctx, s.db.Pool, in.MerchantID, in.Reference)
		if getErr != nil {
			return nil, err
		}
		if existing.Status != domain.StatusCreated {
			return nil, err
		}
		if existing.Amount != in.Amount || existing.Currency != in.Currency {
			return nil, err
		}
		txn = existing
		s.log.InfoContext(ctx, "resuming unfinished authorization",
			"transaction_id", txn.ID.String(), "reference", txn.Reference)
	}

	// --- outside any database transaction: no lock is held across this I/O ---
	authRes, acqErr := s.acq.Authorize(ctx, acquirer.AuthorizeRequest{
		TransactionID: txn.ID.String(),
		Amount:        txn.Amount,
		Currency:      txn.Currency,
		Card:          in.Card,
	})

	var declined *domain.DeclinedError
	switch {
	case acqErr == nil:
		// fall through to the authorized write-back below

	case errors.As(acqErr, &declined):
		if err := s.finalizeFailure(ctx, txn, string(declined.Code)); err != nil {
			return nil, err
		}
		return nil, acqErr

	case errors.Is(acqErr, domain.ErrAcquirerUnavailable):
		// The outcome is unknown: the hold may or may not exist. The row stays
		// `created` so a retry can resume it and the reconciliation job can
		// see it, instead of us asserting a failure that may be false.
		s.log.ErrorContext(ctx, "acquirer authorization outcome unknown",
			"transaction_id", txn.ID.String(), "error", acqErr)
		return nil, acqErr

	default:
		// Validation errors (bad card number, bad amount): a definite no.
		if errors.Is(acqErr, domain.ErrValidation) || errors.Is(acqErr, domain.ErrInvalidAmount) {
			return nil, acqErr
		}
		return nil, fmt.Errorf("acquirer authorize: %w", acqErr)
	}

	err = s.db.WithTx(ctx, func(tx pgx.Tx) error {
		fresh, err := s.transactions.GetForUpdate(ctx, tx, txn.ID)
		if err != nil {
			return err
		}
		if fresh.Status != domain.StatusCreated {
			// Someone finished this transaction while we were on the network.
			return fmt.Errorf("%w: status is %s", domain.ErrConcurrentModification, fresh.Status)
		}
		if err := fresh.Authorize(authRes.Ref, authRes.CardLast4, authRes.CardBrand, s.now().UTC()); err != nil {
			return err
		}
		if err := s.transactions.Update(ctx, tx, fresh); err != nil {
			return err
		}
		if err := s.queueWebhook(ctx, tx, fresh, domain.EventPaymentAuthorized); err != nil {
			return err
		}
		txn = fresh
		return nil
	})
	if err != nil {
		return nil, err
	}

	s.log.InfoContext(ctx, "payment authorized",
		"transaction_id", txn.ID.String(),
		"merchant_id", txn.MerchantID.String(),
		"amount", txn.Amount.Int64(),
		"currency", txn.Currency.String())

	if in.Capture {
		captured, err := s.Capture(ctx, txn.MerchantID, txn.ID, txn.Amount)
		if err != nil {
			// The authorization stands and is returned to the caller; the
			// capture can be retried on its own.
			s.log.WarnContext(ctx, "auto-capture after authorization failed",
				"transaction_id", txn.ID.String(), "error", err)
			return txn, err
		}
		return captured, nil
	}
	return txn, nil
}

// Capture moves money for all or part of an authorization.
//
// amount == 0 means "capture everything still uncaptured".
//
// This is the method the concurrency test exercises. Two concurrent captures
// both pass the pre-check and both call the acquirer, but only one can hold the
// row lock; the second re-reads the row inside its transaction, sees the
// capture already happened, and fails the domain check. Exactly one entry group
// is ever written.
func (s *PaymentService) Capture(ctx context.Context, merchantID, txID uuid.UUID, amount money.Amount) (*domain.Transaction, error) {
	// 1. Cheap pre-check, so an obviously invalid request never reaches the
	//    acquirer. It is not authoritative: the check that counts is inside
	//    the transaction below.
	txn, err := s.transactions.GetByMerchantAndID(ctx, s.db.Pool, merchantID, txID)
	if err != nil {
		return nil, err
	}
	if amount == 0 {
		amount = txn.RemainingCapturable()
	}
	preview := *txn
	if err := preview.Capture(amount, s.now().UTC()); err != nil {
		return nil, err
	}

	// 2. Network I/O with no lock held and no transaction open. Doing this
	//    inside the transaction would hold the row lock for the full acquirer
	//    latency (up to 3s here), serialising unrelated work behind it and
	//    burning a pooled connection for the duration.
	capRes, err := s.acq.Capture(ctx, txn.AcquirerRef, amount)
	if err != nil {
		return nil, err
	}

	// 3. Short transaction: lock, re-validate against the fresh row, write.
	var out *domain.Transaction
	err = s.db.WithTx(ctx, func(tx pgx.Tx) error {
		fresh, err := s.transactions.GetForUpdate(ctx, tx, txID)
		if err != nil {
			return err
		}
		if fresh.MerchantID != merchantID {
			return domain.ErrNotFound
		}
		now := s.now().UTC()
		// The double-check: the same domain rules, applied to the row as it is
		// now rather than as it was before the network call.
		if err := fresh.Capture(amount, now); err != nil {
			s.log.ErrorContext(ctx, "acquirer capture succeeded but transaction moved on; manual reconciliation required",
				"transaction_id", txID.String(),
				"acquirer_ref", capRes.Ref,
				"amount", amount.Int64(),
				"status", string(fresh.Status),
				"error", err)
			return err
		}
		if err := s.transactions.Update(ctx, tx, fresh); err != nil {
			return err
		}

		fee := money.Fee(amount, s.feeBPS)
		group, err := domain.NewCaptureEntryGroup(fresh, amount, fee, now)
		if err != nil {
			return err
		}
		if err := s.ledger.InsertGroup(ctx, tx, group); err != nil {
			return err
		}
		if err := s.queueWebhook(ctx, tx, fresh, domain.EventPaymentCaptured); err != nil {
			return err
		}
		out = fresh
		return nil
	})
	if err != nil {
		return nil, err
	}

	s.log.InfoContext(ctx, "payment captured",
		"transaction_id", out.ID.String(),
		"amount", amount.Int64(),
		"captured_total", out.CapturedAmount.Int64(),
		"fee", money.Fee(amount, s.feeBPS).Int64())
	return out, nil
}

// Refund returns money to the cardholder. amount == 0 refunds everything
// refundable.
func (s *PaymentService) Refund(ctx context.Context, merchantID, txID uuid.UUID, amount money.Amount) (*domain.Transaction, error) {
	txn, err := s.transactions.GetByMerchantAndID(ctx, s.db.Pool, merchantID, txID)
	if err != nil {
		return nil, err
	}
	if amount == 0 {
		amount = txn.RemainingRefundable()
	}
	preview := *txn
	if err := preview.Refund(amount, s.now().UTC()); err != nil {
		return nil, err
	}

	refundRes, err := s.acq.Refund(ctx, txn.AcquirerRef, amount)
	if err != nil {
		return nil, err
	}

	var out *domain.Transaction
	err = s.db.WithTx(ctx, func(tx pgx.Tx) error {
		fresh, err := s.transactions.GetForUpdate(ctx, tx, txID)
		if err != nil {
			return err
		}
		if fresh.MerchantID != merchantID {
			return domain.ErrNotFound
		}
		now := s.now().UTC()
		if err := fresh.Refund(amount, now); err != nil {
			s.log.ErrorContext(ctx, "acquirer refund succeeded but transaction moved on; manual reconciliation required",
				"transaction_id", txID.String(),
				"acquirer_ref", refundRes.Ref,
				"amount", amount.Int64(),
				"error", err)
			return err
		}
		if err := s.transactions.Update(ctx, tx, fresh); err != nil {
			return err
		}
		group, err := domain.NewRefundEntryGroup(fresh, amount, now)
		if err != nil {
			return err
		}
		if err := s.ledger.InsertGroup(ctx, tx, group); err != nil {
			return err
		}
		if err := s.queueWebhook(ctx, tx, fresh, domain.EventPaymentRefunded); err != nil {
			return err
		}
		out = fresh
		return nil
	})
	if err != nil {
		return nil, err
	}

	s.log.InfoContext(ctx, "payment refunded",
		"transaction_id", out.ID.String(),
		"amount", amount.Int64(),
		"refunded_total", out.RefundedAmount.Int64())
	return out, nil
}

// Void releases an authorization that will never be captured. No money moved,
// so no ledger entries are written -- the journal records value movements, not
// intentions.
func (s *PaymentService) Void(ctx context.Context, merchantID, txID uuid.UUID) (*domain.Transaction, error) {
	txn, err := s.transactions.GetByMerchantAndID(ctx, s.db.Pool, merchantID, txID)
	if err != nil {
		return nil, err
	}
	preview := *txn
	if err := preview.Void(s.now().UTC()); err != nil {
		return nil, err
	}

	if _, err := s.acq.Void(ctx, txn.AcquirerRef); err != nil {
		return nil, err
	}

	var out *domain.Transaction
	err = s.db.WithTx(ctx, func(tx pgx.Tx) error {
		fresh, err := s.transactions.GetForUpdate(ctx, tx, txID)
		if err != nil {
			return err
		}
		if fresh.MerchantID != merchantID {
			return domain.ErrNotFound
		}
		if err := fresh.Void(s.now().UTC()); err != nil {
			return err
		}
		if err := s.transactions.Update(ctx, tx, fresh); err != nil {
			return err
		}
		if err := s.queueWebhook(ctx, tx, fresh, domain.EventPaymentVoided); err != nil {
			return err
		}
		out = fresh
		return nil
	})
	if err != nil {
		return nil, err
	}

	s.log.InfoContext(ctx, "payment voided", "transaction_id", out.ID.String())
	return out, nil
}

// Get loads one transaction belonging to a merchant.
func (s *PaymentService) Get(ctx context.Context, merchantID, txID uuid.UUID) (*domain.Transaction, error) {
	return s.transactions.GetByMerchantAndID(ctx, s.db.Pool, merchantID, txID)
}

// List returns a page of a merchant's transactions.
func (s *PaymentService) List(ctx context.Context, f repository.ListFilter) ([]*domain.Transaction, error) {
	return s.transactions.List(ctx, s.db.Pool, f)
}

// finalizeFailure marks a declined transaction as failed and queues the
// payment.failed webhook.
func (s *PaymentService) finalizeFailure(ctx context.Context, txn *domain.Transaction, code string) error {
	return s.db.WithTx(ctx, func(tx pgx.Tx) error {
		fresh, err := s.transactions.GetForUpdate(ctx, tx, txn.ID)
		if err != nil {
			return err
		}
		if fresh.Status != domain.StatusCreated {
			return nil // already resolved by someone else
		}
		if err := fresh.Fail(code, s.now().UTC()); err != nil {
			return err
		}
		if err := s.transactions.Update(ctx, tx, fresh); err != nil {
			return err
		}
		return s.queueWebhook(ctx, tx, fresh, domain.EventPaymentFailed)
	})
}

// queueWebhook writes the outbox row in the caller's database transaction, so
// the notification and the money movement commit or roll back together.
func (s *PaymentService) queueWebhook(ctx context.Context, tx pgx.Tx, txn *domain.Transaction, event domain.WebhookEvent) error {
	payload, err := BuildEventPayload(event, txn, s.now().UTC())
	if err != nil {
		return err
	}
	delivery := domain.NewWebhookDelivery(txn.MerchantID, txn.ID, event, payload, s.now().UTC())
	return s.webhooks.Create(ctx, tx, delivery)
}

// EventPayload is the JSON body merchants receive.
type EventPayload struct {
	ID        string          `json:"id"`
	Type      string          `json:"type"`
	CreatedAt time.Time       `json:"created_at"`
	Data      EventPayloadTxn `json:"data"`
}

// EventPayloadTxn is the transaction snapshot inside a webhook.
type EventPayloadTxn struct {
	ID             string            `json:"id"`
	MerchantID     string            `json:"merchant_id"`
	Reference      string            `json:"reference"`
	Amount         int64             `json:"amount"`
	Currency       string            `json:"currency"`
	Status         string            `json:"status"`
	CapturedAmount int64             `json:"captured_amount"`
	RefundedAmount int64             `json:"refunded_amount"`
	CardLast4      string            `json:"card_last4,omitempty"`
	CardBrand      string            `json:"card_brand,omitempty"`
	FailureCode    string            `json:"failure_code,omitempty"`
	Metadata       map[string]string `json:"metadata,omitempty"`
	CreatedAt      time.Time         `json:"created_at"`
}

// BuildEventPayload renders the webhook body for a transaction.
func BuildEventPayload(event domain.WebhookEvent, txn *domain.Transaction, now time.Time) ([]byte, error) {
	payload := EventPayload{
		ID:        "evt_" + uuid.NewString(),
		Type:      string(event),
		CreatedAt: now,
		Data: EventPayloadTxn{
			ID:             txn.ID.String(),
			MerchantID:     txn.MerchantID.String(),
			Reference:      txn.Reference,
			Amount:         txn.Amount.Int64(),
			Currency:       txn.Currency.String(),
			Status:         string(txn.Status),
			CapturedAmount: txn.CapturedAmount.Int64(),
			RefundedAmount: txn.RefundedAmount.Int64(),
			CardLast4:      txn.CardLast4,
			CardBrand:      txn.CardBrand,
			FailureCode:    txn.FailureCode,
			Metadata:       txn.Metadata,
			CreatedAt:      txn.CreatedAt,
		},
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("marshal webhook payload: %w", err)
	}
	return body, nil
}
