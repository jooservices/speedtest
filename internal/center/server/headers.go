package server

import "net/http"

// contentSecurityPolicy is strict: the UI ships no inline script and loads nothing from third parties except optional map tiles. Only style *attributes* (charts
// set widths and colours that way) are allowed inline; script and style elements are not.
const contentSecurityPolicy = "default-src 'self'; script-src 'self'; style-src 'self'; style-src-attr 'unsafe-inline'; " +
	"img-src 'self' data: https://*.tile.openstreetmap.org; connect-src 'self'; " +
	"object-src 'none'; base-uri 'none'; form-action 'self'; frame-ancestors 'none'"

// withSecurityHeaders adds defensive response headers to every response.
// HSTS is sent only when the request arrived over TLS or through a proxy that
// says so, so plain-HTTP LAN deployments are not pinned to HTTPS.
func withSecurityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", contentSecurityPolicy)
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Permissions-Policy", "camera=(), microphone=(), geolocation=(), payment=()")
		h.Set("Cross-Origin-Opener-Policy", "same-origin")
		if r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https" {
			h.Set("Strict-Transport-Security", "max-age=31536000")
		}
		next.ServeHTTP(w, r)
	})
}
