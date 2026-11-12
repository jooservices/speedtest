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

-- A priority/IP keyset cursor makes the slow cache refresh resumable across UTC days.
CREATE TABLE geo_backfill_state (
    singleton boolean PRIMARY KEY DEFAULT true CHECK (singleton),
    run_day date NOT NULL,
    cursor inet,
    cursor_node_priority boolean NOT NULL DEFAULT true,
    updated_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX node_ip_addresses_ip_idx ON node_ip_addresses(ip);

-- +goose Down
DROP INDEX node_ip_addresses_ip_idx;
DROP TABLE geo_backfill_state;
DROP TABLE geo_provider_usage;
