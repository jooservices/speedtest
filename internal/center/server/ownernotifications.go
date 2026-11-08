package server

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/jooservices/speedtest/internal/center/notify"
	"github.com/jooservices/speedtest/internal/center/settings"
)

// OwnerNotificationsStore exposes delivery metadata and the notification catalog.
type OwnerNotificationsStore interface {
	Test(context.Context, string) (notify.Delivery, error)
	Delivery(context.Context, string) (notify.Delivery, error)
	Deliveries(context.Context, string, int, int) ([]notify.Delivery, int, error)
	ListNotificationEvents(context.Context) ([]notify.EventView, error)
	GetNotificationEvent(context.Context, string) (notify.EventView, error)
	UpdateNotificationEvent(context.Context, string, notify.EventPatch) (notify.EventView, error)
	GetNotificationTemplate(context.Context, string, string) (notify.TemplateView, error)
	PutNotificationTemplate(context.Context, string, string, string) (notify.TemplateView, error)
	ResetNotificationTemplate(context.Context, string, string) (notify.TemplateView, error)
	PreviewNotificationTemplate(context.Context, string, string, string) (notify.TemplatePreview, error)
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
	if notificationCatalogError(w, r, err) {
		return
	}
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
	switch {
	case strings.HasPrefix(rest, "test/"):
		h.queueTest(w, r, strings.TrimPrefix(rest, "test/"))
	case rest == "events" || strings.HasPrefix(rest, "events/") || strings.HasPrefix(rest, "templates/"):
		h.serveCatalog(w, r, rest)
	case rest == "deliveries":
		if r.Method != http.MethodGet {
			methodNotAllowed(w, r, http.MethodGet)
			return
		}
		h.listDeliveries(w, r)
	case strings.HasPrefix(rest, "deliveries/"):
		if r.Method != http.MethodGet {
			methodNotAllowed(w, r, http.MethodGet)
			return
		}
		h.getDelivery(w, r, strings.TrimPrefix(rest, "deliveries/"))
	default:
		writeNotFoundProblem(w, r)
	}
}

// queueTest sends a test message through one channel (POST /api/notifications/test/{channel}).
func (h *ownerNotifications) queueTest(w http.ResponseWriter, r *http.Request, channel string) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w, r, http.MethodPost)
		return
	}
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
	writeEnvelope(w, http.StatusAccepted, "Notification queued", d)
}

// listDeliveries pages the delivery attempts, optionally for one alert event.
func (h *ownerNotifications) listDeliveries(w http.ResponseWriter, r *http.Request) {
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
	writePage(w, items, page, size, total)
}

func (h *ownerNotifications) getDelivery(w http.ResponseWriter, r *http.Request, id string) {
	if !uuidPattern.MatchString(id) {
		writeNotFoundProblem(w, r)
		return
	}
	d, err := h.store.Delivery(r.Context(), id)
	if err != nil {
		notificationError(w, r, err)
		return
	}
	writeEnvelope(w, http.StatusOK, "Success", d)
}
