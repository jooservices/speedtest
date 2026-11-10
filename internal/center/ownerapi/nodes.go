package ownerapi

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jooservices/speedtest/internal/center/nodes"
	"github.com/jooservices/speedtest/internal/center/release"
	"github.com/jooservices/speedtest/internal/shared/agentid"
)

var (
	// ErrNotFound reports an unknown or already deleted resource.
	ErrNotFound = errors.New("not found")
	// ErrStale reports an If-Match value that no longer matches (412).
	ErrStale = errors.New("stale entity tag")
	// ErrConflict reports a state conflict (409), for example a duplicate name or a paused node.
	ErrConflict = errors.New("conflict")
	// ErrInvalid reports invalid input (422).
	ErrInvalid = errors.New("invalid input")
)

// HeartbeatTimeout is how long after its last heartbeat a node is shown as offline.
const HeartbeatTimeout = 2 * time.Minute

// DB is the subset of pgxpool.Pool the store uses.
type DB interface {
	Begin(context.Context) (pgx.Tx, error)
	Query(context.Context, string, ...any) (pgx.Rows, error)
	QueryRow(context.Context, string, ...any) pgx.Row
}

// Store reads and changes nodes and their commands for the signed-in owner.
type Store struct {
	Releases *release.Catalog
	DB       DB
	Now      func() time.Time
}

func (s *Store) now() time.Time {
	if s.Now != nil {
		return s.Now().UTC()
	}
	return time.Now().UTC()
}

const nodeColumns = `n.id::text, n.name, n.label, n.provider, n.plan, n.isp_plan_id::text, n.expected_down_mbps, n.expected_up_mbps, n.threads,
	n.server_id, n.color_slot, n.enabled, n.auto_update, n.binding_interface, host(n.binding_source_ip), n.note,
	n.config_revision, s.interval_minutes, s.offset_minutes, s.enabled, n.host, n.agent, n.created_at, n.last_seen_at,n.installed_engines,n.primary_engine`

type nodeRow struct {
	Engines                                                   []string
	PrimaryEngine                                             string
	ID, Name, Label, Provider, Plan, Iface, Note, Host, Agent string
	ISPPlanID                                                 *string
	Down, Up                                                  float64
	Threads, ColorSlot, Interval, Offset                      int32
	ServerID                                                  *int32
	Enabled, AutoUpdate, ScheduleOn                           bool
	SourceIP                                                  *string
	Revision                                                  int64
	CreatedAt                                                 time.Time
	LastSeen                                                  *time.Time
}

func scanNode(row pgx.Row) (nodeRow, error) {
	var n nodeRow
	err := row.Scan(&n.ID, &n.Name, &n.Label, &n.Provider, &n.Plan, &n.ISPPlanID, &n.Down, &n.Up, &n.Threads, &n.ServerID, &n.ColorSlot,
		&n.Enabled, &n.AutoUpdate, &n.Iface, &n.SourceIP, &n.Note, &n.Revision, &n.Interval, &n.Offset, &n.ScheduleOn,
		&n.Host, &n.Agent, &n.CreatedAt, &n.LastSeen, &n.Engines, &n.PrimaryEngine)
	return n, err
}

// ETag is the strong entity tag of a node's configuration.
func ETag(revision int64) string { return fmt.Sprintf(`"node-%d"`, revision) }

func (n nodeRow) input() nodes.Input {
	in := nodes.Input{Name: n.Name, Label: n.Label, Provider: n.Provider, Plan: n.Plan, ISPPlanID: n.ISPPlanID, ExpectedDown: n.Down,
		ExpectedUp: n.Up, Threads: int(n.Threads), Note: n.Note, Enabled: n.Enabled, AutoUpdate: n.AutoUpdate,
		Binding: nodes.Binding{Iface: n.Iface, SourceIP: n.SourceIP}}
	if n.ServerID != nil {
		id := int(*n.ServerID)
		in.ServerID = &id
	}
	return in
}

// status derives the displayed state: paused, offline, failed, degraded or online.
func (n nodeRow) status(now time.Time, latest map[string]any) string {
	switch {
	case !n.Enabled || !n.ScheduleOn:
		return "paused"
	case n.LastSeen == nil || now.Sub(*n.LastSeen) > HeartbeatTimeout:
		return "offline"
	}
	if latest != nil {
		if st, _ := latest["status"].(string); st == "failed" || st == "degraded" {
			return st
		}
	}
	return "online"
}

func (n nodeRow) view(now time.Time, latest, latestGood map[string]any, currentIP string, geo, currentISP map[string]any) map[string]any {
	var server any = "auto"
	if n.ServerID != nil {
		server = int64(*n.ServerID)
	}
	var lastSeen, ip any
	if n.LastSeen != nil {
		lastSeen = n.LastSeen.UTC().Format(time.RFC3339Nano)
	}
	if currentIP != "" {
		ip = currentIP
	}
	obj := map[string]any{
		"id": n.ID, "name": n.Name, "label": n.Label, "provider": n.Provider, "plan": n.Plan, "ispPlanId": nullable(n.ISPPlanID),
		"expectedDown": n.Down, "expectedUp": n.Up, "threads": int64(n.Threads), "server": server, "note": n.Note,
		"engines": n.Engines, "primaryEngine": n.PrimaryEngine, "enabled": n.Enabled, "autoUpdate": n.AutoUpdate,
		"binding":   map[string]any{"iface": n.Iface, "sourceIp": nullable(n.SourceIP)},
		"colorSlot": int64(n.ColorSlot), "interval": int64(n.Interval), "offset": int64(n.Offset), "host": n.Host, "agent": displayAgent(n.Agent),
		"createdAt": n.CreatedAt.UTC().Format(time.RFC3339Nano), "lastSeen": lastSeen,
		"status": n.status(now, latest), "latest": nilMap(latest), "latestGood": nilMap(latestGood),
		"currentIp": ip, "geo": nilMap(geo), "isp": nilMap(currentISP), "updateVersion": nil,
	}
	return obj
}

func nilMap(m map[string]any) any {
	if m == nil {
		return nil
	}
	return m
}

// extras loads, for the given nodes, the newest result, the newest non-failed result and the
// current public address with its stored location.
type extras struct {
	latest, latestGood map[string]map[string]any
	ip                 map[string]string
	geo                map[string]map[string]any
	isp                map[string]map[string]any
}

func (s *Store) loadExtras(ctx context.Context, q interface {
	Query(context.Context, string, ...any) (pgx.Rows, error)
}, ids []string) (extras, error) {
	ex := extras{latest: map[string]map[string]any{}, latestGood: map[string]map[string]any{}, ip: map[string]string{}, geo: map[string]map[string]any{}, isp: map[string]map[string]any{}}
	if len(ids) == 0 {
		return ex, nil
	}
	for _, spec := range []struct {
		dest  map[string]map[string]any
		where string
	}{{ex.latest, ""}, {ex.latestGood, " AND r.status <> 'failed'"}} {
		rows, err := q.Query(ctx, `SELECT DISTINCT ON (r.node_id) `+ResultColumns+` FROM results r JOIN nodes n ON n.id=r.node_id WHERE r.engine=n.primary_engine AND r.node_id = ANY($1::uuid[])`+spec.where+` ORDER BY r.node_id, r.ts DESC, r.id DESC`, ids)
		if err != nil {
			return ex, err
		}
		for rows.Next() {
			v, err := ScanResult(rows)
			if err != nil {
				rows.Close()
				return ex, err
			}
			spec.dest[v.Object["nodeId"].(string)] = v.Object
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return ex, err
		}
	}
	rows, err := q.Query(ctx, `SELECT DISTINCT ON (r.node_id) r.node_id::text, host(r.ip), a.source, a.geo,
		i.id::text, i.asn, i.organisation, i.name
		FROM results r
		LEFT JOIN ip_addresses a ON a.ip = r.ip
		LEFT JOIN isps i ON i.id = COALESCE(r.isp_id, a.isp_id)
		WHERE r.node_id = ANY($1::uuid[]) AND r.ip IS NOT NULL ORDER BY r.node_id, r.ts DESC, r.id DESC`, ids)
	if err != nil {
		return ex, err
	}
	defer rows.Close()
	for rows.Next() {
		var node, ip string
		var source, ispID, org, name *string
		var asn *uint
		var geo []byte
		if err := rows.Scan(&node, &ip, &source, &geo, &ispID, &asn, &org, &name); err != nil {
			return ex, err
		}
		ex.ip[node] = ip
		src := ""
		if source != nil {
			src = *source
		}
		if g := GeoObject(ip, src, geo); g != nil {
			ex.geo[node] = g
		}
		if ispID != nil && asn != nil && org != nil && name != nil {
			ex.isp[node] = map[string]any{"id": *ispID, "asn": *asn, "organisation": *org, "name": *name}
		}
	}
	return ex, rows.Err()
}

// List returns one page of nodes sorted by name then id, and the total count.
func (s *Store) List(ctx context.Context, page, size int) ([]map[string]any, int, error) {
	var total int
	if err := s.DB.QueryRow(ctx, `SELECT count(*) FROM nodes`).Scan(&total); err != nil {
		return nil, 0, err
	}
	rows, err := s.DB.Query(ctx, `SELECT `+nodeColumns+` FROM nodes n JOIN engine_schedules s ON s.node_id = n.id AND s.engine=n.primary_engine ORDER BY lower(n.name), n.id LIMIT $1 OFFSET $2`, size, (page-1)*size)
	if err != nil {
		return nil, 0, err
	}
	var list []nodeRow
	for rows.Next() {
		n, err := scanNode(rows)
		if err != nil {
			rows.Close()
			return nil, 0, err
		}
		list = append(list, n)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, 0, err
	}
	ids := make([]string, len(list))
	for i, n := range list {
		ids[i] = n.ID
	}
	ex, err := s.loadExtras(ctx, s.DB, ids)
	if err != nil {
		return nil, 0, err
	}
	now := s.now()
	out := make([]map[string]any, 0, len(list))
	for _, n := range list {
		out = append(out, s.nodeView(ctx, s.DB, n, now, ex.latest[n.ID], ex.latestGood[n.ID], ex.ip[n.ID], ex.geo[n.ID], ex.isp[n.ID]))
	}
	return out, total, nil
}

func (s *Store) one(ctx context.Context, q interface {
	Query(context.Context, string, ...any) (pgx.Rows, error)
	QueryRow(context.Context, string, ...any) pgx.Row
}, id string, lock bool) (map[string]any, nodeRow, error) {
	suffix := ""
	if lock {
		suffix = " FOR UPDATE OF n"
	}
	n, err := scanNode(q.QueryRow(ctx, `SELECT `+nodeColumns+` FROM nodes n JOIN engine_schedules s ON s.node_id = n.id AND s.engine=n.primary_engine WHERE n.id = $1`+suffix, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nodeRow{}, ErrNotFound
	}
	if err != nil {
		return nil, nodeRow{}, err
	}
	ex, err := s.loadExtras(ctx, q, []string{id})
	if err != nil {
		return nil, nodeRow{}, err
	}
	return s.nodeView(ctx, q, n, s.now(), ex.latest[id], ex.latestGood[id], ex.ip[id], ex.geo[id], ex.isp[id]), n, nil
}

// Get returns one node and its strong entity tag.
func (s *Store) Get(ctx context.Context, id string) (map[string]any, string, error) {
	view, n, err := s.one(ctx, s.DB, id, false)
	return view, ETag(n.Revision), err
}

// Update applies a JSON merge patch under an If-Match precondition and returns the new node.
func (s *Store) Update(ctx context.Context, id string, patch map[string]json.RawMessage, ifMatch string) (map[string]any, string, error) {
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		return nil, "", err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	_, n, err := s.one(ctx, tx, id, true)
	if err != nil {
		return nil, "", err
	}
	if ifMatch != ETag(n.Revision) {
		return nil, "", ErrStale
	}
	in := n.input()
	if err := applyPatch(&in, patch); err != nil {
		return nil, "", err
	}
	planChanged := !sameNullable(n.ISPPlanID, in.ISPPlanID)
	if planChanged && in.ISPPlanID != nil {
		down, up, err := planDefaultsForNode(ctx, tx, id, *in.ISPPlanID)
		if err != nil {
			return nil, "", err
		}
		if _, explicit := patch["expectedDown"]; !explicit {
			in.ExpectedDown = down
		}
		if _, explicit := patch["expectedUp"]; !explicit {
			in.ExpectedUp = up
		}
	}
	in, err = in.Normalize()
	if err != nil {
		return nil, "", ErrInvalid
	}
	_, err = tx.Exec(ctx, `UPDATE nodes SET name=$2,label=$3,provider=$4,plan=$5,isp_plan_id=$6::uuid,expected_down_mbps=$7,expected_up_mbps=$8,threads=$9,
		server_id=$10,enabled=$11,auto_update=$12,binding_interface=$13,binding_source_ip=$14,note=$15,
		config_revision=config_revision+1,updated_at=now() WHERE id=$1`,
		id, in.Name, in.Label, in.Provider, in.Plan, in.ISPPlanID, in.ExpectedDown, in.ExpectedUp, in.Threads, in.ServerID,
		in.Enabled, in.AutoUpdate, in.Binding.Iface, in.Binding.SourceIP, in.Note)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			return nil, "", ErrConflict
		}
		return nil, "", err
	}
	view, updated, err := s.one(ctx, tx, id, false)
	if err != nil {
		return nil, "", err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, "", err
	}
	return view, ETag(updated.Revision), nil
}

func sameNullable(a, b *string) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

func planDefaultsForNode(ctx context.Context, tx interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}, nodeID, planID string) (float64, float64, error) {
	var planISP string
	var down, up float64
	if err := tx.QueryRow(ctx, `SELECT isp_id::text,advertised_down_mbps,advertised_up_mbps FROM isp_plans WHERE id=$1::uuid`, planID).Scan(&planISP, &down, &up); errors.Is(err, pgx.ErrNoRows) {
		return 0, 0, ErrInvalid
	} else if err != nil {
		return 0, 0, err
	}
	var currentISP *string
	err := tx.QueryRow(ctx, `SELECT (SELECT COALESCE(r.isp_id,a.isp_id)::text FROM results r
		LEFT JOIN ip_addresses a ON a.ip=r.ip WHERE r.node_id=$1::uuid AND r.ip IS NOT NULL
		ORDER BY r.ts DESC,r.id DESC LIMIT 1)`, nodeID).Scan(&currentISP)
	if err != nil {
		return 0, 0, err
	}
	if currentISP != nil && *currentISP != planISP {
		return 0, 0, ErrInvalid
	}
	return down, up, nil
}

func applyPatch(in *nodes.Input, patch map[string]json.RawMessage) error {
	text := func(key string, dest *string) error {
		raw, ok := patch[key]
		if !ok {
			return nil
		}
		return json.Unmarshal(raw, dest)
	}
	for key, raw := range patch {
		if string(raw) == "null" && key != "binding" && key != "ispPlanId" {
			return ErrInvalid // merge-patch null would remove a required property
		}
	}
	for key, dest := range map[string]*string{"name": &in.Name, "label": &in.Label, "provider": &in.Provider, "plan": &in.Plan, "note": &in.Note} {
		if err := text(key, dest); err != nil {
			return ErrInvalid
		}
	}
	if raw, ok := patch["ispPlanId"]; ok {
		if string(raw) == "null" {
			in.ISPPlanID = nil
		} else {
			var planID string
			if json.Unmarshal(raw, &planID) != nil || !validUUID(planID) {
				return ErrInvalid
			}
			in.ISPPlanID = &planID
		}
	}
	for key, dest := range map[string]*float64{"expectedDown": &in.ExpectedDown, "expectedUp": &in.ExpectedUp} {
		if raw, ok := patch[key]; ok && json.Unmarshal(raw, dest) != nil {
			return ErrInvalid
		}
	}
	for key, dest := range map[string]*bool{"enabled": &in.Enabled, "autoUpdate": &in.AutoUpdate} {
		if raw, ok := patch[key]; ok && json.Unmarshal(raw, dest) != nil {
			return ErrInvalid
		}
	}
	if raw, ok := patch["threads"]; ok {
		// Zero would silently fall back to the default in Normalize; the contract says 1 to 64.
		if json.Unmarshal(raw, &in.Threads) != nil || in.Threads < 1 {
			return ErrInvalid
		}
	}
	if raw, ok := patch["server"]; ok {
		var sel nodes.ServerSelection
		if sel.UnmarshalJSON(raw) != nil {
			return ErrInvalid
		}
		in.ServerID = sel.ID
	}
	return applyBindingPatch(in, patch)
}

// Delete removes a node and everything that belongs to it under an If-Match precondition.
func (s *Store) Delete(ctx context.Context, id, ifMatch string) error {
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	_, n, err := s.one(ctx, tx, id, true)
	if err != nil {
		return err
	}
	if ifMatch != ETag(n.Revision) {
		return ErrStale
	}
	if _, err := tx.Exec(ctx, `DELETE FROM nodes WHERE id = $1`, id); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

var commandTypes = map[string]bool{"run_now": true, "pause": true, "resume": true, "update_config": true, "update_agent": true}

const commandColumns = `id::text, node_id::text, type, version, status, created_at, acked_at, phase, pct, result_id::text, error`

func scanCommand(row pgx.Row) (map[string]any, error) {
	var id, nodeID, typ, status, phase string
	var version, resultID, errText *string
	var created time.Time
	var acked *time.Time
	var pct float64
	if err := row.Scan(&id, &nodeID, &typ, &version, &status, &created, &acked, &phase, &pct, &resultID, &errText); err != nil {
		return nil, err
	}
	var ackedAt any
	if acked != nil {
		ackedAt = acked.UTC().Format(time.RFC3339Nano)
	}
	return map[string]any{"id": id, "nodeId": nodeID, "type": typ, "version": nullable(version), "status": status,
		"createdAt": created.UTC().Format(time.RFC3339Nano), "ackedAt": ackedAt, "phase": phase, "pct": pct,
		"resultId": nullable(resultID), "error": nullable(errText)}, nil
}

// QueueCommand queues a control command for a node. Pause, resume and update_config also change
// the node's effective configuration revision; run_now is refused while the node is paused or
// another run is already pending.
func (s *Store) QueueCommand(ctx context.Context, id, typ, version string) (map[string]any, error) {
	if !commandTypes[typ] || (typ == "update_agent" && (strings.TrimSpace(version) == "" || len(version) > 64)) || (typ != "update_agent" && version != "") {
		return nil, ErrInvalid
	}
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	_, n, err := s.one(ctx, tx, id, true)
	if err != nil {
		return nil, err
	}
	switch typ {
	case "run_now":
		if !n.Enabled || !n.ScheduleOn {
			return nil, ErrConflict
		}
		var open int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM agent_commands WHERE node_id=$1 AND type='run_now' AND status IN ('pending','running')`, id).Scan(&open); err != nil {
			return nil, err
		}
		if open > 0 {
			return nil, ErrConflict
		}
	case "pause", "resume":
		if _, err := tx.Exec(ctx, `UPDATE nodes SET enabled=$2, config_revision=config_revision+1, updated_at=now() WHERE id=$1`, id, typ == "resume"); err != nil {
			return nil, err
		}
	case "update_config":
		if _, err := tx.Exec(ctx, `UPDATE nodes SET config_revision=config_revision+1, updated_at=now() WHERE id=$1`, id); err != nil {
			return nil, err
		}
	}
	commandID, err := newUUID()
	if err != nil {
		return nil, err
	}
	var v *string
	if version != "" {
		v = &version
	}
	cmd, err := scanCommand(tx.QueryRow(ctx, `INSERT INTO agent_commands (id,node_id,type,version) VALUES ($1,$2,$3,$4) RETURNING `+commandColumns, commandID, id, typ, v))
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return cmd, nil
}

// Command returns the progress of one queued command.
func (s *Store) Command(ctx context.Context, id, commandID string) (map[string]any, error) {
	cmd, err := scanCommand(s.DB.QueryRow(ctx, `SELECT `+commandColumns+` FROM agent_commands WHERE id=$1 AND node_id=$2`, commandID, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	return cmd, err
}

func newUUID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	h := hex.EncodeToString(b[:])
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:], nil
}

func displayAgent(agent string) string { display, _ := agentid.Parse(agent); return display }

func (s *Store) nodeView(ctx context.Context, q interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}, n nodeRow, now time.Time, latest, latestGood map[string]any, ip string, geo, currentISP map[string]any) map[string]any {
	view := n.view(now, latest, latestGood, ip, geo, currentISP)
	if s.Releases != nil {
		var raw []byte
		err := q.QueryRow(ctx, `SELECT value FROM settings WHERE key='agentRelease'`).Scan(&raw)
		var advertised map[string]any
		if errors.Is(err, pgx.ErrNoRows) || err == nil && (len(raw) == 0 || string(raw) == "null") {
			advertised = s.Releases.Advertisement(n.Agent)
		} else if err == nil {
			_ = json.Unmarshal(raw, &advertised)
		}
		view["updateVersion"] = release.UpdateVersion(n.Agent, advertised)
	}
	return view
}

func applyBindingPatch(in *nodes.Input, patch map[string]json.RawMessage) error {
	if raw, ok := patch["binding"]; ok {
		var b struct {
			Iface    *string         `json:"iface"`
			SourceIP json.RawMessage `json:"sourceIp"`
		}
		if json.Unmarshal(raw, &b) != nil {
			return ErrInvalid
		}
		if b.Iface != nil {
			in.Binding.Iface = *b.Iface
		}
		if len(b.SourceIP) > 0 {
			if string(b.SourceIP) == "null" {
				in.Binding.SourceIP = nil
			} else {
				var ip string
				if json.Unmarshal(b.SourceIP, &ip) != nil || net.ParseIP(ip) == nil {
					return ErrInvalid
				}
				in.Binding.SourceIP = &ip
			}
		}
	}
	return nil
}
