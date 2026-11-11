-- +goose Up
ALTER TABLE geo_provider_usage ADD COLUMN next_request_at timestamptz;

-- +goose Down
ALTER TABLE geo_provider_usage DROP COLUMN next_request_at;
