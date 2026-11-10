-- +goose Up
-- Daily and minute request counts plus provider cooldowns survive center restarts.
CREATE TABLE geo_provider_usage (
    provider text NOT NULL CHECK (length(provider) > 0 AND length(provider) <= 40),
    day date NOT NULL,
    requests integer NOT NULL DEFAULT 0 CHECK (requests >= 0),
    minute_start timestamptz NOT NULL DEFAULT '-infinity',
    minute_requests integer NOT NULL DEFAULT 0 CHECK (minute_requests >= 0),
    blocked_until timestamptz,
    PRIMARY KEY (provider, day)
);

-- A single keyset cursor makes the slow cache refresh resumable within its UTC day.
CREATE TABLE geo_backfill_state (
    singleton boolean PRIMARY KEY DEFAULT true CHECK (singleton),
    run_day date NOT NULL,
    cursor inet,
    updated_at timestamptz NOT NULL DEFAULT now()
);

-- +goose Down
DROP TABLE geo_backfill_state;
DROP TABLE geo_provider_usage;
