-- +goose Up
-- +goose StatementBegin

CREATE EXTENSION IF NOT EXISTS pgcrypto;

-- Users who own routes and send emails.
CREATE TABLE users (
    id         UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    email      VARCHAR(255) NOT NULL UNIQUE,
    created_at TIMESTAMPTZ   NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ   NOT NULL DEFAULT NOW()
);

-- Delivery routes (SMTP server or API provider) owned by a user.
-- encrypted_auth holds the SMTP password or API token, encrypted with
-- crypto.Encrypt() and the master key.
CREATE TABLE delivery_routes (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id         UUID         NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    route_type      VARCHAR(10)  NOT NULL CHECK (route_type IN ('SMTP', 'API')),
    from_email      VARCHAR(255) NOT NULL,
    -- SMTP-specific (nullable for API routes)
    smtp_host       VARCHAR(255),
    smtp_port       VARCHAR(10),
    smtp_username   VARCHAR(255),
    -- API-specific (nullable for SMTP routes)
    api_provider    VARCHAR(50),
    -- Encrypted credential blob; decrypted in-memory at send-time.
    encrypted_auth  BYTEA        NOT NULL,
    created_at      TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    updated_at      TIMESTAMPTZ  NOT NULL DEFAULT NOW()
);

-- Outbound email log entries, written by the API and read by the worker.
CREATE TABLE email_logs (
    id             UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id        UUID         NOT NULL REFERENCES users(id),
    route_id       UUID         NOT NULL REFERENCES delivery_routes(id),
    recipient      TEXT         NOT NULL,
    subject        TEXT         NOT NULL,
    body_html      TEXT         NOT NULL,
    status         VARCHAR(20)  NOT NULL DEFAULT 'queued',
    error_message  TEXT,
    created_at     TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    updated_at     TIMESTAMPTZ  NOT NULL DEFAULT NOW()
);

-- Attachments stored ephemerally on local disk; pruned after delivery.
CREATE TABLE email_attachments (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    email_log_id    UUID         NOT NULL REFERENCES email_logs(id) ON DELETE CASCADE,
    file_name       VARCHAR(255) NOT NULL,
    file_type       VARCHAR(100) NOT NULL,
    local_path      TEXT         NOT NULL,
    file_size_bytes INT          NOT NULL,
    created_at      TIMESTAMP    NOT NULL DEFAULT CURRENT_TIMESTAMP
);

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

DROP TABLE IF EXISTS email_attachments;
DROP TABLE IF EXISTS email_logs;
DROP TABLE IF EXISTS delivery_routes;
DROP TABLE IF EXISTS users;

-- +goose StatementEnd
