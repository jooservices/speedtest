-- +goose Up
-- A full-speed test on a fast line moves several GB, so new nodes start at one test an hour (owner decision 2026-11-06).
-- Existing schedules keep the interval they have.
ALTER TABLE schedules ALTER COLUMN interval_minutes SET DEFAULT 60;

-- +goose Down
ALTER TABLE schedules ALTER COLUMN interval_minutes SET DEFAULT 30;
