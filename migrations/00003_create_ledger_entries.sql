-- +goose Up
-- Append-only double-entry journal. Never UPDATE-ed, never DELETE-ed:
-- a correction is a new, opposite entry group.
CREATE TABLE ledger_entries (
    id             BIGSERIAL PRIMARY KEY,
    entry_group_id UUID        NOT NULL,
    transaction_id UUID        NOT NULL REFERENCES transactions (id),
    account        TEXT        NOT NULL,
    merchant_id    UUID        REFERENCES merchants (id),
    direction      TEXT        NOT NULL,
    amount         BIGINT      NOT NULL,
    currency       TEXT        NOT NULL,
    event_type     TEXT        NOT NULL,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),

    CONSTRAINT ledger_direction_check  CHECK (direction IN ('debit', 'credit')),
    CONSTRAINT ledger_amount_check     CHECK (amount > 0),
    CONSTRAINT ledger_event_type_check CHECK (event_type IN ('capture', 'refund', 'fee', 'settlement'))
);

-- Guard rail: block any UPDATE/DELETE on the journal at the database level.
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION ledger_entries_immutable() RETURNS TRIGGER AS $$
BEGIN
    RAISE EXCEPTION 'ledger_entries is append-only (attempted %)', TG_OP;
END;
$$ LANGUAGE plpgsql;
-- +goose StatementEnd

CREATE TRIGGER ledger_entries_no_mutation
    BEFORE UPDATE OR DELETE ON ledger_entries
    FOR EACH ROW EXECUTE FUNCTION ledger_entries_immutable();

-- +goose Down
DROP TRIGGER IF EXISTS ledger_entries_no_mutation ON ledger_entries;
DROP FUNCTION IF EXISTS ledger_entries_immutable();
DROP TABLE ledger_entries;
