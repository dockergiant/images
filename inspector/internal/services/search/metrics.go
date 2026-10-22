package search

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/dockergiant/images/inspector/internal/core"
	"github.com/dockergiant/images/inspector/internal/metrics"
)

// The Health filter is narrower; this one adds indexing, the caches and GC.
const (
	metricStatsPath   = "/_nodes/stats/indices,jvm?filter_path=nodes.*.indices.search.query_total,nodes.*.indices.search.query_time_in_millis,nodes.*.indices.indexing.index_total,nodes.*.indices.query_cache.hit_count,nodes.*.indices.query_cache.miss_count,nodes.*.indices.request_cache.hit_count,nodes.*.indices.request_cache.miss_count,nodes.*.jvm.mem.heap_used_percent,nodes.*.jvm.gc.collectors"
	metricIndicesPath = "/_cat/indices?format=json&bytes=b&h=index,docs.count,store.size"
)

var metricSpec = metrics.Spec{
	Counters: []string{"queries", "indexing", "gc_ms"},
	Gauges:   []string{"heap_used_pct", "docs.", "bytes."},
	Ratios: []metrics.Ratio{
		{Name: "query_ms", Num: []string{"query_time_ms"}, Den: []string{"queries"}},
		{Name: "query_cache_hit_ratio", Num: []string{"query_cache_hits"}, Den: []string{"query_cache_hits", "query_cache_misses"}},
		{Name: "request_cache_hit_ratio", Num: []string{"request_cache_hits"}, Den: []string{"request_cache_hits", "request_cache_misses"}},
	},
	Groups: []metrics.Group{{Prefixes: []string{"docs."}}, {Prefixes: []string{"bytes."}}},
}

var _ core.MetricsSource = (*adapter)(nil)

type hitMiss struct {
	Hits   int64 `json:"hit_count"`
	Misses int64 `json:"miss_count"`
}

type metricNode struct {
	Indices struct {
		Search struct {
			QueryTotal int64 `json:"query_total"`
			QueryTime  int64 `json:"query_time_in_millis"`
		} `json:"search"`
		Indexing struct {
			Total int64 `json:"index_total"`
		} `json:"indexing"`
		QueryCache   hitMiss `json:"query_cache"`
		RequestCache hitMiss `json:"request_cache"`
	} `json:"indices"`
	JVM struct {
		Mem struct {
			HeapUsedPercent float64 `json:"heap_used_percent"`
		} `json:"mem"`
		GC struct {
			Collectors map[string]struct {
				Millis int64 `json:"collection_time_in_millis"`
			} `json:"collectors"`
		} `json:"gc"`
	} `json:"jvm"`
}

type metricStats struct {
	Nodes map[string]metricNode `json:"nodes"`
}

func (a *adapter) Metrics(ctx context.Context) (map[string]float64, error) {
	return a.history.Collect(ctx, func(ctx context.Context) (map[string]float64, error) {
		c, _, err := a.client(ctx)
		if errors.Is(err, errNotRunning) {
			return nil, fmt.Errorf("%w: %w", core.ErrAbsent, err)
		}
		if err != nil {
			return nil, err
		}
		var stats metricStats
		if err := c.getJSON(ctx, metricStatsPath, &stats); err != nil {
			return nil, err
		}
		data, _, err := c.do(ctx, http.MethodGet, metricIndicesPath, nil)
		if err != nil {
			return nil, err
		}
		ix, err := ParseCatIndices(data)
		if err != nil {
			return nil, err
		}
		return statsMetrics(stats, ix), nil
	})
}

// statsMetrics sums the nodes; heap is the fullest node, since that one runs
// out first.
func statsMetrics(stats metricStats, indices []CatIndex) map[string]float64 {
	m := map[string]float64{}
	add := func(k string, v int64) { m[k] += float64(v) }
	for _, n := range stats.Nodes {
		add("queries", n.Indices.Search.QueryTotal)
		add("query_time_ms", n.Indices.Search.QueryTime)
		add("indexing", n.Indices.Indexing.Total)
		add("query_cache_hits", n.Indices.QueryCache.Hits)
		add("query_cache_misses", n.Indices.QueryCache.Misses)
		add("request_cache_hits", n.Indices.RequestCache.Hits)
		add("request_cache_misses", n.Indices.RequestCache.Misses)
		gc := int64(0)
		for _, c := range n.JVM.GC.Collectors {
			gc += c.Millis
		}
		add("gc_ms", gc)
		m["heap_used_pct"] = max(m["heap_used_pct"], n.JVM.Mem.HeapUsedPercent)
	}
	for _, x := range indices {
		if isSystemIndex(x.Index) {
			continue
		}
		m["docs."+x.Index] = float64(x.Docs)
		m["bytes."+x.Index] = float64(x.Bytes)
	}
	return m
}
