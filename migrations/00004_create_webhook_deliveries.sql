-- +goose Up
CREATE TABLE webhook_deliveries (
    id              UUID PRIMARY KEY,
    merchant_id     UUID        NOT NULL REFERENCES merchants (id),
    transaction_id  UUID        NOT NULL REFERENCES transactions (id),
    event_type      TEXT        NOT NULL,
    payload         JSONB       NOT NULL,
    status          TEXT        NOT NULL DEFAULT 'pending',
    attempt_count   INT         NOT NULL DEFAULT 0,
    next_attempt_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_error      TEXT,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now(),

    CONSTRAINT webhook_status_check CHECK (status IN
        ('pending', 'delivering', 'delivered', 'failed', 'dead'))
);

-- +goose Down
DROP TABLE webhook_deliveries;
