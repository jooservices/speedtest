package server

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/jooservices/speedtest/internal/center/notify"
	"github.com/jooservices/speedtest/internal/center/settings"
)

// OwnerNotificationsStore exposes only safe delivery metadata.
type OwnerNotificationsStore interface {
	Test(context.Context, string) (notify.Delivery, error)
	Delivery(context.Context, string) (notify.Delivery, error)
	Deliveries(context.Context, string, int, int) ([]notify.Delivery, int, error)
}

type ownerNotifications struct {
	store   OwnerNotificationsStore
	limiter *nodeRateLimiter
}

func newOwnerNotificationsHandler(store OwnerNotificationsStore, auth *serverAuth, perMinute int) http.Handler {
	if perMinute <= 0 {
		perMinute = defaultAgentRequestsPerMinute
	}
	h := &ownerNotifications{store, newNodeRateLimiter(perMinute, defaultAgentBurst*2)}
	read := auth.requireSession(http.HandlerFunc(h.serve))
	write := auth.requireOwner(http.HandlerFunc(h.serve))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			read.ServeHTTP(w, r)
		} else {
			write.ServeHTTP(w, r)
		}
	})
}
func ownerNotificationsRoute(path string) bool { return strings.HasPrefix(path, "/api/notifications/") }
func notificationError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, notify.ErrUnconfigured):
		invalid(w, r, "Enable and configure this notification channel in Settings.")
	case errors.Is(err, notify.ErrNotFound):
		writeNotFoundProblem(w, r)
	default:
		writeAPIProblem(w, r, 500, "Internal failure", "Notification operation failed.")
	}
}
func (h *ownerNotifications) serve(w http.ResponseWriter, r *http.Request) {
	principal, err := ownerPrincipal(r)
	if err != nil {
		writeUnauthorized(w, r)
		return
	}
	if !allowRate(h.limiter, "owner:"+principal, w, r) {
		return
	}
	rest := strings.TrimPrefix(r.URL.Path, "/api/notifications/")
	if strings.HasPrefix(rest, "test/") {
		if r.Method != http.MethodPost {
			methodNotAllowed(w, r, http.MethodPost)
			return
		}
		channel := strings.TrimPrefix(rest, "test/")
		if _, ok := notify.Channel(settings.Settings{}, channel); !ok {
			writeNotFoundProblem(w, r)
			return
		}
		d, err := h.store.Test(r.Context(), channel)
		if err != nil {
			notificationError(w, r, err)
			return
		}
		w.Header().Set("Location", "/api/notifications/deliveries/"+d.ID)
		writeEnvelope(w, 202, "Notification queued", d)
		return
	}
	if r.Method != http.MethodGet {
		methodNotAllowed(w, r, http.MethodGet)
		return
	}
	if rest == "deliveries" {
		page, size, ok := paging(w, r)
		if !ok {
			return
		}
		if page > 1000000 {
			invalid(w, r, "page must be at most 1000000.")
			return
		}
		event := ""
		if v, present := r.URL.Query()["event"]; present {
			if len(v) != 1 || !uuidPattern.MatchString(v[0]) {
				invalid(w, r, "event must be an alert event ID.")
				return
			}
			event = v[0]
		}
		items, total, err := h.store.Deliveries(r.Context(), event, page, size)
		if err != nil {
			notificationError(w, r, err)
			return
		}
		last := (total + size - 1) / size
		if last < 1 {
			last = 1
		}
		writeJSON(w, 200, apiEnvelope{Success: true, Code: 200, Message: "Success", Data: items, Meta: map[string]any{"pagination": map[string]any{"current_page": page, "per_page": size, "total": total, "last_page": last}}})
		return
	}
	if strings.HasPrefix(rest, "deliveries/") {
		id := strings.TrimPrefix(rest, "deliveries/")
		if !uuidPattern.MatchString(id) {
			writeNotFoundProblem(w, r)
			return
		}
		d, err := h.store.Delivery(r.Context(), id)
		if err != nil {
			notificationError(w, r, err)
			return
		}
		writeEnvelope(w, 200, "Success", d)
		return
	}
	writeNotFoundProblem(w, r)
}
