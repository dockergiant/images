package redis

import (
	"context"
	"strconv"
	"strings"

	"github.com/dockergiant/images/inspector/internal/core"
	"github.com/dockergiant/images/inspector/internal/metrics"
)

var metricSpec = metrics.Spec{
	Counters: []string{"ops", "hits", "misses", "evicted", "expired", "calls."},
	Gauges:   []string{"used_memory", "maxmemory", "clients", "keys."},
	Ratios:   []metrics.Ratio{{Name: "hit_ratio", Num: []string{"hits"}, Den: []string{"hits", "misses"}}},
	Groups:   []metrics.Group{{Prefixes: []string{"calls."}}},
}

var _ core.MetricsSource = (*adapter)(nil)

type own struct {
	hits, misses, cmds int64
}

func (a *adapter) Metrics(ctx context.Context) (map[string]float64, error) {
	return a.history.Collect(ctx, func(ctx context.Context) (map[string]float64, error) {
		if a.deps.Docker != nil {
			c, err := a.deps.Docker.Service(ctx, "redis")
			if err != nil {
				return nil, err
			}
			if c == nil {
				return nil, core.ErrAbsent
			}
		}
		o := own{hits: a.ownHits.Load(), misses: a.ownMisses.Load(), cmds: a.ownCmds.Load()}
		info, err := a.metricsInfo(ctx)
		if err != nil {
			return nil, err
		}
		return infoMetrics(info, o), nil
	})
}

// metricsInfo takes the INFO a Health call fetched in the last 2 s, so an open
// Health screen and the sampler do not both ask the server.
func (a *adapter) metricsInfo(ctx context.Context) (Info, error) {
	if h, ok := a.cachedHealth(); ok {
		if raw, ok := h.Raw.(map[string]any); ok {
			if info, ok := raw["info"].(Info); ok {
				return info, nil
			}
		}
	}
	c := a.client(0)
	text, err := c.Info(ctx, "all").Result()
	if err != nil {
		text, err = c.Info(ctx).Result()
	}
	if err != nil {
		return nil, err
	}
	return ParseInfo(text), nil
}

// infoMetrics leaves Inspector's own commands and lookups out, as the Health
// cards do. Right after a server restart that can dip below 0; the history
// treats the drop as a counter reset.
func infoMetrics(info Info, o own) map[string]float64 {
	m := map[string]float64{
		"ops":         float64(info.Int("total_commands_processed") - o.cmds),
		"hits":        float64(info.Int("keyspace_hits") - o.hits),
		"misses":      float64(info.Int("keyspace_misses") - o.misses),
		"evicted":     float64(info.Int("evicted_keys")),
		"expired":     float64(info.Int("expired_keys")),
		"used_memory": float64(info.Int("used_memory")),
		"maxmemory":   float64(info.Int("maxmemory")),
		"clients":     float64(info.Int("connected_clients")),
	}
	for _, ks := range ParseKeyspace(info) {
		m["keys.db"+strconv.Itoa(ks.DB)] = float64(ks.Keys)
	}
	for _, cs := range ParseCommandStats(info) {
		m["calls."+strings.ToLower(cs.Cmd)] = float64(cs.Calls)
	}
	return m
}
