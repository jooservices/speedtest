-- +goose Up
-- ISP entities derived from ASN/organisation already stored in ip_addresses.geo (JOOST-74).
-- Numbered 00014: JOOST-78 owns 00011/00012 and JOOST-73 owns 00013.
CREATE TABLE isps (
    id uuid PRIMARY KEY,
    asn bigint NOT NULL UNIQUE CHECK (asn > 0 AND asn <= 4294967295),
    organisation text NOT NULL CHECK (length(trim(organisation)) > 0),
    name text NOT NULL CHECK (length(trim(name)) > 0),
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX isps_name_lower_idx ON isps (lower(name));

CREATE TABLE isp_plans (
    id uuid PRIMARY KEY,
    isp_id uuid NOT NULL REFERENCES isps(id) ON DELETE CASCADE,
    name varchar(200) NOT NULL CHECK (length(trim(name)) > 0),
    advertised_down_mbps double precision NOT NULL CHECK (advertised_down_mbps > 0 AND advertised_down_mbps < 'Infinity'::float8),
    advertised_up_mbps double precision NOT NULL CHECK (advertised_up_mbps > 0 AND advertised_up_mbps < 'Infinity'::float8),
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX isp_plans_isp_id_name_lower_uidx ON isp_plans (isp_id, lower(name));
CREATE INDEX isp_plans_isp_id_idx ON isp_plans(isp_id);

ALTER TABLE alert_events ADD COLUMN isp_id uuid REFERENCES isps(id);
CREATE INDEX alert_events_isp_id_ts_idx ON alert_events(isp_id, ts DESC) WHERE isp_id IS NOT NULL;

ALTER TABLE ip_addresses ADD COLUMN isp_id uuid REFERENCES isps(id);
CREATE INDEX ip_addresses_isp_id_idx ON ip_addresses(isp_id) WHERE isp_id IS NOT NULL;

ALTER TABLE results ADD COLUMN isp_id uuid REFERENCES isps(id);
CREATE INDEX results_isp_id_ts_idx ON results(isp_id, ts DESC) WHERE isp_id IS NOT NULL;

ALTER TABLE node_ip_history ADD COLUMN from_isp_id uuid REFERENCES isps(id);
ALTER TABLE node_ip_history ADD COLUMN to_isp_id uuid REFERENCES isps(id);

ALTER TABLE nodes ADD COLUMN isp_plan_id uuid REFERENCES isp_plans(id) ON DELETE SET NULL;

-- Backfill ISPs from distinct ASN values already present in geolocation JSON.
-- Name mirrors NormaliseName: strip trailing corporate suffixes while keeping ASN unique.
INSERT INTO isps (id, asn, organisation, name)
SELECT gen_random_uuid(),
       asn,
       organisation,
       COALESCE(NULLIF(trim(both FROM regexp_replace(
           regexp_replace(organisation, '[[:space:],]+(group|corp\.?|corporation|inc\.?|incorporated|ltd\.?|limited|llc|co\.?|company|plc|gmbh|s\.?a\.?|bv|ag)\.?$', '', 'i'),
           '[[:space:],]+(group|corp\.?|corporation|inc\.?|incorporated|ltd\.?|limited|llc|co\.?|company|plc|gmbh|s\.?a\.?|bv|ag)\.?$', '', 'i')), ''), organisation)
FROM (
    SELECT DISTINCT ON (((geo->>'asn')::bigint))
           (geo->>'asn')::bigint AS asn,
           COALESCE(NULLIF(trim(geo->>'organisation'), ''), 'AS' || (geo->>'asn')) AS organisation
    FROM ip_addresses
    WHERE geo ? 'asn'
      AND (geo->>'asn') ~ '^[0-9]+$'
      AND length(geo->>'asn') <= 10
      AND (geo->>'asn')::bigint > 0
      AND (geo->>'asn')::bigint < 4294967296
    ORDER BY ((geo->>'asn')::bigint), looked_up_at DESC NULLS LAST, created_at DESC
) src;

UPDATE ip_addresses a
SET isp_id = i.id
FROM isps i
WHERE a.isp_id IS NULL
  AND a.geo ? 'asn'
  AND (a.geo->>'asn') ~ '^[0-9]+$'
  AND length(a.geo->>'asn') <= 10
  AND (a.geo->>'asn')::bigint = i.asn;

UPDATE results r
SET isp_id = a.isp_id
FROM ip_addresses a
WHERE r.isp_id IS NULL
  AND r.ip IS NOT NULL
  AND a.ip = r.ip
  AND a.isp_id IS NOT NULL;

UPDATE node_ip_history h
SET from_isp_id = (SELECT a.isp_id FROM ip_addresses a WHERE a.ip = h.from_ip),
    to_isp_id = (SELECT a.isp_id FROM ip_addresses a WHERE a.ip = h.to_ip)
WHERE EXISTS (SELECT 1 FROM ip_addresses a WHERE a.ip = h.to_ip AND a.isp_id IS NOT NULL)
   OR EXISTS (SELECT 1 FROM ip_addresses a WHERE a.ip IS NOT DISTINCT FROM h.from_ip AND a.isp_id IS NOT NULL);

UPDATE alert_events e
SET isp_id = COALESCE(
    (SELECT r.isp_id FROM results r WHERE r.id=e.trigger_result_id),
    (SELECT h.to_isp_id FROM node_ip_history h WHERE h.node_id=e.node_id AND h.ts <= e.ts AND h.to_isp_id IS NOT NULL ORDER BY h.ts DESC,h.id DESC LIMIT 1),
    (SELECT r.isp_id FROM results r WHERE r.node_id=e.node_id AND r.ts <= e.ts AND r.isp_id IS NOT NULL ORDER BY r.ts DESC,r.id DESC LIMIT 1)
)
WHERE e.isp_id IS NULL;

-- +goose Down
DROP INDEX alert_events_isp_id_ts_idx;
ALTER TABLE alert_events DROP COLUMN isp_id;
ALTER TABLE nodes DROP COLUMN isp_plan_id;
ALTER TABLE node_ip_history DROP COLUMN to_isp_id;
ALTER TABLE node_ip_history DROP COLUMN from_isp_id;
DROP INDEX results_isp_id_ts_idx;
ALTER TABLE results DROP COLUMN isp_id;
DROP INDEX ip_addresses_isp_id_idx;
ALTER TABLE ip_addresses DROP COLUMN isp_id;
DROP TABLE isp_plans;
DROP TABLE isps;
