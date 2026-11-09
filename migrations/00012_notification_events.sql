-- +goose Up
-- Owner overrides for the in-code notification event catalog (JOOST-78).
-- Defaults stay in Go; missing rows mean "use catalog default". Compatible with 1.0.0
-- binaries that ignore these tables.

CREATE TABLE notification_event_settings (
    event_key text PRIMARY KEY
        CHECK (length(trim(event_key)) > 0 AND length(event_key) <= 80),
    enabled boolean NOT NULL,
    severity text NOT NULL CHECK (severity IN ('info', 'warn', 'crit')),
    channels text[] NOT NULL
        CHECK (cardinality(channels) BETWEEN 1 AND 4),
    updated_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE notification_templates (
    event_key text NOT NULL
        CHECK (length(trim(event_key)) > 0 AND length(event_key) <= 80),
    channel text NOT NULL
        CHECK (channel IN ('email', 'telegram', 'slack', 'webhook')),
    body text NOT NULL
        CHECK (length(trim(body)) > 0 AND length(body) <= 4000),
    updated_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (event_key, channel)
);

-- Per-node enable overrides (test.success and future node-scoped catalog events).
CREATE TABLE notification_event_node_settings (
    event_key text NOT NULL
        CHECK (length(trim(event_key)) > 0 AND length(event_key) <= 80),
    node_id uuid NOT NULL REFERENCES nodes(id) ON DELETE CASCADE,
    enabled boolean NOT NULL,
    updated_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (event_key, node_id)
);
CREATE INDEX notification_event_node_settings_node_idx
    ON notification_event_node_settings(node_id);

-- +goose Down
DROP TABLE notification_event_node_settings;
DROP TABLE notification_templates;
DROP TABLE notification_event_settings;
