package server

import (
	"net/http"
	"testing"
)

func TestEngineFilterValidationAndPropagation(t *testing.T) {
	store := &fakeOwnerDashboard{}
	h := dashboardHandlerFor(store)
	for _, path := range []string{"/api/dashboard", "/api/series"} {
		for _, name := range []string{"ookla", "librespeed"} {
			rec := do(h, http.MethodGet, path+"?engine="+name, "", ownerRead)
			if rec.Code != http.StatusOK || store.query.Engine != name {
				t.Fatalf("%s %s: %d %+v", path, name, rec.Code, store.query)
			}
		}
		for _, query := range []string{"engine=", "engine=other", "engine=ookla&engine=librespeed"} {
			if rec := do(h, http.MethodGet, path+"?"+query, "", ownerRead); rec.Code != http.StatusUnprocessableEntity {
				t.Fatalf("invalid %s: %d", query, rec.Code)
			}
		}
	}
}
