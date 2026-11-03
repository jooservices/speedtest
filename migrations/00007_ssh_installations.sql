-- +goose Up
CREATE TABLE installer_probes (
    id uuid PRIMARY KEY,
    host text NOT NULL CHECK (length(trim(host)) > 0 AND length(host) <= 255),
    port integer NOT NULL CHECK (port BETWEEN 1 AND 65535),
    fingerprint text NOT NULL CHECK (length(fingerprint) BETWEEN 16 AND 128),
    created_at timestamptz NOT NULL DEFAULT now(),
    expires_at timestamptz NOT NULL CHECK (expires_at > created_at),
    consumed_at timestamptz
);
CREATE INDEX installer_probes_expires_at_idx ON installer_probes(expires_at);

CREATE TABLE installer_probe_idempotency (
    principal text NOT NULL CHECK (length(trim(principal)) > 0),
    key_hash text NOT NULL CHECK (length(key_hash) = 64),
    request_hash text NOT NULL CHECK (length(request_hash) = 64),
    probe_id uuid NOT NULL REFERENCES installer_probes(id) ON DELETE CASCADE,
    created_at timestamptz NOT NULL DEFAULT now(),
    expires_at timestamptz NOT NULL CHECK (expires_at > created_at),
    PRIMARY KEY (principal, key_hash)
);
CREATE INDEX installer_probe_idempotency_expires_at_idx ON installer_probe_idempotency(expires_at);

CREATE TABLE installer_jobs (
    id uuid PRIMARY KEY,
    node_id uuid NOT NULL REFERENCES nodes(id) ON DELETE CASCADE,
    principal text NOT NULL CHECK (length(trim(principal)) > 0),
    status text NOT NULL CHECK (status IN ('pending','running','succeeded','failed')),
    step text NOT NULL DEFAULT 'queued',
    error text,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    CHECK ((status = 'failed' AND length(trim(error)) > 0 AND error IS NOT NULL)
        OR (status <> 'failed' AND error IS NULL))
);
CREATE UNIQUE INDEX installer_jobs_one_active_per_node_idx ON installer_jobs(node_id)
    WHERE status IN ('pending','running');
CREATE INDEX installer_jobs_updated_at_idx ON installer_jobs(updated_at);

CREATE TABLE installer_events (
    seq bigserial PRIMARY KEY,
    id uuid NOT NULL UNIQUE,
    installation_id uuid NOT NULL REFERENCES installer_jobs(id) ON DELETE CASCADE,
    ts timestamptz NOT NULL DEFAULT now(),
    step text NOT NULL,
    message text NOT NULL,
    pct double precision NOT NULL CHECK (pct BETWEEN 0 AND 100),
    status text NOT NULL CHECK (status IN ('running','succeeded','failed'))
);
CREATE INDEX installer_events_installation_seq_idx ON installer_events(installation_id, seq);

CREATE TABLE installer_idempotency (
    principal text NOT NULL CHECK (length(trim(principal)) > 0),
    key_hash text NOT NULL CHECK (length(key_hash) = 64),
    request_hash text NOT NULL CHECK (length(request_hash) = 64),
    installation_id uuid NOT NULL REFERENCES installer_jobs(id) ON DELETE CASCADE,
    created_at timestamptz NOT NULL DEFAULT now(),
    expires_at timestamptz NOT NULL CHECK (expires_at > created_at),
    PRIMARY KEY (principal, key_hash)
);
CREATE INDEX installer_idempotency_expires_at_idx ON installer_idempotency(expires_at);

-- +goose Down
DROP TABLE installer_idempotency;
DROP TABLE installer_events;
DROP TABLE installer_jobs;
DROP TABLE installer_probe_idempotency;
DROP TABLE installer_probes;
