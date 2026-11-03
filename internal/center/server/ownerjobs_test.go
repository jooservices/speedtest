package server

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/jooservices/speedtest/internal/center/jobs"
)

type fakeOwnerJobs struct{ err error }

func (f fakeOwnerJobs) Get(_ context.Context, id string) (map[string]any, error) {
	return map[string]any{"id": id, "status": "succeeded", "error": nil}, f.err
}

func jobsHandlerFor(store OwnerJobsStore) http.Handler {
	return newNodeHandler(Options{OwnerJobs: store, NodeCredentials: &fakeNodeCredentialService{}})
}

func TestOwnerJobsRead(t *testing.T) {
	h := jobsHandlerFor(fakeOwnerJobs{})
	path := "/api/jobs/" + commandID
	if rec := do(h, http.MethodGet, path, "", nil); rec.Code != http.StatusUnauthorized {
		t.Fatalf("without a session = %d", rec.Code)
	}
	if rec := do(h, http.MethodGet, path, "", ownerRead); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"status":"succeeded"`) {
		t.Fatalf("job = %d %s", rec.Code, rec.Body.String())
	}
	if rec := do(h, http.MethodGet, "/api/jobs/not-a-uuid", "", ownerRead); rec.Code != http.StatusNotFound {
		t.Fatalf("bad id = %d", rec.Code)
	}
	if rec := do(h, http.MethodDelete, path, "", ownerWriteWith(nil)); rec.Code != http.StatusMethodNotAllowed && rec.Code != http.StatusUnauthorized && rec.Code != http.StatusForbidden {
		t.Fatalf("DELETE = %d", rec.Code)
	}
	if rec := do(jobsHandlerFor(fakeOwnerJobs{err: jobs.ErrNotFound}), http.MethodGet, path, "", ownerRead); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown job = %d", rec.Code)
	}
	broken := do(jobsHandlerFor(fakeOwnerJobs{err: errors.New("db down")}), http.MethodGet, path, "", ownerRead)
	if broken.Code != http.StatusInternalServerError || strings.Contains(broken.Body.String(), "db down") {
		t.Fatalf("store failure = %d %s", broken.Code, broken.Body.String())
	}
	for path, want := range map[string]bool{"/api/jobs/" + commandID: true, "/api/jobs": false, "/api/jobs/a/b": false} {
		if ownerJobsRoute(path) != want {
			t.Errorf("route %s = %v", path, !want)
		}
	}
}
