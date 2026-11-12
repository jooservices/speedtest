-- +goose Up
-- Platform events (for example retention and export completion) have no node.
ALTER TABLE alert_events ALTER COLUMN node_id DROP NOT NULL;

-- +goose Down
-- +goose StatementBegin
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM alert_events WHERE node_id IS NULL) THEN
        RAISE EXCEPTION 'cannot restore alert_events.node_id NOT NULL while system events exist';
    END IF;
END $$;
-- +goose StatementEnd
ALTER TABLE alert_events ALTER COLUMN node_id SET NOT NULL;
