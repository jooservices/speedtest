-- +goose Up
-- Preserve the beginning of sub-threshold streaks so recovery messages can
-- report the real duration, including when the configured threshold was not met.
ALTER TABLE alert_state
    ADD COLUMN streak_started_at timestamptz;

ALTER TABLE alert_state
    ADD COLUMN last_notified_at timestamptz;

ALTER TABLE notification_event_settings
    ADD COLUMN thresholds jsonb NOT NULL DEFAULT '{}'::jsonb
        CHECK (jsonb_typeof(thresholds) = 'object'),
    ADD COLUMN cooldown_seconds integer NOT NULL DEFAULT 0
        CHECK (cooldown_seconds BETWEEN 0 AND 604800),
    ADD COLUMN repeat_seconds integer NOT NULL DEFAULT 0
        CHECK (repeat_seconds BETWEEN 0 AND 604800),
    ADD COLUMN maintenance_start time,
    ADD COLUMN maintenance_end time,
    ADD COLUMN maintenance_timezone text NOT NULL DEFAULT 'UTC'
        CHECK (length(maintenance_timezone) BETWEEN 1 AND 80),
    ADD COLUMN isp_scopes text[] NOT NULL DEFAULT '{}'
        CHECK (cardinality(isp_scopes) <= 100);
ALTER TABLE notification_event_settings
    ADD CONSTRAINT notification_event_maintenance_window_check
    CHECK ((maintenance_start IS NULL) = (maintenance_end IS NULL));

-- +goose Down
ALTER TABLE alert_state
    DROP COLUMN streak_started_at;
ALTER TABLE alert_state
    DROP COLUMN last_notified_at;
ALTER TABLE notification_event_settings
    DROP CONSTRAINT notification_event_maintenance_window_check,
    DROP COLUMN thresholds,
    DROP COLUMN cooldown_seconds,
    DROP COLUMN repeat_seconds,
    DROP COLUMN maintenance_start,
    DROP COLUMN maintenance_end,
    DROP COLUMN maintenance_timezone,
    DROP COLUMN isp_scopes;
