package ownerapi

import (
	"context"
	"errors"
	"slices"

	"github.com/jackc/pgx/v5"

	"github.com/jooservices/speedtest/internal/center/schedules"
)

// EngineSchedule is the independently configurable schedule of one installed engine.
type EngineSchedule struct {
	Engine   string `json:"engine"`
	Interval int    `json:"interval"`
	Offset   int    `json:"offset"`
	Enabled  bool   `json:"enabled"`
}

func (s *Store) replaceEngines(ctx context.Context, id string, engines []EngineSchedule, tag string) (map[string]any, string, error) {
	if err := validateEngines(engines); err != nil {
		return nil, "", err
	}
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		return nil, "", err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var installed []string
	if err := tx.QueryRow(ctx, `SELECT installed_engines FROM nodes WHERE id=$1 FOR UPDATE`, id).Scan(&installed); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, "", ErrNotFound
		}
		return nil, "", err
	}
	rows, err := s.scheduleRows(ctx, tx, true, id)
	if err != nil {
		return nil, "", err
	}
	if nodeScheduleETag(rows) != tag {
		return nil, "", ErrStale
	}
	for _, e := range engines {
		if !slices.Contains(installed, e.Engine) {
			return nil, "", ErrInvalid
		}
		if _, err := tx.Exec(ctx, `INSERT INTO engine_schedules(node_id,engine,interval_minutes,offset_minutes,enabled)
   VALUES($1,$2,$3,$4,$5) ON CONFLICT(node_id,engine) DO UPDATE SET interval_minutes=$3,offset_minutes=$4,enabled=$5,revision=engine_schedules.revision+1`, id, e.Engine, e.Interval, e.Offset, e.Enabled); err != nil {
			return nil, "", err
		}
		if e.Engine == "ookla" {
			if _, err := tx.Exec(ctx, `UPDATE schedules SET interval_minutes=$2,offset_minutes=$3,enabled=$4,revision=revision+1,updated_at=now() WHERE node_id=$1`, id, e.Interval, e.Offset, e.Enabled); err != nil {
				return nil, "", err
			}
		}
	}
	names := make([]string, 0, len(engines))
	for _, e := range engines {
		names = append(names, e.Engine)
	}
	if _, err := tx.Exec(ctx, `DELETE FROM engine_schedules WHERE node_id=$1 AND NOT(engine=ANY($2::text[]))`, id, names); err != nil {
		return nil, "", err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, "", err
	}
	return s.Schedule(ctx, id)
}

func validateEngines(engines []EngineSchedule) error {
	if len(engines) < 1 || len(engines) > 2 {
		return ErrInvalid
	}
	seen := map[string]bool{}
	for i := range engines {
		e := &engines[i]
		if seen[e.Engine] || (e.Engine != "ookla" && e.Engine != "librespeed") {
			return ErrInvalid
		}
		seen[e.Engine] = true
		if e.Interval == 0 {
			e.Interval = schedules.DefaultInterval
		}
		if e.Interval < schedules.MinInterval || e.Interval > schedules.MaxInterval || e.Offset < 0 || e.Offset >= schedules.MaxInterval {
			return ErrInvalid
		}
		e.Offset = schedules.Clamp(e.Interval, e.Offset).Offset
	}
	// The rollout compatibility row remains Ookla, even when it is disabled.
	if !seen["ookla"] {
		return ErrInvalid
	}
	return nil
}
