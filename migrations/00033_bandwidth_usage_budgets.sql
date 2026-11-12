-- +goose Up
-- Budget data is optional. New tables keep the existing center binary compatible.
ALTER TABLE schedules ALTER COLUMN interval_minutes SET DEFAULT 60;
CREATE INDEX results_usage_period_idx ON results (ts, engine, node_id);

CREATE TABLE usage_monthly_totals (
    period_start date NOT NULL,
    node_id uuid NOT NULL REFERENCES nodes(id) ON DELETE CASCADE,
    engine text NOT NULL CHECK (engine IN ('ookla', 'librespeed')),
    bytes_down bigint NOT NULL CHECK (bytes_down >= 0),
    bytes_up bigint NOT NULL CHECK (bytes_up >= 0),
    PRIMARY KEY (period_start, node_id, engine)
);
INSERT INTO usage_monthly_totals(period_start,node_id,engine,bytes_down,bytes_up)
SELECT date_trunc('month',ts AT TIME ZONE 'UTC')::date,node_id,engine,COALESCE(sum(bytes_down),0),COALESCE(sum(bytes_up),0)
FROM results GROUP BY 1,node_id,engine;

-- +goose StatementBegin
CREATE FUNCTION speedtest_lock_usage_budget_changes() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    PERFORM pg_advisory_xact_lock_shared(hashtext('speedtest:usage-budget-lifecycle'));
    RETURN NULL;
END;
$$;
-- +goose StatementEnd
CREATE TRIGGER results_usage_budget_lifecycle BEFORE INSERT ON results
FOR EACH STATEMENT EXECUTE FUNCTION speedtest_lock_usage_budget_changes();

-- +goose StatementBegin
CREATE FUNCTION speedtest_record_monthly_usage() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    -- Keep the high-volume ingest path lean until a budget needs a durable rollup.
    IF NOT EXISTS (SELECT 1 FROM usage_budgets) THEN
        RETURN NULL;
    END IF;
    -- The center inserts a batch as individual statements; avoid an aggregate plan per result.
    IF NOT EXISTS (SELECT 1 FROM inserted_results OFFSET 1) THEN
        INSERT INTO usage_monthly_totals(period_start,node_id,engine,bytes_down,bytes_up)
        SELECT date_trunc('month',inserted.ts AT TIME ZONE 'UTC')::date,inserted.node_id,inserted.engine,
               COALESCE(inserted.bytes_down,0),COALESCE(inserted.bytes_up,0)
        FROM inserted_results AS inserted
        ON CONFLICT(period_start,node_id,engine) DO UPDATE SET
            bytes_down=usage_monthly_totals.bytes_down+EXCLUDED.bytes_down,
            bytes_up=usage_monthly_totals.bytes_up+EXCLUDED.bytes_up;
        RETURN NULL;
    END IF;
    INSERT INTO usage_monthly_totals(period_start,node_id,engine,bytes_down,bytes_up)
    SELECT date_trunc('month',inserted.ts AT TIME ZONE 'UTC')::date,inserted.node_id,inserted.engine,
           COALESCE(sum(inserted.bytes_down),0),COALESCE(sum(inserted.bytes_up),0)
    FROM inserted_results AS inserted
    GROUP BY 1,2,3
    ON CONFLICT(period_start,node_id,engine) DO UPDATE SET
        bytes_down=usage_monthly_totals.bytes_down+EXCLUDED.bytes_down,
        bytes_up=usage_monthly_totals.bytes_up+EXCLUDED.bytes_up;
    RETURN NULL;
END;
$$;
-- +goose StatementEnd
CREATE TRIGGER results_record_monthly_usage AFTER INSERT ON results
REFERENCING NEW TABLE AS inserted_results
FOR EACH STATEMENT EXECUTE FUNCTION speedtest_record_monthly_usage();

CREATE TABLE usage_budgets (
    scope_key text PRIMARY KEY,
    node_id uuid REFERENCES nodes(id) ON DELETE CASCADE,
    monthly_bytes bigint NOT NULL CHECK (monthly_bytes > 0),
    action text NOT NULL CHECK (action IN ('widen', 'pause')),
    widen_multiplier integer NOT NULL DEFAULT 2 CHECK (widen_multiplier BETWEEN 2 AND 10),
    revision bigint NOT NULL DEFAULT 1 CHECK (revision > 0),
    updated_at timestamptz NOT NULL DEFAULT now(),
    CHECK ((scope_key = 'global' AND node_id IS NULL) OR
           (scope_key <> 'global' AND node_id IS NOT NULL AND scope_key = node_id::text))
);

CREATE TABLE usage_budget_events (
    id uuid PRIMARY KEY,
    scope_key text NOT NULL,
    node_id uuid REFERENCES nodes(id) ON DELETE CASCADE,
    period_start date NOT NULL,
    event_type text NOT NULL CHECK (event_type IN ('budget_warning', 'budget_limit', 'interval_widened', 'tests_paused')),
    used_bytes bigint NOT NULL CHECK (used_bytes >= 0),
    budget_bytes bigint NOT NULL CHECK (budget_bytes > 0),
    ratio double precision NOT NULL CHECK (ratio >= 0 AND ratio < 'Infinity'::float8),
    created_at timestamptz NOT NULL DEFAULT now(),
    published_at timestamptz,
    UNIQUE (scope_key, period_start, event_type)
);
CREATE INDEX usage_budget_events_pending_idx ON usage_budget_events (created_at, id) WHERE published_at IS NULL;

CREATE TABLE usage_budget_overrides (
    node_id uuid NOT NULL REFERENCES nodes(id) ON DELETE CASCADE,
    engine text NOT NULL CHECK (engine IN ('ookla', 'librespeed')),
    period_start date NOT NULL,
    original_interval_minutes integer NOT NULL CHECK (original_interval_minutes BETWEEN 5 AND 1440),
    original_enabled boolean NOT NULL,
    applied_interval_minutes integer NOT NULL CHECK (applied_interval_minutes BETWEEN 5 AND 1440),
    applied_enabled boolean NOT NULL,
    PRIMARY KEY (node_id, engine, period_start)
);

-- +goose Down
UPDATE engine_schedules AS schedule
SET interval_minutes = CASE
        WHEN schedule.interval_minutes = override.applied_interval_minutes THEN override.original_interval_minutes
        ELSE schedule.interval_minutes
    END,
    offset_minutes = schedule.offset_minutes % CASE
        WHEN schedule.interval_minutes = override.applied_interval_minutes THEN override.original_interval_minutes
        ELSE schedule.interval_minutes
    END,
    enabled = CASE
        WHEN schedule.enabled = override.applied_enabled THEN override.original_enabled
        ELSE schedule.enabled
    END,
    revision = schedule.revision + 1
FROM usage_budget_overrides AS override
WHERE schedule.node_id = override.node_id AND schedule.engine = override.engine
  AND (schedule.interval_minutes = override.applied_interval_minutes OR schedule.enabled = override.applied_enabled);
DROP TABLE usage_budget_overrides;
DROP TABLE usage_budget_events;
DROP TABLE usage_budgets;
DROP TRIGGER results_record_monthly_usage ON results;
DROP FUNCTION speedtest_record_monthly_usage();
DROP TABLE usage_monthly_totals;
DROP INDEX results_usage_period_idx;
DROP TRIGGER results_usage_budget_lifecycle ON results;
DROP FUNCTION speedtest_lock_usage_budget_changes();
-- The migration is additive over the 60-minute default established by 00015.
ALTER TABLE schedules ALTER COLUMN interval_minutes SET DEFAULT 60;
