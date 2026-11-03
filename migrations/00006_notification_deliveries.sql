-- +goose Up
CREATE TABLE notification_deliveries (
    id uuid PRIMARY KEY,
    event_id uuid REFERENCES alert_events(id) ON DELETE CASCADE,
    channel text NOT NULL CHECK (channel IN ('email','telegram','slack','webhook')),
    status text NOT NULL DEFAULT 'pending' CHECK (status IN ('pending','sent','failed')),
    attempts integer NOT NULL DEFAULT 0 CHECK (attempts >= 0),
    created_at timestamptz NOT NULL DEFAULT now(),
    last_attempt timestamptz,
    error varchar(200),
    next_attempt_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE(event_id, channel)
);
CREATE INDEX notification_deliveries_due_idx ON notification_deliveries(next_attempt_at,id) WHERE status='pending';
CREATE INDEX notification_deliveries_created_idx ON notification_deliveries(created_at DESC,id DESC);

-- +goose Down
DROP TABLE notification_deliveries;
