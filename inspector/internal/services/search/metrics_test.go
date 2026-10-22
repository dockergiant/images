package search

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/dockergiant/images/inspector/internal/core"
	"github.com/dockergiant/images/inspector/internal/docker"
	"github.com/dockergiant/images/inspector/internal/metrics"
	"github.com/dockergiant/images/inspector/internal/services"
)

func TestMetricsFromNodeStats(t *testing.T) {
	var stats metricStats
	if err := json.Unmarshal(fixture(t, "nodes-stats-metrics-3.5.json"), &stats); err != nil {
		t.Fatal(err)
	}
	ix, err := ParseCatIndices(fixture(t, "cat-indices-3.5.json"))
	if err != nil {
		t.Fatal(err)
	}
	m := statsMetrics(stats, ix)
	want := map[string]float64{
		"queries":              2009,
		"query_time_ms":        5133,
		"indexing":             4044,
		"heap_used_pct":        20,
		"gc_ms":                8158 + 1862,
		"query_cache_hits":     0,
		"query_cache_misses":   0,
		"request_cache_hits":   0,
		"request_cache_misses": 1,
	}
	for k, v := range want {
		if got, ok := m[k]; !ok || got != v {
			t.Errorf("%s = %v (present %v), want %v", k, got, ok, v)
		}
	}
	var perIndex int
	for _, x := range ix {
		_, docs := m["docs."+x.Index]
		_, bytes := m["bytes."+x.Index]
		if isSystemIndex(x.Index) {
			if docs || bytes {
				t.Errorf("system index %s reported", x.Index)
			}
			continue
		}
		perIndex++
		if m["docs."+x.Index] != float64(x.Docs) || m["bytes."+x.Index] != float64(x.Bytes) {
			t.Errorf("%s docs %v bytes %v", x.Index, m["docs."+x.Index], m["bytes."+x.Index])
		}
	}
	if perIndex == 0 {
		t.Fatal("the fixture has no Magento index to check")
	}
}

func TestMetricsRecordsAndServes(t *testing.T) {
	f := newFakeOS(t)
	f.stats = fixture(t, "nodes-stats-metrics-3.5.json")
	f.addIndex("magento2_product_1_v4", 1, nil)
	f.addIndex(".plugins-ml-config", 1, nil)
	a, _ := testAdapter(t, f, magentoStores)
	at := fixedNow
	a.now = func() time.Time { return at }
	for i := 0; i < 2; i++ {
		if _, err := a.Metrics(context.Background()); err != nil {
			t.Fatal(err)
		}
		at = at.Add(metrics.Step)
	}
	at = at.Add(-metrics.Step)
	code, body := call(t, browseMux(a), "GET", "/metrics?window=5m", nil)
	if code != 200 {
		t.Fatalf("status %d %v", code, body)
	}
	series := body["series"].(map[string]any)
	for _, name := range []string{"queries", "query_ms", "indexing", "heap_used_pct", "query_cache_hit_ratio", "request_cache_hit_ratio", "gc_ms", "docs.magento2_product_1_v4", "bytes.magento2_product_1_v4"} {
		if _, ok := series[name]; !ok {
			t.Errorf("series %s missing", name)
		}
	}
	for _, hidden := range []string{"query_time_ms", "query_cache_hits", "docs..plugins-ml-config"} {
		if _, ok := series[hidden]; ok {
			t.Errorf("series %s should not be served", hidden)
		}
	}
	if q := series["queries"].([]any); len(q) != 1 {
		t.Errorf("queries = %v", q)
	}
	if !slices.ContainsFunc(a.Browse(), func(rt core.Route) bool { return rt.Path == "/metrics" }) {
		t.Fatal("no /metrics route")
	}
}

func TestMetricsWithoutSearch(t *testing.T) {
	a := newAdapter(services.Deps{Docker: &docker.Fake{EnvName: "x"}})
	if _, err := a.Metrics(context.Background()); !errors.Is(err, core.ErrAbsent) {
		t.Fatalf("err = %v, want ErrAbsent", err)
	}
}
