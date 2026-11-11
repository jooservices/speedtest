-- +goose Up
-- Preserve the existing global alert-rule configuration as its starting event setting.
WITH rules AS (
    SELECT DISTINCT ON (event_key)
        event_key, enabled, severity, channels,
        CASE
            WHEN metric = 'failed' THEN jsonb_build_object('for_tests', for_tests)
            WHEN metric IN ('down_pct','up_pct','ping','jitter','loss') THEN jsonb_build_object(metric, value)
            ELSE '{}'::jsonb
        END AS thresholds
    FROM (
        SELECT r.*,
            CASE metric
                WHEN 'offline' THEN 'node.offline'
                WHEN 'failed' THEN 'test.failed'
                WHEN 'down_pct' THEN 'quality.download_low'
                WHEN 'up_pct' THEN 'quality.upload_low'
                WHEN 'ping' THEN 'quality.ping_high'
                WHEN 'jitter' THEN 'quality.jitter_high'
                WHEN 'loss' THEN 'quality.loss_high'
                WHEN 'ip_changed' THEN 'network.ip_changed'
            END AS event_key
        FROM alert_rules r
        WHERE metric IN ('offline','failed','down_pct','up_pct','ping','jitter','loss','ip_changed')
          AND node_ids IS NULL AND engine IS NULL
    ) global_rules
    ORDER BY event_key, updated_at DESC, id
)
INSERT INTO notification_event_settings(event_key,enabled,severity,channels,thresholds)
SELECT event_key,enabled,severity,channels,thresholds FROM rules
ON CONFLICT(event_key) DO NOTHING;

-- Fresh or partially configured installations receive the baseline rules. Existing owner
-- rules remain untouched; a metric with any owner rule is considered intentionally configured.
WITH defaults(metric, name, op, value, for_tests, severity) AS (VALUES
    ('offline', 'Node offline', '>', 3.0::float8, 1, 'crit'),
    ('failed', 'Consecutive failed tests', '=', 1.0::float8, 2, 'crit'),
    ('down_pct', 'Download below expected', '<', 65.0::float8, 2, 'warn'),
    ('up_pct', 'Upload below expected', '<', 40.0::float8, 2, 'warn'),
    ('ping', 'Ping above limit', '>', 30.0::float8, 2, 'warn'),
    ('jitter', 'Jitter above limit', '>', 10.0::float8, 2, 'warn'),
    ('loss', 'Packet loss above limit', '>', 2.0::float8, 2, 'warn'),
    ('ip_changed', 'Public IP changed', '=', 0.0::float8, 1, 'info')
)
INSERT INTO alert_rules(id,name,metric,op,value,for_tests,node_ids,severity,channels,enabled,engine)
SELECT md5('joost78-default:' || d.metric)::uuid, d.name, d.metric, d.op, d.value, d.for_tests, NULL, d.severity,
       ARRAY['telegram']::text[], true, NULL
FROM defaults d
WHERE NOT EXISTS (SELECT 1 FROM alert_rules r WHERE r.metric=d.metric AND r.node_ids IS NULL AND r.engine IS NULL)
ON CONFLICT(id) DO NOTHING;

-- These rows only fill missing preferences; existing owner settings remain untouched.
INSERT INTO settings(key,value) VALUES
    ('notifyQuietStart','"23:00"'::jsonb),
    ('notifyQuietEnd','"07:00"'::jsonb),
    ('notifyQuietTimezone','"Asia/Bangkok"'::jsonb),
    ('notifyQuietCritical','true'::jsonb)
ON CONFLICT(key) DO NOTHING;

-- +goose Down
-- Remove only untouched defaults created by this migration. Customized rules survive rollback.
WITH defaults(metric, name, op, value, for_tests, severity) AS (VALUES
    ('offline', 'Node offline', '>', 3.0::float8, 1, 'crit'),
    ('failed', 'Consecutive failed tests', '=', 1.0::float8, 2, 'crit'),
    ('down_pct', 'Download below expected', '<', 65.0::float8, 2, 'warn'),
    ('up_pct', 'Upload below expected', '<', 40.0::float8, 2, 'warn'),
    ('ping', 'Ping above limit', '>', 30.0::float8, 2, 'warn'),
    ('jitter', 'Jitter above limit', '>', 10.0::float8, 2, 'warn'),
    ('loss', 'Packet loss above limit', '>', 2.0::float8, 2, 'warn'),
    ('ip_changed', 'Public IP changed', '=', 0.0::float8, 1, 'info')
)
DELETE FROM alert_rules r USING defaults d
WHERE r.id=md5('joost78-default:' || d.metric)::uuid AND r.name=d.name AND r.metric=d.metric
  AND r.op=d.op AND r.value=d.value AND r.for_tests=d.for_tests AND r.node_ids IS NULL
  AND r.severity=d.severity AND r.channels=ARRAY['telegram']::text[] AND r.enabled AND r.engine IS NULL;

-- Remove only rows that still equal their migrated source values. Owner-edited rules remain intact.
WITH rules AS (
    SELECT DISTINCT ON (event_key)
        event_key, enabled, severity, channels,
        CASE
            WHEN metric = 'failed' THEN jsonb_build_object('for_tests', for_tests)
            WHEN metric IN ('down_pct','up_pct','ping','jitter','loss') THEN jsonb_build_object(metric, value)
            ELSE '{}'::jsonb
        END AS thresholds
    FROM (
        SELECT r.*,
            CASE metric
                WHEN 'offline' THEN 'node.offline'
                WHEN 'failed' THEN 'test.failed'
                WHEN 'down_pct' THEN 'quality.download_low'
                WHEN 'up_pct' THEN 'quality.upload_low'
                WHEN 'ping' THEN 'quality.ping_high'
                WHEN 'jitter' THEN 'quality.jitter_high'
                WHEN 'loss' THEN 'quality.loss_high'
                WHEN 'ip_changed' THEN 'network.ip_changed'
            END AS event_key
        FROM alert_rules r
        WHERE metric IN ('offline','failed','down_pct','up_pct','ping','jitter','loss','ip_changed')
          AND node_ids IS NULL AND engine IS NULL
    ) global_rules
    ORDER BY event_key, updated_at DESC, id
)
DELETE FROM notification_event_settings e USING rules r
WHERE e.event_key=r.event_key AND e.enabled=r.enabled AND e.severity=r.severity
  AND e.channels=r.channels AND e.thresholds=r.thresholds
  AND e.cooldown_seconds=0 AND e.repeat_seconds=0
  AND e.maintenance_start IS NULL AND e.maintenance_end IS NULL
  AND e.maintenance_timezone='UTC' AND cardinality(e.isp_scopes)=0;

DELETE FROM settings s USING (VALUES
    ('notifyQuietStart','"23:00"'::jsonb),
    ('notifyQuietEnd','"07:00"'::jsonb),
    ('notifyQuietTimezone','"Asia/Bangkok"'::jsonb),
    ('notifyQuietCritical','true'::jsonb)
) AS defaults(key,value)
WHERE s.key=defaults.key AND s.value=defaults.value;
