-- +goose Up
-- One row per daily or weekly digest period. The primary key prevents a second
-- completion for the same local date and kind when the owner time zone changes.
CREATE TABLE digest_periods (
    kind text NOT NULL CHECK (kind IN ('daily', 'weekly')),
    local_date date NOT NULL,
    timezone text NOT NULL CHECK (length(timezone) > 0),
    claimed_at timestamptz NOT NULL DEFAULT now(),
    lease_id uuid,
    lease_until timestamptz,
    attempt_count integer NOT NULL DEFAULT 0 CHECK (attempt_count >= 0),
    next_attempt_at timestamptz NOT NULL DEFAULT now(),
    completed_at timestamptz,
    PRIMARY KEY (kind, local_date)
);
CREATE INDEX digest_periods_pending_idx ON digest_periods (next_attempt_at, local_date)
    WHERE completed_at IS NULL;

-- +goose Down
DROP INDEX digest_periods_pending_idx;
DROP TABLE digest_periods;
