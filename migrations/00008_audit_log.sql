-- +goose Up
-- One row per successful owner change. No request bodies and no secrets are stored: only what was changed.
CREATE TABLE audit_log (
    id uuid PRIMARY KEY,
    ts timestamptz NOT NULL DEFAULT now(),
    actor text NOT NULL CHECK (length(actor) > 0),
    action text NOT NULL CHECK (length(action) > 0),
    target_type text NOT NULL DEFAULT '',
    target_id text NOT NULL DEFAULT '',
    summary text NOT NULL DEFAULT '' CHECK (length(summary) <= 300),
    remote text NOT NULL DEFAULT ''
);
CREATE INDEX audit_log_ts_idx ON audit_log (ts DESC, id);
CREATE INDEX audit_log_action_idx ON audit_log (action, ts DESC);

-- +goose Down
DROP TABLE audit_log;
