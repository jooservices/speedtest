package report

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
)

type summaryPool struct {
	data     [][]any
	queryErr error
	rowsErr  error
	query    string
	args     []any
	queries  int
}

func (p *summaryPool) Query(_ context.Context, query string, args ...any) (pgx.Rows, error) {
	p.queries++
	p.query = query
	p.args = args
	if p.queryErr != nil {
		return nil, p.queryErr
	}
	return &rows{data: p.data, err: p.rowsErr}, nil
}

func TestAC1AC2SummarizeMapsPrimaryEngineNodeAggregates(t *testing.T) {
	period, err := NewPeriod(DailyPeriod, "2026-03-08", "America/New_York")
	if err != nil {
		t.Fatal(err)
	}
	pool := &summaryPool{data: [][]any{
		{"node-1", "alpha", "ookla", 2, 1, 200.0, 40.0},
		{"node-2", "beta", "librespeed", 0, 1, nil, nil},
	}}
	got, err := (&Store{DB: pool}).Summarize(context.Background(), period)
	if err != nil {
		t.Fatalf("Summarize() error = %v", err)
	}
	if got.Kind != DailyPeriod || got.LocalDate != "2026-03-08" || got.Timezone != "America/New_York" ||
		!got.From.Equal(period.From()) || !got.To.Equal(period.To()) || len(got.Nodes) != 2 {
		t.Fatalf("summary period or node count = %+v", got)
	}
	first, second := got.Nodes[0], got.Nodes[1]
	if first.NodeID != "node-1" || first.Node != "alpha" || first.Engine != "ookla" || first.Successes != 2 || first.Failures != 1 ||
		first.AvgDown == nil || *first.AvgDown != 200 || first.AvgUp == nil || *first.AvgUp != 40 {
		t.Fatalf("first node summary = %+v", first)
	}
	if second.NodeID != "node-2" || second.Node != "beta" || second.Engine != "librespeed" || second.Successes != 0 || second.Failures != 1 ||
		second.AvgDown != nil || second.AvgUp != nil {
		t.Fatalf("failure-only node summary = %+v", second)
	}
	if pool.queries != 1 || len(pool.args) != 2 || pool.args[0] != period.From() || pool.args[1] != period.To() {
		t.Fatalf("query count or bounds = %d, %v", pool.queries, pool.args)
	}
	if !strings.Contains(pool.query, "r.engine = n.primary_engine") || !strings.Contains(pool.query, "r.ts >= $1") || !strings.Contains(pool.query, "r.ts < $2") {
		t.Fatalf("summary query is missing its primary-engine or half-open period filter: %s", pool.query)
	}
}

func TestAC2SummarizeReturnsExplicitEmptyNodeList(t *testing.T) {
	period, err := NewPeriod(WeeklyPeriod, "2026-03-02", "UTC")
	if err != nil {
		t.Fatal(err)
	}
	got, err := (&Store{DB: &summaryPool{}}).Summarize(context.Background(), period)
	if err != nil || got.Nodes == nil || len(got.Nodes) != 0 {
		t.Fatalf("empty summary = %+v, %v", got, err)
	}
}

func TestAC2SummarizeReturnsInvalidPeriodAndDatabaseErrors(t *testing.T) {
	valid, err := NewPeriod(DailyPeriod, "2026-03-08", "America/New_York")
	if err != nil {
		t.Fatal(err)
	}
	invalidPool := &summaryPool{}
	if _, err := (&Store{DB: invalidPool}).Summarize(context.Background(), Period{}); !errors.Is(err, ErrInvalidPeriod) || invalidPool.queries != 0 {
		t.Fatalf("invalid period error = %v, queries = %d", err, invalidPool.queries)
	}
	for _, tc := range []struct {
		name string
		pool *summaryPool
	}{
		{name: "query", pool: &summaryPool{queryErr: errors.New("query failed")}},
		{name: "scan", pool: &summaryPool{data: [][]any{{"bad row"}}}},
		{name: "iteration", pool: &summaryPool{rowsErr: errors.New("iteration failed")}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := (&Store{DB: tc.pool}).Summarize(context.Background(), valid); err == nil {
				t.Fatal("Summarize() swallowed a database error")
			}
		})
	}
}
