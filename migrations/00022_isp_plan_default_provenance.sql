-- +goose Up
-- Track which expected speeds still follow their assigned ISP plan.
ALTER TABLE nodes
    ADD COLUMN expected_down_isp_default boolean NOT NULL DEFAULT false,
    ADD COLUMN expected_up_isp_default boolean NOT NULL DEFAULT false;

-- +goose Down
ALTER TABLE nodes
    DROP COLUMN expected_up_isp_default,
    DROP COLUMN expected_down_isp_default;
