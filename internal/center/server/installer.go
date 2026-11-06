package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/jooservices/speedtest/internal/center/installer"
	"github.com/jooservices/speedtest/internal/center/nodes"
)

const maxInstallerRequestBody = 48 << 10

// InstallerService is the owner-authenticated SSH installation API.
type InstallerService interface {
	Probe(context.Context, string, string, installer.ProbeInput) (installer.SSHProbe, error)
	Start(context.Context, string, string, string, installer.InstallInput) (installer.Installation, error)
	StartUninstall(context.Context, string, string, string, installer.InstallInput) (installer.Installation, error)
	Get(context.Context, string) (installer.Installation, error)
	Events(context.Context, string, string) ([]installer.Event, installer.Installation, error)
}

type installerAPI struct {
	service InstallerService
	auth    *serverAuth
	limiter *nodeRateLimiter
}

func newInstallerHandler(service InstallerService, auth *serverAuth, perMinute int) http.Handler {
	h := &installerAPI{service: service, auth: auth, limiter: newNodeRateLimiter(perMinute, defaultAgentBurst)}
	read := auth.requireSession(http.HandlerFunc(h.serve))
	write := auth.requireOwner(http.HandlerFunc(h.serve))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			write.ServeHTTP(w, r)
			return
		}
		read.ServeHTTP(w, r)
	})
}

func installerRoute(path string) bool {
	if path == "/api/installer/probes" {
		return true
	}
	parts := strings.Split(strings.Trim(path, "/"), "/")
	if len(parts) == 4 && parts[0] == "api" && parts[1] == "nodes" && (parts[3] == "installations" || parts[3] == "uninstallations") {
		return true
	}
	return len(parts) == 3 && parts[0] == "api" && parts[1] == "installations" ||
		len(parts) == 4 && parts[0] == "api" && parts[1] == "installations" && parts[3] == "events"
}

func (h *installerAPI) serve(w http.ResponseWriter, r *http.Request) {
	principal, err := ownerPrincipal(r)
	if err != nil {
		writeServiceError(w, r, err)
		return
	}
	if !allowRate(h.limiter, "installer:"+principal, w, r) {
		return
	}

	if h.service == nil {
		writeAPIProblem(w, r, http.StatusServiceUnavailable, "Service unavailable", "The installer is not configured.")
		return
	}
	switch {
	case r.URL.Path == "/api/installer/probes":
		if r.Method != http.MethodPost {
			methodNotAllowed(w, r, http.MethodPost)
			return
		}
		h.probe(w, r)
	case strings.HasPrefix(r.URL.Path, "/api/nodes/") && strings.HasSuffix(r.URL.Path, "/installations"):
		if r.Method != http.MethodPost {
			methodNotAllowed(w, r, http.MethodPost)
			return
		}
		h.start(w, r, false)
	case strings.HasPrefix(r.URL.Path, "/api/nodes/") && strings.HasSuffix(r.URL.Path, "/uninstallations"):
		if r.Method != http.MethodPost {
			methodNotAllowed(w, r, http.MethodPost)
			return
		}
		h.start(w, r, true)
	case strings.HasPrefix(r.URL.Path, "/api/installations/") && strings.HasSuffix(r.URL.Path, "/events"):
		if r.Method != http.MethodGet {
			methodNotAllowed(w, r, http.MethodGet)
			return
		}
		h.stream(w, r)
	case strings.HasPrefix(r.URL.Path, "/api/installations/"):
		if r.Method != http.MethodGet {
			methodNotAllowed(w, r, http.MethodGet)
			return
		}
		h.get(w, r)
	default:
		writeNotFoundProblem(w, r)
	}
}

func (h *installerAPI) probe(w http.ResponseWriter, r *http.Request) {
	principal, err := ownerPrincipal(r)
	if err != nil {
		writeServiceError(w, r, err)
		return
	}
	key, ok := idempotencyKey(w, r)
	if !ok {
		return
	}
	var input installer.ProbeInput
	if !decodeInstallerJSON(w, r, &input) {
		return
	}
	result, err := h.service.Probe(r.Context(), principal, key, input)
	if err != nil {
		h.writeError(w, r, err, true)
		return
	}
	writeEnvelope(w, http.StatusCreated, "SSH host fingerprint observed", result)
}

func (h *installerAPI) start(w http.ResponseWriter, r *http.Request, uninstall bool) {
	principal, err := ownerPrincipal(r)
	if err != nil {
		writeServiceError(w, r, err)
		return
	}
	key, ok := idempotencyKey(w, r)
	if !ok {
		return
	}
	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	if len(parts) != 4 || parts[0] != "api" || parts[1] != "nodes" {
		writeNotFoundProblem(w, r)
		return
	}
	var input installer.InstallInput
	if !decodeInstallerJSON(w, r, &input) {
		return
	}
	begin, message := h.service.Start, "Agent installation accepted"
	if uninstall {
		begin, message = h.service.StartUninstall, "Agent removal accepted"
	}
	result, err := begin(r.Context(), principal, key, parts[2], input)
	if err != nil {
		h.writeError(w, r, err, false)
		return
	}
	w.Header().Set("Location", "/api/installations/"+result.ID)
	writeEnvelope(w, http.StatusAccepted, message, result)
}

func (h *installerAPI) get(w http.ResponseWriter, r *http.Request) {
	id, ok := installationID(r.URL.Path, false)
	if !ok {
		writeNotFoundProblem(w, r)
		return
	}
	result, err := h.service.Get(r.Context(), id)
	if err != nil {
		h.writeError(w, r, err, false)
		return
	}
	writeEnvelope(w, http.StatusOK, "Installation status", result)
}

func (h *installerAPI) stream(w http.ResponseWriter, r *http.Request) {
	id, ok := installationID(r.URL.Path, true)
	if !ok {
		writeNotFoundProblem(w, r)
		return
	}
	cursor, ok := lastEventID(w, r)
	if !ok {
		return
	}
	events, job, err := h.service.Events(r.Context(), id, cursor)
	if err != nil {
		h.writeError(w, r, err, false)
		return
	}
	if !supportsFlush(w) {
		writeAPIProblem(w, r, http.StatusInternalServerError, "Internal failure", "Server-sent events are unavailable.")
		return
	}
	controller := http.NewResponseController(w)
	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	if err := controller.Flush(); err != nil {
		return
	}
	last := cursor
	if !writeInstallEvents(w, controller.Flush, events) {
		return
	}
	if len(events) > 0 {
		last = events[len(events)-1].ID
	}
	if terminal(job.Status) {
		return
	}
	h.follow(w, r, controller, id, last)
}

// lastEventID reads the optional single Last-Event-ID header used to resume a stream.
func lastEventID(w http.ResponseWriter, r *http.Request) (string, bool) {
	headers := r.Header.Values("Last-Event-ID")
	if len(headers) > 1 {
		writeAPIProblem(w, r, http.StatusBadRequest, "Bad request", "Only one Last-Event-ID value is allowed.")
		return "", false
	}
	if len(headers) == 1 {
		return headers[0], true
	}
	return "", true
}

// follow streams new progress events until the job ends, the client leaves or the owner's session expires.
func (h *installerAPI) follow(w http.ResponseWriter, r *http.Request, controller *http.ResponseController, id, last string) {
	poll := time.NewTicker(250 * time.Millisecond)
	defer poll.Stop()
	sessionCheck := time.NewTicker(10 * time.Second)
	defer sessionCheck.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-sessionCheck.C:
			if !h.sessionValid(r) {
				return
			}
		case <-poll.C:
			events, job, err := h.service.Events(r.Context(), id, last)
			if err != nil {
				_ = writeInstallEvents(w, controller.Flush, []installer.Event{{ID: last, TS: time.Now().UTC(), Step: "failed", Message: "Progress is unavailable. Reconnect to check the installation status.", Pct: 0, Status: "failed"}})
				return
			}
			if !writeInstallEvents(w, controller.Flush, events) {
				return
			}
			if len(events) > 0 {
				last = events[len(events)-1].ID
			}
			if terminal(job.Status) {
				return
			}
		}
	}
}

func (h *installerAPI) sessionValid(r *http.Request) bool {
	token, present, invalid := readSessionCookie(r)
	if !present || invalid {
		return false
	}
	session, err := h.auth.service.Session(r.Context(), token)
	return err == nil && session.Identity != nil && session.Identity.ID != ""
}

func (h *installerAPI) writeError(w http.ResponseWriter, r *http.Request, err error, probing bool) {
	switch {
	case errors.Is(err, installer.ErrInvalid), errors.Is(err, nodes.ErrInvalidInput):
		writeAPIProblem(w, r, http.StatusUnprocessableEntity, "Validation failed", "The installer request is invalid.")
	case errors.Is(err, installer.ErrNotFound), errors.Is(err, nodes.ErrNotFound):
		writeNotFoundProblem(w, r)
	case errors.Is(err, installer.ErrConflict), errors.Is(err, installer.ErrBusy), errors.Is(err, installer.ErrHostKeyChange), errors.Is(err, installer.ErrCursorExpired):
		writeAPIProblem(w, r, http.StatusConflict, "Conflict", "The installation conflicts with the confirmed node state. Probe again before retrying.")
	case errors.Is(err, installer.ErrDisconnect) && probing:
		writeAPIProblem(w, r, http.StatusUnprocessableEntity, "SSH connection failed", "Could not reach the SSH host. Check the host and port, then retry.")
	default:
		writeAPIProblem(w, r, http.StatusInternalServerError, "Internal failure", "Internal failure; safe detail only.")
	}
}

func decodeInstallerJSON(w http.ResponseWriter, r *http.Request, target any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, maxInstallerRequestBody)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			writeAPIProblem(w, r, http.StatusRequestEntityTooLarge, "Request too large", "The request body exceeds the allowed size.")
		} else {
			writeAPIProblem(w, r, http.StatusBadRequest, "Bad request", "The JSON request body is invalid.")
		}
		return false
	}
	if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			writeAPIProblem(w, r, http.StatusRequestEntityTooLarge, "Request too large", "The request body exceeds the allowed size.")
		} else {
			writeAPIProblem(w, r, http.StatusBadRequest, "Bad request", "The request body must contain one JSON value.")
		}
		return false
	}
	return true
}

func installationID(path string, events bool) (string, bool) {
	parts := strings.Split(strings.Trim(path, "/"), "/")
	if events {
		if len(parts) != 4 || parts[0] != "api" || parts[1] != "installations" || parts[3] != "events" {
			return "", false
		}
	} else if len(parts) != 3 || parts[0] != "api" || parts[1] != "installations" {
		return "", false
	}
	return parts[2], true
}

func writeInstallEvents(w http.ResponseWriter, flush func() error, events []installer.Event) bool {
	for _, event := range events {
		raw, err := json.Marshal(event)
		if err != nil {
			return false
		}
		if _, err := fmt.Fprintf(w, "id: %s\nevent: progress\ndata: %s\n\n", event.ID, raw); err != nil {
			return false
		}
		if err := flush(); err != nil {
			return false
		}
	}
	return true
}

func supportsFlush(w http.ResponseWriter) bool {
	for {
		if _, ok := w.(http.Flusher); ok {
			return true
		}
		unwrapper, ok := w.(interface{ Unwrap() http.ResponseWriter })
		if !ok {
			return false
		}
		w = unwrapper.Unwrap()
	}
}

func terminal(status string) bool { return status == "succeeded" || status == "failed" }
