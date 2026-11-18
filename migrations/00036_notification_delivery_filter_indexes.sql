-- +goose Up
CREATE INDEX notification_deliveries_status_created_idx ON notification_deliveries(status, created_at DESC, id DESC);
CREATE INDEX notification_deliveries_channel_created_idx ON notification_deliveries(channel, created_at DESC, id DESC);
CREATE INDEX notification_deliveries_status_channel_created_idx ON notification_deliveries(status, channel, created_at DESC, id DESC);

-- +goose Down
DROP INDEX IF EXISTS notification_deliveries_status_channel_created_idx;
DROP INDEX IF EXISTS notification_deliveries_channel_created_idx;
DROP INDEX IF EXISTS notification_deliveries_status_created_idx;
