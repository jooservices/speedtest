package agentapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// ResultHook runs after insertion in the same transaction. Implementations must
// use tx so a hook failure rolls back the result too. Replays never invoke it.
// JOOST-35 can implement IP history without changing the ingestion protocol.
type ResultHook interface {
	AfterResultStored(context.Context, pgx.Tx, string, Result) error
}

type Database interface {
	Begin(context.Context) (pgx.Tx, error)
	Query(context.Context, string, ...any) (pgx.Rows, error)
	QueryRow(context.Context, string, ...any) pgx.Row
}

type Postgres struct {
	Pool Database
	Hook ResultHook
}

func jsonValue(value any) any {
	if value == nil {
		return nil
	}
	data, _ := json.Marshal(value)
	if string(data) == "null" {
		return nil
	}
	return string(data)
}
func (r Result) values(node string) ([]string, []any) {
	ts, _ := time.Parse(time.RFC3339Nano, r.TS)
	return []string{"id", "node_id", "ts", "status", "down_mbps", "up_mbps", "ping_ms", "jitter_ms", "down_latency_ms", "up_latency_ms", "loss_percent", "server_id", "ip", "threads", "bytes_down", "bytes_up", "duration_seconds", "error", "manual", "raw", "loaded_down", "loaded_up", "server", "engine_isp", "engine_result_id", "engine_result_url", "engine_ip", "warnings"},
		[]any{r.ID, node, ts, r.Status, r.Down, r.Up, r.Ping, r.Jitter, r.DLat, r.ULat, r.Loss, r.ServerID, r.IP, r.Threads, r.BytesDown, r.BytesUp, r.Duration, r.Error, r.Manual, jsonValue(r.Raw), jsonValue(r.LoadedDown), jsonValue(r.LoadedUp), jsonValue(r.Server), r.EngineISP, r.EngineResultID, r.EngineResultURL, r.EngineIP, jsonValue(r.Warnings)}
}

func (s *Postgres) SaveResult(ctx context.Context, node string, r Result) (bool, error) {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if r.IP != nil {
		if _, err = tx.Exec(ctx, `INSERT INTO ip_addresses(ip) VALUES ($1) ON CONFLICT DO NOTHING`, r.IP); err != nil {
			return false, err
		}
	}
	columns, args := r.values(node)
	params := make([]string, len(columns))
	comparisons := make([]string, len(columns))
	for i, column := range columns {
		params[i] = fmt.Sprintf("$%d", i+1)
		comparisons[i] = column + " IS NOT DISTINCT FROM " + params[i]
	}
	query := "INSERT INTO results (" + strings.Join(columns, ",") + ") VALUES (" + strings.Join(params, ",") + ") ON CONFLICT(id) DO NOTHING"
	tag, err := tx.Exec(ctx, query, args...)
	if err != nil {
		return false, err
	}
	if tag.RowsAffected() == 0 {
		var identical bool
		// A new READ COMMITTED statement sees a concurrent insert after ON CONFLICT
		// waits for it. Compare every stored field, never return foreign row data.
		err = tx.QueryRow(ctx, "SELECT "+strings.Join(comparisons, " AND ")+" FROM results WHERE id=$1", args...).Scan(&identical)
		if err != nil {
			return false, err
		}
		if !identical {
			return false, ErrConflict
		}
		return true, nil
	}
	if s.Hook != nil && r.IP != nil {
		if err = s.Hook.AfterResultStored(ctx, tx, node, r); err != nil {
			return false, err
		}
	}
	return false, tx.Commit(ctx)
}

func (s *Postgres) Config(ctx context.Context, node string) (map[string]any, error) {
	var id, name, iface string
	var down, up float64
	var threads, interval, offset int
	var server *int
	var source *string
	var enabled, autoUpdate bool
	var revision, scheduleRevision int64
	var settings []byte
	err := s.Pool.QueryRow(ctx, `SELECT n.id::text,n.name,n.expected_down_mbps,n.expected_up_mbps,n.threads,n.server_id,n.binding_interface,host(n.binding_source_ip),n.auto_update,n.config_revision,s.interval_minutes,s.offset_minutes,(s.enabled AND n.enabled),s.revision,
 COALESCE((SELECT jsonb_object_agg(key,value) FROM settings WHERE key IN ('engine','quietStart','quietEnd','timezone','agentRelease') AND value IS NOT NULL),'{}'::jsonb)
 FROM nodes n JOIN schedules s ON s.node_id=n.id WHERE n.id=$1`, node).Scan(&id, &name, &down, &up, &threads, &server, &iface, &source, &autoUpdate, &revision, &interval, &offset, &enabled, &scheduleRevision, &settings)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	defaults := map[string]any{"engine": "ookla", "quietStart": "", "quietEnd": "", "timezone": "UTC", "agentRelease": nil}
	if err = json.Unmarshal(settings, &defaults); err != nil {
		return nil, err
	}
	var serverValue any = "auto"
	if server != nil {
		serverValue = *server
	}
	return map[string]any{"revision": fmt.Sprintf("%d.%d", revision, scheduleRevision), "name": name, "expectedDown": down, "expectedUp": up,
		"schedule": map[string]any{"interval": interval, "offset": offset, "enabled": enabled},
		"quiet":    map[string]any{"quietStart": defaults["quietStart"], "quietEnd": defaults["quietEnd"], "timezone": defaults["timezone"]},
		"engine":   defaults["engine"], "threads": threads, "server": serverValue, "binding": map[string]any{"iface": iface, "sourceIp": source}, "autoUpdate": autoUpdate, "release": defaults["agentRelease"]}, nil
}

func (s *Postgres) Heartbeat(ctx context.Context, node string, h Heartbeat) (time.Time, error) {
	var received time.Time
	err := s.Pool.QueryRow(ctx, `UPDATE nodes SET last_seen_at=clock_timestamp(),agent=$2 WHERE id=$1 RETURNING last_seen_at`, node, h.Agent).Scan(&received)
	if errors.Is(err, pgx.ErrNoRows) {
		return received, ErrNotFound
	}
	return received, err
}

const commandColumns = `id::text,node_id::text,type,version,status,created_at,acked_at,phase,pct,result_id::text,error`

type scanner interface{ Scan(...any) error }

func scanCommand(row scanner) (Command, error) {
	var c Command
	err := row.Scan(&c.ID, &c.NodeID, &c.Type, &c.Version, &c.Status, &c.CreatedAt, &c.AckedAt, &c.Phase, &c.Pct, &c.ResultID, &c.Error)
	c.CreatedAt = c.CreatedAt.UTC()
	if c.AckedAt != nil {
		utc := c.AckedAt.UTC()
		c.AckedAt = &utc
	}
	return c, err
}
func (s *Postgres) Commands(ctx context.Context, node string) ([]Command, error) {
	rows, err := s.Pool.Query(ctx, `SELECT `+commandColumns+` FROM agent_commands WHERE node_id=$1 AND status IN ('pending','running') ORDER BY created_at,id LIMIT 100`, node)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	commands := []Command{}
	for rows.Next() {
		c, scanErr := scanCommand(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		commands = append(commands, c)
	}
	return commands, rows.Err()
}
func (s *Postgres) Ack(ctx context.Context, node, id string, a Ack) (Command, error) {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return Command{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	current, err := scanCommand(tx.QueryRow(ctx, `SELECT `+commandColumns+` FROM agent_commands WHERE id=$1 AND node_id=$2 FOR UPDATE`, id, node))
	if errors.Is(err, pgx.ErrNoRows) {
		return Command{}, ErrNotFound
	}
	if err != nil {
		return Command{}, err
	}
	next, err := ApplyAck(current, a)
	if err != nil {
		return Command{}, err
	}
	if sameAck(current, next) {
		return current, nil
	}
	next, err = scanCommand(tx.QueryRow(ctx, `UPDATE agent_commands SET status=$3,phase=$4,pct=$5,result_id=$6,error=$7,acked_at=CASE WHEN $3 IN ('succeeded','failed') THEN clock_timestamp() ELSE NULL END WHERE id=$1 AND node_id=$2 RETURNING `+commandColumns, id, node, next.Status, next.Phase, next.Pct, next.ResultID, next.Error))
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23503" {
			return Command{}, ErrInvalid
		}
		return Command{}, err
	}
	return next, tx.Commit(ctx)
}
