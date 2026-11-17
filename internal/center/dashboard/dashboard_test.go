package dashboard

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jooservices/speedtest/internal/center/alerts"
	"github.com/jooservices/speedtest/internal/center/ownerapi"
	"github.com/jooservices/speedtest/internal/center/stats"
)

type fakeNodes struct {
	total, calls int
	err          error
}

func (f *fakeNodes) List(_ context.Context, page, size int) ([]map[string]any, int, error) {
	f.calls++
	if f.err != nil {
		return nil, 0, f.err
	}
	var out []map[string]any
	for i := (page - 1) * size; i < page*size && i < f.total; i++ {
		out = append(out, map[string]any{"id": fmt.Sprintf("n%03d", i), "name": fmt.Sprintf("node-%d", i)})
	}
	return out, f.total, nil
}

type fakeStats struct {
	summaries []stats.SummaryComparison
	series    []stats.Series
	err       error
	query     stats.Query
	percent   bool
	maxPoints int
}

func (f *fakeStats) Summary(_ context.Context, q stats.Query) ([]stats.SummaryComparison, error) {
	f.query = q
	return f.summaries, f.err
}
func (f *fakeStats) Series(_ context.Context, q stats.Query, percent bool, maxPoints int) ([]stats.Series, error) {
	f.query, f.percent, f.maxPoints = q, percent, maxPoints
	return f.series, f.err
}

type fakeResults struct {
	filter ownerapi.ResultFilter
	size   int
	err    error
}

func (f *fakeResults) ListResults(_ context.Context, filter ownerapi.ResultFilter, _, size int) ([]map[string]any, int, error) {
	f.filter, f.size = filter, size
	return []map[string]any{{"id": "r1"}}, 1, f.err
}

type fakeAlerts struct {
	filter alerts.EventFilter
	size   int
	events []alerts.Event
	total  int
	err    error
}

func (f *fakeAlerts) ListEvents(_ context.Context, filter alerts.EventFilter, _, size int) ([]alerts.Event, int, error) {
	f.filter, f.size = filter, size
	return f.events, f.total, f.err
}

type fakeRow struct {
	ping *float64
	ips  int
	err  error
}

func (r fakeRow) Scan(dest ...any) error {
	if r.err != nil {
		return r.err
	}
	*(dest[0].(**float64)) = r.ping
	*(dest[1].(*int)) = r.ips
	return nil
}

type fakeDB struct {
	row    fakeRow
	args   [][]any
	failAt int
}

func (f *fakeDB) QueryRow(_ context.Context, _ string, args ...any) pgx.Row {
	f.args = append(f.args, args)
	if f.failAt == len(f.args) {
		return fakeRow{err: errors.New("boom")}
	}
	return f.row
}

func fp(v float64) *float64 { return &v }

func summary(downPct, upPct *float64, ok, count int) stats.NodeSummary {
	return stats.NodeSummary{DownPct: downPct, UpPct: upPct, OK: ok, Count: count}
}

func newStore(f *fakeStats, db *fakeDB) (*Store, *fakeResults, *fakeAlerts) {
	results, al := &fakeResults{}, &fakeAlerts{}
	return &Store{DB: db, Nodes: &fakeNodes{total: 2}, Stats: f, Results: results, Alerts: al,
		Now: func() time.Time { return time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC) }}, results, al
}

func window() stats.Query {
	to := time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)
	return stats.Query{From: to.Add(-24 * time.Hour), To: to, PreviousFrom: to.Add(-48 * time.Hour), PreviousTo: to.Add(-24 * time.Hour), Metric: stats.Ping, NodeIDs: []string{"x"}, NodesSet: true}
}

func TestSnapshotFleetFollowsThePreviewFormulas(t *testing.T) {
	f := &fakeStats{
		summaries: []stats.SummaryComparison{
			{NodeID: "n000", Current: summary(fp(90), fp(80), 9, 10), Previous: summary(fp(50), nil, 1, 4)},
			{NodeID: "n001", Current: summary(fp(70), nil, 3, 10), Previous: summary(nil, nil, 0, 0)},
		},
		series: []stats.Series{{NodeID: strPtr("n000"), Points: []stats.Point{{TS: time.Unix(0, 0), Value: fp(1), Count: 1}}}},
	}
	db := &fakeDB{row: fakeRow{ping: fp(12.5), ips: 3}}
	store, results, al := newStore(f, db)
	al.events, al.total = []alerts.Event{{ID: "e1"}}, 17
	snap, err := store.Snapshot(t.Context(), window())
	if err != nil {
		t.Fatal(err)
	}
	assertSnapshotAggregates(t, snap)
	assertSnapshotFleetQuery(t, db)
	assertSnapshotNodes(t, snap)
	assertSnapshotSparklineQuery(t, f)
	assertSnapshotResultsQuery(t, results)
	assertSnapshotAlertsQuery(t, al, snap)
}

func assertSnapshotAggregates(t *testing.T, snap Snapshot) {
	t.Helper()
	assertCurrentSnapshotAggregates(t, snap.Current)
	assertPreviousSnapshotAggregates(t, snap.Previous)
}

func assertCurrentSnapshotAggregates(t *testing.T, summary Fleet) {
	t.Helper()
	// Unweighted mean of nodes with a percentage; success is non-failed over all tests.
	if *summary.Down != 80 || *summary.Up != 80 || *summary.Success != 60 || summary.Count != 20 || summary.IPs != 3 || *summary.Ping != 12.5 {
		t.Fatalf("current = %+v", summary)
	}
}

func assertPreviousSnapshotAggregates(t *testing.T, summary Fleet) {
	t.Helper()
	if *summary.Down != 50 || summary.Up != nil || *summary.Success != 25 || summary.Count != 4 {
		t.Fatalf("previous = %+v", summary)
	}
}

func assertSnapshotFleetQuery(t *testing.T, db *fakeDB) {
	t.Helper()
	if len(db.args) != 2 || db.args[0][0].(time.Time).After(db.args[0][1].(time.Time)) {
		t.Fatalf("fleet queries = %v", db.args)
	}
}

func assertSnapshotNodes(t *testing.T, snap Snapshot) {
	t.Helper()
	if len(snap.Nodes) != 2 || snap.Nodes[0].Summary.Count != 10 || len(snap.Nodes[0].Sparkline) != 1 || snap.Nodes[1].Sparkline == nil || len(snap.Nodes[1].Sparkline) != 0 {
		t.Fatalf("nodes = %+v", snap.Nodes)
	}
}

func assertSnapshotSparklineQuery(t *testing.T, f *fakeStats) {
	t.Helper()
	if f.maxPoints != SparkPoints || f.percent || f.query.Metric != stats.Download || f.query.To.Sub(f.query.From) < SparkHours*time.Hour {
		t.Fatalf("sparkline query = %+v max %d percent %v", f.query, f.maxPoints, f.percent)
	}
}

func assertSnapshotResultsQuery(t *testing.T, results *fakeResults) {
	t.Helper()
	if results.size != RecentResults || results.filter.Sort != "ts" || results.filter.Dir != "desc" || !results.filter.From.IsZero() {
		t.Fatalf("latest results query = %+v size %d", results.filter, results.size)
	}
}

func assertSnapshotAlertsQuery(t *testing.T, al *fakeAlerts, snap Snapshot) {
	t.Helper()
	if al.size != OpenAlerts || al.filter.State != "open" || len(snap.OpenAlerts) != 1 || snap.OpenAlertCount != 17 {
		t.Fatalf("alerts = %+v count %d", snap.OpenAlerts, snap.OpenAlertCount)
	}
}

func TestSnapshotOfAnEmptyFleetHasEmptyListsAndNullAggregates(t *testing.T) {
	store, _, _ := newStore(&fakeStats{}, &fakeDB{})
	store.Nodes = &fakeNodes{}
	snap, err := store.Snapshot(t.Context(), window())
	if err != nil {
		t.Fatal(err)
	}
	if snap.Nodes == nil || len(snap.Nodes) != 0 || snap.OpenAlerts == nil || snap.LatestResults == nil || snap.Current.Down != nil || snap.Current.Success != nil || snap.Current.Ping != nil ||
		snap.Current.Count != 0 || snap.Current.IPs != 0 || snap.OpenAlertCount != 0 {
		t.Fatalf("empty snapshot = %+v", snap)
	}
}

func TestAllNodesReadsEveryPage(t *testing.T) {
	nodes := &fakeNodes{total: 250}
	store, _, _ := newStore(&fakeStats{}, &fakeDB{})
	store.Nodes = nodes
	got, err := store.allNodes(t.Context())
	if err != nil || len(got) != 250 || nodes.calls != 3 {
		t.Fatalf("%d nodes in %d calls: %v", len(got), nodes.calls, err)
	}
}

func TestSnapshotReportsEveryFailure(t *testing.T) {
	boom := errors.New("boom")
	for name, mutate := range map[string]func(*Store, *fakeStats, *fakeDB){
		"nodes":    func(s *Store, _ *fakeStats, _ *fakeDB) { s.Nodes = &fakeNodes{err: boom} },
		"summary":  func(_ *Store, f *fakeStats, _ *fakeDB) { f.err = boom },
		"fleet":    func(_ *Store, _ *fakeStats, db *fakeDB) { db.failAt = 1 },
		"previous": func(_ *Store, _ *fakeStats, db *fakeDB) { db.failAt = 2 },
		"results":  func(s *Store, _ *fakeStats, _ *fakeDB) { s.Results = &fakeResults{err: boom} },
		"alerts":   func(s *Store, _ *fakeStats, _ *fakeDB) { s.Alerts = &fakeAlerts{err: boom} },
	} {
		f, db := &fakeStats{}, &fakeDB{}
		store, _, _ := newStore(f, db)
		mutate(store, f, db)
		if _, err := store.Snapshot(t.Context(), window()); err == nil {
			t.Errorf("%s failure was swallowed", name)
		}
	}
	// A sparkline failure after the summaries succeed is also reported.
	f := &fakeStats{}
	store, _, _ := newStore(f, &fakeDB{})
	failing := &sparkFails{fakeStats: f}
	store.Stats = failing
	if _, err := store.Snapshot(t.Context(), window()); err == nil {
		t.Error("sparkline failure was swallowed")
	}
}

type sparkFails struct{ *fakeStats }

func (s *sparkFails) Series(context.Context, stats.Query, bool, int) ([]stats.Series, error) {
	return nil, errors.New("boom")
}

func TestSeriesDelegatesAndClockDefaults(t *testing.T) {
	f := &fakeStats{series: []stats.Series{{Name: "x"}}}
	store, _, _ := newStore(f, &fakeDB{})
	got, err := store.Series(t.Context(), stats.Query{Metric: stats.Upload}, true, 120)
	if err != nil || len(got) != 1 || !f.percent || f.maxPoints != 120 || f.query.Metric != stats.Upload {
		t.Fatalf("series = %v %v (%+v)", got, err, f)
	}
	if (&Store{}).now().IsZero() {
		t.Fatal("zero-value clock")
	}
	if mean(nil) != nil || *mean([]float64{1, 2}) != 1.5 {
		t.Fatal("mean")
	}
}

func strPtr(s string) *string { return &s }
