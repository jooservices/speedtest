-- +goose Up
-- Goose creates its journal before applying version 1; normalize its timestamp too.
ALTER TABLE goose_db_version ALTER COLUMN tstamp TYPE timestamptz USING tstamp AT TIME ZONE 'UTC';
CREATE TABLE users (
    id uuid PRIMARY KEY,
    email text NOT NULL CHECK (length(trim(email)) > 0),
    password_hash text NOT NULL CHECK (length(password_hash) > 0),
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX users_email_unique ON users (lower(email));

-- Store only hashes of opaque session and CSRF tokens, never their bearer values.
CREATE TABLE sessions (
    token_hash text PRIMARY KEY CHECK (length(token_hash) > 0),
    user_id uuid REFERENCES users(id) ON DELETE CASCADE,
    csrf_token_hash text NOT NULL CHECK (length(csrf_token_hash) > 0),
    created_at timestamptz NOT NULL DEFAULT now(),
    expires_at timestamptz NOT NULL CHECK (expires_at > created_at)
);
CREATE INDEX sessions_user_id_idx ON sessions(user_id);
CREATE INDEX sessions_expires_at_idx ON sessions(expires_at);

CREATE TABLE nodes (
    id uuid PRIMARY KEY,
    name varchar(100) NOT NULL UNIQUE CHECK (length(trim(name)) > 0),
    label varchar(200) NOT NULL DEFAULT '',
    provider varchar(200) NOT NULL DEFAULT '',
    plan varchar(200) NOT NULL DEFAULT '',
    expected_down_mbps double precision NOT NULL CHECK (expected_down_mbps > 0 AND expected_down_mbps < 'Infinity'::float8),
    expected_up_mbps double precision NOT NULL CHECK (expected_up_mbps > 0 AND expected_up_mbps < 'Infinity'::float8),
    threads integer NOT NULL DEFAULT 4 CHECK (threads BETWEEN 1 AND 64),
    server_id integer CHECK (server_id > 0), -- NULL means automatic server selection.
    color_slot integer NOT NULL DEFAULT 0 CHECK (color_slot >= 0),
    enabled boolean NOT NULL DEFAULT true,
    auto_update boolean NOT NULL DEFAULT false,
    binding_interface varchar(200) NOT NULL DEFAULT '',
    binding_source_ip inet,
    host text NOT NULL DEFAULT '',
    agent text NOT NULL DEFAULT '',
    note varchar(4000) NOT NULL DEFAULT '',
    config_revision bigint NOT NULL DEFAULT 1 CHECK (config_revision > 0),
    last_seen_at timestamptz,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);
CREATE TABLE node_credentials (
    id uuid PRIMARY KEY,
    node_id uuid NOT NULL REFERENCES nodes(id) ON DELETE CASCADE,
    kind text NOT NULL CHECK (kind IN ('enrollment', 'bearer')),
    token_hash text NOT NULL UNIQUE CHECK (length(token_hash) > 0),
    created_at timestamptz NOT NULL DEFAULT now(),
    expires_at timestamptz,
    consumed_at timestamptz,
    revoked_at timestamptz,
    CHECK (kind <> 'enrollment' OR expires_at IS NOT NULL),
    CHECK (expires_at IS NULL OR expires_at > created_at)
);
CREATE INDEX node_credentials_node_id_idx ON node_credentials(node_id);

CREATE TABLE ip_addresses (
    ip inet PRIMARY KEY CHECK (masklen(ip) = CASE family(ip) WHEN 4 THEN 32 ELSE 128 END),
    geo jsonb NOT NULL DEFAULT '{}' CHECK (jsonb_typeof(geo) = 'object'),
    source text NOT NULL DEFAULT '',
    looked_up_at timestamptz,
    created_at timestamptz NOT NULL DEFAULT now()
);
CREATE TABLE results (
    id uuid PRIMARY KEY, -- Supplied by the agent before spooling; no server default.
    node_id uuid NOT NULL REFERENCES nodes(id) ON DELETE CASCADE,
    ts timestamptz NOT NULL,
    status text NOT NULL CHECK (status IN ('ok', 'degraded', 'failed')),
    down_mbps double precision CHECK (down_mbps >= 0 AND down_mbps < 'Infinity'::float8),
    up_mbps double precision CHECK (up_mbps >= 0 AND up_mbps < 'Infinity'::float8),
    ping_ms double precision CHECK (ping_ms >= 0 AND ping_ms < 'Infinity'::float8),
    jitter_ms double precision CHECK (jitter_ms >= 0 AND jitter_ms < 'Infinity'::float8),
    down_latency_ms double precision CHECK (down_latency_ms >= 0 AND down_latency_ms < 'Infinity'::float8),
    up_latency_ms double precision CHECK (up_latency_ms >= 0 AND up_latency_ms < 'Infinity'::float8),
    loss_percent double precision CHECK (loss_percent BETWEEN 0 AND 100),
    server_id integer CHECK (server_id > 0),
    server jsonb CHECK (jsonb_typeof(server) = 'object'),
    ip inet NOT NULL REFERENCES ip_addresses(ip),
    threads integer NOT NULL CHECK (threads BETWEEN 1 AND 64),
    bytes_down bigint CHECK (bytes_down >= 0),
    bytes_up bigint CHECK (bytes_up >= 0),
    duration_seconds double precision CHECK (duration_seconds >= 0 AND duration_seconds < 'Infinity'::float8),
    error varchar(2000),
    manual boolean NOT NULL DEFAULT false,
    raw jsonb NOT NULL DEFAULT '{}' CHECK (jsonb_typeof(raw) = 'object'),
    loaded_down jsonb CHECK (jsonb_typeof(loaded_down) = 'object'),
    loaded_up jsonb CHECK (jsonb_typeof(loaded_up) = 'object'),
    engine_isp text,
    engine_result_id text,
    engine_result_url text,
    engine_ip inet,
    warnings jsonb NOT NULL DEFAULT '[]' CHECK (jsonb_typeof(warnings) = 'array'),
    created_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (id, node_id),
    CHECK ((status = 'failed' AND length(trim(error)) > 0 AND error IS NOT NULL)
        OR (status IN ('ok', 'degraded') AND error IS NULL AND down_mbps IS NOT NULL
            AND up_mbps IS NOT NULL AND ping_ms IS NOT NULL AND server_id IS NOT NULL))
);
CREATE INDEX results_node_id_ts_idx ON results(node_id, ts);
CREATE INDEX results_ts_idx ON results(ts);
CREATE INDEX results_ip_idx ON results(ip);

CREATE TABLE node_ip_history (
    id uuid PRIMARY KEY,
    node_id uuid NOT NULL REFERENCES nodes(id) ON DELETE CASCADE,
    ts timestamptz NOT NULL,
    from_ip inet REFERENCES ip_addresses(ip),
    to_ip inet NOT NULL REFERENCES ip_addresses(ip),
    CHECK (from_ip IS DISTINCT FROM to_ip)
);
CREATE INDEX node_ip_history_node_id_ts_idx ON node_ip_history(node_id, ts);
CREATE INDEX node_ip_history_ts_idx ON node_ip_history(ts);
CREATE TABLE schedules (
    node_id uuid PRIMARY KEY REFERENCES nodes(id) ON DELETE CASCADE,
    interval_minutes integer NOT NULL DEFAULT 30 CHECK (interval_minutes BETWEEN 5 AND 1440),
    offset_minutes integer NOT NULL DEFAULT 0 CHECK (offset_minutes >= 0 AND offset_minutes < interval_minutes),
    enabled boolean NOT NULL DEFAULT true,
    revision bigint NOT NULL DEFAULT 1 CHECK (revision > 0),
    updated_at timestamptz NOT NULL DEFAULT now()
);
CREATE TABLE alert_rules (
    id uuid PRIMARY KEY,
    name varchar(200) NOT NULL CHECK (length(trim(name)) > 0),
    metric text NOT NULL CHECK (metric IN ('down_pct','up_pct','ping','jitter','loss','failed','ip_changed','offline')),
    op text NOT NULL CHECK (op IN ('<','>','<=','>=','=')),
    value double precision NOT NULL CHECK (value >= 0 AND value < 'Infinity'::float8),
    for_tests integer NOT NULL CHECK (for_tests BETWEEN 1 AND 1000),
    node_ids uuid[], -- NULL selects all nodes; explicit selections are validated by the API.
    severity text NOT NULL CHECK (severity IN ('info','warn','crit')),
    channels text[] NOT NULL CHECK (cardinality(channels) BETWEEN 1 AND 4 AND channels <@ ARRAY['email','telegram','slack','webhook']::text[]),
    enabled boolean NOT NULL DEFAULT true,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    CHECK (node_ids IS NULL OR cardinality(node_ids) BETWEEN 1 AND 100)
);
CREATE TABLE alert_events (
    id uuid PRIMARY KEY,
    rule_id uuid REFERENCES alert_rules(id) ON DELETE SET NULL,
    node_id uuid NOT NULL REFERENCES nodes(id) ON DELETE CASCADE,
    severity text NOT NULL CHECK (severity IN ('info','warn','crit')),
    ts timestamptz NOT NULL,
    resolved_at timestamptz,
    title text NOT NULL,
    detail text NOT NULL DEFAULT '',
    acked boolean NOT NULL DEFAULT false,
    CHECK (resolved_at IS NULL OR resolved_at >= ts)
);
CREATE INDEX alert_events_node_id_ts_idx ON alert_events(node_id, ts);
CREATE INDEX alert_events_rule_id_idx ON alert_events(rule_id);
CREATE INDEX alert_events_ts_idx ON alert_events(ts);
CREATE TABLE alert_state (
    rule_id uuid NOT NULL REFERENCES alert_rules(id) ON DELETE CASCADE,
    node_id uuid NOT NULL REFERENCES nodes(id) ON DELETE CASCADE,
    consecutive_count integer NOT NULL DEFAULT 0 CHECK (consecutive_count >= 0),
    active boolean NOT NULL DEFAULT false,
    last_evaluated_at timestamptz,
    PRIMARY KEY (rule_id, node_id)
);
CREATE INDEX alert_state_node_id_idx ON alert_state(node_id);
-- Public settings and encrypted integration secrets are deliberately separate fields.
CREATE TABLE settings (
    key text PRIMARY KEY CHECK (length(trim(key)) > 0),
    value jsonb,
    secret_ciphertext bytea,
    revision bigint NOT NULL DEFAULT 1 CHECK (revision > 0),
    updated_at timestamptz NOT NULL DEFAULT now(),
    CHECK ((value IS NOT NULL) <> (secret_ciphertext IS NOT NULL))
);
CREATE TABLE agent_commands (
    id uuid PRIMARY KEY,
    node_id uuid NOT NULL REFERENCES nodes(id) ON DELETE CASCADE,
    type text NOT NULL CHECK (type IN ('run_now','pause','resume','update_config','update_agent')),
    version varchar(64),
    status text NOT NULL DEFAULT 'pending' CHECK (status IN ('pending','running','succeeded','failed')),
    phase varchar(200) NOT NULL DEFAULT '',
    pct double precision NOT NULL DEFAULT 0 CHECK (pct BETWEEN 0 AND 100),
    result_id uuid,
    error varchar(2000),
    created_at timestamptz NOT NULL DEFAULT now(),
    acked_at timestamptz,
    FOREIGN KEY (result_id, node_id) REFERENCES results(id, node_id) ON DELETE SET NULL (result_id),
    CHECK (type <> 'update_agent' OR length(trim(version)) > 0 AND version IS NOT NULL),
    CHECK (status <> 'failed' OR length(trim(error)) > 0 AND error IS NOT NULL)
);
CREATE INDEX agent_commands_node_id_created_at_idx ON agent_commands(node_id, created_at);

-- +goose Down
-- No CASCADE: unexpected external dependencies must stop rollback, not be deleted.
DROP TABLE agent_commands;
DROP TABLE settings;
DROP TABLE alert_state;
DROP TABLE alert_events;
DROP TABLE alert_rules;
DROP TABLE schedules;
DROP TABLE node_ip_history;
DROP TABLE results;
DROP TABLE ip_addresses;
DROP TABLE node_credentials;
DROP TABLE nodes;
DROP TABLE sessions;
DROP TABLE users;
