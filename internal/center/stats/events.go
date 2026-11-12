package stats

import (
	"context"
	"fmt"
	"time"
)

const MaxEvents = 500

type Event struct {
	ID     string    `json:"id"`
	NodeID string    `json:"nodeId"`
	TS     time.Time `json:"ts"`
	Type   string    `json:"type"`
	Label  string    `json:"label"`
}

type EventsResponse struct {
	Items     []Event `json:"items"`
	Truncated bool    `json:"truncated"`
}

const eventsSQL = `WITH selected_nodes AS (` + selectedNodes + `),
scoped AS (
 SELECT r.id, r.node_id, r.ts, r.engine, r.ip,
        CASE WHEN NULLIF(btrim(r.engine_isp),'') IS NOT NULL THEN btrim(r.engine_isp)
             ELSE COALESCE(NULLIF(btrim((SELECT address.geo->>'organisation' FROM ip_addresses address WHERE address.ip=r.ip)),''),'') END AS isp
 FROM results r JOIN selected_nodes n ON n.id=r.node_id
 WHERE r.ts >= $2 AND r.ts < $3
   AND r.engine=COALESCE(NULLIF($4::text,''),n.primary_engine)
),
window_groups AS (SELECT DISTINCT node_id,engine FROM scoped),
prior AS (
 SELECT g.node_id,g.engine,p.id,p.ts,p.isp
 FROM window_groups g
 JOIN LATERAL (
   SELECT r.id,r.ts,COALESCE(NULLIF(btrim(r.engine_isp),''),NULLIF(btrim(a.geo->>'organisation'),''),'') AS isp
   FROM results r LEFT JOIN ip_addresses a ON a.ip=r.ip
   WHERE r.node_id=g.node_id AND r.engine=g.engine AND r.ts < $2
     AND COALESCE(NULLIF(btrim(r.engine_isp),''),NULLIF(btrim(a.geo->>'organisation'),''),'') <> ''
   ORDER BY r.ts DESC,r.id DESC LIMIT 1
 ) p ON true
),
prior_ip AS (
 SELECT g.node_id,g.engine,p.id,p.ts,p.ip
 FROM window_groups g
 JOIN LATERAL (
   SELECT r.id,r.ts,r.ip
   FROM results r
   WHERE r.node_id=g.node_id AND r.engine=g.engine AND r.ts < $2 AND r.ip IS NOT NULL
   ORDER BY r.ts DESC,r.id DESC LIMIT 1
 ) p ON true
),
known AS (
 SELECT id,node_id,engine,ts,isp FROM scoped WHERE isp <> ''
 UNION ALL SELECT id,node_id,engine,ts,isp FROM prior
),
known_ip AS (
 SELECT id,node_id,engine,ts,ip,isp FROM scoped WHERE ip IS NOT NULL
 UNION ALL SELECT id,node_id,engine,ts,ip,''::text AS isp FROM prior_ip
),
ordered AS (
 SELECT id,node_id,engine,ts,isp,
        lag(isp) OVER (PARTITION BY node_id,engine ORDER BY ts,id) AS previous_isp
 FROM known
),
ordered_ip AS (
 SELECT id,node_id,engine,ts,ip,isp,
        lag(ip) OVER (PARTITION BY node_id,engine ORDER BY ts,id) AS previous_ip
 FROM known_ip
),
ip_events AS (
 SELECT id::text,node_id::text,ts,'ip_change'::text AS type,
        'IP '||host(previous_ip)||' → '||host(ip) AS label
 FROM ordered_ip
 WHERE previous_ip IS NOT NULL AND ip <> previous_ip
   AND ts >= $2 AND ts < $3
   AND ($5::text='' OR (isp <> '' AND lower(isp)=lower($5::text)))
),
isp_events AS (
 SELECT id::text,node_id::text,ts,'isp_change'::text AS type,
        'ISP '||previous_isp||' → '||isp AS label
 FROM ordered
 WHERE previous_isp IS NOT NULL AND lower(previous_isp) <> lower(isp)
   AND ts >= $2 AND ts < $3
   AND ($5::text='' OR lower(isp)=lower($5::text))
),
linked_alerts AS MATERIALIZED (
 SELECT e.id,e.node_id,e.ts,e.title,e.detail,r.node_id AS result_node_id,r.engine,r.engine_isp,r.ip
 FROM alert_events e JOIN results r ON r.id=e.trigger_result_id
 WHERE e.ts >= $2 AND e.ts < $3
),
linked_alert_marks AS (
 SELECT e.id::text,e.node_id::text,e.ts,'alert'::text AS type,
        e.title || CASE WHEN btrim(e.detail) <> '' THEN ': '||btrim(e.detail) ELSE '' END AS label
 FROM linked_alerts e JOIN selected_nodes n ON n.id=e.node_id
 LEFT JOIN ip_addresses a ON a.ip=e.ip
 WHERE e.result_node_id=e.node_id
   AND e.engine=COALESCE(NULLIF($4::text,''),n.primary_engine)
   AND ($5::text='' OR lower(COALESCE(NULLIF(btrim(e.engine_isp),''),NULLIF(btrim(a.geo->>'organisation'),''),''))=lower($5::text))
),
unlinked_alert_marks AS (
 SELECT e.id::text,e.node_id::text,e.ts,'alert'::text AS type,
        e.title || CASE WHEN btrim(e.detail) <> '' THEN ': '||btrim(e.detail) ELSE '' END || ' (engine unknown)' AS label
 FROM alert_events e JOIN selected_nodes n ON n.id=e.node_id
 WHERE e.ts >= $2 AND e.ts < $3 AND e.trigger_result_id IS NULL AND $5::text=''
),
all_events AS (
 SELECT * FROM ip_events UNION ALL SELECT * FROM isp_events
 UNION ALL SELECT * FROM linked_alert_marks UNION ALL SELECT * FROM unlinked_alert_marks
)
SELECT id,node_id,ts,type,label FROM all_events
ORDER BY ts DESC,type,id LIMIT 501`

func (s *Store) Events(ctx context.Context, q Query) (EventsResponse, error) {
	if q.NodesSet && len(q.NodeIDs) == 0 {
		return EventsResponse{Items: []Event{}}, nil
	}
	rows, err := s.DB.Query(ctx, eventsSQL, q.nodeIDs(), q.From, q.To, q.Engine, q.ISP)
	if err != nil {
		return EventsResponse{}, fmt.Errorf("query statistics events: %w", err)
	}
	defer rows.Close()
	return collectEvents(rows)
}

func collectEvents(rows interface {
	Next() bool
	Scan(...any) error
	Err() error
	Close()
}) (EventsResponse, error) {
	result := EventsResponse{Items: make([]Event, 0, MaxEvents)}
	for rows.Next() {
		var item Event
		if err := rows.Scan(&item.ID, &item.NodeID, &item.TS, &item.Type, &item.Label); err != nil {
			return EventsResponse{}, fmt.Errorf("scan statistics event: %w", err)
		}
		item.TS = item.TS.UTC()
		if len(result.Items) == MaxEvents {
			result.Truncated = true
			break
		}
		result.Items = append(result.Items, item)
	}
	if err := rows.Err(); err != nil {
		return EventsResponse{}, fmt.Errorf("read statistics events: %w", err)
	}
	return result, nil
}
