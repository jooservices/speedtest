-- +goose Up
-- Keep the owner identity while recording which authentication source authorized a change.
ALTER TABLE audit_log
    ADD COLUMN auth_method text NOT NULL DEFAULT 'session'
        CHECK (auth_method IN ('session', 'bearer')),
    ADD COLUMN credential_id uuid;

-- +goose Down
ALTER TABLE audit_log
    DROP COLUMN credential_id,
    DROP COLUMN auth_method;
