package server

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/jooservices/speedtest/internal/center/usage"
)

// OwnerUsageStore reads measured traffic and manages optional monthly budgets.
type OwnerUsageStore interface {
	Usage(context.Context, usage.Query) (usage.Response, error)
	Budgets(context.Context) ([]usage.Budget, string, error)
	ReplaceBudget(context.Context, usage.Budget, string) ([]usage.Budget, string, error)
	DeleteBudget(context.Context, string, string) ([]usage.Budget, string, error)
	Advice(context.Context, string, string, int, int64) (usage.Advice, error)
}

type ownerUsage struct {
	store   OwnerUsageStore
	limiter *nodeRateLimiter
	now     func() time.Time
}

func newOwnerUsageHandler(store OwnerUsageStore, auth *serverAuth) http.Handler {
	h := &ownerUsage{store: store, limiter: newOwnerRateLimiter(), now: time.Now}
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

func ownerUsageRoute(path string) bool {
	return path == "/api/usage" || path == "/api/usage/advice" || path == "/api/usage/budgets" ||
		(strings.HasPrefix(path, "/api/usage/budgets/") && strings.Count(strings.Trim(path, "/"), "/") == 3)
}

func (h *ownerUsage) serve(w http.ResponseWriter, r *http.Request) {
	principal, err := ownerPrincipal(r)
	if err != nil {
		writeUnauthorized(w, r)
		return
	}
	if !allowRate(h.limiter, "owner:"+principal, w, r) {
		return
	}
	switch r.URL.Path {
	case "/api/usage":
		h.readUsage(w, r)
	case "/api/usage/advice":
		h.readAdvice(w, r)
	case "/api/usage/budgets":
		h.budgetCollection(w, r)
	default:
		h.budgetResource(w, r, strings.TrimPrefix(r.URL.Path, "/api/usage/budgets/"))
	}
}

func (h *ownerUsage) readUsage(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w, r, http.MethodGet)
		return
	}
	q, ok := usageQuery(w, r, h.now())
	if !ok {
		return
	}
	data, err := h.store.Usage(r.Context(), q)
	if err != nil {
		writeUsageError(w, r, err)
		return
	}
	writeEnvelope(w, http.StatusOK, "Success", data)
}

func usageQuery(w http.ResponseWriter, r *http.Request, now time.Time) (usage.Query, bool) {
	values := r.URL.Query()
	q := usage.Query{}
	var ok bool
	if q.NodeID, ok = optionalParam(w, r, values, "node"); !ok {
		return usage.Query{}, false
	}
	if q.Engine, ok = optionalParam(w, r, values, "engine"); !ok {
		return usage.Query{}, false
	}
	if q.Group, ok = optionalParam(w, r, values, "group"); !ok {
		return usage.Query{}, false
	}
	from, to := values.Get("from"), values.Get("to")
	if (from == "") != (to == "") || len(values["from"]) > 1 || len(values["to"]) > 1 {
		invalid(w, r, "from and to must be supplied once each.")
		return usage.Query{}, false
	}
	if from != "" {
		var err error
		q.From, err = time.Parse(time.RFC3339Nano, from)
		if err != nil {
			invalid(w, r, "from must be an RFC 3339 timestamp.")
			return usage.Query{}, false
		}
		q.To, err = time.Parse(time.RFC3339Nano, to)
		if err != nil {
			invalid(w, r, "to must be an RFC 3339 timestamp.")
			return usage.Query{}, false
		}
	}
	if normalized, err := q.Normalize(now); err != nil || q.NodeID != "" && !uuidPattern.MatchString(q.NodeID) {
		invalid(w, r, "usage filters are invalid or the date range exceeds 366 days.")
		return usage.Query{}, false
	} else {
		q = normalized
	}
	return q, true
}

func optionalParam(w http.ResponseWriter, r *http.Request, values map[string][]string, key string) (string, bool) {
	items := values[key]
	if len(items) > 1 {
		invalid(w, r, key+" may be supplied once.")
		return "", false
	}
	if len(items) == 0 {
		return "", true
	}
	return items[0], true
}

func (h *ownerUsage) readAdvice(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w, r, http.MethodGet)
		return
	}
	values := r.URL.Query()
	node, ok := optionalParam(w, r, values, "node")
	if !ok {
		return
	}
	engine, ok := optionalParam(w, r, values, "engine")
	if !ok {
		return
	}
	interval, err := strconv.Atoi(values.Get("interval"))
	if err != nil {
		invalid(w, r, "interval must be an integer from 5 to 1440 minutes.")
		return
	}
	budget, err := strconv.ParseInt(values.Get("budgetBytes"), 10, 64)
	if values.Get("budgetBytes") == "" {
		budget, err = 0, nil
	}
	if err != nil || budget < 0 {
		invalid(w, r, "budgetBytes must be a non-negative integer.")
		return
	}
	data, err := h.store.Advice(r.Context(), node, engine, interval, budget)
	if err != nil {
		writeUsageError(w, r, err)
		return
	}
	writeEnvelope(w, http.StatusOK, "Success", data)
}

func (h *ownerUsage) budgetCollection(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w, r, http.MethodGet)
		return
	}
	items, etag, err := h.store.Budgets(r.Context())
	if err != nil {
		writeUsageError(w, r, err)
		return
	}
	w.Header().Set("ETag", etag)
	writeEnvelope(w, http.StatusOK, "Success", items)
}

func (h *ownerUsage) budgetResource(w http.ResponseWriter, r *http.Request, scope string) {
	if scope != "global" && !uuidPattern.MatchString(scope) {
		writeNotFoundProblem(w, r)
		return
	}
	if r.Method != http.MethodPut && r.Method != http.MethodDelete {
		methodNotAllowed(w, r, http.MethodPut, http.MethodDelete)
		return
	}
	ifMatch, ok := requireIfMatch(w, r)
	if !ok {
		return
	}
	if r.Method == http.MethodDelete {
		_, etag, err := h.store.DeleteBudget(r.Context(), scope, ifMatch)
		if err != nil {
			writeUsageError(w, r, err)
			return
		}
		w.Header().Set("ETag", etag)
		w.WriteHeader(http.StatusNoContent)
		return
	}
	h.replaceBudget(w, r, scope, ifMatch)
}

func (h *ownerUsage) replaceBudget(w http.ResponseWriter, r *http.Request, scope, ifMatch string) {
	var input struct {
		MonthlyBytes    int64  `json:"monthlyBytes"`
		Action          string `json:"action"`
		WidenMultiplier int    `json:"widenMultiplier"`
	}
	if !decodeJSON(w, r, &input) {
		return
	}
	b := usage.Budget{MonthlyBytes: input.MonthlyBytes, Action: input.Action, WidenMultiplier: input.WidenMultiplier}
	if scope != "global" {
		b.NodeID = strings.ToLower(scope)
	}
	items, etag, err := h.store.ReplaceBudget(r.Context(), b, ifMatch)
	if err != nil {
		writeUsageError(w, r, err)
		return
	}
	w.Header().Set("ETag", etag)
	writeEnvelope(w, http.StatusOK, "Budget updated", items)
}

func writeUsageError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, usage.ErrInvalid):
		invalid(w, r, "The usage or budget request is invalid.")
	case errors.Is(err, usage.ErrNotFound):
		writeNotFoundProblem(w, r)
	case errors.Is(err, usage.ErrPrecondition):
		writePreconditionRequiredProblem(w, r)
	case errors.Is(err, usage.ErrStale):
		writeStaleProblem(w, r)
	default:
		writeAPIProblem(w, r, http.StatusInternalServerError, "Internal failure", "Internal failure; safe detail only.")
	}
}
