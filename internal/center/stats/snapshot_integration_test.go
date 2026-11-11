//go:build integration

package stats

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type insertAfterFirstSummaryQuery struct {
	*pgxpool.Pool
	insert func() error
	done   bool
}

func (db *insertAfterFirstSummaryQuery) Query(ctx context.Context, query string, args ...any) (pgx.Rows, error) {
	rows, err := db.Pool.Query(ctx, query, args...)
	if err != nil || db.done {
		return rows, err
	}
	db.done = true
	if err := db.insert(); err != nil {
		rows.Close()
		return nil, err
	}
	return rows, nil
}

func TestSummaryCountsShareSnapshotDuringConcurrentRateLimitIngestion(t *testing.T) {
	pool, ctx, store := openStatsIntegrationStore(t)
	const node = "00000000-0000-4000-8000-000000000076"
	start := time.Now().UTC().Truncate(time.Second).Add(-2 * time.Hour)
	if _, err := pool.Exec(ctx, `INSERT INTO nodes(id,name,expected_down_mbps,expected_up_mbps) VALUES ($1,'Concurrent summary',100,50)`, node); err != nil {
		t.Fatal("could not create summary node")
	}
	if _, err := pool.Exec(ctx, `INSERT INTO ip_addresses(ip) VALUES ('203.0.113.76')`); err != nil {
		t.Fatal("could not create summary IP")
	}
	for i, at := range []time.Time{start.Add(time.Hour), start.Add(90 * time.Minute)} {
		seedSummarySuccess(t, pool, ctx, node, i+1, at)
	}
	interleaved := &insertAfterFirstSummaryQuery{Pool: pool, insert: func() error {
		_, err := pool.Exec(ctx, `INSERT INTO results(id,node_id,ts,status,threads,error,engine,failure_class)
			VALUES ('00000000-0000-4000-8000-000000000079',$1,$2,'failed',4,'Too many requests','ookla','rate_limited')`, node, start.Add(105*time.Minute))
		return err
	}}
	store.DB = interleaved
	query := Query{From: start, To: start.Add(2 * time.Hour), PreviousFrom: start.Add(-2 * time.Hour), PreviousTo: start, Metric: Download}
	items, err := store.Summary(ctx, query)
	if err != nil || len(items) != 1 {
		t.Fatalf("summary = %+v, error %v", items, err)
	}
	current := items[0].Current
	if interleaved.done != true || current.Count != 2 || current.OK != 2 || current.RateLimited != 0 || current.SuccessRate == nil || *current.SuccessRate != 100 {
		t.Fatalf("statement snapshot = %+v; concurrent result must be outside the complete snapshot", current)
	}
	items, err = store.Summary(ctx, query)
	if err != nil || len(items) != 1 {
		t.Fatalf("subsequent summary = %+v, error %v", items, err)
	}
	current = items[0].Current
	if current.Count != 3 || current.OK != 2 || current.RateLimited != 1 || current.SuccessRate == nil || *current.SuccessRate != 100 {
		t.Fatalf("summary after ingestion = %+v", current)
	}
}

func seedSummarySuccess(t *testing.T, pool *pgxpool.Pool, ctx context.Context, node string, index int, at time.Time) {
	t.Helper()
	id := fmt.Sprintf("00000000-0000-4000-8000-%012x", 0x76+index)
	if _, err := pool.Exec(ctx, `INSERT INTO results(id,node_id,ts,status,down_mbps,up_mbps,ping_ms,threads,engine,ip,server)
		VALUES ($1,$2,$3,'ok',100,50,10,4,'ookla','203.0.113.76','{"name":"Fixture"}')`, id, node, at); err != nil {
		t.Fatal("could not create summary result")
	}
}
