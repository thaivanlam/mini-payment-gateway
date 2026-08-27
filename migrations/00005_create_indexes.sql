-- +goose Up

-- GET /api/v1/payments : list a merchant's transactions, newest first
-- (cursor pagination walks this index backwards).
CREATE INDEX idx_transactions_merchant_created
    ON transactions (merchant_id, created_at DESC, id DESC);

-- Reconciliation job: scan captured transactions of a given day.
CREATE INDEX idx_transactions_status_captured_at
    ON transactions (status, captured_at);

-- GET /api/v1/ledger/balance and /ledger/entries : aggregate one account
-- of one merchant over a time window.
CREATE INDEX idx_ledger_merchant_account_created
    ON ledger_entries (merchant_id, account, created_at DESC);

-- Ledger invariant check: fetch both legs of one accounting event.
CREATE INDEX idx_ledger_entry_group
    ON ledger_entries (entry_group_id);

-- Reconciliation joins the journal back to its transaction.
CREATE INDEX idx_ledger_transaction
    ON ledger_entries (transaction_id);

-- Webhook worker poll: due jobs, oldest first (FOR UPDATE SKIP LOCKED).
CREATE INDEX idx_webhook_due
    ON webhook_deliveries (status, next_attempt_at)
    WHERE status IN ('pending', 'failed');

-- +goose Down
DROP INDEX IF EXISTS idx_webhook_due;
DROP INDEX IF EXISTS idx_ledger_transaction;
DROP INDEX IF EXISTS idx_ledger_entry_group;
DROP INDEX IF EXISTS idx_ledger_merchant_account_created;
DROP INDEX IF EXISTS idx_transactions_status_captured_at;
DROP INDEX IF EXISTS idx_transactions_merchant_created;
