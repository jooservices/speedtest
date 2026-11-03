-- +goose Up
CREATE INDEX results_node_timeline_idx ON results(node_id, ts, id) INCLUDE (ip) WHERE ip IS NOT NULL;
CREATE TABLE node_ip_addresses (
    node_id uuid NOT NULL REFERENCES nodes(id) ON DELETE CASCADE,
    ip inet NOT NULL REFERENCES ip_addresses(ip),
    tests bigint NOT NULL CHECK (tests > 0),
    first_seen timestamptz NOT NULL,
    last_seen timestamptz NOT NULL CHECK (last_seen >= first_seen),
    speed_tests bigint NOT NULL CHECK (speed_tests BETWEEN 0 AND tests),
    down_sum numeric NOT NULL CHECK (down_sum >= 0),
    up_sum numeric NOT NULL CHECK (up_sum >= 0),
    PRIMARY KEY (node_id, ip)
);
-- Durable handoff: the alert engine marks consumed_at in its event transaction.
CREATE TABLE ip_change_signals (
    result_id uuid PRIMARY KEY REFERENCES results(id) ON DELETE CASCADE,
    node_id uuid NOT NULL REFERENCES nodes(id) ON DELETE CASCADE,
    ts timestamptz NOT NULL,
    from_ip inet NOT NULL REFERENCES ip_addresses(ip),
    to_ip inet NOT NULL REFERENCES ip_addresses(ip),
    consumed_at timestamptz,
    CHECK (from_ip <> to_ip)
);
CREATE INDEX ip_change_signals_pending_idx ON ip_change_signals(ts, result_id) WHERE consumed_at IS NULL;
INSERT INTO node_ip_addresses
SELECT node_id, ip, count(*), min(ts), max(ts), count(*) FILTER (WHERE status <> 'failed'),
       coalesce(sum(down_mbps::numeric) FILTER (WHERE status <> 'failed'),0),
       coalesce(sum(up_mbps::numeric) FILTER (WHERE status <> 'failed'),0)
FROM results WHERE ip IS NOT NULL GROUP BY node_id, ip;
-- Rebuild the derived timeline deterministically, without replaying old alerts.
DELETE FROM node_ip_history;
INSERT INTO node_ip_history (id,node_id,ts,from_ip,to_ip)
SELECT id,node_id,ts,previous_ip,ip FROM (
    SELECT id,node_id,ts,ip,lag(ip) OVER (PARTITION BY node_id ORDER BY ts,id) AS previous_ip FROM results WHERE ip IS NOT NULL
) timeline WHERE previous_ip IS DISTINCT FROM ip;

-- +goose Down
DROP TABLE ip_change_signals;
DROP TABLE node_ip_addresses;
DROP INDEX results_node_timeline_idx;
-- node_ip_history retains the derived history; the previous schema supports it.
