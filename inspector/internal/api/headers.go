package api

import (
	"crypto/subtle"
	"mime"
	"net"
	"net/http"
	"strings"
)

const csrfHeader = "X-Inspector-Csrf"

const csp = "default-src 'self'; img-src 'self' data:; style-src 'self'; script-src 'self'; connect-src 'self'; frame-ancestors 'none'"

func isRead(method string) bool {
	return method == http.MethodGet || method == http.MethodHead
}

func (s *Server) checkHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", csp)
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("X-Frame-Options", "DENY")

		if !strings.EqualFold(hostOnly(r.Host), s.host) {
			writeError(w, http.StatusMisdirectedRequest, "this is inspector."+s.cfg.Domain+"; requests for "+r.Host+" are refused")
			return
		}
		if openRead(r.URL.Path) {
			serveOpen(w, r, next)
			return
		}
		origin := r.Header.Get("Origin")
		if origin != "" && origin != s.origin {
			writeError(w, http.StatusForbidden, "cross-origin request refused")
			return
		}
		// Accepted risk (2026-10-21): links from the shop, the userscript and the DevTools panel open
		// the UI as a cross-site top-level navigation; the page's own API calls stay same-origin
		if uiNavigation(r) {
			next.ServeHTTP(w, r)
			return
		}
		if site := r.Header.Get("Sec-Fetch-Site"); site != "" && site != "same-origin" && site != "none" {
			writeError(w, http.StatusForbidden, "cross-site request refused (Sec-Fetch-Site: "+site+")")
			return
		}
		if isRead(r.Method) {
			next.ServeHTTP(w, r)
			return
		}
		if origin == "" {
			writeError(w, http.StatusForbidden, "writes need an Origin header")
			return
		}
		mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
		if err != nil || mt != "application/json" {
			writeError(w, http.StatusUnsupportedMediaType, "writes need Content-Type: application/json")
			return
		}
		if subtle.ConstantTimeCompare([]byte(r.Header.Get(csrfHeader)), []byte(s.csrf)) != 1 {
			writeError(w, http.StatusForbidden, "missing or wrong "+csrfHeader+" header; reload the page")
			return
		}
		if lan, why := s.lan(r.Context()); lan {
			writeError(w, http.StatusLocked, why)
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, maxBody)
		next.ServeHTTP(w, r)
	})
}

func uiNavigation(r *http.Request) bool {
	return isRead(r.Method) &&
		r.Header.Get("Sec-Fetch-Mode") == "navigate" &&
		r.URL.Path != "/api" && !strings.HasPrefix(r.URL.Path, "/api/")
}

func hostOnly(hostport string) string {
	if h, _, err := net.SplitHostPort(hostport); err == nil {
		return h
	}
	return strings.TrimSuffix(hostport, ".")
}
