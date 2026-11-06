-- +goose Up
ALTER TABLE nodes ADD COLUMN installed_engines text[] NOT NULL DEFAULT ARRAY['ookla']::text[]
    CHECK (installed_engines <@ ARRAY['ookla','librespeed']::text[]);
ALTER TABLE nodes ADD COLUMN primary_engine text NOT NULL DEFAULT 'ookla'
    CHECK (primary_engine IN ('ookla','librespeed'));
ALTER TABLE results ADD COLUMN engine text NOT NULL DEFAULT 'ookla'
    CHECK (engine IN ('ookla','librespeed'));
CREATE INDEX results_node_engine_ts_idx ON results(node_id, engine, ts DESC);
CREATE TABLE engine_schedules (
    node_id uuid NOT NULL REFERENCES nodes(id) ON DELETE CASCADE,
    engine text NOT NULL CHECK (engine IN ('ookla','librespeed')),
    interval_minutes integer NOT NULL DEFAULT 60 CHECK (interval_minutes BETWEEN 5 AND 1440),
    offset_minutes integer NOT NULL DEFAULT 0 CHECK (offset_minutes >= 0 AND offset_minutes < interval_minutes),
    enabled boolean NOT NULL DEFAULT true,
    revision bigint NOT NULL DEFAULT 1 CHECK (revision > 0),
    PRIMARY KEY (node_id, engine)
);
INSERT INTO engine_schedules SELECT node_id,'ookla',interval_minutes,offset_minutes,enabled,revision FROM schedules;
ALTER TABLE alert_rules ADD COLUMN engine text CHECK (engine IN ('ookla','librespeed'));

-- +goose Down
ALTER TABLE alert_rules DROP COLUMN engine;
DROP TABLE engine_schedules;
DROP INDEX results_node_engine_ts_idx;
ALTER TABLE results DROP COLUMN engine;
ALTER TABLE nodes DROP COLUMN primary_engine;
ALTER TABLE nodes DROP COLUMN installed_engines;
