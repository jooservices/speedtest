-- +goose Up
ALTER TABLE notification_deliveries ADD COLUMN escalated_at timestamptz;

-- +goose Down
ALTER TABLE notification_deliveries DROP COLUMN escalated_at;
