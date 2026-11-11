package ipapi

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jooservices/speedtest/internal/center/geo"
)

type ipapiTestDB struct {
	rowErr   error
	queryErr error
	scanErr  error
	rowsErr  error
	rows     bool
}

func (d *ipapiTestDB) QueryRow(context.Context, string, ...any) pgx.Row {
	return ipapiTestRow{err: d.rowErr}
}

func (d *ipapiTestDB) Query(_ context.Context, query string, _ ...any) (pgx.Rows, error) {
	if d.queryErr != nil {
		return nil, d.queryErr
	}
	values := ipapiViewValues(query)
	return &ipapiTestRows{values: values, scanErr: d.scanErr, rowsErr: d.rowsErr, next: !d.rows}, nil
}

type ipapiTestRow struct{ err error }

func (r ipapiTestRow) Scan(dest ...any) error {
	if r.err != nil {
		return r.err
	}
	if len(dest) > 0 {
		switch target := dest[0].(type) {
		case *int:
			*target = 1
		case *string:
			*target = "ip-api"
		}
	}
	return nil
}

type ipapiTestRows struct {
	pgx.Rows
	values  []any
	scanErr error
	rowsErr error
	next    bool
	done    bool
}

func (r *ipapiTestRows) Next() bool {
	if r.done || !r.next {
		return false
	}
	r.done = true
	return true
}
func (r *ipapiTestRows) Scan(dest ...any) error {
	if r.scanErr != nil {
		return r.scanErr
	}
	for i, target := range dest {
		value := reflect.ValueOf(target).Elem()
		if r.values[i] == nil {
			value.SetZero()
			continue
		}
		value.Set(reflect.ValueOf(r.values[i]))
	}
	return nil
}
func (*ipapiTestRows) Close()                                       {}
func (r *ipapiTestRows) Err() error                                 { return r.rowsErr }
func (*ipapiTestRows) CommandTag() pgconn.CommandTag                { return pgconn.CommandTag{} }
func (*ipapiTestRows) FieldDescriptions() []pgconn.FieldDescription { return nil }
func (*ipapiTestRows) Values() ([]any, error)                       { return nil, nil }
func (*ipapiTestRows) RawValues() [][]byte                          { return nil }
func (*ipapiTestRows) Conn() *pgx.Conn                              { return nil }

func ipapiViewValues(query string) []any {
	now := time.Now().UTC()
	location := []byte(`{"city":"London","country":"GB","quality_score":90,"field_sources":{"city":"ip-api"}}`)
	source := "ip-api"
	switch {
	case strings.Contains(query, "node_ip_history"):
		ispID, asn, organisation, name := "isp-id", uint(64500), "Example Network", "Example ISP"
		return []any{"history-id", "node-id", now, "203.0.113.1", "203.0.113.2", &source, location, &now, &source, location, &now,
			&ispID, &asn, &organisation, &name, &ispID, &asn, &organisation, &name}
	case strings.Contains(query, "node_ip_addresses") && !strings.Contains(query, "DISTINCT ON"):
		down, up := 100.0, 50.0
		return []any{"node-id", "203.0.113.2", now, now, int64(2), &down, &up, &source, location, &now}
	default:
		return []any{"node-id", "203.0.113.2", &source, location, &now}
	}
}

func TestIPViewsReturnQualityAndHandleDatabaseErrors(t *testing.T) {
	ctx := context.Background()
	store := &Store{DB: &ipapiTestDB{}}
	history, count, err := store.History(ctx, "", 1, 25)
	if err != nil || count != 1 || history[0]["fromGeo"].(map[string]any)["qualityScore"] != 90 {
		t.Fatalf("History = (%+v,%d,%v)", history, count, err)
	}
	addresses, total, err := store.Addresses(ctx, "", 1, 25)
	if err != nil || total != 1 || addresses[0]["geo"].(map[string]any)["fieldSources"].(map[string]string)["city"] != "ip-api" {
		t.Fatalf("Addresses = (%+v,%d,%v)", addresses, total, err)
	}
	locations, err := store.Map(ctx, "")
	if err != nil || len(locations) != 1 || locations[0]["geo"].(map[string]any)["qualityScore"] != 90 {
		t.Fatalf("Map = (%+v,%v)", locations, err)
	}
	if geoOf("203.0.113.2", nil, nil, nil) != nil {
		t.Fatal("address without a source unexpectedly has geolocation")
	}

	for _, operation := range []struct {
		name string
		call func(DB) error
	}{
		{name: "history count", call: func(db DB) error { _, _, err := (&Store{DB: db}).History(ctx, "", 1, 25); return err }},
		{name: "history query", call: func(db DB) error { _, _, err := (&Store{DB: db}).History(ctx, "", 1, 25); return err }},
		{name: "history scan", call: func(db DB) error { _, _, err := (&Store{DB: db}).History(ctx, "", 1, 25); return err }},
		{name: "addresses count", call: func(db DB) error { _, _, err := (&Store{DB: db}).Addresses(ctx, "", 1, 25); return err }},
		{name: "addresses query", call: func(db DB) error { _, _, err := (&Store{DB: db}).Addresses(ctx, "", 1, 25); return err }},
		{name: "addresses scan", call: func(db DB) error { _, _, err := (&Store{DB: db}).Addresses(ctx, "", 1, 25); return err }},
		{name: "map query", call: func(db DB) error { _, err := (&Store{DB: db}).Map(ctx, ""); return err }},
		{name: "map scan", call: func(db DB) error { _, err := (&Store{DB: db}).Map(ctx, ""); return err }},
	} {
		t.Run(operation.name, func(t *testing.T) {
			db := &ipapiTestDB{}
			switch operation.name {
			case "history count", "addresses count":
				db.rowErr = errors.New("count failed")
			case "history query", "addresses query", "map query":
				db.queryErr = errors.New("query failed")
			case "history scan", "addresses scan", "map scan":
				db.scanErr = errors.New("scan failed")
			}
			if err := operation.call(db); err == nil {
				t.Fatal("database failure was ignored")
			}
		})
	}
	for _, operation := range []struct {
		name string
		call func(DB) error
	}{
		{name: "history rows", call: func(db DB) error { _, _, err := (&Store{DB: db}).History(ctx, "", 1, 25); return err }},
		{name: "addresses rows", call: func(db DB) error { _, _, err := (&Store{DB: db}).Addresses(ctx, "", 1, 25); return err }},
		{name: "map rows", call: func(db DB) error { _, err := (&Store{DB: db}).Map(ctx, ""); return err }},
	} {
		t.Run(operation.name, func(t *testing.T) {
			if err := operation.call(&ipapiTestDB{rowsErr: errors.New("iteration failed"), rows: true}); err == nil {
				t.Fatal("row iteration failure was ignored")
			}
		})
	}
	if providers, err := (&Store{}).Providers(ctx); err != nil || len(providers) != 0 {
		t.Fatalf("unconfigured providers = (%+v,%v)", providers, err)
	}
	if providers, err := (&Store{ProviderSettings: &geo.ProviderConfig{}}).Providers(ctx); err != nil || len(providers) != 8 {
		t.Fatalf("configured providers = (%+v,%v)", providers, err)
	}
}
