package isp

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
)

func mustTime() time.Time { return time.Date(2026, 11, 8, 0, 0, 0, 0, time.UTC) }

type fakeDB struct {
	queryErr, execErr error
	isps              map[uint]*Entity
	byID              map[string]*Entity
	plans             map[string]map[string]any
	addrISP           map[string]string
	listRows          [][]any
	trendRows         [][]any
	exists            bool
	affected          int64
}

func newFake() *fakeDB {
	return &fakeDB{
		isps: map[uint]*Entity{}, byID: map[string]*Entity{}, plans: map[string]map[string]any{},
		addrISP: map[string]string{}, exists: true, affected: 1,
	}
}

func (f *fakeDB) put(e *Entity) { f.isps[e.ASN] = e; f.byID[e.ID] = e }

func (f *fakeDB) QueryRow(_ context.Context, sql string, args ...any) pgx.Row {
	if f.queryErr != nil {
		return scanRow{err: f.queryErr}
	}
	switch {
	case strings.Contains(sql, "FROM isps WHERE asn"):
		if e, ok := f.isps[args[0].(uint)]; ok {
			return scanRow{vals: []any{e.ID, e.ASN, e.Organisation, e.Name}}
		}
		return scanRow{err: pgx.ErrNoRows}
	case strings.Contains(sql, "SELECT EXISTS"):
		if len(args) > 0 {
			if id, ok := args[0].(string); ok {
				_, found := f.byID[id]
				if !f.exists {
					found = false
				}
				return scanRow{vals: []any{found}}
			}
		}
		return scanRow{vals: []any{f.exists}}
	case strings.Contains(sql, "FROM isps WHERE id") && strings.Contains(sql, "SELECT id::text"):
		if e, ok := f.byID[args[0].(string)]; ok {
			return scanRow{vals: []any{e.ID, e.ASN, e.Organisation, e.Name}}
		}
		return scanRow{err: pgx.ErrNoRows}
	case strings.Contains(sql, "FROM isp_plans WHERE id"):
		if p, ok := f.plans[args[0].(string)]; ok {
			return scanRow{vals: []any{p["id"], p["name"], p["advertisedDown"], p["advertisedUp"], p["createdAt"], p["updatedAt"]}}
		}
		return scanRow{err: pgx.ErrNoRows}
	case len(f.listRows) > 0:
		return scanRow{vals: f.listRows[0]}
	default:
		return scanRow{err: pgx.ErrNoRows}
	}
}

func (f *fakeDB) Exec(_ context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	if f.execErr != nil {
		return pgconn.CommandTag{}, f.execErr
	}
	switch {
	case strings.Contains(sql, "INSERT INTO isps"):
		e := &Entity{ID: args[0].(string), ASN: args[1].(uint), Organisation: args[2].(string), Name: args[3].(string)}
		f.put(e)
	case strings.Contains(sql, "UPDATE isps SET"):
		if e := f.byID[args[0].(string)]; e != nil {
			e.Organisation, e.Name = args[1].(string), args[2].(string)
		}
	case strings.Contains(sql, "UPDATE ip_addresses SET isp_id"):
		f.addrISP[fmt.Sprint(args[0])] = fmt.Sprint(args[1])
	case strings.Contains(sql, "INSERT INTO isp_plans"):
		id := args[0].(string)
		f.plans[id] = map[string]any{"id": id, "name": args[2], "advertisedDown": args[3], "advertisedUp": args[4],
			"createdAt": mustTime(), "updatedAt": mustTime()}
	case strings.Contains(sql, "UPDATE isp_plans"):
		if p := f.plans[args[0].(string)]; p != nil {
			p["name"], p["advertisedDown"], p["advertisedUp"] = args[2], args[3], args[4]
		} else {
			f.affected = 0
		}
	case strings.Contains(sql, "DELETE FROM isp_plans"):
		if _, ok := f.plans[args[0].(string)]; ok {
			delete(f.plans, args[0].(string))
		} else {
			f.affected = 0
		}
	}
	tag := pgconn.NewCommandTag(fmt.Sprintf("UPDATE %d", f.affected))
	f.affected = 1
	return tag, nil
}

func (f *fakeDB) Query(_ context.Context, sql string, _ ...any) (pgx.Rows, error) {
	if f.queryErr != nil {
		return nil, f.queryErr
	}
	if strings.Contains(sql, "FROM isp_plans") {
		var rows [][]any
		for _, p := range f.plans {
			rows = append(rows, []any{p["id"], p["name"], p["advertisedDown"], p["advertisedUp"], p["createdAt"], p["updatedAt"]})
		}
		return &rowSet{rows: rows}, nil
	}
	if strings.Contains(sql, "date_trunc") {
		return &rowSet{rows: f.trendRows}, nil
	}
	return &rowSet{rows: f.listRows}, nil
}

type scanRow struct {
	vals []any
	err  error
}

func (r scanRow) Scan(dest ...any) error {
	if r.err != nil {
		return r.err
	}
	for i := range dest {
		switch d := dest[i].(type) {
		case *string:
			*d = r.vals[i].(string)
		case *uint:
			*d = r.vals[i].(uint)
		case *int:
			*d = r.vals[i].(int)
		case *float64:
			*d = r.vals[i].(float64)
		case **float64:
			if r.vals[i] == nil {
				*d = nil
			} else {
				v := r.vals[i].(float64)
				*d = &v
			}
		case *bool:
			*d = r.vals[i].(bool)
		case *time.Time:
			*d = r.vals[i].(time.Time)
		default:
			return fmt.Errorf("unsupported %T", dest[i])
		}
	}
	return nil
}

type rowSet struct {
	rows [][]any
	i    int
	err  error
}

func (r *rowSet) Close()                                       {}
func (r *rowSet) Err() error                                   { return r.err }
func (r *rowSet) CommandTag() pgconn.CommandTag                { return pgconn.CommandTag{} }
func (r *rowSet) FieldDescriptions() []pgconn.FieldDescription { return nil }
func (r *rowSet) RawValues() [][]byte                          { return nil }
func (r *rowSet) Values() ([]any, error)                       { return nil, nil }
func (r *rowSet) Conn() *pgx.Conn                              { return nil }
func (r *rowSet) TypeMap() *pgtype.Map                         { return nil }
func (r *rowSet) Next() bool {
	if r.i >= len(r.rows) {
		return false
	}
	r.i++
	return true
}
func (r *rowSet) Scan(dest ...any) error {
	if r.i == 0 || r.i > len(r.rows) {
		return errors.New("no row")
	}
	return scanRow{vals: r.rows[r.i-1]}.Scan(dest...)
}
