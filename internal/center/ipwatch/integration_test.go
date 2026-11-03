//go:build integration

package ipwatch_test

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/joho/godotenv"
	"github.com/jooservices/speedtest/internal/center/agentapi"
	"github.com/jooservices/speedtest/internal/center/db"
	"github.com/jooservices/speedtest/internal/center/db/dbtest"
	"github.com/jooservices/speedtest/internal/center/geo"
	"github.com/jooservices/speedtest/internal/center/ipwatch"
	"github.com/jooservices/speedtest/internal/shared/config"
)

const nodeA = "00000000-0000-4000-8000-000000000001"
const nodeB = "00000000-0000-4000-8000-000000000002"

type locator struct{ calls atomic.Int64 }

func (l *locator) Lookup(context.Context, string) (geo.Location, error) {
	l.calls.Add(1)
	return geo.Location{City: "Fixture city", Country: "ZZ"}, nil
}
func setup(t *testing.T) (*pgxpool.Pool, *agentapi.Postgres, *locator) {
	t.Helper()
	values, err := godotenv.Read("../../../.env")
	if err != nil {
		t.Fatal("worktree .env required")
	}
	for k, v := range values {
		t.Setenv(k, v)
	}
	cfg, err := config.LoadCenter()
	if err != nil || db.CheckTestDatabase(cfg.Database.Name) != nil {
		t.Fatal("configured Speedtest worktree/test database required")
	}
	p, err := db.Open(t.Context(), cfg.Database)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(p.Close)
	dbtest.Reset(t, p)
	t.Cleanup(func() { dbtest.Reset(t, p) })
	dbtest.MigrateUp(t, p)
	if _, err = p.Exec(t.Context(), `INSERT INTO nodes(id,name,expected_down_mbps,expected_up_mbps) VALUES ($1,'one',100,50),($2,'two',100,50)`, nodeA, nodeB); err != nil {
		t.Fatal(err)
	}
	l := &locator{}
	return p, &agentapi.Postgres{Pool: p, Hook: &ipwatch.Watcher{Offline: l, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}}, l
}
func result(id, second int, ip string) agentapi.Result {
	d, u, ping := float64(id), float64(id)/2, 3.0
	server := 1
	return agentapi.Result{ID: fmt.Sprintf("10000000-0000-4000-8000-%012d", id), TS: time.Date(2026, 11, 3, 0, 0, second, 0, time.UTC).Format(time.RFC3339Nano), Status: "ok", Down: &d, Up: &u, Ping: &ping, ServerID: &server, IP: &ip, Threads: 1, Raw: []byte(`{}`), Server: &agentapi.Server{ID: &server, Name: "Server 1"}, Warnings: []string{}}
}
func save(t *testing.T, s *agentapi.Postgres, node string, r agentapi.Result) {
	t.Helper()
	if duplicate, err := s.SaveResult(t.Context(), node, r); err != nil || duplicate {
		t.Fatal("save", r.ID, duplicate, err)
	}
}
func scalar(t *testing.T, p *pgxpool.Pool, q string, args ...any) int {
	t.Helper()
	var n int
	if err := p.QueryRow(t.Context(), q, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}
func assertDerived(t *testing.T, p *pgxpool.Pool) {
	t.Helper()
	// Independent full recomputation is the test oracle; production repairs only
	// the inserted position and its successor, with no timeline rescan.
	q := `WITH ordered AS (SELECT id,node_id,ts,ip,lag(ip) OVER(PARTITION BY node_id ORDER BY ts,id) prev FROM results WHERE ip IS NOT NULL),
 expected AS (SELECT id,node_id,ts,prev from_ip,ip to_ip FROM ordered WHERE prev IS DISTINCT FROM ip),
 differences AS ((SELECT * FROM expected EXCEPT SELECT id,node_id,ts,from_ip,to_ip FROM node_ip_history)
 UNION ALL (SELECT id,node_id,ts,from_ip,to_ip FROM node_ip_history EXCEPT SELECT * FROM expected)) SELECT count(*) FROM differences`
	if n := scalar(t, p, q); n != 0 {
		t.Fatal("timeline differs from measurement-order oracle", n)
	}
	q = `WITH expected AS (SELECT node_id,ip,count(*) tests,min(ts) first_seen,max(ts) last_seen,
 count(*) FILTER(WHERE status<>'failed') speed_tests,coalesce(sum(down_mbps::numeric) FILTER(WHERE status<>'failed'),0) down_sum,
 coalesce(sum(up_mbps::numeric) FILTER(WHERE status<>'failed'),0) up_sum FROM results WHERE ip IS NOT NULL GROUP BY node_id,ip),
 differences AS ((SELECT * FROM expected EXCEPT SELECT * FROM node_ip_addresses) UNION ALL
 (SELECT * FROM node_ip_addresses EXCEPT SELECT * FROM expected)) SELECT count(*) FROM differences`
	if n := scalar(t, p, q); n != 0 {
		t.Fatal("aggregates differ from oracle", n)
	}
}
func TestMeasurementTimelineIntegration(t *testing.T) {
	p, s, l := setup(t)
	a, b, c := "203.0.113.1", "203.0.113.2", "2001:db8::1"
	// Starts with A->B, then late samples insert, split and merge boundaries.
	sequence := []agentapi.Result{result(10, 10, a), result(30, 30, b), result(20, 20, a), result(25, 25, b), result(15, 15, c), result(5, 5, c), result(35, 35, b), result(40, 40, a), result(39, 40, c), result(41, 40, b)}
	// Failed measurements participate in the IP timeline but not speed averages.
	sequence[4].Status = "failed"
	message := "engine unavailable"
	sequence[4].Error = &message
	sequence[4].Down = nil
	sequence[4].Up = nil
	sequence[4].Ping = nil
	sequence[4].ServerID = nil
	listener, err := p.Acquire(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Release()
	if _, err = listener.Exec(t.Context(), "LISTEN ip_changed"); err != nil {
		t.Fatal(err)
	}
	for i, r := range sequence {
		save(t, s, nodeA, r)
		assertDerived(t, p)
		want := 0
		if i >= 1 {
			want = 1
		}
		if i >= 7 {
			want = 2
		}
		if i >= 9 {
			want = 3
		}
		if n := scalar(t, p, "SELECT count(*) FROM ip_change_signals"); n != want {
			t.Fatalf("step %d signals=%d want %d", i, n, want)
		}
		if i == 1 || i == 7 || i == 9 {
			ctx, cancel := context.WithTimeout(t.Context(), time.Second)
			notification, e := listener.Conn().WaitForNotification(ctx)
			cancel()
			if e != nil || notification.Payload != r.ID {
				t.Fatal("commit notification", notification, e)
			}
		}
	}
	if n := scalar(t, p, `SELECT count(*) FROM node_ip_history WHERE from_ip IS NULL`); n != 1 {
		t.Fatal("missing initial opening", n)
	}
	if n := scalar(t, p, `SELECT count(*) FROM ip_addresses WHERE looked_up_at IS NOT NULL AND source='geolite2'`); n != 3 || l.calls.Load() != 3 {
		t.Fatal("geo cache", n, l.calls.Load())
	}
	// Same address on another independent node reuses the global location cache.
	save(t, s, nodeB, result(100, 1, a))
	assertDerived(t, p)
	if l.calls.Load() != 3 {
		t.Fatal("repeated global geolocation")
	}
	before := scalar(t, p, "SELECT count(*) FROM ip_change_signals")
	duplicate, err := s.SaveResult(t.Context(), nodeA, sequence[7])
	if err != nil || !duplicate {
		t.Fatal("replay", duplicate, err)
	}
	conflict := sequence[7]
	conflict.IP = &b
	if _, err = s.SaveResult(t.Context(), nodeA, conflict); err != agentapi.ErrConflict {
		t.Fatal("conflicting replay", err)
	}
	if n := scalar(t, p, "SELECT count(*) FROM ip_change_signals"); n != before {
		t.Fatal("replay side effect")
	}
	assertDerived(t, p)
	var current string
	if err = p.QueryRow(t.Context(), `SELECT host(ip) FROM results WHERE node_id=$1 AND ip IS NOT NULL ORDER BY ts DESC,id DESC LIMIT 1`, nodeA).Scan(&current); err != nil || current != b {
		t.Fatal("current IP", current, err)
	}
	lateUnknown := result(14, 18, "203.0.113.99")
	lateUnknown.IP = nil
	lateUnknown.Status = "failed"
	message = "speedtest: no_route"
	lateUnknown.Error = &message
	lateUnknown.Down = nil
	lateUnknown.Up = nil
	lateUnknown.Ping = nil
	lateUnknown.ServerID = nil
	historyBefore := scalar(t, p, "SELECT count(*) FROM node_ip_history")
	addressesBefore := scalar(t, p, "SELECT count(*) FROM node_ip_addresses")
	signalsBefore := scalar(t, p, "SELECT count(*) FROM ip_change_signals")
	save(t, s, nodeA, lateUnknown)
	if scalar(t, p, `SELECT count(*) FROM results WHERE id=$1 AND ip IS NULL AND status='failed'`, lateUnknown.ID) != 1 {
		t.Fatal("late failed NULL-IP result was not stored")
	}
	if scalar(t, p, "SELECT count(*) FROM node_ip_history") != historyBefore || scalar(t, p, "SELECT count(*) FROM node_ip_addresses") != addressesBefore || scalar(t, p, "SELECT count(*) FROM ip_change_signals") != signalsBefore {
		t.Fatal("late failed NULL-IP result changed derived IP state")
	}
	if l.calls.Load() != 3 {
		t.Fatal("late failed NULL-IP result performed geolocation", l.calls.Load())
	}
	assertDerived(t, p)
	// No pending notification was created for a late arrival or a duplicate.
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Millisecond)
	defer cancel()
	if n, e := listener.Conn().WaitForNotification(ctx); e == nil {
		t.Fatal("unexpected notification", n)
	}
	t.Log("PASS: first/repeated/alternating IPs; late insert/split/merge; equal timestamps; known-IP and NULL-IP failures; replay/conflict; cached IPv4/IPv6; only newest changes signal")
}
func TestConcurrentIngestionIntegration(t *testing.T) {
	p, s, l := setup(t)
	var wg sync.WaitGroup
	errs := make(chan error, 80)
	for i := range 80 {
		wg.Go(func() {
			node := nodeA
			if i%2 == 1 {
				node = nodeB
			}
			r := result(i+1, i/2, fmt.Sprintf("203.0.113.%d", i%3+1))
			_, err := s.SaveResult(t.Context(), node, r)
			errs <- err
			// Concurrent identical retry must never increment counters.
			_, err = s.SaveResult(t.Context(), node, r)
			errs <- err
		})
	}
	// Drain while writers run so the error channel cannot block the workers.
	go func() { wg.Wait(); close(errs) }()
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	assertDerived(t, p)
	if n := scalar(t, p, "SELECT count(*) FROM results"); n != 80 {
		t.Fatal(n)
	}
	if l.calls.Load() != 3 {
		t.Fatal("shared-address cache serialized", l.calls.Load())
	}
	t.Log("PASS: 80 concurrent samples on two independent nodes; concurrent UUID retries; exact timeline/aggregates; one lookup per shared address")
}
func TestRandomArrivalOracleIntegration(t *testing.T) {
	p, s, _ := setup(t)
	order := rand.New(rand.NewPCG(35, 35)).Perm(120)
	for _, i := range order {
		// Timestamp ties and alternating IPv4/IPv6 spellings test stored canonical IP.
		addresses := []string{"203.0.113.1", "203.0.113.2", "2001:0db8:0000::1", "2001:db8::1"}
		save(t, s, nodeA, result(i+1, i/3, addresses[i%4]))
		assertDerived(t, p)
	}
	t.Log("PASS: deterministic shuffled 120-result oracle with tied timestamps and canonical IPv6")
}
func TestMigrationBackfillAndRollbackIntegration(t *testing.T) {
	p, s, _ := setup(t)
	for i := range 8 {
		save(t, s, nodeA, result(i+1, i, fmt.Sprintf("203.0.113.%d", i%2+1)))
	}
	for range 3 {
		if err := db.Migrate(t.Context(), p, "down"); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.Migrate(t.Context(), p, "down"); err != nil {
		t.Fatal(err)
	}
	// Populate version-3 data, including an obsolete history row, as a pre-upgrade fixture.
	s.Hook = nil
	save(t, s, nodeA, result(20, 3, "203.0.113.3"))
	unknown := result(21, 21, "203.0.113.4")
	unknown.IP = nil
	unknown.Status = "failed"
	errorMessage := "speedtest: no_route"
	unknown.Error = &errorMessage
	unknown.Down = nil
	unknown.Up = nil
	unknown.Ping = nil
	unknown.ServerID = nil
	save(t, s, nodeA, unknown)
	if err := db.Migrate(t.Context(), p, "up"); err != nil {
		t.Fatal(err)
	}
	assertDerived(t, p)
	if n := scalar(t, p, "SELECT count(*) FROM ip_change_signals"); n != 0 {
		t.Fatal("upgrade replayed old alerts")
	}
	if n := scalar(t, p, "SELECT count(*) FROM results"); n != 10 {
		t.Fatal("rollback lost results")
	}
	if scalar(t, p, `SELECT count(*) FROM node_ip_history h JOIN results r USING(id) WHERE r.ip IS NULL`) != 0 || scalar(t, p, `SELECT count(*) FROM node_ip_addresses a JOIN results r ON a.node_id=r.node_id AND a.ip IS NOT DISTINCT FROM r.ip WHERE r.id=$1`, unknown.ID) != 0 {
		t.Fatal("nullable result leaked into migration backfill")
	}
	t.Log("PASS: populated version-3 upgrade backfills exact non-NULL timeline/counters without old alerts; preserves NULL-IP results and cached locations")
}
func TestAtomicRollbackIntegration(t *testing.T) {
	p, s, _ := setup(t)
	save(t, s, nodeA, result(1, 1, "203.0.113.1"))
	// A signal write failure occurs after history, counters and geolocation writes.
	if _, err := p.Exec(t.Context(), `ALTER TABLE ip_change_signals ADD CONSTRAINT injected_failure CHECK(false)`); err != nil {
		t.Fatal(err)
	}
	r := result(2, 2, "203.0.113.2")
	if _, err := s.SaveResult(t.Context(), nodeA, r); err == nil || !strings.Contains(err.Error(), "injected_failure") {
		t.Fatal("expected injected failure", err)
	}
	assertDerived(t, p)
	if n := scalar(t, p, "SELECT count(*) FROM results"); n != 1 {
		t.Fatal("result leaked")
	}
	if n := scalar(t, p, "SELECT count(*) FROM ip_addresses"); n != 1 {
		t.Fatal("address/geo leaked")
	}
	if n := scalar(t, p, "SELECT count(*) FROM ip_change_signals"); n != 0 {
		t.Fatal("signal leaked")
	}
	if _, err := p.Exec(t.Context(), `ALTER TABLE ip_change_signals DROP CONSTRAINT injected_failure`); err != nil {
		t.Fatal(err)
	}
	save(t, s, nodeA, r)
	assertDerived(t, p)
	t.Log("PASS: signal failure rolls back result, timeline, counters, address and geolocation; retry succeeds")
}

func TestMappedIPv6CacheKeyIntegration(t *testing.T) {
	p, s, l := setup(t)
	r := result(1, 1, "::ffff:203.0.113.1")
	save(t, s, nodeA, r)
	r = result(2, 2, "::ffff:cb00:7101")
	save(t, s, nodeA, r)
	assertDerived(t, p)
	if n := scalar(t, p, `SELECT count(*) FROM ip_addresses WHERE looked_up_at IS NOT NULL`); n != 1 || l.calls.Load() != 1 {
		t.Fatal("mapped IPv6 cache key", n, l.calls.Load())
	}
	if n := scalar(t, p, `SELECT count(*) FROM ip_change_signals`); n != 0 {
		t.Fatal("mapped alias emitted change")
	}
	t.Log("PASS: mapped IPv6 aliases preserve the locked inet cache key and reuse location")
}

func TestDurableSignalConsumerIntegration(t *testing.T) {
	p, s, _ := setup(t)
	save(t, s, nodeA, result(1, 1, "203.0.113.1"))
	save(t, s, nodeA, result(2, 2, "203.0.113.2"))
	// There was no LISTEN connection at commit. Simulate a restarted consumer
	// finding the durable row and a failed event transaction releasing it again.
	drain := func(commit bool) {
		t.Helper()
		tx, err := p.Begin(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = tx.Rollback(context.Background()) }()
		var id string
		if err = tx.QueryRow(t.Context(), `SELECT result_id::text FROM ip_change_signals WHERE consumed_at IS NULL ORDER BY ts,result_id LIMIT 100 FOR UPDATE SKIP LOCKED`).Scan(&id); err != nil || id != result(2, 2, "").ID {
			t.Fatal("durable signal", id, err)
		}
		if _, err = tx.Exec(t.Context(), `UPDATE ip_change_signals SET consumed_at=clock_timestamp() WHERE result_id=$1`, id); err != nil {
			t.Fatal(err)
		}
		if commit {
			if err = tx.Commit(t.Context()); err != nil {
				t.Fatal(err)
			}
		}
	}
	drain(false)
	if n := scalar(t, p, `SELECT count(*) FROM ip_change_signals WHERE consumed_at IS NULL`); n != 1 {
		t.Fatal("failed consumption lost signal")
	}
	drain(true)
	if n := scalar(t, p, `SELECT count(*) FROM ip_change_signals WHERE consumed_at IS NULL`); n != 0 {
		t.Fatal("signal was not consumed")
	}
	r := result(2, 2, "203.0.113.2")
	if duplicate, err := s.SaveResult(t.Context(), nodeA, r); err != nil || !duplicate {
		t.Fatal("replay", duplicate, err)
	}
	if n := scalar(t, p, `SELECT count(*) FROM ip_change_signals WHERE consumed_at IS NULL`); n != 0 {
		t.Fatal("replay reopened consumed signal")
	}
	t.Log("PASS: consumer restart drains committed outbox without NOTIFY; rollback preserves pending work; commit consumes once; UUID retry does not reopen it")
}

func TestStoredTimestampPrecisionIntegration(t *testing.T) {
	p, s, _ := setup(t)
	newest := result(50, 50, "203.0.113.1")
	newest.TS = "2026-11-03T00:00:50.000000100Z"
	late := result(49, 50, "203.0.113.2")
	late.TS = "2026-11-03T00:00:50.000000900Z"
	save(t, s, nodeA, newest)
	save(t, s, nodeA, late)
	assertDerived(t, p)
	var current string
	if err := p.QueryRow(t.Context(), `SELECT id::text FROM results WHERE node_id=$1 ORDER BY ts DESC,id DESC LIMIT 1`, nodeA).Scan(&current); err != nil || current != newest.ID {
		t.Fatal("stored timestamp ordering", current, err)
	}
	if n := scalar(t, p, `SELECT count(*) FROM ip_change_signals`); n != 0 {
		t.Fatal("sub-microsecond late observation signaled")
	}
	t.Log("PASS: PostgreSQL timestamp precision is authoritative; UUID breaks stored timestamp ties; late sub-microsecond sample emits no signal")
}

func TestCounterNullZeroAndDegradedIntegration(t *testing.T) {
	p, s, _ := setup(t)
	r := result(1, 1, "203.0.113.1")
	r.Status = "failed"
	message := "offline"
	r.Error = &message
	r.Down = nil
	r.Up = nil
	r.Ping = nil
	r.ServerID = nil
	save(t, s, nodeA, r)
	var down, up *float64
	averages := func() {
		t.Helper()
		if err := p.QueryRow(t.Context(), `SELECT (down_sum/nullif(speed_tests,0))::float8,(up_sum/nullif(speed_tests,0))::float8 FROM node_ip_addresses WHERE node_id=$1`, nodeA).Scan(&down, &up); err != nil {
			t.Fatal(err)
		}
	}
	averages()
	if down != nil || up != nil {
		t.Fatal("failed-only averages are not null")
	}
	// Even a failed result that retains partial engine speeds is excluded.
	r = result(2, 2, "203.0.113.1")
	r.Status = "failed"
	r.Error = &message
	save(t, s, nodeA, r)
	zero := 0.0
	r = result(3, 3, "203.0.113.1")
	r.Status = "degraded"
	r.Down = &zero
	r.Up = &zero
	save(t, s, nodeA, r)
	averages()
	if down == nil || up == nil || *down != 0 || *up != 0 {
		t.Fatal("degraded zero speeds lost")
	}
	r = result(4, 4, "203.0.113.1")
	d, u := 10.0, 20.0
	r.Down = &d
	r.Up = &u
	save(t, s, nodeA, r)
	averages()
	if down == nil || up == nil || *down != 5 || *up != 10 {
		t.Fatal("average includes failed results")
	}
	assertDerived(t, p)
	t.Log("PASS: all tests count; failed/partial speeds excluded; failed-only averages null; degraded zeros count; averages 5/10 Mbps")
}
