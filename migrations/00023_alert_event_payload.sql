-- +goose Up
-- Additive metadata for catalog-driven notification rendering (JOOST-78).
-- Defaults keep the shipped 1.0.0 insert paths working without naming the columns.
ALTER TABLE alert_events
    ADD COLUMN event_key text NOT NULL DEFAULT '',
    ADD COLUMN payload jsonb NOT NULL DEFAULT '{}'::jsonb;

CREATE INDEX alert_events_event_key_idx ON alert_events(event_key) WHERE event_key <> '';

-- +goose Down
DROP INDEX IF EXISTS alert_events_event_key_idx;
ALTER TABLE alert_events DROP COLUMN payload;
ALTER TABLE alert_events DROP COLUMN event_key;
