package report

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/csv"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
	"unicode/utf16"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
)

type fakePool struct {
	main, worst, down, changes [][]any
	err                        error
	failOn                     string
	args                       [][]any
}

func (f *fakePool) Query(_ context.Context, sql string, args ...any) (pgx.Rows, error) {
	f.args = append(f.args, args)
	if f.err != nil {
		return nil, f.err
	}
	switch {
	case strings.Contains(sql, "row_number()"):
		return &rows{data: f.worst, err: f.rowErr("worst")}, nil
	case strings.Contains(sql, "LEAD("):
		return &rows{data: f.down, err: f.rowErr("down")}, nil
	case strings.Contains(sql, "FROM node_ip_history\nWHERE"):
		return &rows{data: f.changes, err: f.rowErr("changes")}, nil
	}
	return &rows{data: f.main, err: f.rowErr("main")}, nil
}

func (f *fakePool) rowErr(name string) error {
	if f.failOn == name {
		return errors.New("rows failed")
	}
	return nil
}

type rows struct {
	data [][]any
	i    int
	err  error
}

func (r *rows) Close()                                       {}
func (r *rows) Err() error                                   { return r.err }
func (r *rows) CommandTag() pgconn.CommandTag                { return pgconn.CommandTag{} }
func (r *rows) FieldDescriptions() []pgconn.FieldDescription { return nil }
func (r *rows) RawValues() [][]byte                          { return nil }
func (r *rows) Values() ([]any, error)                       { return nil, nil }
func (r *rows) Conn() *pgx.Conn                              { return nil }
func (r *rows) TypeMap() *pgtype.Map                         { return nil }
func (r *rows) Next() bool                                   { r.i++; return r.i <= len(r.data) }
func (r *rows) Scan(dest ...any) error {
	row := r.data[r.i-1]
	if len(row) == 1 {
		return errors.New("scan failed")
	}
	for i, d := range dest {
		switch p := d.(type) {
		case *string:
			*p = row[i].(string)
		case *int:
			*p = row[i].(int)
		case *int64:
			*p = row[i].(int64)
		case **string:
			if row[i] != nil {
				if v, ok := row[i].(*string); ok {
					*p = v
				} else {
					v := row[i].(string)
					*p = &v
				}
			}
		case **float64:
			if row[i] != nil {
				v := row[i].(float64)
				*p = &v
			}
		default:
			return fmt.Errorf("unsupported %T", d)
		}
	}
	return nil
}

var now = time.Date(2026, 11, 2, 3, 0, 0, 0, time.UTC)

func mainRow(isp, node string, plan *string, adv, avg any) []any {
	return []any{"i-1", isp, int64(7552), node, node + "-name", plan, adv, adv, 100, 5, avg, avg, avg, avg}
}

func TestWindowAndPreviousMonth(t *testing.T) {
	from, to, err := Window("2026-10", now)
	if err != nil || !from.Equal(time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)) || !to.Equal(time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("%v %v %v", from, to, err)
	}
	for _, bad := range []string{"", "2026-13", "2026-1", "26-10", "2026-10-01", "1999-01", "2027-01"} {
		if _, _, err := Window(bad, now); !errors.Is(err, ErrInvalid) {
			t.Fatalf("%q accepted: %v", bad, err)
		}
	}
	if PreviousMonth(now) != "2026-10" || PreviousMonth(time.Date(2026, 1, 5, 0, 0, 0, 0, time.UTC)) != "2025-12" {
		t.Fatal("previous month")
	}
}

func TestBuildMergesSections(t *testing.T) {
	plan := "Fast 300"
	f := &fakePool{
		main:    [][]any{mainRow("Viettel", "n1", &plan, 300.0, 270.0), mainRow("Viettel", "n2", nil, nil, nil)},
		worst:   [][]any{{"i-1", "n1", 3}, {"i-1", "n1", 4}},
		down:    [][]any{{"i-1", "n1", 25}},
		changes: [][]any{{"i-1", "n2", 2}},
	}
	s := &Store{DB: f, Now: func() time.Time { return now }}
	r, err := s.Build(context.Background(), "2026-10", "", "")
	if err != nil || len(r.Rows) != 2 || r.Partial {
		t.Fatalf("%v %#v", err, r)
	}
	a, b := r.Rows[0], r.Rows[1]
	if *a.DeliveredPct != 90 || a.DowntimeMin != 25 || len(a.WorstHours) != 2 || b.IPChanges != 2 || b.DeliveredPct != nil || b.WorstHours == nil {
		t.Fatalf("%#v %#v", a, b)
	}
	// An explicit engine and ISP add their arguments; the current month is partial.
	if _, err = s.Build(context.Background(), "2026-11", "librespeed", "00000000-0000-4000-8000-000000000001"); err != nil {
		t.Fatal(err)
	}
	last := f.args[len(f.args)-1]
	if len(last) != 3 || last[2] != "00000000-0000-4000-8000-000000000001" {
		t.Fatalf("changes args %v", last)
	}
}

func TestBuildPartialEmptyAndErrors(t *testing.T) {
	s := &Store{DB: &fakePool{}, Now: func() time.Time { return now }}
	r, err := s.Build(context.Background(), "2026-11", "", "")
	if err != nil || !r.Partial || len(r.Rows) != 0 || r.Rows == nil {
		t.Fatalf("%v %#v", err, r)
	}
	if _, err = s.Build(context.Background(), "nope", "", ""); !errors.Is(err, ErrInvalid) {
		t.Fatal(err)
	}
	plan := "p"
	good := [][]any{mainRow("A", "n1", &plan, 100.0, 90.0)}
	for _, name := range []string{"main", "worst", "down", "changes"} {
		bad := &Store{DB: &fakePool{main: good, worst: [][]any{{"i-1", "n1", 1}}, down: [][]any{{"i-1", "n1", 1}}, changes: [][]any{{"i-1", "n1", 1}}, failOn: name}, Now: s.Now}
		if _, err = bad.Build(context.Background(), "2026-10", "", ""); err == nil {
			t.Fatalf("%s: error swallowed", name)
		}
	}
	for _, name := range []string{"main", "worst", "down", "changes"} {
		f := &fakePool{main: [][]any{{"short"}}, worst: [][]any{{"short"}}, down: [][]any{{"short"}}, changes: [][]any{{"short"}}}
		if name != "main" {
			f.main = good
		}
		if _, err = (&Store{DB: f, Now: s.Now}).Build(context.Background(), "2026-10", "", ""); err == nil {
			t.Fatalf("%s: scan error swallowed", name)
		}
	}
	if _, err = (&Store{DB: &fakePool{err: errors.New("down")}, Now: s.Now}).Build(context.Background(), "2026-10", "", ""); err == nil {
		t.Fatal("query error swallowed")
	}
	if (&Store{DB: &fakePool{}}).now().IsZero() {
		t.Fatal("default clock")
	}
}

func sample() Report {
	plan := "=Fast"
	v := 270.0
	pct := 90.0
	return Report{Month: "2026-10", Rows: []Row{
		{ISP: "@Viettel (VN)", ASN: 7552, Node: "n1", Plan: &plan, AvgDown: &v, P95Down: &v, DeliveredPct: &pct, Tests: 10, Failures: 1, WorstHours: []int{3, 14}, DowntimeMin: 30, IPChanges: 1},
		{ISP: "FPT, Telecom", ASN: 18403, Node: "n2", Tests: 2, WorstHours: []int{}},
	}}
}

func TestCSVGuardsFormulasAndQuotes(t *testing.T) {
	raw, err := CSV(sample())
	if err != nil {
		t.Fatal(err)
	}
	recs, err := csv.NewReader(bytes.NewReader(raw)).ReadAll()
	if err != nil || len(recs) != 3 || len(recs[0]) != len(recs[1]) {
		t.Fatalf("%v %v", err, recs)
	}
	if recs[1][1] != "'@Viettel (VN)" || recs[1][4] != "'=Fast" || recs[1][14] != "03 14" || recs[2][1] != "FPT, Telecom" || recs[2][13] != "" {
		t.Fatalf("%v", recs)
	}
}

func TestPDFIsWellFormedAndPaginates(t *testing.T) {
	r := sample()
	r.Partial, r.Engine = true, "ookla"
	r.Rows[0].ISP = "Nguyễn Viễn Thông (AS 7552)"
	r.Rows[0].Node = strings.Repeat("long-node-name-", 9) + "tail"
	pdf, err := PDF(r)
	if err != nil {
		t.Fatal(err)
	}
	text := string(pdf)
	for _, want := range []string{"%PDF-1.3", "%%EOF", "/ToUnicode", "/FontFile2"} {
		if !strings.Contains(text, want) {
			t.Fatalf("missing %q", want)
		}
	}
	if !pdfHasText(pdf, "Nguyễn") || !pdfHasText(pdf, "tail") || !pdfHasText(pdf, "partial month") {
		t.Fatal("long value was clipped")
	}
	big := Report{Month: "2026-10"}
	for i := 0; i < 120; i++ {
		big.Rows = append(big.Rows, Row{ISP: "ISP-with-a-very-long-name-that-wraps", Node: "n", Tests: i})
	}
	pagePDF, err := PDF(big)
	if err != nil {
		t.Fatal(err)
	}
	if pages := strings.Count(string(pagePDF), "/Type /Page\n"); pages < 2 {
		t.Fatalf("pages=%d", pages)
	}
	empty, err := PDF(Report{Month: "2026-10"})
	if err != nil || !pdfHasText(empty, "No tests reported this month") {
		t.Fatal("empty report text")
	}
	if !pdfHasText(empty, "engine: primary") {
		t.Fatal("engine name")
	}
}

func pdfHasText(pdf []byte, value string) bool {
	encoded := make([]byte, 0, len(value)*2)
	for _, unit := range utf16.Encode([]rune(value)) {
		var pair [2]byte
		binary.BigEndian.PutUint16(pair[:], unit)
		encoded = append(encoded, pair[:]...)
	}
	marker, endMarker := []byte("stream\n"), []byte("endstream")
	for offset := 0; offset < len(pdf); {
		start := bytes.Index(pdf[offset:], marker)
		if start < 0 {
			return false
		}
		start += offset + len(marker)
		end := bytes.Index(pdf[start:], endMarker)
		if end < 0 {
			return false
		}
		if bytes.Contains(pdf[start:start+end], encoded) {
			return true
		}
		offset = start + end + len(endMarker)
	}
	return false
}

type claimDB struct {
	runs       map[string]*claimRun
	err        error
	releaseErr error
	sentErr    error
}

type claimRun struct {
	leaseID    string
	leaseUntil time.Time
	nextTry    time.Time
	attempts   int
	sent       bool
}

func (c *claimDB) Exec(_ context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	if c.err != nil {
		return pgconn.CommandTag{}, c.err
	}
	switch {
	case strings.HasPrefix(sql, "INSERT INTO monthly_report_runs"):
		month, id, until, at := args[0].(string), args[1].(string), args[2].(time.Time), args[3].(time.Time)
		run := c.runs[month]
		if run != nil && (run.sent || run.nextTry.After(at) || (run.leaseID != "" && run.leaseUntil.After(at))) {
			return pgconn.NewCommandTag("INSERT 0 0"), nil
		}
		if run == nil {
			run = &claimRun{nextTry: at}
			c.runs[month] = run
		}
		run.leaseID, run.leaseUntil, run.attempts = id, until, run.attempts+1
		return pgconn.NewCommandTag("INSERT 0 1"), nil
	case strings.Contains(sql, "SET lease_id = NULL"):
		if c.releaseErr != nil {
			return pgconn.CommandTag{}, c.releaseErr
		}
		run := c.runs[args[0].(string)]
		if run == nil || run.leaseID != args[1].(string) || run.sent {
			return pgconn.NewCommandTag("UPDATE 0"), nil
		}
		run.leaseID, run.leaseUntil, run.nextTry = "", time.Time{}, args[2].(time.Time)
		return pgconn.NewCommandTag("UPDATE 1"), nil
	case strings.Contains(sql, "SET sent_at"):
		if c.sentErr != nil {
			return pgconn.CommandTag{}, c.sentErr
		}
		run := c.runs[args[0].(string)]
		if run == nil || run.leaseID != args[1].(string) || run.sent {
			return pgconn.NewCommandTag("UPDATE 0"), nil
		}
		run.sent, run.leaseID, run.leaseUntil = true, "", time.Time{}
		return pgconn.NewCommandTag("UPDATE 1"), nil
	default:
		return pgconn.CommandTag{}, fmt.Errorf("unexpected claim query")
	}
}

func TestDueWindow(t *testing.T) {
	for at, want := range map[time.Time]bool{
		time.Date(2026, 11, 1, 0, 59, 0, 0, time.UTC):  false,
		time.Date(2026, 11, 1, 1, 0, 0, 0, time.UTC):   true,
		time.Date(2026, 11, 3, 23, 0, 0, 0, time.UTC):  true,
		time.Date(2026, 11, 4, 0, 0, 0, 0, time.UTC):   false,
		time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC): false,
	} {
		if Due(at) != want {
			t.Fatalf("%v", at)
		}
	}
}

func TestMailerSendsOnceAndRetriesAfterFailure(t *testing.T) {
	plan := "p"
	pool := &fakePool{main: [][]any{mainRow("Viettel", "n1", &plan, 100.0, 90.0)}}
	db := &claimDB{runs: map[string]*claimRun{}}
	var bodies []string
	fail := true
	at := now
	m := &Mailer{Reports: &Store{DB: pool, Now: func() time.Time { return now }}, DB: db, Now: func() time.Time { return at },
		Send: func(_ context.Context, build func(string) string) error {
			if fail {
				return errors.New("smtp down")
			}
			bodies = append(bodies, build("https://speedtest.example/"))
			return nil
		}}
	if err := m.RunOnce(t.Context()); err == nil || db.runs["2026-10"].leaseID != "" || !db.runs["2026-10"].nextTry.Equal(now.Add(retryDelay)) {
		t.Fatalf("failed send must be durably delayed: %v %+v", err, db.runs["2026-10"])
	}
	fail = false
	if err := m.RunOnce(t.Context()); err != nil || len(bodies) != 0 {
		t.Fatalf("retry ran before its due time: %v %d", err, len(bodies))
	}
	at = now.Add(retryDelay)
	m.Now = func() time.Time { return at }
	if err := m.RunOnce(t.Context()); err != nil || !db.runs["2026-10"].sent || len(bodies) != 1 {
		t.Fatalf("%v %+v %d", err, db.runs["2026-10"], len(bodies))
	}
	if err := m.RunOnce(t.Context()); err != nil || len(bodies) != 1 {
		t.Fatal("sent twice")
	}
	if !strings.Contains(bodies[0], "Viettel / n1-name") || !strings.Contains(bodies[0], "https://speedtest.example/api/reports/monthly?month=2026-10&format=pdf") {
		t.Fatal(bodies[0])
	}
	m.Now = func() time.Time { return time.Date(2026, 11, 15, 0, 0, 0, 0, time.UTC) }
	bodies = nil
	if err := m.RunOnce(t.Context()); err != nil || len(bodies) != 0 {
		t.Fatal("sent outside the window")
	}
}

func TestMailerRecoversWhenLeaseReleaseFails(t *testing.T) {
	db := &claimDB{runs: map[string]*claimRun{}, releaseErr: errors.New("write failed")}
	at, sends := now, 0
	m := &Mailer{Reports: &Store{DB: &fakePool{}, Now: func() time.Time { return now }}, DB: db, Now: func() time.Time { return at },
		Send: func(context.Context, func(string) string) error {
			sends++
			if sends == 1 {
				return errors.New("mail transport failed")
			}
			return nil
		}}
	if err := m.RunOnce(t.Context()); err == nil || db.runs["2026-10"].leaseID == "" {
		t.Fatalf("failed release must keep its expiring lease: %v %+v", err, db.runs["2026-10"])
	}
	if err := m.RunOnce(t.Context()); err != nil || sends != 1 {
		t.Fatalf("active lease allowed another sender: %d %v", sends, err)
	}
	db.releaseErr = nil
	at = now.Add(claimLease)
	if err := m.RunOnce(t.Context()); err != nil || !db.runs["2026-10"].sent || sends != 2 {
		t.Fatalf("expired lease did not recover: %d %+v %v", sends, db.runs["2026-10"], err)
	}
}

func TestMailerRecoversWhenSentStateWriteFails(t *testing.T) {
	db := &claimDB{runs: map[string]*claimRun{}, sentErr: errors.New("write failed")}
	at, sends := now, 0
	m := &Mailer{Reports: &Store{DB: &fakePool{}, Now: func() time.Time { return now }}, DB: db, Now: func() time.Time { return at },
		Send: func(context.Context, func(string) string) error { sends++; return nil }}
	if err := m.RunOnce(t.Context()); err == nil || db.runs["2026-10"].leaseID == "" || db.runs["2026-10"].sent {
		t.Fatalf("failed sent-state write must retain an expiring lease: %v %+v", err, db.runs["2026-10"])
	}
	if err := m.RunOnce(t.Context()); err != nil || sends != 1 {
		t.Fatalf("active lease allowed a concurrent retry: %d %v", sends, err)
	}
	db.sentErr = nil
	at = now.Add(claimLease)
	if err := m.RunOnce(t.Context()); err != nil || !db.runs["2026-10"].sent || sends != 2 {
		t.Fatalf("expired lease did not recover: %d %+v %v", sends, db.runs["2026-10"], err)
	}
}

func TestMailerCancellationReleasesClaimForRetry(t *testing.T) {
	db := &claimDB{runs: map[string]*claimRun{}}
	at, sends := now, 0
	ctx, cancel := context.WithCancel(t.Context())
	m := &Mailer{Reports: &Store{DB: &fakePool{}, Now: func() time.Time { return now }}, DB: db, Now: func() time.Time { return at },
		Send: func(context.Context, func(string) string) error { sends++; cancel(); return ctx.Err() }}
	if err := m.RunOnce(ctx); err == nil || db.runs["2026-10"].leaseID != "" || !db.runs["2026-10"].nextTry.Equal(now.Add(retryDelay)) {
		t.Fatalf("cancellation must persist a retry time: %v %+v", err, db.runs["2026-10"])
	}
	at = now.Add(retryDelay)
	m.Send = func(context.Context, func(string) string) error { sends++; return nil }
	if err := m.RunOnce(t.Context()); err != nil || !db.runs["2026-10"].sent || sends != 2 {
		t.Fatalf("cancelled delivery did not retry: sends=%d run=%+v err=%v", sends, db.runs["2026-10"], err)
	}
}

func TestMailerErrorsAndLoop(t *testing.T) {
	db := &claimDB{runs: map[string]*claimRun{}, err: errors.New("db")}
	m := &Mailer{Reports: &Store{DB: &fakePool{}, Now: func() time.Time { return now }}, DB: db, Now: func() time.Time { return now }, Every: time.Millisecond,
		Send: func(context.Context, func(string) string) error { return nil }}
	if m.RunOnce(t.Context()) == nil {
		t.Fatal("claim error swallowed")
	}
	if (&Mailer{}).now().IsZero() {
		t.Fatal("default clock")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Millisecond)
	defer cancel()
	m.Loop(ctx) // logs nothing without a logger and stops with the context
	empty := Summary(Report{Month: "2026-10"}, "")
	if !strings.Contains(empty, "No tests") || strings.Contains(empty, "http") {
		t.Fatal(empty)
	}
	m.Reports = &Store{DB: &fakePool{err: errors.New("x")}, Now: func() time.Time { return now }}
	m.DB = &claimDB{runs: map[string]*claimRun{}}
	if m.RunOnce(t.Context()) == nil {
		t.Fatal("build error swallowed")
	}
}
