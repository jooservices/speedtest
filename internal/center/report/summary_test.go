package report

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

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
	want := PeriodSummary{
		Kind: DailyPeriod, LocalDate: "2026-03-08", Timezone: "America/New_York",
		From: period.From(), To: period.To(),
		Nodes: []NodeSummary{
			{NodeID: "node-1", Node: "alpha", Engine: "ookla", Successes: 2, Failures: 1, AvgDown: floatPointer(200), AvgUp: floatPointer(40)},
			{NodeID: "node-2", Node: "beta", Engine: "librespeed", Successes: 0, Failures: 1},
		},
	}
	if len(got.Nodes) != len(want.Nodes) {
		t.Fatalf("node count: got %d, want %d", len(got.Nodes), len(want.Nodes))
	}
	t.Run("period metadata", func(t *testing.T) { assertPeriodSummary(t, got, want) })
	t.Run("mixed-result primary-engine aggregate", func(t *testing.T) {
		assertNodeSummary(t, "mixed-result primary-engine aggregate", got.Nodes[0], want.Nodes[0])
	})
	t.Run("failure-only primary-engine aggregate", func(t *testing.T) {
		assertNodeSummary(t, "failure-only primary-engine aggregate", got.Nodes[1], want.Nodes[1])
	})
	t.Run("single bounded query", func(t *testing.T) {
		assertEqual(t, "query count", pool.queries, 1)
		if len(pool.args) != 2 {
			t.Fatalf("query arguments: got %d, want 2: %v", len(pool.args), pool.args)
			return
		}
		assertEqual(t, "inclusive UTC start argument", pool.args[0], period.From())
		assertEqual(t, "exclusive UTC end argument", pool.args[1], period.To())
	})
	for _, check := range []struct{ behavior, clause string }{
		{behavior: "primary engine filter", clause: "r.engine = n.primary_engine"},
		{behavior: "inclusive period start filter", clause: "r.ts >= $1"},
		{behavior: "exclusive period end filter", clause: "r.ts < $2"},
	} {
		t.Run(check.behavior, func(t *testing.T) {
			if !strings.Contains(pool.query, check.clause) {
				t.Fatalf("summary query missing %s (%q): %s", check.behavior, check.clause, pool.query)
			}
		})
	}
}

func assertPeriodSummary(t *testing.T, got, want PeriodSummary) {
	t.Helper()
	assertEqual(t, "period kind", got.Kind, want.Kind)
	assertEqual(t, "owner-local start date", got.LocalDate, want.LocalDate)
	assertEqual(t, "IANA time zone", got.Timezone, want.Timezone)
	assertEqual(t, "inclusive UTC start", got.From, want.From)
	assertEqual(t, "exclusive UTC end", got.To, want.To)
}

func assertNodeSummary(t *testing.T, behavior string, got, want NodeSummary) {
	t.Helper()
	assertEqual(t, behavior+" node ID", got.NodeID, want.NodeID)
	assertEqual(t, behavior+" node name", got.Node, want.Node)
	assertEqual(t, behavior+" primary engine", got.Engine, want.Engine)
	assertEqual(t, behavior+" success count", got.Successes, want.Successes)
	assertEqual(t, behavior+" failure count", got.Failures, want.Failures)
	assertEqual(t, behavior+" average download", got.AvgDown, want.AvgDown)
	assertEqual(t, behavior+" average upload", got.AvgUp, want.AvgUp)
}

func assertEqual(t *testing.T, behavior string, got, want any) {
	t.Helper()
	if !reflect.DeepEqual(got, want) {
		t.Errorf("%s: got %#v, want %#v", behavior, got, want)
	}
}

func floatPointer(value float64) *float64 { return &value }

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

func TestAC1SummarizeBuildsOneIndexablePredicatePerLocalDateInterval(t *testing.T) {
	period, err := NewPeriod(DailyPeriod, "2010-11-06", "America/St_Johns")
	if err != nil {
		t.Fatal(err)
	}
	pool := &summaryPool{}
	if _, err := (&Store{DB: pool}).Summarize(context.Background(), period); err != nil {
		t.Fatalf("Summarize() error = %v", err)
	}
	wantArgs := []any{
		time.Date(2010, 11, 6, 2, 30, 0, 0, time.UTC),
		time.Date(2010, 11, 7, 2, 30, 0, 0, time.UTC),
		time.Date(2010, 11, 7, 2, 31, 0, 0, time.UTC),
		time.Date(2010, 11, 7, 3, 30, 0, 0, time.UTC),
	}
	if !reflect.DeepEqual(pool.args, wantArgs) {
		t.Fatalf("interval query arguments = %#v, want %#v", pool.args, wantArgs)
	}
	wantPredicate := "((r.ts >= $1 AND r.ts < $2) OR (r.ts >= $3 AND r.ts < $4)) AND r.engine = n.primary_engine"
	if !strings.Contains(pool.query, wantPredicate) {
		t.Fatalf("summary query missing exact, indexable interval predicates %q: %s", wantPredicate, pool.query)
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
