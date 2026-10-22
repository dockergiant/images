package search

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/dockergiant/images/inspector/internal/core"
)

const (
	nodeStatsPath = "/_nodes/stats/indices,jvm,thread_pool,breaker?filter_path=nodes.*.name,nodes.*.jvm.mem.heap_used_percent,nodes.*.jvm.mem.heap_used_in_bytes,nodes.*.jvm.mem.heap_max_in_bytes,nodes.*.indices.docs,nodes.*.indices.store.size_in_bytes,nodes.*.indices.search.query_total,nodes.*.indices.search.query_time_in_millis,nodes.*.indices.search.fetch_total,nodes.*.thread_pool.search,nodes.*.thread_pool.write,nodes.*.breakers"
	catNodesPath  = "/_cat/nodes?format=json&h=name,heap.percent,heap.max,ram.percent,ram.max,cpu,load_1m,load_5m,load_15m,node.role"
)

type clusterHealth struct {
	ClusterName         string `json:"cluster_name"`
	Status              string `json:"status"`
	NumberOfNodes       int    `json:"number_of_nodes"`
	ActivePrimaryShards int    `json:"active_primary_shards"`
	ActiveShards        int    `json:"active_shards"`
	RelocatingShards    int    `json:"relocating_shards"`
	InitializingShards  int    `json:"initializing_shards"`
	UnassignedShards    int    `json:"unassigned_shards"`
}

type nodeStat struct {
	Name    string `json:"name"`
	Indices struct {
		Docs struct {
			Count int64 `json:"count"`
		} `json:"docs"`
		Store struct {
			Size int64 `json:"size_in_bytes"`
		} `json:"store"`
		Search struct {
			QueryTotal int64 `json:"query_total"`
			QueryTime  int64 `json:"query_time_in_millis"`
		} `json:"search"`
	} `json:"indices"`
	JVM struct {
		Mem struct {
			HeapUsedPercent int   `json:"heap_used_percent"`
			HeapUsed        int64 `json:"heap_used_in_bytes"`
			HeapMax         int64 `json:"heap_max_in_bytes"`
		} `json:"mem"`
	} `json:"jvm"`
	ThreadPool map[string]struct {
		Rejected int64 `json:"rejected"`
	} `json:"thread_pool"`
	Breakers map[string]struct {
		Tripped int64 `json:"tripped"`
	} `json:"breakers"`
}

type nodeStats struct {
	Nodes map[string]nodeStat `json:"nodes"`
}

type healthInput struct {
	Flavor      Flavor
	Insights    bool
	Cluster     clusterHealth
	Stats       *nodeStats
	Nodes       []map[string]any
	Views       []IndexView
	Stores      storeSet
	Capturing   bool
	Prev        sample
	Now         time.Time
	StatsErrors []string
}

func toneRank(t core.Tone) int {
	switch t {
	case core.Error:
		return 3
	case core.Warn:
		return 2
	case core.OK:
		return 1
	}
	return 0
}

func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "KMGTPE"[exp])
}

func plural(n int, one, many string) string {
	if n == 1 {
		return fmt.Sprintf("%d %s", n, one)
	}
	return fmt.Sprintf("%d %s", n, many)
}

func clusterCard(ch clusterHealth) (core.Card, string) {
	card := core.Card{Label: "Cluster", Value: ch.Status}
	switch {
	case ch.Status == "green":
		card.Sub = plural(ch.NumberOfNodes, "node", "nodes") + " · " + plural(ch.ActiveShards, "shard", "shards")
		card.Tone = core.OK
		return card, "green"
	case ch.Status == "yellow" && ch.NumberOfNodes == 1 && ch.UnassignedShards > 0:
		card.Sub = "replicas unassigned (normal on one node)"
		card.Tone = core.OK
		return card, "yellow, replicas unassigned (normal on one node)"
	case ch.Status == "yellow":
		card.Sub = plural(ch.UnassignedShards, "shard unassigned", "shards unassigned")
		card.Tone = core.Warn
	default:
		card.Sub = plural(ch.UnassignedShards, "shard unassigned", "shards unassigned")
		card.Tone = core.Error
	}
	return card, ch.Status + ", " + card.Sub
}

func buildHealth(in healthInput) (core.Health, sample) {
	h := core.Health{Cards: []core.Card{}, Warnings: []core.Warning{}}
	cluster, clusterText := clusterCard(in.Cluster)
	h.Cards = append(h.Cards, cluster)
	status := cluster.Tone

	next := in.Prev
	if in.Stats != nil {
		var heapPct int
		var heapUsed, heapMax, queries, millis, rejected, tripped int64
		for _, n := range in.Stats.Nodes {
			heapPct = max(heapPct, n.JVM.Mem.HeapUsedPercent)
			heapUsed += n.JVM.Mem.HeapUsed
			heapMax += n.JVM.Mem.HeapMax
			queries += n.Indices.Search.QueryTotal
			millis += n.Indices.Search.QueryTime
			rejected += n.ThreadPool["search"].Rejected
			for _, b := range n.Breakers {
				tripped += b.Tripped
			}
		}
		heap := core.Card{Label: "Heap", Value: fmt.Sprintf("%d%%", heapPct), Sub: humanBytes(heapUsed) + " of " + humanBytes(heapMax), Tone: core.OK}
		switch {
		case heapPct >= 90:
			heap.Tone = core.Error
		case heapPct >= 75:
			heap.Tone = core.Warn
		}
		h.Cards = append(h.Cards, heap)

		searches := core.Card{Label: "Searches", Value: fmt.Sprintf("%d", queries), Sub: "queries since start", Tone: core.ToneInfo}
		if dt := in.Now.Sub(in.Prev.at).Seconds(); !in.Prev.at.IsZero() && dt > 0 && queries >= in.Prev.queries {
			dq := queries - in.Prev.queries
			searches.Value = fmt.Sprintf("%.1f/s", float64(dq)/dt)
			searches.Sub = "no queries since the last sample"
			if dq > 0 {
				searches.Sub = fmt.Sprintf("avg %d ms per query", (millis-in.Prev.millis)/dq)
			}
		}
		h.Cards = append(h.Cards, searches)
		next = sample{at: in.Now, queries: queries, millis: millis}

		if rejected > 0 {
			h.Warnings = append(h.Warnings, core.Warning{ID: "search.rejected", Text: fmt.Sprintf("%d search requests were rejected because the search thread pool was full", rejected), Tone: core.Warn})
		}
		if tripped > 0 {
			h.Warnings = append(h.Warnings, core.Warning{ID: "search.breaker", Text: fmt.Sprintf("Circuit breakers tripped %d times: the node ran short of heap", tripped), Fix: "raise OPENSEARCH_JAVA_OPTS in .env.roll", Tone: core.Warn})
		}
	}

	indices, orphans, magento := 0, 0, 0
	var docs, size int64
	for _, v := range in.Views {
		if v.System {
			continue
		}
		indices++
		docs += v.Docs
		size += v.Bytes
		if v.Label != nil {
			magento++
		}
		if v.Orphan {
			orphans++
		}
	}
	ixCard := core.Card{Label: "Indices", Value: fmt.Sprintf("%d", indices), Sub: fmt.Sprintf("%d docs · %s", docs, humanBytes(size)), Tone: core.OK}
	if orphans > 0 {
		ixCard.Sub += " · " + plural(orphans, "orphan", "orphans")
		ixCard.Tone = core.Warn
		h.Warnings = append(h.Warnings, core.Warning{ID: "search.orphans",
			Text: plural(orphans, "orphan index", "orphan indices") + ": no alias, older than 10 minutes, and its store no longer exists",
			Fix:  "delete it from Browse, Indices", Tone: core.Warn})
	}
	h.Cards = append(h.Cards, ixCard)

	if len(in.Nodes) > 0 {
		n := in.Nodes[0]
		h.Cards = append(h.Cards, core.Card{Label: "Host", Value: "load " + catString(n["load_1m"]),
			Sub:  fmt.Sprintf("RAM %s%% of %s · host-wide, the whole Docker VM", catString(n["ram.percent"]), catString(n["ram.max"])),
			Tone: core.ToneInfo})
	}

	if in.Capturing {
		h.Warnings = append(h.Warnings, core.Warning{ID: "search.capture", Text: "Capturing every query: slowlog thresholds are at 0 ms", Fix: "close the drawer to restore them", Tone: core.ToneInfo})
	}
	if in.Flavor.Engine == EngineOpenSearch && !in.Insights {
		h.Warnings = append(h.Warnings, core.Warning{ID: "search.insights", Text: "Query Insights needs OpenSearch 2.12 or later; the live lane needs the slowlog capture", Tone: core.ToneInfo})
	}
	if in.Stores.Magento && in.Stores.Err != nil {
		h.Warnings = append(h.Warnings, core.Warning{ID: "search.stores", Text: "Store list unavailable, so orphan detection is off: " + in.Stores.Err.Error(), Tone: core.ToneInfo})
	}
	for _, e := range in.StatsErrors {
		h.Warnings = append(h.Warnings, core.Warning{ID: "search.partial", Text: e, Tone: core.ToneInfo})
	}
	for _, w := range h.Warnings {
		if toneRank(w.Tone) > toneRank(status) {
			status = w.Tone
		}
	}
	h.Status = status

	h.Summary = fmt.Sprintf("%s %s · %s · %s", in.Flavor.Name(), in.Flavor.Version, clusterText, plural(indices, "index", "indices"))
	if magento > 0 {
		h.Summary += fmt.Sprintf(" (%d Magento)", magento)
	}
	h.Raw = map[string]any{"cluster": in.Cluster, "stats": in.Stats, "nodes": in.Nodes}
	return h, next
}

func (a *adapter) Health(ctx context.Context) (core.Health, error) {
	c, _, err := a.client(ctx)
	if errors.Is(err, errNotRunning) {
		return core.Health{Status: core.ToneInfo, Summary: "No search engine runs in this environment", Cards: []core.Card{},
			Warnings: []core.Warning{{ID: "search.absent", Text: "OpenSearch is not running", Fix: "set ROLL_OPENSEARCH=1 in .env.roll and run roll env up", Tone: core.ToneInfo}}}, nil
	}
	if err != nil {
		return core.Health{}, err
	}
	in := healthInput{Now: a.now(), Capturing: a.captureActive()}
	in.Flavor, in.Insights, err = a.probe(ctx, c, false)
	if err != nil {
		return core.Health{Status: core.Error, Summary: "Search engine unreachable: " + err.Error(), Cards: []core.Card{}, Warnings: []core.Warning{}}, nil
	}
	if err := c.getJSON(ctx, "/_cluster/health", &in.Cluster); err != nil {
		return core.Health{Status: core.Error, Summary: "Cluster health unavailable: " + err.Error(), Cards: []core.Card{}, Warnings: []core.Warning{}}, nil
	}
	var stats nodeStats
	if err := c.getJSON(ctx, nodeStatsPath, &stats); err != nil {
		in.StatsErrors = append(in.StatsErrors, "Node stats unavailable: "+err.Error())
	} else {
		in.Stats = &stats
	}
	if data, _, err := c.do(ctx, http.MethodGet, catNodesPath, nil); err == nil {
		json.Unmarshal(data, &in.Nodes)
	}
	if ix, al, err := c.catalog(ctx); err != nil {
		in.StatsErrors = append(in.StatsErrors, "Index list unavailable: "+err.Error())
	} else {
		in.Stores = a.stores(ctx)
		in.Views = BuildIndexViews(ix, al, in.Stores.Stores, in.Stores.Magento, in.Now)
	}
	a.mu.Lock()
	in.Prev = a.prev
	a.mu.Unlock()
	h, next := buildHealth(in)
	a.mu.Lock()
	a.prev = next
	a.mu.Unlock()
	return h, nil
}
