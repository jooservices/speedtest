package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/jooservices/speedtest/internal/center/isp"
)

// OwnerISPStore is the dashboard ISP API.
type OwnerISPStore interface {
	List(context.Context, isp.Query) ([]map[string]any, error)
	Get(context.Context, string, isp.Query) (map[string]any, error)
	Trend(context.Context, string, isp.Query) ([]map[string]any, error)
	Compare(context.Context, isp.Query) ([]map[string]any, error)
	ListPlans(context.Context, string) ([]map[string]any, error)
	CreatePlan(context.Context, string, string, float64, float64) (map[string]any, error)
	UpdatePlan(context.Context, string, string, string, float64, float64) (map[string]any, error)
	DeletePlan(context.Context, string, string) error
}

type ownerISP struct {
	store   OwnerISPStore
	limiter *nodeRateLimiter
	now     func() time.Time
}

func newOwnerISPHandler(store OwnerISPStore, auth *serverAuth) http.Handler {
	h := &ownerISP{store: store, limiter: newOwnerRateLimiter(), now: time.Now}
	read := auth.requireSession(http.HandlerFunc(h.serve))
	write := auth.requireOwner(http.HandlerFunc(h.serve))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			read.ServeHTTP(w, r)
			return
		}
		write.ServeHTTP(w, r)
	})
}

func ownerISPRoute(path string) bool {
	return path == "/api/isps" || path == "/api/isps/compare" || strings.HasPrefix(path, "/api/isps/")
}

func (h *ownerISP) serve(w http.ResponseWriter, r *http.Request) {
	principal, err := ownerPrincipal(r)
	if err != nil {
		writeUnauthorized(w, r)
		return
	}
	if !allowRate(h.limiter, "owner:"+principal, w, r) {
		return
	}
	switch {
	case r.URL.Path == "/api/isps" && r.Method == http.MethodGet:
		h.list(w, r)
	case r.URL.Path == "/api/isps/compare" && r.Method == http.MethodGet:
		h.compare(w, r)
	case strings.HasPrefix(r.URL.Path, "/api/isps/"):
		h.resource(w, r)
	default:
		writeMethodNotAllowedProblem(w, r)
	}
}

func (h *ownerISP) ispWindow(w http.ResponseWriter, r *http.Request) (isp.Query, bool) {
	statsQ, ok := (&ownerStats{now: h.now}).window(w, r)
	if !ok {
		return isp.Query{}, false
	}
	return isp.Query{Engine: statsQ.Engine, From: statsQ.From, To: statsQ.To}, true
}

func (h *ownerISP) list(w http.ResponseWriter, r *http.Request) {
	q, ok := h.ispWindow(w, r)
	if !ok {
		return
	}
	items, err := h.store.List(r.Context(), q)
	if err != nil {
		writeISPError(w, r, err)
		return
	}
	writeEnvelope(w, http.StatusOK, "Success", items)
}

func (h *ownerISP) compare(w http.ResponseWriter, r *http.Request) {
	q, ok := h.ispWindow(w, r)
	if !ok {
		return
	}
	raw := strings.TrimSpace(r.URL.Query().Get("isps"))
	if raw == "" {
		writeAPIProblem(w, r, http.StatusUnprocessableEntity, "Validation failed", "isps must list at least one ISP ID.")
		return
	}
	for _, id := range strings.Split(raw, ",") {
		id = strings.TrimSpace(id)
		if id == "" {
			continue
		}
		q.ISPIDs = append(q.ISPIDs, id)
	}
	if len(q.ISPIDs) == 0 || len(q.ISPIDs) > 20 {
		writeAPIProblem(w, r, http.StatusUnprocessableEntity, "Validation failed", "isps must contain between 1 and 20 ISP IDs.")
		return
	}
	items, err := h.store.Compare(r.Context(), q)
	if err != nil {
		writeISPError(w, r, err)
		return
	}
	writeEnvelope(w, http.StatusOK, "Success", items)
}

func (h *ownerISP) resource(w http.ResponseWriter, r *http.Request) {
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/api/isps/"), "/")
	if len(parts) == 0 || parts[0] == "" || !uuidPattern.MatchString(parts[0]) {
		writeNotFoundProblem(w, r)
		return
	}
	id := parts[0]
	switch {
	case len(parts) == 1 && r.Method == http.MethodGet:
		h.get(w, r, id)
	case len(parts) == 2 && parts[1] == "trend" && r.Method == http.MethodGet:
		h.trend(w, r, id)
	case len(parts) == 2 && parts[1] == "plans":
		h.plans(w, r, id)
	case len(parts) == 3 && parts[1] == "plans" && uuidPattern.MatchString(parts[2]):
		h.planItem(w, r, id, parts[2])
	default:
		writeNotFoundProblem(w, r)
	}
}

func (h *ownerISP) get(w http.ResponseWriter, r *http.Request, id string) {
	q, ok := h.ispWindow(w, r)
	if !ok {
		return
	}
	item, err := h.store.Get(r.Context(), id, q)
	if err != nil {
		writeISPError(w, r, err)
		return
	}
	writeEnvelope(w, http.StatusOK, "Success", item)
}

func (h *ownerISP) trend(w http.ResponseWriter, r *http.Request, id string) {
	q, ok := h.ispWindow(w, r)
	if !ok {
		return
	}
	points, err := h.store.Trend(r.Context(), id, q)
	if err != nil {
		writeISPError(w, r, err)
		return
	}
	writeEnvelope(w, http.StatusOK, "Success", points)
}

func (h *ownerISP) plans(w http.ResponseWriter, r *http.Request, ispID string) {
	switch r.Method {
	case http.MethodGet:
		items, err := h.store.ListPlans(r.Context(), ispID)
		if err != nil {
			writeISPError(w, r, err)
			return
		}
		writeEnvelope(w, http.StatusOK, "Success", items)
	case http.MethodPost:
		name, down, up, ok := readPlanBody(w, r)
		if !ok {
			return
		}
		item, err := h.store.CreatePlan(r.Context(), ispID, name, down, up)
		if err != nil {
			writeISPError(w, r, err)
			return
		}
		writeEnvelope(w, http.StatusCreated, "Created", item)
	default:
		writeMethodNotAllowedProblem(w, r)
	}
}

func (h *ownerISP) planItem(w http.ResponseWriter, r *http.Request, ispID, planID string) {
	switch r.Method {
	case http.MethodPut:
		name, down, up, ok := readPlanBody(w, r)
		if !ok {
			return
		}
		item, err := h.store.UpdatePlan(r.Context(), ispID, planID, name, down, up)
		if err != nil {
			writeISPError(w, r, err)
			return
		}
		writeEnvelope(w, http.StatusOK, "Success", item)
	case http.MethodDelete:
		if err := h.store.DeletePlan(r.Context(), ispID, planID); err != nil {
			writeISPError(w, r, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	default:
		writeMethodNotAllowedProblem(w, r)
	}
}

func readPlanBody(w http.ResponseWriter, r *http.Request) (string, float64, float64, bool) {
	defer func() { _ = r.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		writeAPIProblem(w, r, http.StatusBadRequest, "Bad request", "Request body could not be read.")
		return "", 0, 0, false
	}
	var in struct {
		Name string   `json:"name"`
		Down *float64 `json:"advertisedDown"`
		Up   *float64 `json:"advertisedUp"`
	}
	if json.Unmarshal(body, &in) != nil || in.Down == nil || in.Up == nil {
		writeAPIProblem(w, r, http.StatusUnprocessableEntity, "Validation failed", "name, advertisedDown and advertisedUp are required.")
		return "", 0, 0, false
	}
	return in.Name, *in.Down, *in.Up, true
}

func writeISPError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, isp.ErrNotFound):
		writeAPIProblem(w, r, http.StatusNotFound, "Not found", "ISP or plan was not found.")
	case errors.Is(err, isp.ErrInvalid):
		writeAPIProblem(w, r, http.StatusUnprocessableEntity, "Validation failed", "ISP plan input is invalid.")
	default:
		writeAPIProblem(w, r, http.StatusInternalServerError, "Internal failure", "Internal failure; safe detail only.")
	}
}
