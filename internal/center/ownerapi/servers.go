package ownerapi

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jooservices/speedtest/internal/center/agentapi"
)

// ServerCatalog returns the latest list reported by one installed engine.
func (s *Store) ServerCatalog(ctx context.Context, nodeID, engine string) (map[string]any, string, error) {
	if !validServerEngine(engine) {
		return nil, "", ErrInvalid
	}
	var installed []string
	var revision int64
	var raw []byte
	var pinned *int64
	var refreshed *time.Time
	err := s.DB.QueryRow(ctx, `SELECT n.installed_engines,n.config_revision,COALESCE(c.servers,'[]'::jsonb),c.pinned_server_id,c.refreshed_at
FROM nodes n LEFT JOIN node_engine_servers c ON c.node_id=n.id AND c.engine=$2 WHERE n.id=$1`, nodeID, engine).
		Scan(&installed, &revision, &raw, &pinned, &refreshed)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, "", ErrNotFound
	}
	if err != nil {
		return nil, "", err
	}
	if !slices.Contains(installed, engine) {
		return nil, "", ErrInvalid
	}
	var servers []agentapi.ServerOption
	if err := json.Unmarshal(raw, &servers); err != nil {
		return nil, "", err
	}
	if servers == nil {
		servers = []agentapi.ServerOption{}
	}
	var serverID any = "auto"
	if pinned != nil {
		serverID = *pinned
	}
	return map[string]any{"engine": engine, "servers": servers, "serverId": serverID, "refreshedAt": serverRefreshTime(refreshed)}, ETag(revision), nil
}

func serverRefreshTime(at *time.Time) any {
	if at == nil {
		return nil
	}
	return at.UTC().Format(time.RFC3339Nano)
}

// SelectServer pins a listed server for one engine or sets automatic selection.
func (s *Store) SelectServer(ctx context.Context, nodeID, engine string, serverID *int64, tag string) (map[string]any, string, error) {
	if !validServerEngine(engine) || serverID != nil && *serverID < 1 {
		return nil, "", ErrInvalid
	}
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		return nil, "", err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var installed []string
	var revision int64
	var primary string
	err = tx.QueryRow(ctx, `SELECT installed_engines,config_revision,primary_engine FROM nodes WHERE id=$1 FOR UPDATE`, nodeID).
		Scan(&installed, &revision, &primary)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, "", ErrNotFound
	}
	if err != nil {
		return nil, "", err
	}
	if !slices.Contains(installed, engine) {
		return nil, "", ErrInvalid
	}
	if tag != ETag(revision) {
		return nil, "", ErrStale
	}
	var catalog []byte
	err = tx.QueryRow(ctx, `SELECT servers FROM node_engine_servers WHERE node_id=$1 AND engine=$2 FOR UPDATE`, nodeID, engine).Scan(&catalog)
	if errors.Is(err, pgx.ErrNoRows) {
		if serverID != nil {
			return nil, "", ErrConflict
		}
		if _, err := tx.Exec(ctx, `INSERT INTO node_engine_servers(node_id,engine) VALUES($1,$2) ON CONFLICT DO NOTHING`, nodeID, engine); err != nil {
			return nil, "", err
		}
	}
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return nil, "", err
	}
	if serverID != nil && !catalogHas(catalog, *serverID) {
		return nil, "", ErrInvalid
	}
	if _, err := tx.Exec(ctx, `UPDATE node_engine_servers SET pinned_server_id=$3 WHERE node_id=$1 AND engine=$2`, nodeID, engine, serverID); err != nil {
		return nil, "", err
	}
	if engine == primary {
		if _, err := tx.Exec(ctx, `UPDATE nodes SET server_id=$2,config_revision=config_revision+1,updated_at=now() WHERE id=$1`, nodeID, serverID); err != nil {
			return nil, "", err
		}
	} else if _, err := tx.Exec(ctx, `UPDATE nodes SET config_revision=config_revision+1,updated_at=now() WHERE id=$1`, nodeID); err != nil {
		return nil, "", err
	}
	if err := queueConfigRefresh(ctx, tx, nodeID); err != nil {
		return nil, "", err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, "", err
	}
	return s.ServerCatalog(ctx, nodeID, engine)
}

func queueConfigRefresh(ctx context.Context, tx pgx.Tx, nodeID string) error {
	var pending bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM agent_commands WHERE node_id=$1 AND type='update_config' AND status IN ('pending','running'))`, nodeID).Scan(&pending); err != nil || pending {
		return err
	}
	commandID, err := newUUID()
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `INSERT INTO agent_commands(id,node_id,type) VALUES($1,$2,'update_config')`, commandID, nodeID)
	return err
}

// QueueServerList asks one installed engine on a node to refresh its catalog.
func (s *Store) QueueServerList(ctx context.Context, nodeID, engine string) (map[string]any, error) {
	if !validServerEngine(engine) {
		return nil, ErrInvalid
	}
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var installed []string
	if err := tx.QueryRow(ctx, `SELECT installed_engines FROM nodes WHERE id=$1 FOR UPDATE`, nodeID).Scan(&installed); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	if !slices.Contains(installed, engine) {
		return nil, ErrInvalid
	}
	var pending bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM agent_commands WHERE node_id=$1 AND type='list_servers' AND engine=$2 AND status IN ('pending','running'))`, nodeID, engine).Scan(&pending); err != nil {
		return nil, err
	}
	if pending {
		return nil, ErrConflict
	}
	commandID, err := newUUID()
	if err != nil {
		return nil, err
	}
	command, err := scanCommand(tx.QueryRow(ctx, `INSERT INTO agent_commands(id,node_id,type,engine) VALUES($1,$2,'list_servers',$3) RETURNING `+commandColumns, commandID, nodeID, engine))
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return command, nil
}

func validServerEngine(engine string) bool { return engine == "ookla" || engine == "librespeed" }

func catalogHas(raw []byte, id int64) bool {
	var servers []agentapi.ServerOption
	if json.Unmarshal(raw, &servers) != nil {
		return false
	}
	return slices.ContainsFunc(servers, func(server agentapi.ServerOption) bool { return server.ID == id })
}
