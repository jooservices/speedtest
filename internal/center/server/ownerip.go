package server

import (
	"context"
	"errors"
	"net/http"

	"github.com/jooservices/speedtest/internal/center/ipapi"
)

// OwnerIPStore is the dashboard-side IP and location API; all routes are read-only and need a session.
type OwnerIPStore interface {
	History(ctx context.Context, node string, page, size int) ([]map[string]any, int, error)
	Addresses(ctx context.Context, node string, page, size int) ([]map[string]any, int, error)
	Map(ctx context.Context, node string) ([]map[string]any, error)
	Lookup(ctx context.Context, ip string) (map[string]any, error)
}

type ownerIP struct {
	store   OwnerIPStore
	limiter *nodeRateLimiter
}

func newOwnerIPHandler(store OwnerIPStore, auth *serverAuth, perMinute int) http.Handler {
	if perMinute <= 0 {
		perMinute = defaultAgentRequestsPerMinute
	}
	h := &ownerIP{store: store, limiter: newNodeRateLimiter(perMinute, defaultAgentBurst*2)}
	return auth.requireSession(http.HandlerFunc(h.serve))
}

func ownerIPRoute(path string) bool {
	switch path {
	case "/api/ip/history", "/api/ip/addresses", "/api/ip/lookup", "/api/ip/map":
		return true
	}
	return false
}

func (h *ownerIP) serve(w http.ResponseWriter, r *http.Request) {
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
	switch r.URL.Path {
	case "/api/ip/lookup":
		h.lookup(w, r)
	case "/api/ip/map":
		node, ok := ipNodeFilter(w, r)
		if !ok {
			return
		}
		items, err := h.store.Map(r.Context(), node)
		if err != nil {
			writeIPError(w, r, err)
			return
		}
		writeEnvelope(w, http.StatusOK, "Success", items)
	default:
		h.list(w, r)
	}
}

// ipNodeFilter reads the optional single-node filter.
func ipNodeFilter(w http.ResponseWriter, r *http.Request) (string, bool) {
	values, present := r.URL.Query()["node"]
	if !present {
		return "", true
	}
	if len(values) != 1 || !uuidPattern.MatchString(values[0]) {
		writeAPIProblem(w, r, http.StatusUnprocessableEntity, "Validation failed", "node must be a single node ID.")
		return "", false
	}
	return values[0], true
}

func (h *ownerIP) list(w http.ResponseWriter, r *http.Request) {
	node, ok := ipNodeFilter(w, r)
	if !ok {
		return
	}
	page, size, ok := paging(w, r)
	if !ok {
		return
	}
	var items []map[string]any
	var total int
	var err error
	if r.URL.Path == "/api/ip/history" {
		items, total, err = h.store.History(r.Context(), node, page, size)
	} else {
		items, total, err = h.store.Addresses(r.Context(), node, page, size)
	}
	if err != nil {
		writeIPError(w, r, err)
		return
	}
	last := (total + size - 1) / size
	if last < 1 {
		last = 1
	}
	writeJSON(w, http.StatusOK, apiEnvelope{Success: true, Code: http.StatusOK, Message: "Success", Data: items,
		Meta: map[string]any{"pagination": map[string]any{"current_page": page, "per_page": size, "total": total, "last_page": last}}})
}

func (h *ownerIP) lookup(w http.ResponseWriter, r *http.Request) {
	values := r.URL.Query()["ip"]
	if len(values) != 1 {
		writeAPIProblem(w, r, http.StatusUnprocessableEntity, "Validation failed", "ip must be supplied once, as an IPv4 or IPv6 address.")
		return
	}
	geo, err := h.store.Lookup(r.Context(), values[0])
	if err != nil {
		writeIPError(w, r, err)
		return
	}
	if geo == nil {
		writeEnvelope(w, http.StatusOK, "No location is known for this address", nil)
		return
	}
	writeEnvelope(w, http.StatusOK, "Success", geo)
}

func writeIPError(w http.ResponseWriter, r *http.Request, err error) {
	if errors.Is(err, ipapi.ErrInvalid) {
		writeAPIProblem(w, r, http.StatusUnprocessableEntity, "Validation failed", "ip must be an IPv4 or IPv6 address literal; host names are never resolved.")
		return
	}
	writeAPIProblem(w, r, http.StatusInternalServerError, "Internal failure", "Internal failure; safe detail only.")
}
