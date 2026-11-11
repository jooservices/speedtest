-- +goose Up
ALTER TABLE monthly_report_runs
    ADD COLUMN lease_id uuid,
    ADD COLUMN lease_until timestamptz,
    ADD COLUMN attempt_count integer NOT NULL DEFAULT 0 CHECK (attempt_count >= 0),
    ADD COLUMN next_attempt_at timestamptz NOT NULL DEFAULT now();

-- +goose Down
ALTER TABLE monthly_report_runs
    DROP COLUMN next_attempt_at,
    DROP COLUMN attempt_count,
    DROP COLUMN lease_until,
    DROP COLUMN lease_id;
