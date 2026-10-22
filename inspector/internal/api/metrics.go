package api

import (
	"net/http"

	"github.com/dockergiant/images/inspector/internal/apm"
	"github.com/dockergiant/images/inspector/internal/metrics"
)

func (s *Server) handleTraceMetrics(w http.ResponseWriter, r *http.Request) {
	window, err := metrics.ParseWindow(r.URL.Query().Get("window"))
	if err != nil {
		metrics.WindowError(w, err)
		return
	}
	var list []apm.Summary
	if s.cfg.Store != nil {
		list = s.cfg.Store.List(apm.ListFilter{Sort: "recent"})
	}
	writeJSON(w, http.StatusOK, metrics.PHP(list, window, s.now()))
}
