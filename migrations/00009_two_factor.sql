-- +goose Up
-- Optional TOTP two-factor sign-in for the owner. The secret is AES-GCM encrypted with APP_KEY (nonce then ciphertext, the layout rotate-app-key re-encrypts); recovery codes are
-- stored as SHA-256 digests and removed when used. last_step blocks replaying a code within its time window.
CREATE TABLE user_two_factor (
    user_id uuid PRIMARY KEY REFERENCES users(id) ON DELETE CASCADE,
    secret_enc bytea NOT NULL CHECK (length(secret_enc) > 0),
    enabled boolean NOT NULL DEFAULT false,
    last_step bigint NOT NULL DEFAULT 0 CHECK (last_step >= 0),
    recovery_hashes text[] NOT NULL DEFAULT '{}',
    created_at timestamptz NOT NULL DEFAULT now(),
    enabled_at timestamptz
);

-- +goose Down
DROP TABLE user_two_factor;
