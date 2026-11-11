package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jooservices/speedtest/internal/center/apitoken"
)

const maxAccessTokenBody = 8 * 1024

// AccessTokenStore persists and authenticates owner API tokens.
type AccessTokenStore interface {
	Authenticate(context.Context, string) (apitoken.Principal, error)
	Create(context.Context, string, apitoken.CreateInput) (apitoken.Token, string, error)
	List(context.Context, string, int, int) ([]apitoken.Token, int, error)
	Revoke(context.Context, string, string) error
}

type accessTokens struct {
	store AccessTokenStore
}

type issuedAccessToken struct {
	apitoken.Token
	Bearer string `json:"token"`
}

func newAccessTokenHandler(store AccessTokenStore, auth *serverAuth) http.Handler {
	return newAccessTokenHandlerWithLimiter(store, auth, newOwnerRateLimiter())
}

func newAccessTokenHandlerWithLimiter(store AccessTokenStore, auth *serverAuth, limiter *nodeRateLimiter) http.Handler {
	h := &accessTokens{store: store}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if len(r.Header.Values("Authorization")) != 0 {
			writeBearerUnauthorized(w, r)
			return
		}
		if r.URL.Path == "/api/access-tokens" {
			switch r.Method {
			case http.MethodGet:
				ownerReader(auth, limiter, h.list).ServeHTTP(w, r)
			case http.MethodPost:
				auth.requireOwner(http.HandlerFunc(h.create)).ServeHTTP(w, r)
			default:
				methodNotAllowed(w, r, "GET, POST")
			}
			return
		}
		const prefix = "/api/access-tokens/"
		id := strings.TrimPrefix(r.URL.Path, prefix)
		if !strings.HasPrefix(r.URL.Path, prefix) || id == "" || strings.Contains(id, "/") {
			writeNotFoundProblem(w, r)
			return
		}
		if r.Method != http.MethodDelete {
			methodNotAllowed(w, r, http.MethodDelete)
			return
		}
		auth.requireOwner(http.HandlerFunc(h.revoke)).ServeHTTP(w, r)
	})
}

func (h *accessTokens) list(w http.ResponseWriter, r *http.Request) {
	page, size, ok := paging(w, r)
	if !ok {
		return
	}
	principal, err := ownerPrincipal(r)
	if err != nil {
		writeUnauthorized(w, r)
		return
	}
	items, total, err := h.store.List(r.Context(), principal, page, size)
	if err != nil {
		writeAPIProblem(w, r, http.StatusInternalServerError, "Internal failure", "The token list could not be loaded.")
		return
	}
	writePage(w, items, page, size, total)
}

func (h *accessTokens) create(w http.ResponseWriter, r *http.Request) {
	principal, err := ownerPrincipal(r)
	if err != nil {
		writeUnauthorized(w, r)
		return
	}
	var input apitoken.CreateInput
	if !decodeAccessTokenBody(w, r, &input) {
		return
	}
	if !apitoken.ValidInput(input, time.Now()) {
		writeAPIProblem(w, r, http.StatusUnprocessableEntity, "Validation failed", "Provide a name, a supported scope, and an expiry in the future.")
		return
	}
	item, raw, err := h.store.Create(r.Context(), principal, input)
	if err != nil {
		writeAPIProblem(w, r, http.StatusInternalServerError, "Internal failure", "The API token could not be created.")
		return
	}
	setAuditTargetID(r.Context(), item.ID)
	w.Header().Set("Location", "/api/access-tokens/"+item.ID)
	writeSuccess(w, r, http.StatusCreated, "API token created. Copy it now; it will not be shown again.", issuedAccessToken{Token: item, Bearer: raw})
}

func (h *accessTokens) revoke(w http.ResponseWriter, r *http.Request) {
	principal, err := ownerPrincipal(r)
	if err != nil {
		writeUnauthorized(w, r)
		return
	}
	const prefix = "/api/access-tokens/"
	id := strings.TrimPrefix(r.URL.Path, prefix)
	if !strings.HasPrefix(r.URL.Path, prefix) || strings.Contains(id, "/") {
		writeNotFoundProblem(w, r)
		return
	}
	parsed, err := uuid.Parse(id)
	if err != nil {
		writeNotFoundProblem(w, r)
		return
	}
	err = h.store.Revoke(r.Context(), principal, parsed.String())
	if errors.Is(err, apitoken.ErrNotFound) {
		writeNotFoundProblem(w, r)
		return
	}
	if err != nil {
		writeAPIProblem(w, r, http.StatusInternalServerError, "Internal failure", "The API token could not be revoked.")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func decodeAccessTokenBody(w http.ResponseWriter, r *http.Request, target any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, maxAccessTokenBody)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			writeAPIProblem(w, r, http.StatusRequestEntityTooLarge, "Request too large", "The API token request exceeds the allowed size.")
			return false
		}
		writeAPIProblem(w, r, http.StatusBadRequest, "Invalid JSON", "Provide a valid API token request.")
		return false
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		writeAPIProblem(w, r, http.StatusBadRequest, "Invalid JSON", "Provide exactly one API token request.")
		return false
	}
	return true
}
