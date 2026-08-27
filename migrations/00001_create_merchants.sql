-- +goose Up
CREATE TABLE merchants (
    id              UUID PRIMARY KEY,
    name            TEXT        NOT NULL,
    email           TEXT        NOT NULL UNIQUE,
    api_key         TEXT        NOT NULL UNIQUE,
    -- AES-256-GCM ciphertext, never plaintext. HMAC request signing is a
    -- symmetric scheme, so the server must be able to recover the secret;
    -- a one-way hash would make verification impossible. See README.
    api_secret_enc  TEXT        NOT NULL,
    webhook_url     TEXT,
    webhook_secret_enc TEXT     NOT NULL,
    status          TEXT        NOT NULL DEFAULT 'active',
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now(),

    CONSTRAINT merchants_status_check CHECK (status IN ('active', 'suspended'))
);

-- +goose Down
DROP TABLE merchants;
