package api

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/dockergiant/images/inspector/internal/apm"
)

func TestTraceMetricsRoute(t *testing.T) {
	s, store := traceServer(t)
	list := store.List(apm.ListFilter{Sort: "recent"})
	if len(list) == 0 {
		t.Fatal("no traces in the fixture")
	}
	s.now = func() time.Time { return list[0].Start.Add(30 * time.Second) }

	w := serve(s, browserReq(http.MethodGet, "/api/traces/metrics?window=60m", nil))
	if w.Code != http.StatusOK || w.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("metrics: %d %s", w.Code, w.Body)
	}
	var m struct {
		Step      int                      `json:"step"`
		Window    int                      `json:"window"`
		Samples   int                      `json:"samples"`
		Since     *float64                 `json:"since"`
		Series    map[string][][2]*float64 `json:"series"`
		Totals    map[string]*float64      `json:"totals"`
		Histogram []map[string]any         `json:"histogram"`
		Routes    []map[string]any         `json:"routes"`
		Kinds     map[string]string        `json:"kinds"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &m); err != nil {
		t.Fatal(err, w.Body.String())
	}
	if m.Step != 60 || m.Window != 3600 || m.Samples == 0 || m.Since == nil || len(m.Series["requests"]) != 60 || len(m.Histogram) != 8 || len(m.Routes) == 0 {
		t.Fatalf("metrics = %s", w.Body)
	}
	for _, name := range []string{"requests", "p50", "p95", "max", "sql_p50", "sql_p95"} {
		if _, ok := m.Series[name]; !ok {
			t.Errorf("series %s missing", name)
		}
	}

	w = serve(s, browserReq(http.MethodGet, "/api/traces/metrics?window=7m", nil))
	var e map[string]string
	json.Unmarshal(w.Body.Bytes(), &e)
	if w.Code != http.StatusBadRequest || e["error"] == "" || e["hint"] == "" {
		t.Fatalf("bad window: %d %s", w.Code, w.Body)
	}
}

func TestTraceMetricsWithoutStore(t *testing.T) {
	s, _ := traceServer(t)
	s.cfg.Store = nil
	w := serve(s, browserReq(http.MethodGet, "/api/traces/metrics", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
}
