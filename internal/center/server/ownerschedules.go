package server

import (
	"context"
	"net/http"
	"strings"

	"github.com/jooservices/speedtest/internal/center/ownerapi"
	"github.com/jooservices/speedtest/internal/center/schedules"
)

// OwnerSchedulesStore is the dashboard-side schedule API. Reads need a session; writes also CSRF, origin and If-Match.
type OwnerSchedulesStore interface {
	Schedules(ctx context.Context) (map[string]any, string, error)
	Schedule(ctx context.Context, id string) (map[string]any, string, error)
	ReplaceSchedule(ctx context.Context, id string, in ownerapi.ScheduleInput, ifMatch string) (map[string]any, string, error)
	ReplaceRules(ctx context.Context, in ownerapi.ScheduleRules, ifMatch string) (map[string]any, string, error)
	Stagger(ctx context.Context, ifMatch string) (map[string]any, string, error)
}

type ownerSchedules struct {
	store   OwnerSchedulesStore
	limiter *nodeRateLimiter
}

func newOwnerSchedulesHandler(store OwnerSchedulesStore, auth *serverAuth) http.Handler {
	h := &ownerSchedules{store: store, limiter: newOwnerRateLimiter()}
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

// ownerSchedulesRoute reports whether the path belongs to the schedule API.
func ownerSchedulesRoute(path string) bool {
	return path == "/api/schedules" || (strings.HasPrefix(path, "/api/schedules/") && strings.Count(strings.Trim(path, "/"), "/") == 2)
}

func (h *ownerSchedules) serve(w http.ResponseWriter, r *http.Request) {
	principal, err := ownerPrincipal(r)
	if err != nil {
		writeUnauthorized(w, r)
		return
	}
	if !allowRate(h.limiter, "owner:"+principal, w, r) {
		return
	}
	switch rest := strings.TrimPrefix(r.URL.Path, "/api/schedules"); rest {
	case "":
		if r.Method != http.MethodGet {
			methodNotAllowed(w, r, http.MethodGet)
			return
		}
		h.respond(w, r, http.StatusOK)(h.store.Schedules(r.Context()))
	case "/rules":
		if r.Method != http.MethodPut {
			methodNotAllowed(w, r, http.MethodPut)
			return
		}
		h.rules(w, r)
	case "/stagger":
		if r.Method != http.MethodPost {
			methodNotAllowed(w, r, http.MethodPost)
			return
		}
		ifMatch, ok := requireIfMatch(w, r)
		if !ok {
			return
		}
		h.respond(w, r, http.StatusOK)(h.store.Stagger(r.Context(), ifMatch))
	default:
		id := strings.TrimPrefix(rest, "/")
		if !uuidPattern.MatchString(id) {
			writeNotFoundProblem(w, r)
			return
		}
		switch r.Method {
		case http.MethodGet:
			h.respond(w, r, http.StatusOK)(h.store.Schedule(r.Context(), id))
		case http.MethodPut:
			h.replace(w, r, id)
		default:
			methodNotAllowed(w, r, http.MethodGet, http.MethodPut)
		}
	}
}

// respond returns a function that writes a store result as an envelope with its ETag, or the mapped error.
func (h *ownerSchedules) respond(w http.ResponseWriter, r *http.Request, status int) func(map[string]any, string, error) {
	return func(data map[string]any, etag string, err error) {
		if err != nil {
			writeOwnerError(w, r, err)
			return
		}
		w.Header().Set("ETag", etag)
		writeEnvelope(w, status, "Success", data)
	}
}

func (h *ownerSchedules) replace(w http.ResponseWriter, r *http.Request, id string) {
	ifMatch, ok := requireIfMatch(w, r)
	if !ok {
		return
	}
	var in struct {
		Interval *int  `json:"interval"`
		Offset   *int  `json:"offset"`
		Enabled  *bool `json:"enabled"`
	}
	if !decodeJSON(w, r, &in) {
		return
	}
	if in.Interval == nil || in.Offset == nil || in.Enabled == nil {
		invalid(w, r, "interval, offset and enabled are required.")
		return
	}
	if *in.Interval < schedules.MinInterval || *in.Interval > schedules.MaxInterval {
		invalid(w, r, "interval must be between 5 and 1440 minutes.")
		return
	}
	if *in.Offset < 0 || *in.Offset > schedules.MaxInterval-1 {
		invalid(w, r, "offset must be between 0 and 1439 minutes.")
		return
	}
	h.respond(w, r, http.StatusOK)(h.store.ReplaceSchedule(r.Context(), id, ownerapi.ScheduleInput{Interval: *in.Interval, Offset: *in.Offset, Enabled: *in.Enabled}, ifMatch))
}

func (h *ownerSchedules) rules(w http.ResponseWriter, r *http.Request) {
	ifMatch, ok := requireIfMatch(w, r)
	if !ok {
		return
	}
	var in struct {
		Stagger       *bool `json:"stagger"`
		MaxConcurrent *int  `json:"maxConcurrent"`
		Quiet         *struct {
			Start    *string `json:"quietStart"`
			End      *string `json:"quietEnd"`
			Timezone *string `json:"timezone"`
		} `json:"quiet"`
	}
	if !decodeJSON(w, r, &in) {
		return
	}
	if in.Stagger == nil || in.MaxConcurrent == nil || in.Quiet == nil || in.Quiet.Start == nil || in.Quiet.End == nil || in.Quiet.Timezone == nil {
		invalid(w, r, "stagger, maxConcurrent and quiet (quietStart, quietEnd, timezone) are required.")
		return
	}
	rules := ownerapi.ScheduleRules{Stagger: *in.Stagger, MaxConcurrent: *in.MaxConcurrent,
		Quiet: schedules.Quiet{Start: *in.Quiet.Start, End: *in.Quiet.End, Timezone: *in.Quiet.Timezone}}
	if rules.MaxConcurrent < 1 || rules.MaxConcurrent > 5 {
		invalid(w, r, "maxConcurrent must be between 1 and 5.")
		return
	}
	if err := rules.Quiet.Validate(); err != nil {
		invalid(w, r, err.Error())
		return
	}
	h.respond(w, r, http.StatusOK)(h.store.ReplaceRules(r.Context(), rules, ifMatch))
}
