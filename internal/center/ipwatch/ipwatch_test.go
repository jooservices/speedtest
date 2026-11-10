package ipwatch

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jooservices/speedtest/internal/center/agentapi"
	"github.com/jooservices/speedtest/internal/center/geo"
)

type row struct {
	values []any
	err    error
}

func (r row) Scan(dest ...any) error {
	if r.err != nil {
		return r.err
	}
	for i, v := range r.values {
		reflect.ValueOf(dest[i]).Elem().Set(reflect.ValueOf(v))
	}
	return nil
}

type transaction struct {
	pgx.Tx
	rows           []row
	queries, execs []string
	execErrAt      int
}

var errFailure = errors.New("test failure")

func (tx *transaction) QueryRow(_ context.Context, q string, _ ...any) pgx.Row {
	tx.queries = append(tx.queries, q)
	if len(tx.rows) == 0 {
		panic("unexpected query: " + q)
	}
	r := tx.rows[0]
	tx.rows = tx.rows[1:]
	return r
}
func (tx *transaction) Exec(_ context.Context, q string, _ ...any) (pgconn.CommandTag, error) {
	tx.execs = append(tx.execs, q)
	if tx.execErrAt == len(tx.execs) {
		return pgconn.CommandTag{}, errFailure
	}
	return pgconn.NewCommandTag("INSERT 0 1"), nil
}

func (tx *transaction) Begin(context.Context) (pgx.Tx, error) { return tx, nil }
func (tx *transaction) Commit(context.Context) error          { return nil }
func (tx *transaction) Rollback(context.Context) error        { return nil }

type lookup struct {
	err   error
	calls int
}

func (l *lookup) Lookup(context.Context, string) (geo.Location, error) {
	l.calls++
	return geo.Location{City: "fixture"}, l.err
}
func testTransaction(before, after *position, cached bool) *transaction {
	tx := &transaction{rows: []row{{values: []any{time.Unix(1, 0), "203.0.113.2"}}}}
	for _, p := range []*position{before, after} {
		if p == nil {
			tx.rows = append(tx.rows, row{err: pgx.ErrNoRows})
		} else {
			tx.rows = append(tx.rows, row{values: []any{p.id, p.ip}})
		}
	}
	lookedUpAt := time.Unix(1, 0)
	tx.rows = append(tx.rows, row{values: []any{cached}})
	tx.rows = append(tx.rows, row{values: []any{[]byte(`{"city":"fixture"}`), "fixture", cached, lookedUpAt}})
	tx.rows = append(tx.rows, row{values: []any{[]byte(`{"city":"fixture"}`), "fixture", cached, lookedUpAt}})
	return tx
}
func hookResult() agentapi.Result {
	ip := "203.0.113.2"
	return agentapi.Result{ID: "result", Status: "failed", IP: &ip}
}

func TestEnabledIPAPIWarnsAboutPlaintextAndTerms(t *testing.T) {
	for _, key := range []string{"IPINFO", "IPWHO", "FREEIPAPI", "GEOJS", "RIPESTAT", "IPAPICO", "IPGEOLOCATION"} {
		t.Setenv("SPEEDTEST_GEO_"+key+"_ENABLED", "false")
	}
	t.Setenv("SPEEDTEST_GEO_IPAPI_ENABLED", "true")
	t.Setenv("SPEEDTEST_GEO_IPAPI_URL", "http://127.0.0.1:18080/json")
	var logs bytes.Buffer
	watcher, closeReader, err := Open(slog.New(slog.NewTextHandler(&logs, nil)))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = closeReader() }()
	if watcher == nil || !strings.Contains(logs.String(), "plaintext HTTP") || !strings.Contains(logs.String(), "non-commercial") {
		t.Fatalf("missing ip-api safety warning: %q", logs.String())
	}
}

func TestOpenObservedBuildsOfflineReaderAndBoundedProviderSet(t *testing.T) {
	for _, name := range []string{"IPAPI", "IPINFO", "IPWHO", "FREEIPAPI", "GEOJS", "RIPESTAT", "IPAPICO", "IPGEOLOCATION"} {
		t.Setenv("SPEEDTEST_GEO_"+name+"_ENABLED", "false")
	}
	t.Setenv("SPEEDTEST_GEO_CONSENSUS", "false")
	var observed bool
	watcher, closeReader, err := OpenObserved(slog.New(slog.NewTextHandler(io.Discard, nil)), nil, func(string, string) { observed = true })
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = closeReader() }()
	if watcher.Reader == nil || len(watcher.Fallbacks) != 0 || watcher.Providers.Observe == nil || observed {
		t.Fatalf("observed watcher = %+v, called observer=%v", watcher, observed)
	}
}

type boundTestCache struct {
	putIP string
	err   error
}

func (c *boundTestCache) Get(context.Context, string) (geo.Location, string, bool, error) {
	return geo.Location{}, "", false, nil
}
func (c *boundTestCache) Put(_ context.Context, ip string, _ geo.Location, _ string) error {
	c.putIP = ip
	return c.err
}

func TestBoundCachePutUsesLockedAddress(t *testing.T) {
	cache := &boundTestCache{}
	bound := boundCache{Cache: cache, ip: "203.0.113.10"}
	if err := bound.Put(t.Context(), "198.51.100.20", geo.Location{Country: "GB"}, "ip-api"); err != nil || cache.putIP != bound.ip {
		t.Fatalf("bound Put = %v, stored IP=%q", err, cache.putIP)
	}
	cache.err = errFailure
	if err := bound.Put(t.Context(), "198.51.100.20", geo.Location{}, "ip-api"); !errors.Is(err, errFailure) {
		t.Fatalf("bound Put error = %v", err)
	}
}

func TestTimelineBranches(t *testing.T) {
	different := &position{id: "previous", ip: "203.0.113.1"}
	same := &position{id: "same", ip: "203.0.113.2"}
	for _, tc := range []struct {
		name          string
		before, after *position
		signal        bool
	}{
		{"first", nil, nil, false}, {"same", same, nil, false}, {"newest change", different, nil, true},
		{"late first", nil, different, false}, {"late merge", same, same, false}, {"late split", different, different, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tx := testTransaction(tc.before, tc.after, true)
			var log bytes.Buffer
			w := &Watcher{Logger: slog.New(slog.NewTextHandler(&log, nil))}
			err := w.AfterResultStored(t.Context(), tx, "node", hookResult())
			if err != nil {
				t.Fatal(err)
			}
			joined := strings.Join(tx.execs, "\n")
			if strings.Contains(joined, "pg_notify") != tc.signal || strings.Contains(joined, "INSERT INTO ip_change_signals") != tc.signal {
				t.Fatal(joined)
			}
			if strings.Contains(log.String(), "late IP observation") != (tc.after != nil) {
				t.Fatal(log.String())
			}
			if !strings.Contains(tx.queries[1], "DESC") || !strings.Contains(tx.queries[2], "ASC") {
				t.Fatal(tx.queries)
			}
		})
	}
}
func TestTransactionFailures(t *testing.T) {
	different := &position{id: "previous", ip: "203.0.113.1"}
	// Inject a failure at every database boundary in the newest-change path.
	for i := range 4 {
		t.Run("query"+string(rune('0'+i)), func(t *testing.T) {
			tx := testTransaction(different, nil, true)
			tx.rows[i].err = errFailure
			if err := (&Watcher{}).AfterResultStored(t.Context(), tx, "node", hookResult()); err == nil {
				t.Fatal("query failure lost")
			}
		})
	}
	for i := 1; i <= 5; i++ {
		t.Run("exec"+string(rune('0'+i)), func(t *testing.T) {
			tx := testTransaction(different, nil, false)
			tx.execErrAt = i
			w := &Watcher{Offline: &lookup{}}
			if err := w.AfterResultStored(t.Context(), tx, "node", hookResult()); err == nil {
				t.Fatal("write failure lost", tx.execs)
			}
		})
	}
	tx := testTransaction(nil, different, true)
	tx.execErrAt = 2
	if err := (&Watcher{}).AfterResultStored(t.Context(), tx, "node", hookResult()); !errors.Is(err, errFailure) {
		t.Fatal(err)
	}
	tx = testTransaction(different, &position{id: "next", ip: "203.0.113.2"}, true)
	tx.execErrAt = 2
	if err := (&Watcher{}).AfterResultStored(t.Context(), tx, "node", hookResult()); !errors.Is(err, errFailure) {
		t.Fatal(err)
	}
}
func TestGeoLookupAndDegradation(t *testing.T) {
	for _, err := range []error{nil, geo.ErrNotFound, errFailure} {
		tx := testTransaction(nil, nil, false)
		source := &lookup{err: err}
		w := &Watcher{Offline: source, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
		got := w.AfterResultStored(t.Context(), tx, "node", hookResult())
		if (got != nil) != (err == errFailure) || source.calls != 1 {
			t.Fatal(got, source.calls)
		}
	}
	tx := &transaction{}
	if err := (&Watcher{}).AfterResultStored(t.Context(), tx, "node", agentapi.Result{ID: "result"}); err != nil || len(tx.queries) != 0 || len(tx.execs) != 0 {
		t.Fatal("NULL IP reached the watcher", err, tx.queries, tx.execs)
	}
	if (&Watcher{}).logger() != slog.Default() {
		t.Fatal("default logger")
	}
}
func TestOpen(t *testing.T) {
	t.Setenv("SPEEDTEST_GEO_IPAPI_ENABLED", "false")
	t.Setenv("SPEEDTEST_GEO_IPINFO_ENABLED", "false")
	t.Setenv("SPEEDTEST_GEOIP_CITY_DB", t.TempDir()+"/city.mmdb")
	t.Setenv("SPEEDTEST_GEOIP_ASN_DB", t.TempDir()+"/asn.mmdb")
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	w, closeReader, err := Open(logger)
	if err != nil || w.Offline == nil {
		t.Fatal(err)
	}
	if err = closeReader(); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SPEEDTEST_GEO_PROVIDER_TIMEOUT", "bad")
	if _, _, err = Open(logger); err == nil {
		t.Fatal("invalid configuration accepted")
	}
	t.Setenv("SPEEDTEST_GEO_PROVIDER_TIMEOUT", "5s")
	t.Setenv("SPEEDTEST_GEO_IPAPI_ENABLED", "true")
	t.Setenv("SPEEDTEST_GEO_IPAPI_URL", ":bad")
	if _, _, err = Open(logger); err == nil {
		t.Fatal("invalid provider accepted")
	}
	t.Setenv("SPEEDTEST_GEO_IPAPI_ENABLED", "false")
	t.Setenv("SPEEDTEST_GEOIP_ASN_DB", "same.mmdb")
	t.Setenv("SPEEDTEST_GEOIP_CITY_DB", "same.mmdb")
	if _, _, err = Open(logger); err == nil {
		t.Fatal("overlapping databases accepted")
	}
}
