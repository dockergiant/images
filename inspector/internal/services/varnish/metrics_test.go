package varnish

import (
	"context"
	"errors"
	"log/slog"
	"slices"
	"testing"
	"time"

	"github.com/dockergiant/images/inspector/internal/core"
	"github.com/dockergiant/images/inspector/internal/docker"
	"github.com/dockergiant/images/inspector/internal/metrics"
	"github.com/dockergiant/images/inspector/internal/services"
)

func TestMetricsFromVarnishstat(t *testing.T) {
	counters, err := ParseStat(fixture(t, "stat-8.0.json"))
	if err != nil {
		t.Fatal(err)
	}
	m := statMetrics(counters)
	want := map[string]string{
		"hit": "MAIN.cache_hit", "miss": "MAIN.cache_miss", "pass": "MAIN.s_pass", "synth": "MAIN.s_synth",
		"pipe": "MAIN.s_pipe", "hitpass": "MAIN.cache_hitpass", "hitmiss": "MAIN.cache_hitmiss",
		"client_req": "MAIN.client_req", "backend_req": "MAIN.backend_req", "backend_fail": "MAIN.backend_fail",
		"n_object": "MAIN.n_object", "g_bytes": "SMA.s0.g_bytes", "g_space": "SMA.s0.g_space",
		"bans": "MAIN.bans", "n_expired": "MAIN.n_expired", "n_lru_nuked": "MAIN.n_lru_nuked",
	}
	for name, src := range want {
		v, ok := m[name]
		if !ok {
			t.Errorf("%s missing", name)
			continue
		}
		if v != float64(counters[src]) {
			t.Errorf("%s = %v, want %s = %d", name, v, src, counters[src])
		}
	}
	if len(m) != len(want) {
		t.Errorf("metrics = %v", m)
	}
}

func TestMetricsFromVarnish60LeaveOutWhatItLacks(t *testing.T) {
	counters, err := ParseStat(fixture(t, "stat-6.0-flat.json"))
	if err != nil {
		t.Fatal(err)
	}
	m := statMetrics(counters)
	if m["hit"] != 120 || m["miss"] != 40 || m["pass"] != 2547 || m["n_object"] != 12 || m["g_bytes"] != 1048576 {
		t.Fatalf("metrics = %v", m)
	}
	for _, absent := range []string{"hitpass", "hitmiss", "client_req", "g_space"} {
		if _, ok := m[absent]; ok {
			t.Errorf("%s reported though varnishstat lacks it", absent)
		}
	}
}

func TestMetricsRecordsIntoTheHistory(t *testing.T) {
	a, _, stub := newTestAdapter(t, "8.0")
	at := time.Unix(1_792_000_000, 0)
	a.now = func() time.Time { return at }
	stub.set("varnishstat -j", docker.ExecResult{Stdout: statJSON(map[string]uint64{"MAIN.cache_hit": 100, "MAIN.cache_miss": 20})})
	if _, err := a.Metrics(context.Background()); err != nil {
		t.Fatal(err)
	}
	at = at.Add(metrics.Step)
	stub.set("varnishstat -j", docker.ExecResult{Stdout: statJSON(map[string]uint64{"MAIN.cache_hit": 180, "MAIN.cache_miss": 40})})
	if _, err := a.Metrics(context.Background()); err != nil {
		t.Fatal(err)
	}

	srv := serve(t, a)
	var r metrics.Result
	if code := get(t, srv, "/api/varnish/metrics?window=5m", &r); code != 200 {
		t.Fatalf("status %d", code)
	}
	if hit := r.Series["hit"]; len(hit) != 1 || hit[0][1] != 8 {
		t.Fatalf("hit = %v", hit)
	}
	if ratio := r.Series["hit_ratio"]; len(ratio) != 1 || ratio[0][1] != 0.8 {
		t.Fatalf("hit_ratio = %v", ratio)
	}
	for _, name := range []string{"pass", "synth", "pipe", "n_object", "g_bytes", "g_space", "bans", "n_lru_nuked"} {
		if _, ok := r.Series[name]; !ok {
			t.Errorf("series %s missing", name)
		}
	}
	if !slices.ContainsFunc(a.Browse(), func(rt core.Route) bool { return rt.Path == "/metrics" && rt.Method == "GET" }) {
		t.Fatal("GET /metrics is not in the route list")
	}
}

func TestHitRatioCountsPassesNotSynthOrPipe(t *testing.T) {
	a, _, stub := newTestAdapter(t, "8.0")
	at := time.Unix(1_792_000_000, 0)
	a.now = func() time.Time { return at }
	stub.set("varnishstat -j", docker.ExecResult{Stdout: statJSON(map[string]uint64{"MAIN.cache_hit": 100, "MAIN.cache_miss": 20, "MAIN.s_pass": 0, "MAIN.cache_hitpass": 0, "MAIN.s_synth": 0, "MAIN.s_pipe": 0})})
	if _, err := a.Metrics(context.Background()); err != nil {
		t.Fatal(err)
	}
	at = at.Add(metrics.Step)
	stub.set("varnishstat -j", docker.ExecResult{Stdout: statJSON(map[string]uint64{"MAIN.cache_hit": 180, "MAIN.cache_miss": 40, "MAIN.s_pass": 100, "MAIN.cache_hitpass": 30, "MAIN.s_synth": 500, "MAIN.s_pipe": 70})})
	if _, err := a.Metrics(context.Background()); err != nil {
		t.Fatal(err)
	}
	r := a.history.Query(5 * time.Minute)
	if got := float64(r.Totals["hit_ratio"]); got != 0.4 {
		t.Fatalf("hit_ratio over the window = %v, want 80 / (80 + 20 + 100) = 0.4", got)
	}
	if got := float64(r.Latest["hit_ratio"]); got != 180.0/320 {
		t.Fatalf("lifetime hit_ratio = %v, want 180 / 320", got)
	}
}

func TestMetricsWithoutVarnish(t *testing.T) {
	a := newAdapter(services.Deps{Docker: &docker.Fake{EnvName: "x"}, Log: slog.New(slog.DiscardHandler)})
	if _, err := a.Metrics(context.Background()); !errors.Is(err, core.ErrAbsent) {
		t.Fatalf("err = %v, want ErrAbsent", err)
	}
}
