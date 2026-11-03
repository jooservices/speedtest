-- +goose Up
-- Existing successful rows may predate the typed server object.
UPDATE results SET server = COALESCE(server, '{}'::jsonb) || jsonb_build_object('name', 'Server ' || server_id::text)
WHERE status IN ('ok','degraded') AND (server IS NULL OR
    (length(trim(COALESCE(server->>'name',''))) = 0 AND length(trim(COALESCE(server->>'host',''))) = 0));
ALTER TABLE results DROP CONSTRAINT results_check;
ALTER TABLE results ALTER COLUMN ip DROP NOT NULL;
ALTER TABLE results ADD CONSTRAINT results_ip_available CHECK (ip IS NOT NULL OR status = 'failed');
ALTER TABLE results ADD CONSTRAINT results_check CHECK (
    (status = 'failed' AND error IS NOT NULL AND length(trim(error)) > 0)
    OR (status IN ('ok','degraded') AND error IS NULL AND down_mbps IS NOT NULL
        AND up_mbps IS NOT NULL AND ping_ms IS NOT NULL AND server IS NOT NULL
        AND (length(trim(COALESCE(server->>'name',''))) > 0 OR length(trim(COALESCE(server->>'host',''))) > 0)));

-- +goose Down
-- Refuse a lossy rollback. Export/retain incompatible results before downgrading.
-- +goose StatementBegin
DO $$ BEGIN
 IF EXISTS (SELECT 1 FROM results WHERE ip IS NULL OR (status IN ('ok','degraded') AND server_id IS NULL)) THEN
  RAISE EXCEPTION 'Cannot downgrade: retain results with unknown IP/server ID before rollback';
 END IF;
END $$;
-- +goose StatementEnd
ALTER TABLE results DROP CONSTRAINT results_check;
ALTER TABLE results DROP CONSTRAINT results_ip_available;
ALTER TABLE results ALTER COLUMN ip SET NOT NULL;
ALTER TABLE results ADD CONSTRAINT results_check CHECK (
    (status = 'failed' AND length(trim(error)) > 0 AND error IS NOT NULL)
    OR (status IN ('ok','degraded') AND error IS NULL AND down_mbps IS NOT NULL
        AND up_mbps IS NOT NULL AND ping_ms IS NOT NULL AND server_id IS NOT NULL));
