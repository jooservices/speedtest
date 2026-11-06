package server

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/jooservices/speedtest/internal/center/jobs"
)

// OwnerJobsStore reads the status of owner-started maintenance jobs.
type OwnerJobsStore interface {
	Get(ctx context.Context, id string) (map[string]any, error)
}

func ownerJobsRoute(path string) bool {
	return strings.HasPrefix(path, "/api/jobs/") && strings.Count(strings.Trim(path, "/"), "/") == 2
}

func newOwnerJobsHandler(store OwnerJobsStore, auth *serverAuth) http.Handler {
	return ownerReader(auth, newOwnerRateLimiter(), func(w http.ResponseWriter, r *http.Request) {
		id := strings.TrimPrefix(r.URL.Path, "/api/jobs/")
		if !uuidPattern.MatchString(id) {
			writeNotFoundProblem(w, r)
			return
		}
		job, err := store.Get(r.Context(), strings.ToLower(id))
		switch {
		case errors.Is(err, jobs.ErrNotFound):
			writeNotFoundProblem(w, r)
		case err != nil:
			writeAPIProblem(w, r, http.StatusInternalServerError, "Internal failure", "Internal failure; safe detail only.")
		default:
			writeEnvelope(w, http.StatusOK, "Success", job)
		}
	})
}
