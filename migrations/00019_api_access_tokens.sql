-- +goose Up
-- API tokens are high-entropy bearer credentials; only their SHA-256 digest is stored.
CREATE TABLE api_access_tokens (
    id uuid PRIMARY KEY,
    user_id uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    name text NOT NULL CHECK (length(name) BETWEEN 1 AND 80),
    scope text NOT NULL CHECK (scope IN ('read', 'read-write', 'full')),
    token_hash bytea NOT NULL UNIQUE CHECK (octet_length(token_hash) = 32),
    created_at timestamptz NOT NULL DEFAULT now(),
    expires_at timestamptz,
    last_used_at timestamptz,
    revoked_at timestamptz
);
CREATE INDEX api_access_tokens_owner_idx ON api_access_tokens (user_id, created_at DESC, id);

-- +goose Down
DROP TABLE api_access_tokens;
