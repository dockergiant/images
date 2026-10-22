package varnish

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/dockergiant/images/inspector/internal/core"
	"github.com/dockergiant/images/inspector/internal/metrics"
)

var statNames = map[string]string{
	"MAIN.cache_hit":     "hit",
	"MAIN.cache_miss":    "miss",
	"MAIN.s_pass":        "pass",
	"MAIN.s_synth":       "synth",
	"MAIN.s_pipe":        "pipe",
	"MAIN.cache_hitpass": "hitpass",
	"MAIN.cache_hitmiss": "hitmiss",
	"MAIN.client_req":    "client_req",
	"MAIN.backend_req":   "backend_req",
	"MAIN.backend_fail":  "backend_fail",
	"MAIN.n_object":      "n_object",
	"SMA.s0.g_bytes":     "g_bytes",
	"SMA.s0.g_space":     "g_space",
	"MAIN.bans":          "bans",
	"MAIN.n_expired":     "n_expired",
	"MAIN.n_lru_nuked":   "n_lru_nuked",
}

// The hit ratio is hits / (hits + misses + passes), the same on the Health
// card, the donut and the Overview tile: a pass is a request PHP answers
// every time. Hit-for-pass ends in a pass and is already in s_pass; synth
// and pipe never reach the cache and stay out.
var metricSpec = metrics.Spec{
	Counters: []string{"hit", "miss", "pass", "synth", "pipe", "hitpass", "hitmiss", "client_req", "backend_req", "backend_fail", "n_expired", "n_lru_nuked"},
	Gauges:   []string{"n_object", "g_bytes", "g_space", "bans"},
	Ratios:   []metrics.Ratio{{Name: "hit_ratio", Num: []string{"hit"}, Den: []string{"hit", "miss", "pass"}}},
}

var _ core.MetricsSource = (*adapter)(nil)

func (a *adapter) stat(ctx context.Context) (map[string]uint64, error) {
	res, err := a.exec(ctx, execTimeout, "varnishstat", "-j")
	if err != nil {
		return nil, err
	}
	if res.ExitCode != 0 {
		return nil, fmt.Errorf("varnishstat -j: %s", outputText(res))
	}
	return ParseStat(res.Stdout)
}

// statWithin reuses a varnishstat taken less than maxAge ago, by Health or
// by the sampler.
func (a *adapter) statWithin(ctx context.Context, maxAge time.Duration) (map[string]uint64, error) {
	a.statMu.Lock()
	defer a.statMu.Unlock()
	if a.lastStat != nil && a.now().Sub(a.statAt) < maxAge {
		return a.lastStat, nil
	}
	counters, err := a.stat(ctx)
	if err != nil {
		return nil, err
	}
	a.lastStat, a.statAt = counters, a.now()
	return counters, nil
}

func (a *adapter) Metrics(ctx context.Context) (map[string]float64, error) {
	return a.history.Collect(ctx, func(ctx context.Context) (map[string]float64, error) {
		counters, err := a.statWithin(ctx, statReuse)
		if errors.Is(err, errNotRunning) {
			return nil, fmt.Errorf("%w: %w", core.ErrAbsent, err)
		}
		if err != nil {
			return nil, err
		}
		return statMetrics(counters), nil
	})
}

func statMetrics(counters map[string]uint64) map[string]float64 {
	out := make(map[string]float64, len(statNames))
	for src, name := range statNames {
		if v, ok := counters[src]; ok {
			out[name] = float64(v)
		}
	}
	return out
}
