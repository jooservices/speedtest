package ipapi

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jooservices/speedtest/internal/center/geo"
)

func TestParseLiteralAcceptsOnlyAddressLiterals(t *testing.T) {
	for in, want := range map[string]string{
		"203.0.113.5": "203.0.113.5", "\t203.0.113.5\n": "203.0.113.5", "2001:db8::1": "2001:db8::1", "2001:DB8:0:0:0:0:0:1": "2001:db8::1",
		"::ffff:203.0.113.5": "203.0.113.5", "::1": "::1",
	} {
		if got, err := ParseLiteral(in); err != nil || got != want {
			t.Errorf("ParseLiteral(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, in := range []string{"", "example.com", "localhost", "203.0.113.5:80", "203.0.113.0/24", "fe80::1%eth0", "[2001:db8::1]", "999.1.1.1", "1.2.3", "203.0.113.5\n1", "0x7f.1",
		"2001:db8::1 2001:db8::2", "http://203.0.113.5", "1234567890123456789012345678901234567890123456"} {
		if _, err := ParseLiteral(in); !errors.Is(err, ErrInvalid) {
			t.Errorf("ParseLiteral(%q) = %v, want ErrInvalid", in, err)
		}
	}
}

type fakeResolver struct {
	loc   geo.Location
	err   error
	calls []string
}

func (f *fakeResolver) Lookup(_ context.Context, ip string) (geo.Location, error) {
	f.calls = append(f.calls, ip)
	return f.loc, f.err
}

type sourceRow struct {
	source string
	err    error
}

func (r sourceRow) Scan(dest ...any) error {
	if r.err != nil {
		return r.err
	}
	*(dest[0].(*string)) = r.source
	return nil
}

type fakeDB struct{ row sourceRow }

func (fakeDB) Query(context.Context, string, ...any) (pgx.Rows, error) {
	return nil, errors.New("unused")
}
func (f fakeDB) QueryRow(context.Context, string, ...any) pgx.Row { return f.row }

func TestLookupNormalisesRefusesAndMapsResults(t *testing.T) {
	resolver := &fakeResolver{loc: geo.Location{City: "Hanoi", Country: "VN", Latitude: 21.0285, Longitude: 105.8542, TimeZone: "Asia/Ho_Chi_Minh", ASN: 7552, Organisation: "Viettel"}}
	s := &Store{DB: fakeDB{row: sourceRow{source: "GeoLite2"}}, Resolver: resolver}
	got, err := s.Lookup(t.Context(), " 2001:DB8::1 ")
	if err != nil || got["ip"] != "2001:db8::1" || got["version"] != 6 || got["city"] != "Hanoi" || got["source"] != "GeoLite2" || got["countryCode"] != "VN" {
		t.Fatalf("lookup = %v %v", got, err)
	}
	if len(resolver.calls) != 1 || resolver.calls[0] != "2001:db8::1" {
		t.Fatalf("the resolver must see the canonical literal: %v", resolver.calls)
	}
	if _, err := s.Lookup(t.Context(), "example.com"); !errors.Is(err, ErrInvalid) || len(resolver.calls) != 1 {
		t.Fatalf("host names must be refused before any lookup: %v %v", err, resolver.calls)
	}
	resolver.err = geo.ErrNotFound
	if got, err := s.Lookup(t.Context(), "203.0.113.5"); err != nil || got != nil {
		t.Fatalf("unknown address = %v %v", got, err)
	}
	resolver.err = errors.New("provider down")
	if _, err := s.Lookup(t.Context(), "203.0.113.5"); err == nil {
		t.Fatal("provider failure must be reported")
	}
	resolver.err = nil
	s.DB = fakeDB{row: sourceRow{err: pgx.ErrNoRows}}
	if got, err := s.Lookup(t.Context(), "203.0.113.5"); err != nil || got["source"] != "unknown" {
		t.Fatalf("a result that was not cached still has a source: %v %v", got, err)
	}
	s.DB = fakeDB{row: sourceRow{err: errors.New("db down")}}
	if _, err := s.Lookup(t.Context(), "203.0.113.5"); err == nil {
		t.Fatal("database failure must be reported")
	}
	if got, err := (&Store{}).Lookup(t.Context(), "203.0.113.5"); err != nil || got != nil {
		t.Fatalf("no resolver configured = %v %v", got, err)
	}
	resolver.loc = geo.Location{}
	s.DB = fakeDB{row: sourceRow{source: "ip-api"}}
	if got, err := s.Lookup(t.Context(), "203.0.113.5"); err != nil || got != nil {
		t.Fatalf("an empty location is no location: %v %v", got, err)
	}
	if nodeArg("") != nil || nodeArg("x") != "x" {
		t.Fatal("nodeArg")
	}
}

type brokenDB struct{ queryRowFails bool }

type failRow struct{}

func (failRow) Scan(...any) error { return errors.New("boom") }

type zeroRow struct{}

func (zeroRow) Scan(dest ...any) error {
	if n, ok := dest[0].(*int); ok {
		*n = 0
	}
	return nil
}

func (b brokenDB) Query(context.Context, string, ...any) (pgx.Rows, error) {
	return nil, errors.New("boom")
}
func (b brokenDB) QueryRow(context.Context, string, ...any) pgx.Row {
	if b.queryRowFails {
		return failRow{}
	}
	return zeroRow{}
}

func TestStoreReportsDatabaseFailures(t *testing.T) {
	for name, db := range map[string]brokenDB{"count": {queryRowFails: true}, "query": {}} {
		s := &Store{DB: db}
		if _, _, err := s.History(t.Context(), "", 1, 10); err == nil {
			t.Errorf("%s: history swallowed the failure", name)
		}
		if _, _, err := s.Addresses(t.Context(), "", 1, 10); err == nil {
			t.Errorf("%s: addresses swallowed the failure", name)
		}
	}
	if _, err := (&Store{DB: brokenDB{}}).Map(t.Context(), ""); err == nil {
		t.Error("map swallowed the failure")
	}
}
