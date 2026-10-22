package redis

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"

	"github.com/dockergiant/images/inspector/internal/core"
	"github.com/dockergiant/images/inspector/internal/docker"
	"github.com/dockergiant/images/inspector/internal/metrics"
	"github.com/dockergiant/images/inspector/internal/services"
)

func TestMetricsFromInfo(t *testing.T) {
	info := ParseInfo(readFixture(t, "info-valkey90.txt"))
	m := infoMetrics(info, own{hits: 1, misses: 0, cmds: 5})
	want := map[string]float64{
		"ops":          15 - 5,
		"hits":         1 - 1,
		"misses":       1,
		"used_memory":  1137064,
		"maxmemory":    0,
		"evicted":      0,
		"expired":      0,
		"clients":      3,
		"keys.db0":     3,
		"keys.db1":     1,
		"keys.db3":     1,
		"calls.get":    2,
		"calls.set":    3,
		"calls.select": 4,
	}
	for k, v := range want {
		if got, ok := m[k]; !ok || got != v {
			t.Errorf("%s = %v (present %v), want %v", k, got, ok, v)
		}
	}
	if _, ok := m["calls.client|setname"]; !ok {
		t.Error("subcommands keep their name")
	}
}

func TestMetricsRecordsAndServes(t *testing.T) {
	srv := newFakeServer(t)
	infoReplies(t, srv, "info-valkey81.txt")
	a := newTestAdapter(srv.addr(), services.Deps{}, nil)
	for i := 0; i < 2; i++ {
		if _, err := a.Metrics(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	mux := http.NewServeMux()
	for _, rt := range a.Browse() {
		mux.HandleFunc(rt.Method+" "+rt.Path, rt.Handler)
	}
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics?window=5m", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d %s", rec.Code, rec.Body)
	}
	var r metrics.Result
	if err := json.Unmarshal(rec.Body.Bytes(), &r); err != nil {
		t.Fatal(err)
	}
	if r.Samples != 2 {
		t.Fatalf("samples = %d", r.Samples)
	}
	for _, name := range []string{"ops", "hits", "misses", "used_memory", "maxmemory", "evicted", "expired", "clients", "hit_ratio"} {
		if _, ok := r.Series[name]; !ok {
			t.Errorf("series %s missing", name)
		}
	}
	if r.Kinds["used_memory"] != "gauge" || r.Kinds["ops"] != "rate" {
		t.Errorf("kinds = %v", r.Kinds)
	}
	if !slices.ContainsFunc(a.Browse(), func(rt core.Route) bool { return rt.Path == "/metrics" }) {
		t.Fatal("no /metrics route")
	}
}

func TestMetricsReuseTheHealthCache(t *testing.T) {
	srv := newFakeServer(t)
	infoReplies(t, srv, "info-valkey81.txt")
	a := newTestAdapter(srv.addr(), services.Deps{}, nil)
	if _, err := a.Health(context.Background()); err != nil {
		t.Fatal(err)
	}
	before := countInfo(srv.seen())
	if _, err := a.Metrics(context.Background()); err != nil {
		t.Fatal(err)
	}
	if after := countInfo(srv.seen()); after != before {
		t.Fatalf("INFO ran %d more times; a fresh Health already holds it", after-before)
	}
}

func countInfo(cmds [][]string) int {
	n := 0
	for _, c := range cmds {
		if len(c) > 0 && (c[0] == "INFO" || c[0] == "info") {
			n++
		}
	}
	return n
}

func TestMetricsWithoutRedis(t *testing.T) {
	a := newTestAdapter("127.0.0.1:1", services.Deps{Docker: &docker.Fake{EnvName: "x"}}, nil)
	if _, err := a.Metrics(context.Background()); !errors.Is(err, core.ErrAbsent) {
		t.Fatalf("err = %v, want ErrAbsent", err)
	}
}
