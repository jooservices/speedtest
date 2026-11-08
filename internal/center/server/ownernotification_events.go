package server

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/jooservices/speedtest/internal/center/catalog"
	"github.com/jooservices/speedtest/internal/center/notify"
	"github.com/jooservices/speedtest/internal/center/notify/template"
)

const maxNotificationEventBody = 1 << 16

type notificationEventPatch struct {
	Enabled  *bool    `json:"enabled"`
	Severity *string  `json:"severity"`
	Channels []string `json:"channels"`
}

type notificationTemplateBody struct {
	Body string `json:"body"`
}

type notificationPreviewBody struct {
	Body string `json:"body"`
}

func (h *ownerNotifications) serveCatalog(w http.ResponseWriter, r *http.Request, rest string) {
	switch {
	case rest == "events" && r.Method == http.MethodGet:
		h.listEvents(w, r)
	case strings.HasPrefix(rest, "events/"):
		h.serveEvent(w, r, strings.TrimPrefix(rest, "events/"))
	case strings.HasPrefix(rest, "templates/"):
		h.serveTemplate(w, r, strings.TrimPrefix(rest, "templates/"))
	default:
		writeNotFoundProblem(w, r)
	}
}

func (h *ownerNotifications) listEvents(w http.ResponseWriter, r *http.Request) {
	items, err := h.store.ListNotificationEvents(r.Context())
	if err != nil {
		notificationError(w, r, err)
		return
	}
	writeEnvelope(w, http.StatusOK, "Success", items)
}

func (h *ownerNotifications) serveEvent(w http.ResponseWriter, r *http.Request, key string) {
	key = strings.Trim(key, "/")
	if !catalog.ValidEventKey(key) {
		writeNotFoundProblem(w, r)
		return
	}
	switch r.Method {
	case http.MethodGet:
		view, err := h.store.GetNotificationEvent(r.Context(), key)
		if err != nil {
			notificationError(w, r, err)
			return
		}
		writeEnvelope(w, http.StatusOK, "Success", view)
	case http.MethodPatch:
		var body notificationEventPatch
		if !decodeNotificationJSON(w, r, &body) {
			return
		}
		view, err := h.store.UpdateNotificationEvent(r.Context(), key, notify.EventPatch{
			Enabled: body.Enabled, Severity: body.Severity, Channels: body.Channels,
		})
		if err != nil {
			notificationError(w, r, err)
			return
		}
		writeEnvelope(w, http.StatusOK, "Notification event updated", view)
	default:
		methodNotAllowed(w, r, http.MethodGet+", "+http.MethodPatch)
	}
}

func (h *ownerNotifications) serveTemplate(w http.ResponseWriter, r *http.Request, rest string) {
	parts := strings.Split(strings.Trim(rest, "/"), "/")
	if len(parts) < 2 || !catalog.ValidEventKey(parts[0]) {
		writeNotFoundProblem(w, r)
		return
	}
	eventKey, channel := parts[0], parts[1]
	if _, ok := catalog.LookupChannel(channel); !ok {
		writeNotFoundProblem(w, r)
		return
	}
	if len(parts) == 3 && parts[2] == "previews" {
		h.previewTemplate(w, r, eventKey, channel)
		return
	}
	if len(parts) == 3 && parts[2] == "resets" {
		h.resetTemplate(w, r, eventKey, channel)
		return
	}
	if len(parts) != 2 {
		writeNotFoundProblem(w, r)
		return
	}
	switch r.Method {
	case http.MethodGet:
		view, err := h.store.GetNotificationTemplate(r.Context(), eventKey, channel)
		if err != nil {
			notificationError(w, r, err)
			return
		}
		writeEnvelope(w, http.StatusOK, "Success", view)
	case http.MethodPut:
		var body notificationTemplateBody
		if !decodeNotificationJSON(w, r, &body) {
			return
		}
		view, err := h.store.PutNotificationTemplate(r.Context(), eventKey, channel, body.Body)
		if err != nil {
			notificationError(w, r, err)
			return
		}
		writeEnvelope(w, http.StatusOK, "Notification template updated", view)
	default:
		methodNotAllowed(w, r, http.MethodGet+", "+http.MethodPut)
	}
}

func (h *ownerNotifications) previewTemplate(w http.ResponseWriter, r *http.Request, eventKey, channel string) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w, r, http.MethodPost)
		return
	}
	var body notificationPreviewBody
	if r.ContentLength != 0 && !decodeNotificationJSON(w, r, &body) {
		return
	}
	view, err := h.store.PreviewNotificationTemplate(r.Context(), eventKey, channel, body.Body)
	if err != nil {
		notificationError(w, r, err)
		return
	}
	writeEnvelope(w, http.StatusOK, "Success", view)
}

func (h *ownerNotifications) resetTemplate(w http.ResponseWriter, r *http.Request, eventKey, channel string) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w, r, http.MethodPost)
		return
	}
	view, err := h.store.ResetNotificationTemplate(r.Context(), eventKey, channel)
	if err != nil {
		notificationError(w, r, err)
		return
	}
	writeEnvelope(w, http.StatusOK, "Notification template reset", view)
}

func decodeNotificationJSON(w http.ResponseWriter, r *http.Request, dest any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, maxNotificationEventBody)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(dest); err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			writeAPIProblem(w, r, http.StatusRequestEntityTooLarge, "Request too large", "The request body exceeds the allowed size.")
		} else {
			invalid(w, r, "Request body must be valid JSON.")
		}
		return false
	}
	if err := dec.Decode(new(any)); !errors.Is(err, io.EOF) {
		invalid(w, r, "Request body must contain one JSON value.")
		return false
	}
	return true
}

func notificationCatalogError(w http.ResponseWriter, r *http.Request, err error) bool {
	switch {
	case errors.Is(err, notify.ErrEventNotFound), errors.Is(err, notify.ErrTemplateNotFound):
		writeNotFoundProblem(w, r)
		return true
	case errors.Is(err, notify.ErrInvalidEvent), errors.Is(err, template.ErrInvalidTemplate), errors.Is(err, template.ErrUnknownPlaceholder):
		invalid(w, r, err.Error())
		return true
	}
	return false
}
