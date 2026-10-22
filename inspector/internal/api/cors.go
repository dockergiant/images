package api

import (
	"net/http"
	"path"
	"strings"
)

const (
	v1Prefix       = "/api/v1"
	userscriptPath = "/userscript/roll-inspector.user.js"
	extensionPath  = "/extension/roll-inspector.zip"
	openMethods    = "GET, HEAD, OPTIONS"
)

// openRead names the paths any origin may read: the v1 API and the two client
// downloads, which Tampermonkey and Chrome fetch from an extension origin. A
// path that changes when cleaned is not open, so "/api/v1/../actions" stays
// under the /api rules.
func openRead(p string) bool {
	clean := path.Clean(p)
	if clean != p && clean+"/" != p {
		return false
	}
	return clean == v1Prefix || strings.HasPrefix(clean, v1Prefix+"/") || clean == userscriptPath || clean == extensionPath
}

// serveOpen answers an open path for any origin without credentials, as
// decided for the local dev stack: no Origin, Sec-Fetch-Site, CSRF or cookie
// check. The headers go on before dispatch so errors carry them too.
func serveOpen(w http.ResponseWriter, r *http.Request, next http.Handler) {
	h := w.Header()
	h.Set("Access-Control-Allow-Origin", "*")
	h.Set("Access-Control-Expose-Headers", "*")
	switch r.Method {
	case http.MethodGet, http.MethodHead:
		next.ServeHTTP(w, r)
	case http.MethodOptions:
		h.Set("Allow", openMethods)
		h.Set("Access-Control-Allow-Methods", openMethods)
		h.Set("Access-Control-Allow-Headers", "*")
		h.Set("Access-Control-Max-Age", "600")
		w.WriteHeader(http.StatusNoContent)
	default:
		h.Set("Allow", openMethods)
		writeHint(w, http.StatusMethodNotAllowed, r.Method+" is not part of the read API", "writes (purge, flush, delete) stay on /api behind the Inspector page's confirm dialog")
	}
}

func writeHint(w http.ResponseWriter, status int, msg, hint string) {
	writeJSON(w, status, map[string]string{"error": msg, "hint": hint})
}
