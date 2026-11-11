package server

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/jooservices/speedtest/internal/center/report"
)

type fakeReports struct {
	err               error
	month, engine, id string
}

func (f *fakeReports) Build(_ context.Context, month, engine, id string) (report.Report, error) {
	f.month, f.engine, f.id = month, engine, id
	if f.err != nil {
		return report.Report{}, f.err
	}
	v := 100.0
	return report.Report{Month: month, Rows: []report.Row{{ISP: "Viettel", Node: "n1", ASN: 7552, Tests: 3, AvgDown: &v, WorstHours: []int{2}}}}, nil
}

func reportHandler(f *fakeReports) http.Handler { return newNodeHandler(Options{OwnerReport: f}) }

func TestOwnerReportRequiresSession(t *testing.T) {
	rec := do(reportHandler(&fakeReports{}), http.MethodGet, "/api/reports/monthly?month=2026-10", "", nil)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("code=%d", rec.Code)
	}
}

func TestOwnerReportFormats(t *testing.T) {
	f := &fakeReports{}
	h := reportHandler(f)
	for format, want := range map[string]string{"": "application/json", "json": "application/json", "csv": "text/csv; charset=utf-8", "pdf": "application/pdf"} {
		rec := do(h, http.MethodGet, "/api/reports/monthly?month=2026-10&format="+format+"&engine=ookla&isp=00000000-0000-4000-8000-000000000001", "", ownerRead)
		if rec.Code != http.StatusOK || !strings.HasPrefix(rec.Header().Get("Content-Type"), want) {
			t.Fatalf("%q -> %d %s", format, rec.Code, rec.Header().Get("Content-Type"))
		}
		if format == "csv" || format == "pdf" {
			if !strings.Contains(rec.Header().Get("Content-Disposition"), "speedtest-report-2026-10."+format) || rec.Header().Get("Cache-Control") != "no-store" {
				t.Fatalf("headers %v", rec.Header())
			}
		}
		if format == "pdf" && (!strings.HasPrefix(rec.Body.String(), "%PDF-") || !strings.Contains(rec.Body.String(), "/ToUnicode")) {
			t.Fatal("PDF response did not contain a Unicode PDF")
		}
	}
	if f.engine != "ookla" || f.id != "00000000-0000-4000-8000-000000000001" {
		t.Fatalf("filters %q %q", f.engine, f.id)
	}
}

func TestOwnerReportDefaultsToPreviousMonthAndRejectsBadInput(t *testing.T) {
	f := &fakeReports{}
	h := newNodeHandler(Options{OwnerReport: f})
	rec := do(h, http.MethodGet, "/api/reports/monthly", "", ownerRead)
	if rec.Code != http.StatusOK || f.month != report.PreviousMonth(time.Now()) {
		t.Fatalf("%d %q", rec.Code, f.month)
	}
	for _, path := range []string{"/api/reports/monthly?format=xml", "/api/reports/monthly?isp=nope", "/api/reports/monthly?engine=other"} {
		if rec := do(h, http.MethodGet, path, "", ownerRead); rec.Code != http.StatusUnprocessableEntity {
			t.Fatalf("%s -> %d", path, rec.Code)
		}
	}
	if rec := do(h, http.MethodPost, "/api/reports/monthly", "", ownerRead); rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("post -> %d", rec.Code)
	}
}

func TestOwnerReportErrors(t *testing.T) {
	if rec := do(reportHandler(&fakeReports{err: report.ErrInvalid}), http.MethodGet, "/api/reports/monthly?month=2999-01", "", ownerRead); rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("invalid -> %d", rec.Code)
	}
	rec := do(reportHandler(&fakeReports{err: errors.New("db password=secret")}), http.MethodGet, "/api/reports/monthly?month=2026-10", "", ownerRead)
	if rec.Code != http.StatusInternalServerError || strings.Contains(rec.Body.String(), "secret") {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
}
