package server

import (
	"net/http"
	"strings"

	"github.com/jooservices/speedtest/internal/center/apicontract"
	"github.com/jooservices/speedtest/internal/center/apitoken"
)

func withAPITokenAuth(next http.Handler, routes routeTable, auth *serverAuth) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		required, tokenRoute := routes.tokenPermission(r)
		if !tokenRoute || len(r.Header.Values("Authorization")) == 0 {
			next.ServeHTTP(w, r)
			return
		}
		raw, ok := readOwnerBearer(r)
		if !ok || auth.tokens == nil {
			writeBearerUnauthorized(w, r)
			return
		}
		if auth.requireHTTPS && !auth.isHTTPS(r) {
			writeProblemResponse(w, http.StatusForbidden, problemResponse{Type: "/problems/forbidden", Title: "HTTPS required",
				Status: http.StatusForbidden, Detail: "HTTPS is required outside local and test environments.", TraceID: requestIDFromContext(r.Context())})
			return
		}
		principal, err := auth.tokens.Authenticate(r.Context(), raw)
		if err != nil {
			if err == apitoken.ErrInvalidToken {
				writeBearerUnauthorized(w, r)
			} else {
				writeAuthInternalError(w, r)
			}
			return
		}
		if !allowRate(auth.tokenLimiter, "token:"+principal.TokenID, w, r) {
			return
		}
		if required != "" && !apicontract.Allows(apicontract.Scope(principal.Scope), apicontract.Scope(required)) {
			writeForbidden(w, r)
			return
		}
		ctx := apitoken.WithPrincipal(r.Context(), principal)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func readOwnerBearer(r *http.Request) (string, bool) {
	values := r.Header.Values("Authorization")
	if len(values) != 1 {
		return "", false
	}
	parts := strings.Fields(values[0])
	if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") || len(parts[1]) == 0 || len(parts[1]) > 8192 {
		return "", false
	}
	return parts[1], true
}

func writeBearerUnauthorized(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("WWW-Authenticate", `Bearer realm="speedtest"`)
	writeProblemResponse(w, http.StatusUnauthorized, problemResponse{
		Type: "/problems/unauthorized", Title: "Missing, expired, revoked or invalid credential",
		Status: http.StatusUnauthorized, Detail: "Missing, expired, revoked or invalid credential.",
		TraceID: requestIDFromContext(r.Context()),
	})
}
