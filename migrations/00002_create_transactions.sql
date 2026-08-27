-- +goose Up
CREATE TABLE transactions (
    id              UUID PRIMARY KEY,
    merchant_id     UUID        NOT NULL REFERENCES merchants (id),
    reference       TEXT        NOT NULL,
    amount          BIGINT      NOT NULL,
    currency        TEXT        NOT NULL,
    status          TEXT        NOT NULL,
    captured_amount BIGINT      NOT NULL DEFAULT 0,
    refunded_amount BIGINT      NOT NULL DEFAULT 0,
    card_last4      TEXT,
    card_brand      TEXT,
    acquirer_ref    TEXT,
    failure_code    TEXT,
    metadata        JSONB       NOT NULL DEFAULT '{}'::jsonb,
    version         INT         NOT NULL DEFAULT 0,
    authorized_at   TIMESTAMPTZ,
    captured_at     TIMESTAMPTZ,
    settled_at      TIMESTAMPTZ,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now(),

    -- The same merchant must not be able to submit the same order twice.
    CONSTRAINT transactions_merchant_reference_key UNIQUE (merchant_id, reference),
    CONSTRAINT transactions_amount_check   CHECK (amount > 0),
    CONSTRAINT transactions_captured_check CHECK (captured_amount >= 0 AND captured_amount <= amount),
    CONSTRAINT transactions_refunded_check CHECK (refunded_amount >= 0 AND refunded_amount <= captured_amount),
    CONSTRAINT transactions_status_check   CHECK (status IN
        ('created', 'authorized', 'captured', 'settled', 'refunded', 'voided', 'failed'))
);

-- +goose Down
DROP TABLE transactions;
