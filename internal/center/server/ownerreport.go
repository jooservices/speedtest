package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/jooservices/speedtest/internal/center/report"
)

// OwnerReportStore builds the monthly ISP and plan report.
type OwnerReportStore interface {
	Build(ctx context.Context, month, engine, ispID string) (report.Report, error)
}

type ownerReport struct {
	store   OwnerReportStore
	limiter *nodeRateLimiter
	now     func() time.Time
}

func newOwnerReportHandler(store OwnerReportStore, auth *serverAuth) http.Handler {
	h := &ownerReport{store: store, limiter: newOwnerRateLimiter(), now: time.Now}
	return auth.requireSession(http.HandlerFunc(h.serve))
}

func ownerReportRoute(path string) bool { return path == "/api/reports/monthly" }

func (h *ownerReport) serve(w http.ResponseWriter, r *http.Request) {
	principal, err := ownerPrincipal(r)
	if err != nil {
		writeUnauthorized(w, r)
		return
	}
	if r.Method != http.MethodGet {
		methodNotAllowed(w, r, http.MethodGet)
		return
	}
	if !allowRate(h.limiter, "owner:"+principal, w, r) {
		return
	}
	engine, ok := parseEngine(w, r)
	if !ok {
		return
	}
	q := r.URL.Query()
	month, format, ispID := q.Get("month"), q.Get("format"), q.Get("isp")
	if month == "" {
		month = report.PreviousMonth(h.now())
	}
	if format == "" {
		format = "json"
	}
	if format != "json" && format != "csv" && format != "pdf" {
		writeAPIProblem(w, r, http.StatusUnprocessableEntity, "Validation failed", "format must be json, csv or pdf.")
		return
	}
	if ispID != "" && !idSegment.MatchString(ispID) {
		writeAPIProblem(w, r, http.StatusUnprocessableEntity, "Validation failed", "isp must be an ISP ID.")
		return
	}
	data, err := h.store.Build(r.Context(), month, engine, ispID)
	if errors.Is(err, report.ErrInvalid) {
		writeAPIProblem(w, r, http.StatusUnprocessableEntity, "Validation failed", "month must be a past or current month as YYYY-MM.")
		return
	}
	if err != nil {
		writeAPIProblem(w, r, http.StatusInternalServerError, "Internal failure", "Internal failure; safe detail only.")
		return
	}
	h.write(w, r, format, data)
}

func (h *ownerReport) write(w http.ResponseWriter, r *http.Request, format string, data report.Report) {
	var body []byte
	switch format {
	case "csv":
		raw, err := report.CSV(data)
		if err != nil {
			writeAPIProblem(w, r, http.StatusInternalServerError, "Internal failure", "Internal failure; safe detail only.")
			return
		}
		body = raw
		w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	case "pdf":
		body = report.PDF(data)
		w.Header().Set("Content-Type", "application/pdf")
	default:
		writeEnvelope(w, http.StatusOK, "Success", data)
		return
	}
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="speedtest-report-%s.%s"`, data.Month, format))
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
}
