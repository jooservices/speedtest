-- +goose Up
-- One row per month whose scheduled report e-mail was claimed; the primary key makes the send idempotent across restarts.
CREATE TABLE monthly_report_runs (
    month char(7) PRIMARY KEY CHECK (month ~ '^[0-9]{4}-(0[1-9]|1[0-2])$'),
    claimed_at timestamptz NOT NULL DEFAULT now(),
    sent_at timestamptz
);

-- +goose Down
DROP TABLE monthly_report_runs;
