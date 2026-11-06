package server

import "net/http"

// ownerReader wraps a read-only owner API: it requires a signed-in session, gives every owner one rate-limit bucket
// and refuses anything but GET, in that order. The handler it wraps only deals with its own routes. Every read-only
// owner area (dashboard, statistics, results, IP, jobs, audit) is served through it.
func ownerReader(auth *serverAuth, limiter *nodeRateLimiter, next http.HandlerFunc) http.Handler {
	return auth.requireSession(ownerGuards(limiter, next))
}

// ownerGuards is the part of ownerReader that runs after the session check: an owner identity must be present in
// the context, the request is rate limited per owner, and only GET gets through.
func ownerGuards(limiter *nodeRateLimiter, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		principal, err := ownerPrincipal(r)
		if err != nil {
			writeUnauthorized(w, r)
			return
		}
		if !allowRate(limiter, "owner:"+principal, w, r) {
			return
		}
		if r.Method != http.MethodGet {
			methodNotAllowed(w, r, http.MethodGet)
			return
		}
		next(w, r)
	}
}
