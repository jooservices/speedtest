-- +goose Up
-- Failure classes are a fixed product vocabulary (JOOST-73). Existing rows default to other.
ALTER TABLE results ADD COLUMN failure_class text NOT NULL DEFAULT 'other'
    CHECK (failure_class IN ('rate_limited', 'no_network', 'server_unreachable', 'engine_missing', 'timeout', 'other'));
CREATE INDEX results_rate_limited_node_engine_ts_idx ON results (node_id, engine, ts DESC)
    WHERE status = 'failed' AND failure_class = 'rate_limited';

-- +goose Down
DROP INDEX results_rate_limited_node_engine_ts_idx;
ALTER TABLE results DROP COLUMN failure_class;
