package geo

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

type cacheTestDB struct {
	row      pgx.Row
	tx       *cacheTestTx
	beginErr error
}

func (d *cacheTestDB) Begin(context.Context) (pgx.Tx, error) {
	return d.tx, d.beginErr
}
func (d *cacheTestDB) QueryRow(context.Context, string, ...any) pgx.Row { return d.row }
func (*cacheTestDB) Exec(context.Context, string, ...any) (pgconn.CommandTag, error) {
	return pgconn.NewCommandTag("UPDATE 1"), nil
}

type cacheTestRow struct {
	raw      []byte
	source   string
	lookedUp bool
	lookedAt time.Time
	err      error
}

func (r cacheTestRow) Scan(dest ...any) error {
	if r.err != nil {
		return r.err
	}
	*dest[0].(*[]byte) = r.raw
	*dest[1].(*string) = r.source
	*dest[2].(*bool) = r.lookedUp
	*dest[3].(*time.Time) = r.lookedAt
	return nil
}

type cacheTestTx struct {
	pgx.Tx
	row       pgx.Row
	insertErr error
	updateErr error
	commitErr error
	inserted  bool
	updated   bool
	committed bool
	rollbacks int
	data      []byte
	source    string
}

func (tx *cacheTestTx) Exec(_ context.Context, query string, args ...any) (pgconn.CommandTag, error) {
	if strings.Contains(query, "INSERT INTO") {
		tx.inserted = true
		return pgconn.CommandTag{}, tx.insertErr
	}
	tx.updated = true
	if len(args) > 2 {
		tx.data, _ = args[1].([]byte)
		tx.source, _ = args[2].(string)
	}
	return pgconn.CommandTag{}, tx.updateErr
}
func (tx *cacheTestTx) QueryRow(context.Context, string, ...any) pgx.Row { return tx.row }
func (tx *cacheTestTx) Commit(context.Context) error {
	tx.committed = true
	return tx.commitErr
}
func (tx *cacheTestTx) Rollback(context.Context) error {
	tx.rollbacks++
	return nil
}

func TestPostgresCacheGetHandlesRowsAndFailures(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name      string
		row       cacheTestRow
		wantFound bool
		wantErr   string
	}{
		{name: "missing", row: cacheTestRow{err: pgx.ErrNoRows}},
		{name: "database error", row: cacheTestRow{err: errors.New("database down")}, wantErr: "database down"},
		{name: "not looked up", row: cacheTestRow{source: "seed"}},
		{name: "malformed JSON", row: cacheTestRow{raw: []byte(`{"city":4}`), source: "fixture", lookedUp: true}, wantErr: "invalid cached geolocation"},
		{name: "location", row: cacheTestRow{raw: []byte(`{"city":"London","country":"GB"}`), source: "ip-api", lookedUp: true, lookedAt: time.Now().Add(-48 * time.Hour)}, wantFound: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cache := NewPostgresCache(&cacheTestDB{row: tc.row})
			location, source, found, err := cache.Get(ctx, "8.8.8.8")
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("Get error = %v", err)
				}
				return
			}
			if err != nil || found != tc.wantFound || (!tc.wantFound && tc.name != "missing" && source != tc.row.source) {
				t.Fatalf("Get = (%+v,%q,%v,%v)", location, source, found, err)
			}
			if tc.wantFound && (location.City != "London" || location.AgeDays < 1 || location.QualityScore == 0) {
				t.Fatalf("cached location metadata = %+v", location)
			}
		})
	}
}

func TestPostgresCacheMergePersistsAndRollsBack(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name      string
		db        *cacheTestDB
		source    string
		wantError string
	}{
		{name: "source required", wantError: "source is required"},
		{name: "begin failure", db: &cacheTestDB{beginErr: errors.New("begin failed")}, source: "ip-api", wantError: "begin failed"},
		{name: "prepare failure", db: &cacheTestDB{tx: &cacheTestTx{insertErr: errors.New("insert failed")}}, source: "ip-api", wantError: "cannot prepare"},
		{name: "read failure", db: &cacheTestDB{tx: &cacheTestTx{row: cacheTestRow{err: errors.New("read failed")}}}, source: "ip-api", wantError: "cannot read"},
		{name: "malformed cache", db: &cacheTestDB{tx: &cacheTestTx{row: cacheTestRow{raw: []byte(`{"city":4}`), lookedUp: true}}}, source: "ip-api", wantError: "invalid cached"},
		{name: "persist failure", db: &cacheTestDB{tx: &cacheTestTx{row: cacheTestRow{}}}, source: "ip-api", wantError: "cannot persist"},
		{name: "commit failure", db: &cacheTestDB{tx: &cacheTestTx{row: cacheTestRow{}, commitErr: errors.New("commit failed")}}, source: "ip-api", wantError: "cannot commit"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := tc.db
			if db == nil {
				db = &cacheTestDB{}
			}
			if db.tx != nil && tc.name == "persist failure" {
				db.tx.updateErr = errors.New("update failed")
			}
			cache := NewPostgresCache(db)
			_, _, err := cache.Merge(ctx, "8.8.8.8", Location{City: "London", Country: "GB"}, tc.source)
			if err == nil || !strings.Contains(err.Error(), tc.wantError) {
				t.Fatalf("Merge error = %v, want %q", err, tc.wantError)
			}
			if db.tx != nil && db.tx.rollbacks != 1 {
				t.Fatalf("rollback count = %d, want one", db.tx.rollbacks)
			}
		})
	}

	tx := &cacheTestTx{row: cacheTestRow{raw: []byte(`{"city":"Paris"}`), source: "ipwho", lookedUp: true}}
	db := &cacheTestDB{tx: tx}
	cache := NewPostgresCache(db)
	merged, source, err := cache.Merge(ctx, "8.8.8.8", Location{Country: "FR"}, "ip-api")
	if err != nil || merged.City != "Paris" || merged.Country != "FR" || !strings.Contains(source, "ip-api") || !tx.committed || tx.rollbacks != 1 {
		t.Fatalf("Merge = (%+v,%q,%v), transaction=%+v", merged, source, err, tx)
	}
	var stored Location
	if err := json.Unmarshal(tx.data, &stored); err != nil || stored.City != "Paris" || stored.Country != "FR" || tx.source != source {
		t.Fatalf("persisted merged record = %+v source=%q err=%v", stored, tx.source, err)
	}
	putTx := &cacheTestTx{row: cacheTestRow{}}
	if err := NewPostgresCache(&cacheTestDB{tx: putTx}).Put(ctx, "8.8.4.4", Location{Country: "US"}, "ip-api"); err != nil || !putTx.committed {
		t.Fatalf("Put = %v, committed=%v", err, putTx.committed)
	}
}
