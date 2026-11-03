package server

import (
	"context"
	"net/http"
	"strconv"
	"time"

	"github.com/jooservices/speedtest/internal/center/dashboard"
	"github.com/jooservices/speedtest/internal/center/stats"
)

// OwnerDashboardStore builds the dashboard snapshot and the comparison series.
type OwnerDashboardStore interface {
	Snapshot(ctx context.Context, q stats.Query) (dashboard.Snapshot, error)
	Series(ctx context.Context, q stats.Query, percent bool, maxPoints int) ([]stats.Series, error)
}

type ownerDashboard struct {
	store   OwnerDashboardStore
	limiter *nodeRateLimiter
	window  *ownerStats
}

func newOwnerDashboardHandler(store OwnerDashboardStore, auth *serverAuth) http.Handler {
	h := &ownerDashboard{store: store, limiter: newOwnerRateLimiter(), window: &ownerStats{now: time.Now}}
	return auth.requireSession(http.HandlerFunc(h.serve))
}

func ownerDashboardRoute(path string) bool { return path == "/api/dashboard" || path == "/api/series" }

func (h *ownerDashboard) serve(w http.ResponseWriter, r *http.Request) {
	principal, err := ownerPrincipal(r)
	if err != nil {
		writeUnauthorized(w, r)
		return
	}
	if !allowRate(h.limiter, "owner:"+principal, w, r) {
		return
	}
	if r.Method != http.MethodGet {
		methodNotAllowed(w, r, http.MethodGet)
		return
	}
	q, ok := h.window.window(w, r)
	if !ok {
		return
	}
	if r.URL.Path == "/api/dashboard" {
		snapshot, err := h.store.Snapshot(r.Context(), q)
		if err != nil {
			writeStatsError(w, r, err)
			return
		}
		writeEnvelope(w, http.StatusOK, "Success", snapshot)
		return
	}
	h.series(w, r, q)
}

func (h *ownerDashboard) series(w http.ResponseWriter, r *http.Request, q stats.Query) {
	var ok bool
	if q.NodeIDs, q.NodesSet, ok = parseStatsNodes(w, r); !ok {
		return
	}
	if q.Metric, ok = parseStatsMetric(w, r); !ok {
		return
	}
	if _, ok = parseStatsTimezone(w, r); !ok {
		return
	}
	percent := false
	if raw, present, valid := singleStatsParam(w, r, "mode"); !valid {
		return
	} else if present {
		switch raw {
		case "absolute":
		case "percent":
			percent = true
		default:
			statsInvalid(w, r, "mode must be absolute or percent.")
			return
		}
	}
	if percent && q.Metric != stats.Download && q.Metric != stats.Upload {
		statsInvalid(w, r, "percent mode is available only for the down and up metrics.")
		return
	}
	maxPoints := stats.DefaultMaxPoints
	if raw, present, valid := singleStatsParam(w, r, "maxPoints"); !valid {
		return
	} else if present {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 || n > stats.MaxPointsLimit {
			statsInvalid(w, r, "maxPoints must be an integer from 1 to 300.")
			return
		}
		maxPoints = n
	}
	items, err := h.store.Series(r.Context(), q, percent, maxPoints)
	if err != nil {
		writeStatsError(w, r, err)
		return
	}
	writeEnvelope(w, http.StatusOK, "Success", items)
}
