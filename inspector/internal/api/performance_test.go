package api

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dockergiant/images/inspector/internal/apm"
	"github.com/dockergiant/images/inspector/internal/core"
	"github.com/dockergiant/images/inspector/internal/docker"
	"github.com/dockergiant/images/inspector/internal/metrics"
	"github.com/dockergiant/images/inspector/internal/perf"
)

// phpBox answers the tracing execs like a php-fpm image with tracing on.
type phpBox struct {
	mu      sync.Mutex
	on      bool
	lean    bool
	profile bool
	reloads int
}

func (b *phpBox) exec(ctx context.Context, spec docker.ExecSpec) (docker.ExecResult, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	bit := func(v bool) string {
		if v {
			return "1"
		}
		return "0"
	}
	switch spec.Cmd[0] {
	case "sh":
		out := "script=1\ndetail=1\ntemplate=1\nddtrace=" + bit(b.on) + "\nsettings=" + bit(b.on) + "\nprofile=" + bit(b.profile) + "\nnewrelic=0\nlean=" + bit(b.lean) + "\n"
		return docker.ExecResult{Stdout: []byte(out)}, nil
	case "roll-inspector-ini":
		b.on = spec.Cmd[1] == "on"
		b.lean, b.profile = false, false
		for _, kv := range spec.Env {
			b.lean = b.lean || kv == "ROLL_INSPECTOR_TRACE_DETAIL=lean"
			b.profile = b.profile || kv == "ROLL_INSPECTOR_PROFILE=excimer"
		}
		if !b.on {
			b.lean, b.profile = false, false
		}
	case "kill":
		b.reloads++
	}
	return docker.ExecResult{}, nil
}

func (b *phpBox) state() (bool, bool, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.on, b.lean, b.profile
}

func newPerfServer(t *testing.T, nginx string) (*Server, *phpBox, *perf.Perf) {
	t.Helper()
	box := &phpBox{on: true}
	d := fakeDocker(traefikAddr)
	d.ContainerList = []docker.Container{{ID: "fpm", Service: "php-fpm", Running: true}}
	d.ContainerEnvs = map[string][]string{"fpm": {"ROLL_INSPECTOR_TRACE=1"}}
	d.ExecFunc = box.exec
	store := apm.NewStore(0, 0)
	p := perf.New(perf.Options{
		Docker: d, Env: "shop", Domain: testDomain, Store: store,
		Sampler: metrics.NewSampler(metrics.Step), SampleDefault: metrics.Step,
		Bench:       perf.BenchConfig{Base: nginx, Rounds: 1, Warmups: 1, ReadyWait: time.Second, MarkerPath: filepath.Join(t.TempDir(), "bench.json")},
		DefaultHost: func(context.Context) string { return "app." + testDomain },
	})
	s := New(Config{Env: "shop", Domain: testDomain, Secret: testSecret, Version: "test", Docker: d, Store: store, Perf: p, Log: quietLog()})
	return s, box, p
}

func decode[T any](t *testing.T, w *httptest.ResponseRecorder) T {
	t.Helper()
	var v T
	if err := json.Unmarshal(w.Body.Bytes(), &v); err != nil {
		t.Fatalf("%v: %s", err, w.Body)
	}
	return v
}

func TestPerformanceGet(t *testing.T) {
	s, _, _ := newPerfServer(t, "http://127.0.0.1:1")
	w := serve(s, browserReq(http.MethodGet, "/api/performance", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	snap := decode[perf.Snapshot](t, w)
	if len(snap.PHP) != 2 || !snap.PHP[0].Tracing || snap.PHP[1].Present || snap.Sampler.Interval != 10 || snap.Live == nil {
		t.Fatalf("snapshot %+v", snap)
	}
}

func TestPerformanceSettingsNeedCSRF(t *testing.T) {
	s, box, _ := newPerfServer(t, "http://127.0.0.1:1")
	r := browserReq(http.MethodPost, "/api/performance/settings", map[string]any{"service": "php-fpm", "tracing": false})
	r.Header.Set("Origin", testOrigin)
	r.Header.Set("Content-Type", "application/json")
	if w := serve(s, r); w.Code != http.StatusForbidden {
		t.Fatalf("without csrf: %d", w.Code)
	}
	if on, _, _ := box.state(); !on {
		t.Fatal("switched without csrf")
	}
}

func TestPerformanceSettingsSwitchTracing(t *testing.T) {
	s, box, _ := newPerfServer(t, "http://127.0.0.1:1")
	w := serve(s, writeReq(s, "/api/performance/settings", map[string]any{"service": "php-fpm", "detail": "lean", "profiling": true}))
	if w.Code != http.StatusOK {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	snap := decode[perf.Snapshot](t, w)
	if on, lean, prof := box.state(); !on || !lean || !prof || snap.PHP[0].Detail != "lean" || !snap.PHP[0].Profiling {
		t.Fatalf("box %v %v %v snap %+v", on, lean, prof, snap.PHP[0])
	}
	if snap.Summary.Label != "Tracing · lean · profiling" {
		t.Fatalf("label %q", snap.Summary.Label)
	}
	w = serve(s, writeReq(s, "/api/performance/settings", map[string]any{"service": "php-fpm", "tracing": false}))
	if on, _, _ := box.state(); w.Code != http.StatusOK || on {
		t.Fatalf("off: %d %s", w.Code, w.Body)
	}
	w = serve(s, writeReq(s, "/api/performance/settings", map[string]any{"service": "php-debug", "tracing": true}))
	if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "not running") {
		t.Fatalf("absent debug: %d %s", w.Code, w.Body)
	}
	for _, bad := range []map[string]any{
		{"service": "php-fpm", "detail": "medium"},
		{"service": "varnish", "tracing": true},
		{},
		{"sample": "15"},
		{"service": "php-fpm", "nope": 1},
	} {
		if w := serve(s, writeReq(s, "/api/performance/settings", bad)); w.Code != http.StatusBadRequest {
			t.Errorf("%v: %d %s", bad, w.Code, w.Body)
		}
	}
}

func TestPerformanceSettingsSample(t *testing.T) {
	s, _, p := newPerfServer(t, "http://127.0.0.1:1")
	w := serve(s, writeReq(s, "/api/performance/settings", map[string]any{"sample": "off"}))
	if w.Code != http.StatusOK || p.Sampler.Interval() != 0 {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	if snap := decode[perf.Snapshot](t, w); !snap.Sampler.Paused {
		t.Fatalf("sampler %+v", snap.Sampler)
	}
}

func TestPerformanceBenchmarkLifecycle(t *testing.T) {
	gate := make(chan struct{})
	var once sync.Once
	release := func() { once.Do(func() { close(gate) }) }
	defer release()
	nginx := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-gate
		w.Write([]byte("ok"))
	}))
	defer nginx.Close()
	s, box, p := newPerfServer(t, nginx.URL)

	w := serve(s, writeReq(s, "/api/performance/benchmark", map[string]any{"url": "https://app.shop.test/women.html"}))
	if w.Code != http.StatusAccepted {
		t.Fatalf("start %d %s", w.Code, w.Body)
	}
	if st := decode[perf.BenchState](t, w); !st.Running || st.Path != "/women.html" {
		t.Fatalf("state %+v", st)
	}
	if w := serve(s, writeReq(s, "/api/performance/benchmark", map[string]any{"url": "/"})); w.Code != http.StatusConflict {
		t.Fatalf("second start %d", w.Code)
	}
	if w := serve(s, writeReq(s, "/api/performance/settings", map[string]any{"service": "php-fpm", "tracing": false})); w.Code != http.StatusConflict {
		t.Fatalf("settings while running %d %s", w.Code, w.Body)
	}
	if w := serve(s, writeReq(s, "/api/performance/benchmark/cancel", nil)); w.Code != http.StatusOK {
		t.Fatalf("cancel %d %s", w.Code, w.Body)
	}
	release()
	deadline := time.Now().Add(5 * time.Second)
	for p.Bench.State().Running && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if st := p.Bench.State(); st.Phase != "cancelled" || !st.Restored {
		t.Fatalf("after cancel %+v", st)
	}
	if on, _, _ := box.state(); !on {
		t.Fatal("tracing not restored")
	}
	r := browserReq(http.MethodDelete, "/api/performance/benchmark", nil)
	r.Header.Set("Origin", testOrigin)
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set(csrfHeader, s.csrf)
	if w := serve(s, r); w.Code != http.StatusConflict {
		t.Fatalf("delete with nothing running %d %s", w.Code, w.Body)
	}
	if w := serve(s, writeReq(s, "/api/performance/benchmark", map[string]any{"url": "https://evil.example/"})); w.Code != http.StatusBadRequest {
		t.Fatalf("foreign url %d", w.Code)
	}
}

func TestPerformanceOnV1IsReadOnly(t *testing.T) {
	s, _, _ := newPerfServer(t, "http://127.0.0.1:1")
	r := browserReq(http.MethodGet, "/api/v1/performance", nil)
	r.Header.Set("Origin", "https://app.shop.test")
	w := serve(s, r)
	if w.Code != http.StatusOK || w.Header().Get("Access-Control-Allow-Origin") != "*" {
		t.Fatalf("v1 get %d %s", w.Code, w.Body)
	}
	r = browserReq(http.MethodPost, "/api/v1/performance/settings", map[string]any{"sample": "off"})
	r.Header.Set("Origin", "https://app.shop.test")
	r.Header.Set("Content-Type", "application/json")
	if w := serve(s, r); w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("v1 write %d", w.Code)
	}
}

func TestSessionCarriesThePerformanceSummary(t *testing.T) {
	s, _, _ := newPerfServer(t, "http://127.0.0.1:1")
	w := serve(s, browserReq(http.MethodGet, "/api/session", nil))
	var body struct {
		Performance *perf.Summary `json:"performance"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil || body.Performance == nil || body.Performance.Label != "Tracing · full" {
		t.Fatalf("session %s", w.Body)
	}
}

func TestBenchmarkSubResolves(t *testing.T) {
	s, _, _ := newPerfServer(t, "http://127.0.0.1:1")
	sub, err := s.resolveSub("perf:benchmark")
	if err != nil || sub.key != benchKey || sub.spec.Start != nil {
		t.Fatalf("%+v %v", sub, err)
	}
}

func TestPerformanceWithoutPerf(t *testing.T) {
	s := newServer(t, fakeDocker(traefikAddr))
	if w := serve(s, browserReq(http.MethodGet, "/api/performance", nil)); w.Code != http.StatusServiceUnavailable {
		t.Fatalf("%d", w.Code)
	}
}

func TestLiveStopEndsASourceAndKeepsV1Off(t *testing.T) {
	s, _, _ := newPerfServer(t, "http://127.0.0.1:1")
	redis := &fakeAdapter{kind: core.Redis, present: true, streams: []core.StreamSpec{idleStream("monitor", "db", "match")}}
	s.adapters[core.Redis] = redis
	s.cfg.Adapters = append(s.cfg.Adapters, redis)
	res := liveStream(t, s, "/api/v1/stream?sub=redis:monitor", http.Header{"Origin": {"https://evil.test"}})
	if res.StatusCode != http.StatusOK {
		t.Fatalf("open: %d", res.StatusCode)
	}
	waitFor(t, func() bool { return len(s.cfg.Hub.Active()) == 1 })
	snap := decode[perf.Snapshot](t, serve(s, browserReq(http.MethodGet, "/api/performance", nil)))
	if len(snap.Live) != 1 || snap.Live[0].Key != "redis:monitor" || strings.Join(snap.Live[0].Holders, ",") != "api" {
		t.Fatalf("live %+v", snap.Live)
	}
	if w := serve(s, writeReq(s, "/api/performance/live/stop", map[string]any{"key": "redis:monitor"})); w.Code != http.StatusOK {
		t.Fatalf("stop %d %s", w.Code, w.Body)
	}
	body, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), "event: stream.error") || !strings.Contains(string(body), "stopped from Inspector") {
		t.Fatalf("the stream did not end with a stop notice: %q", body)
	}
	if n := len(s.cfg.Hub.Active()); n != 0 {
		t.Fatalf("still running: %v", s.cfg.Hub.Active())
	}
	w := serveBriefly(s, fromOrigin(http.MethodGet, "/api/v1/stream?sub=redis:monitor", "https://evil.test"), 100*time.Millisecond)
	if w.Code != http.StatusConflict {
		t.Fatalf("v1 restarted a stopped source: %d %s", w.Code, w.Body)
	}
	if w := serveBriefly(s, browserReq(http.MethodGet, "/api/stream?sub=redis:monitor", nil), 100*time.Millisecond); w.Code != http.StatusOK {
		t.Fatalf("the Inspector page could not start it again: %d", w.Code)
	}
	if w := serve(s, writeReq(s, "/api/performance/live/stop", map[string]any{"key": "nope"})); w.Code != http.StatusNotFound {
		t.Fatalf("stop of nothing %d", w.Code)
	}
	if w := serve(s, browserReq(http.MethodPost, "/api/performance/live/stop", map[string]any{"key": "redis:monitor"})); w.Code != http.StatusForbidden {
		t.Fatalf("stop without csrf %d", w.Code)
	}
}
