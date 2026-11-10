package metrics

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
)

type rowStub struct {
	values []any
	err    error
}

func (r rowStub) Scan(dest ...any) error {
	if r.err != nil {
		return r.err
	}
	if len(dest) != len(r.values) {
		return errors.New("scan destination count mismatch")
	}
	for i, value := range r.values {
		if err := assign(dest[i], value); err != nil {
			return err
		}
	}
	return nil
}

type rowsStub struct {
	values [][]any
	index  int
	err    error
}

func (r *rowsStub) Close()                                       {}
func (r *rowsStub) Err() error                                   { return r.err }
func (r *rowsStub) CommandTag() pgconn.CommandTag                { return pgconn.CommandTag{} }
func (r *rowsStub) FieldDescriptions() []pgconn.FieldDescription { return nil }
func (r *rowsStub) Values() ([]any, error)                       { return r.values[r.index-1], nil }
func (r *rowsStub) RawValues() [][]byte                          { return nil }
func (r *rowsStub) Conn() *pgx.Conn                              { return nil }
func (r *rowsStub) TypeMap() *pgtype.Map                         { return nil }
func (r *rowsStub) Next() bool {
	if r.index >= len(r.values) {
		return false
	}
	r.index++
	return true
}
func (r *rowsStub) Scan(dest ...any) error {
	return rowStub{values: r.values[r.index-1]}.Scan(dest...)
}

type querierStub struct {
	row       pgx.Row
	rows      []pgx.Rows
	queryErrs []error
	queries   int
	rowArgs   []any
}

func (q *querierStub) QueryRow(_ context.Context, _ string, args ...any) pgx.Row {
	q.rowArgs = args
	return q.row
}
func (q *querierStub) Query(context.Context, string, ...any) (pgx.Rows, error) {
	index := q.queries
	q.queries++
	if len(q.queryErrs) > index && q.queryErrs[index] != nil {
		return nil, q.queryErrs[index]
	}
	return q.rows[index], nil
}

func assign(destination, value any) error {
	switch target := destination.(type) {
	case *int64:
		*target = value.(int64)
	case *float64:
		*target = value.(float64)
	case *string:
		*target = value.(string)
	default:
		return errors.New("unsupported scan destination")
	}
	return nil
}

func snapshotRow() rowStub {
	return rowStub{values: []any{int64(0), int64(0), float64(-1), int64(0), int64(0), int64(0), int64(0), float64(0), int64(0)}}
}

func TestCollectAndWriteBandwidthGauges(t *testing.T) {
	now := time.Date(2026, 11, 9, 12, 0, 0, 0, time.FixedZone("local", 7*60*60))
	db := &querierStub{
		row: rowStub{values: []any{int64(4), int64(1), float64(12), int64(20), int64(2), int64(3), int64(1), float64(5), int64(6)}},
		rows: []pgx.Rows{
			&rowsStub{values: [][]any{{"node-1", "ookla", float64(1690)}}},
			&rowsStub{values: [][]any{{"global", "", float64(0.9)}, {"node", "node-1", float64(0.8)}}},
		},
	}
	snapshot, err := (Collector{DB: db, Now: func() time.Time { return now }}).Collect(context.Background())
	if err != nil || snapshot.NodesEnabled != 4 || len(snapshot.Bandwidth) != 1 || snapshot.Bandwidth[0].Bytes != 1690 || len(snapshot.BandwidthBudgetRatios) != 2 {
		t.Fatalf("snapshot = %+v, err = %v", snapshot, err)
	}
	if got, ok := db.rowArgs[0].(time.Time); !ok || !got.Equal(now.Add(-5*time.Minute)) {
		t.Fatalf("offline cutoff = %v", db.rowArgs[0])
	}

	var output bytes.Buffer
	snapshot.Write(&output)
	for _, want := range []string{
		"speedtest_nodes_enabled 4",
		`speedtest_bandwidth_month_to_date_bytes{node_id="node-1",engine="ookla"} 1690`,
		`speedtest_bandwidth_budget_ratio{scope="global",node_id=""} 0.9`,
	} {
		if !strings.Contains(output.String(), want) {
			t.Fatalf("metrics missing %q", want)
		}
	}
}

func TestCollectPropagatesQueryErrors(t *testing.T) {
	queryErr := errors.New("query failed")
	t.Run("snapshot", func(t *testing.T) {
		_, err := (Collector{DB: &querierStub{row: rowStub{err: queryErr}}}).Collect(context.Background())
		if !errors.Is(err, queryErr) {
			t.Fatalf("collect error = %v", err)
		}
	})
	t.Run("bandwidth", func(t *testing.T) {
		db := &querierStub{row: snapshotRow(), queryErrs: []error{queryErr}}
		_, err := (Collector{DB: db}).Collect(context.Background())
		if !errors.Is(err, queryErr) {
			t.Fatalf("collect error = %v", err)
		}
	})
	t.Run("budgets", func(t *testing.T) {
		db := &querierStub{row: snapshotRow(), rows: []pgx.Rows{&rowsStub{}}, queryErrs: []error{nil, queryErr}}
		_, err := (Collector{DB: db}).Collect(context.Background())
		if !errors.Is(err, queryErr) {
			t.Fatalf("collect error = %v", err)
		}
	})
}
