package server

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"

	"github.com/jooservices/speedtest/internal/center/audit"
	"github.com/jooservices/speedtest/internal/center/catalog"
	"github.com/jooservices/speedtest/internal/center/notify"
	"github.com/jooservices/speedtest/internal/center/settings"
)

// OwnerNotificationsStore exposes delivery metadata and the notification catalog.
type OwnerNotificationsStore interface {
	Test(context.Context, string) (notify.Delivery, error)
	Delivery(context.Context, string) (notify.Delivery, error)
	Deliveries(context.Context, notify.DeliveryFilter, int, int) ([]notify.Delivery, int, error)
	ListNotificationEvents(context.Context) ([]notify.EventView, error)
	GetNotificationEvent(context.Context, string) (notify.EventView, error)
	UpdateNotificationEvent(context.Context, string, notify.EventPatch) (notify.EventView, error)
	SetNotificationNodeEnabled(context.Context, string, string, bool) error
	NotificationNodeEnabled(context.Context, string, string) (bool, error)
	GetNotificationTemplate(context.Context, string, string) (notify.TemplateView, error)
	PutNotificationTemplate(context.Context, string, string, string) (notify.TemplateView, error)
	ResetNotificationTemplate(context.Context, string, string) (notify.TemplateView, error)
	PreviewNotificationTemplate(context.Context, string, string, string) (notify.TemplatePreview, error)
	TestNotificationEvent(context.Context, string, string) (notify.Delivery, error)
}

type ownerNotifications struct {
	store   OwnerNotificationsStore
	limiter *nodeRateLimiter
	auth    *serverAuth
}

type notificationEventAuditStore interface {
	UpdateNotificationEventAudited(context.Context, string, notify.EventPatch, audit.Entry) (notify.EventView, error)
	SetNotificationNodeEnabledAudited(context.Context, string, string, bool, audit.Entry) error
	PutNotificationTemplateAudited(context.Context, string, string, string, audit.Entry) (notify.TemplateView, error)
	ResetNotificationTemplateAudited(context.Context, string, string, audit.Entry) (notify.TemplateView, error)
}

func newOwnerNotificationsHandler(store OwnerNotificationsStore, auth *serverAuth, perMinute int) http.Handler {
	if perMinute <= 0 {
		perMinute = defaultAgentRequestsPerMinute
	}
	h := &ownerNotifications{store: store, limiter: newNodeRateLimiter(perMinute, defaultAgentBurst*2), auth: auth}
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

func (h *ownerNotifications) auditEntry(r *http.Request) audit.Entry {
	entry, _ := audit.Classify(r.Method, r.URL.Path)
	entry.Actor = h.auth.actor(r)
	entry.Remote = remoteIP(r.RemoteAddr, r.Header.Get("X-Forwarded-For"), h.auth.trustedProxyCIDRs)
	return entry
}

func (h *ownerNotifications) updateEvent(r *http.Request, key string, patch notify.EventPatch) (notify.EventView, error) {
	if store, ok := h.store.(notificationEventAuditStore); ok {
		view, err := store.UpdateNotificationEventAudited(r.Context(), key, patch, h.auditEntry(r))
		if err == nil {
			markAuditRecorded(r.Context())
		}
		return view, err
	}
	return h.store.UpdateNotificationEvent(r.Context(), key, patch)
}

func (h *ownerNotifications) setNodeEvent(r *http.Request, key, node string, enabled bool) error {
	if store, ok := h.store.(notificationEventAuditStore); ok {
		err := store.SetNotificationNodeEnabledAudited(r.Context(), key, node, enabled, h.auditEntry(r))
		if err == nil {
			markAuditRecorded(r.Context())
		}
		return err
	}
	return h.store.SetNotificationNodeEnabled(r.Context(), key, node, enabled)
}

func (h *ownerNotifications) putTemplate(r *http.Request, key, channel, body string) (notify.TemplateView, error) {
	if store, ok := h.store.(notificationEventAuditStore); ok {
		view, err := store.PutNotificationTemplateAudited(r.Context(), key, channel, body, h.auditEntry(r))
		if err == nil {
			markAuditRecorded(r.Context())
		}
		return view, err
	}
	return h.store.PutNotificationTemplate(r.Context(), key, channel, body)
}

func (h *ownerNotifications) storeResetTemplate(r *http.Request, key, channel string) (notify.TemplateView, error) {
	if store, ok := h.store.(notificationEventAuditStore); ok {
		view, err := store.ResetNotificationTemplateAudited(r.Context(), key, channel, h.auditEntry(r))
		if err == nil {
			markAuditRecorded(r.Context())
		}
		return view, err
	}
	return h.store.ResetNotificationTemplate(r.Context(), key, channel)
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
		writeRouteNotFound(w, r)
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

// listDeliveries pages the delivery attempts, optionally for one alert event or one status/channel pair.
func (h *ownerNotifications) listDeliveries(w http.ResponseWriter, r *http.Request) {
	query, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		invalid(w, r, "query parameters must be valid URL encoding.")
		return
	}
	page, size, ok := paging(w, r)
	if !ok {
		return
	}
	if page > 1000000 {
		invalid(w, r, "page must be at most 1000000.")
		return
	}
	filter, ok := deliveryFilter(w, r, query)
	if !ok {
		return
	}
	items, total, err := h.store.Deliveries(r.Context(), filter, page, size)
	if err != nil {
		notificationError(w, r, err)
		return
	}
	writePage(w, items, page, size, total)
}

// deliveryFilter reads the optional event, status and channel filters, rejecting unknown enum values.
func deliveryFilter(w http.ResponseWriter, r *http.Request, query url.Values) (notify.DeliveryFilter, bool) {
	var filter notify.DeliveryFilter
	var present bool
	var ok bool
	if filter.Event, present, ok = notificationFilterValue(w, r, query, "event", "event must be an alert event ID."); !ok {
		return filter, false
	}
	if present && !uuidPattern.MatchString(filter.Event) {
		invalid(w, r, "event must be an alert event ID.")
		return filter, false
	}
	if filter.Status, present, ok = notificationFilterValue(w, r, query, "status", "status must be pending, sent or failed."); !ok {
		return filter, false
	}
	if present && !notify.ValidDeliveryStatus(filter.Status) {
		invalid(w, r, "status must be pending, sent or failed.")
		return filter, false
	}
	if filter.Channel, present, ok = notificationFilterValue(w, r, query, "channel", "channel must be email, telegram, slack or webhook."); !ok {
		return filter, false
	}
	if present {
		if _, known := catalog.LookupChannel(filter.Channel); !known {
			invalid(w, r, "channel must be email, telegram, slack or webhook.")
			return filter, false
		}
	}
	return filter, true
}

// notificationFilterValue returns one optional query value and rejects a repeated parameter.
func notificationFilterValue(w http.ResponseWriter, r *http.Request, query url.Values, name, detail string) (string, bool, bool) {
	values, present := query[name]
	if !present {
		return "", false, true
	}
	if len(values) != 1 {
		invalid(w, r, detail)
		return "", true, false
	}
	return values[0], true, true
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
