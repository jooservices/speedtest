-- +goose Up
ALTER TABLE alert_rules
    ADD COLUMN revision bigint NOT NULL DEFAULT 1 CHECK (revision > 0);

ALTER TABLE alert_events
    ADD COLUMN trigger_result_id uuid REFERENCES results(id) ON DELETE SET NULL;
CREATE UNIQUE INDEX alert_events_rule_node_trigger_idx
    ON alert_events(rule_id, node_id, trigger_result_id)
    WHERE trigger_result_id IS NOT NULL;

ALTER TABLE alert_state
    ADD COLUMN active_event_id uuid REFERENCES alert_events(id) ON DELETE SET NULL;

-- One durable handoff per event. JOOST-30 consumes these rows and snapshots
-- channels so later rule edits do not change the event's delivery intent.
CREATE TABLE alert_notification_outbox (
    event_id uuid PRIMARY KEY REFERENCES alert_events(id) ON DELETE CASCADE,
    channels text[] NOT NULL CHECK (cardinality(channels) BETWEEN 1 AND 4),
    created_at timestamptz NOT NULL DEFAULT now(),
    consumed_at timestamptz
);
CREATE INDEX alert_notification_outbox_pending_idx
    ON alert_notification_outbox(created_at, event_id)
    WHERE consumed_at IS NULL;

CREATE TABLE maintenance_jobs (
    id uuid PRIMARY KEY,
    kind text NOT NULL CHECK (length(trim(kind)) > 0),
    status text NOT NULL CHECK (status IN ('pending','running','succeeded','failed')),
    error varchar(500),
    created_at timestamptz NOT NULL DEFAULT now(),
    finished_at timestamptz
);
CREATE INDEX maintenance_jobs_created_at_idx ON maintenance_jobs(created_at);

-- +goose Down
DROP TABLE maintenance_jobs;
DROP TABLE alert_notification_outbox;
ALTER TABLE alert_state DROP COLUMN active_event_id;
DROP INDEX alert_events_rule_node_trigger_idx;
ALTER TABLE alert_events DROP COLUMN trigger_result_id;
ALTER TABLE alert_rules DROP COLUMN revision;
