-- +goose Up
CREATE TABLE api_idempotency (
    principal text NOT NULL CHECK (length(principal) > 0),
    operation text NOT NULL CHECK (length(operation) > 0),
    key_hash text NOT NULL CHECK (length(key_hash) = 64),
    request_hash text NOT NULL CHECK (length(request_hash) = 64),
    response_ciphertext bytea NOT NULL CHECK (octet_length(response_ciphertext) >= 28),
    created_at timestamptz NOT NULL DEFAULT now(),
    expires_at timestamptz NOT NULL CHECK (expires_at > created_at),
    PRIMARY KEY (principal, operation, key_hash)
);
CREATE INDEX api_idempotency_expires_at_idx ON api_idempotency (expires_at);

-- +goose Down
DROP TABLE api_idempotency;
