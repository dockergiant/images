package metrics

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dockergiant/images/inspector/internal/core"
)

var t0 = time.Unix(1_792_000_000, 0)

type clock struct {
	mu sync.Mutex
	at time.Time
}

func (c *clock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.at
}

func (c *clock) set(at time.Time) {
	c.mu.Lock()
	c.at = at
	c.mu.Unlock()
}

func newTestHistory(spec Spec) (*History, *clock) {
	c := &clock{at: t0}
	h := NewHistory(spec)
	h.now = c.now
	return h, c
}

func values(p []Point) []float64 {
	out := make([]float64, len(p))
	for i, x := range p {
		out[i] = float64(x[1])
	}
	return out
}

func near(a, b float64) bool {
	return math.Abs(a-b) < 1e-9
}

func TestCounterRatesAndGauges(t *testing.T) {
	h, c := newTestHistory(Spec{Counters: []string{"hit"}, Gauges: []string{"objects"}})
	for i, v := range []float64{100, 150, 250} {
		at := t0.Add(time.Duration(i) * Step)
		h.Add(at, map[string]float64{"hit": v, "objects": float64(10 + i)})
	}
	c.set(t0.Add(2 * Step))
	r := h.Query(5 * time.Minute)

	if r.Step != 10 || r.Window != 300 || r.Samples != 3 {
		t.Fatalf("step %d window %d samples %d", r.Step, r.Window, r.Samples)
	}
	hit := r.Series["hit"]
	if len(hit) != 2 || !near(float64(hit[0][1]), 5) || !near(float64(hit[1][1]), 10) {
		t.Fatalf("hit rates = %v", hit)
	}
	if int64(hit[0][0]) != t0.Add(Step).Unix() {
		t.Fatalf("a rate is stamped at the later sample, got %v", hit[0][0])
	}
	if got := values(r.Series["objects"]); len(got) != 3 || got[0] != 10 || got[2] != 12 {
		t.Fatalf("objects = %v", got)
	}
	if r.Latest["hit"] != 250 || r.Latest["objects"] != 12 {
		t.Fatalf("latest = %v", r.Latest)
	}
	if r.Totals["hit"] != 150 {
		t.Fatalf("totals hit = %v, want the increase over the window", r.Totals["hit"])
	}
	if _, ok := r.Totals["objects"]; ok {
		t.Fatal("gauges have no window total")
	}
	if r.Kinds["hit"] != "rate" || r.Kinds["objects"] != "gauge" {
		t.Fatalf("kinds = %v", r.Kinds)
	}
}

func TestCounterResetGivesZeroNotNegative(t *testing.T) {
	h, c := newTestHistory(Spec{Counters: []string{"hit"}})
	for i, v := range []float64{500, 600, 20, 70} {
		h.Add(t0.Add(time.Duration(i)*Step), map[string]float64{"hit": v})
	}
	c.set(t0.Add(3 * Step))
	r := h.Query(5 * time.Minute)
	got := values(r.Series["hit"])
	want := []float64{10, 0, 5}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("rates = %v, want %v", got, want)
	}
	if r.Totals["hit"] != 150 {
		t.Fatalf("total = %v, the reset interval counts as 0", r.Totals["hit"])
	}
}

func TestRateUsesTheRealGap(t *testing.T) {
	h, c := newTestHistory(Spec{Counters: []string{"hit"}})
	h.Add(t0, map[string]float64{"hit": 0})
	h.Add(t0.Add(30*time.Second), map[string]float64{"hit": 60})
	c.set(t0.Add(30 * time.Second))
	got := values(h.Query(5 * time.Minute).Series["hit"])
	if len(got) != 1 || got[0] != 2 {
		t.Fatalf("rate over a missed sample = %v, want 2/s", got)
	}
}

func TestEverySpecNameIsPresentEvenWithoutData(t *testing.T) {
	h, _ := newTestHistory(Spec{
		Counters: []string{"hit", "calls."},
		Gauges:   []string{"objects"},
		Ratios:   []Ratio{{Name: "hit_ratio", Num: []string{"hit"}, Den: []string{"hit", "miss"}}},
	})
	r := h.Query(5 * time.Minute)
	for _, name := range []string{"hit", "objects", "hit_ratio"} {
		s, ok := r.Series[name]
		if !ok || s == nil || len(s) != 0 {
			t.Fatalf("series %q = %v, want an empty array", name, s)
		}
	}
	if r.Samples != 0 {
		t.Fatalf("samples = %d", r.Samples)
	}
	b, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	var back map[string]any
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatal(err)
	}
	if _, ok := back["series"].(map[string]any)["hit"].([]any); !ok {
		t.Fatalf("empty series must encode as [], got %s", b)
	}
}

func TestRatioNullOnZeroDenominator(t *testing.T) {
	h, c := newTestHistory(Spec{
		Counters: []string{"queries"},
		Ratios:   []Ratio{{Name: "query_ms", Num: []string{"query_time_ms"}, Den: []string{"queries"}}},
	})
	h.Add(t0, map[string]float64{"queries": 10, "query_time_ms": 100})
	h.Add(t0.Add(Step), map[string]float64{"queries": 10, "query_time_ms": 100})
	h.Add(t0.Add(2*Step), map[string]float64{"queries": 14, "query_time_ms": 120})
	c.set(t0.Add(2 * Step))
	r := h.Query(5 * time.Minute)
	q := r.Series["query_ms"]
	if len(q) != 2 || !math.IsNaN(float64(q[0][1])) || q[1][1] != 5 {
		t.Fatalf("query_ms = %v, want [null 5]", q)
	}
	b, _ := json.Marshal(q)
	if want := fmt.Sprintf("[[%d,null],[%d,5]]", t0.Add(Step).Unix(), t0.Add(2*Step).Unix()); string(b) != want {
		t.Fatalf("json = %s, want %s", b, want)
	}
	if _, ok := r.Series["query_time_ms"]; ok {
		t.Fatal("a name only used by a ratio is not a series")
	}
	if r.Totals["query_ms"] != 5 {
		t.Fatalf("window ratio = %v", r.Totals["query_ms"])
	}
	if r.Latest["query_ms"] != 120.0/14 {
		t.Fatalf("lifetime ratio = %v", r.Latest["query_ms"])
	}
	if r.Kinds["query_ms"] != "ratio" {
		t.Fatalf("kind = %q", r.Kinds["query_ms"])
	}
}

func TestHitRatioSumsTheDenominator(t *testing.T) {
	h, c := newTestHistory(Spec{
		Counters: []string{"hit", "miss"},
		Ratios:   []Ratio{{Name: "hit_ratio", Num: []string{"hit"}, Den: []string{"hit", "miss"}}},
	})
	h.Add(t0, map[string]float64{"hit": 0, "miss": 0})
	h.Add(t0.Add(Step), map[string]float64{"hit": 30, "miss": 10})
	c.set(t0.Add(Step))
	r := h.Query(5 * time.Minute)
	if got := values(r.Series["hit_ratio"]); len(got) != 1 || got[0] != 0.75 {
		t.Fatalf("hit_ratio = %v", got)
	}
}

func TestWindowKeepsTheBaselineSampleOut(t *testing.T) {
	h, c := newTestHistory(Spec{Counters: []string{"hit"}, Gauges: []string{"objects"}})
	for i := 0; i < 60; i++ {
		h.Add(t0.Add(time.Duration(i)*Step), map[string]float64{"hit": float64(i * 10), "objects": float64(i)})
	}
	c.set(t0.Add(59 * Step))
	r := h.Query(5 * time.Minute)
	if r.Samples != 31 {
		t.Fatalf("samples in 5m = %d, want 31", r.Samples)
	}
	if n := len(r.Series["hit"]); n != 31 {
		t.Fatalf("rate points = %d, the sample before the window is only a baseline", n)
	}
	if n := len(r.Series["objects"]); n != 31 {
		t.Fatalf("gauge points = %d", n)
	}
	from := c.now().Add(-5 * time.Minute).Unix()
	for _, p := range r.Series["objects"] {
		if int64(p[0]) < from {
			t.Fatalf("point %v is before the window", p)
		}
	}
}

func TestRingNeverHoldsMoreThanAnHour(t *testing.T) {
	h, c := newTestHistory(Spec{Gauges: []string{"g"}})
	for i := 0; i < 400; i++ {
		h.Add(t0.Add(time.Duration(i)*Step), map[string]float64{"g": float64(i)})
	}
	if n := h.Len(); n != Capacity || Capacity != 360 {
		t.Fatalf("ring holds %d (capacity %d)", n, Capacity)
	}
	c.set(t0.Add(399 * Step))
	r := h.Query(60 * time.Minute)
	got := values(r.Series["g"])
	if got[0] != 40 || got[len(got)-1] != 399 {
		t.Fatalf("oldest %v newest %v", got[0], got[len(got)-1])
	}

	c.set(t0.Add(399*Step + 2*time.Hour))
	if r := h.Query(60 * time.Minute); r.Samples != 0 || len(r.Series["g"]) != 0 {
		t.Fatalf("stale samples served: %d", r.Samples)
	}
}

func TestGroupsKeepTopEightPlusOther(t *testing.T) {
	h, c := newTestHistory(Spec{
		Counters: []string{"calls."},
		Groups:   []Group{{Prefixes: []string{"calls."}}},
	})
	cur := map[string]float64{}
	for step := 0; step < 3; step++ {
		v := map[string]float64{}
		for i := 0; i < 11; i++ {
			name := fmt.Sprintf("calls.c%02d", i)
			cur[name] += float64((i + 1) * 10)
			v[name] = cur[name]
		}
		h.Add(t0.Add(time.Duration(step)*Step), v)
	}
	c.set(t0.Add(2 * Step))
	r := h.Query(5 * time.Minute)
	if len(r.Series) != 9 {
		t.Fatalf("series = %d, want 8 + other: %v", len(r.Series), keys(r.Series))
	}
	for i := 3; i < 11; i++ {
		if _, ok := r.Series[fmt.Sprintf("calls.c%02d", i)]; !ok {
			t.Fatalf("calls.c%02d should be in the top 8", i)
		}
	}
	// c00..c02 add 10+20+30 per step: 6/s.
	if got := values(r.Series["calls.other"]); len(got) != 2 || got[0] != 6 || got[1] != 6 {
		t.Fatalf("calls.other = %v", got)
	}
	if r.Latest["calls.other"] != 180 || r.Totals["calls.other"] != 120 {
		t.Fatalf("other latest %v totals %v", r.Latest["calls.other"], r.Totals["calls.other"])
	}
	if r.Kinds["calls.other"] != "rate" || r.Kinds["calls.c00"] != "" {
		t.Fatalf("kinds = %v", r.Kinds)
	}
}

func TestGroupSharesMembersAcrossPrefixes(t *testing.T) {
	h, c := newTestHistory(Spec{
		Gauges: []string{"ready.", "unacked.", "consumers."},
		Groups: []Group{{Prefixes: []string{"ready.", "unacked.", "consumers."}, Rank: []string{"ready.", "unacked."}}},
	})
	v := map[string]float64{}
	for i := 0; i < 10; i++ {
		q := fmt.Sprintf("q%d", i)
		v["ready."+q] = float64(i)
		v["unacked."+q] = 0
		v["consumers."+q] = float64(10 - i)
	}
	h.Add(t0, v)
	c.set(t0)
	r := h.Query(5 * time.Minute)
	for _, p := range []string{"ready.", "unacked.", "consumers."} {
		if _, ok := r.Series[p+"q9"]; !ok {
			t.Fatalf("%sq9 missing: %v", p, keys(r.Series))
		}
		if _, ok := r.Series[p+"q1"]; ok {
			t.Fatalf("%sq1 should be folded into other", p)
		}
		if _, ok := r.Series[p+"other"]; !ok {
			t.Fatalf("%sother missing", p)
		}
	}
	if r.Latest["consumers.other"] != 19 {
		t.Fatalf("consumers.other = %v, want q0+q1 = 10+9", r.Latest["consumers.other"])
	}
}

func TestSmallGroupHasNoOther(t *testing.T) {
	h, c := newTestHistory(Spec{Gauges: []string{"keys."}, Groups: []Group{{Prefixes: []string{"keys."}}}})
	h.Add(t0, map[string]float64{"keys.db0": 3, "keys.db1": 1})
	c.set(t0)
	r := h.Query(5 * time.Minute)
	if _, ok := r.Series["keys.other"]; ok || len(r.Series) != 2 {
		t.Fatalf("series = %v", keys(r.Series))
	}
}

func TestUndeclaredNamesAreNotServed(t *testing.T) {
	h, c := newTestHistory(Spec{Gauges: []string{"a"}})
	h.Add(t0, map[string]float64{"a": 1, "secret": 2})
	c.set(t0)
	r := h.Query(5 * time.Minute)
	if _, ok := r.Series["secret"]; ok {
		t.Fatal("undeclared series served")
	}
	if _, ok := r.Latest["secret"]; ok {
		t.Fatal("undeclared latest served")
	}
}

func TestLatestCounterNeverNegative(t *testing.T) {
	h, c := newTestHistory(Spec{Counters: []string{"hits"}})
	h.Add(t0, map[string]float64{"hits": -4})
	c.set(t0)
	if v := h.Query(5 * time.Minute).Latest["hits"]; v != 0 {
		t.Fatalf("latest = %v", v)
	}
}

type blockingSource struct {
	calls   atomic.Int32
	release chan struct{}
	entered chan struct{}
}

func (b *blockingSource) fetch(ctx context.Context) (map[string]float64, error) {
	b.calls.Add(1)
	b.entered <- struct{}{}
	<-b.release
	return map[string]float64{"g": 1}, nil
}

func TestCollectNeverRunsTwoSamplesAtOnce(t *testing.T) {
	h, _ := newTestHistory(Spec{Gauges: []string{"g"}})
	src := &blockingSource{release: make(chan struct{}), entered: make(chan struct{}, 1)}
	done := make(chan error, 1)
	go func() {
		_, err := h.Collect(context.Background(), src.fetch)
		done <- err
	}()
	<-src.entered
	if _, err := h.Collect(context.Background(), src.fetch); !errors.Is(err, ErrBusy) {
		t.Fatalf("second collect err = %v, want ErrBusy", err)
	}
	close(src.release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if n := src.calls.Load(); n != 1 {
		t.Fatalf("fetch ran %d times", n)
	}
	if h.Len() != 1 {
		t.Fatalf("samples = %d", h.Len())
	}
}

func TestCollectKeepsTheLastError(t *testing.T) {
	h, c := newTestHistory(Spec{Gauges: []string{"g"}})
	boom := errors.New("varnishstat failed")
	if _, err := h.Collect(context.Background(), func(context.Context) (map[string]float64, error) { return nil, boom }); err != boom {
		t.Fatal(err)
	}
	if r := h.Query(5 * time.Minute); r.Error != "varnishstat failed" {
		t.Fatalf("error = %q", r.Error)
	}
	if _, err := h.Collect(context.Background(), func(context.Context) (map[string]float64, error) { return nil, core.ErrAbsent }); err == nil {
		t.Fatal("absent is still returned")
	}
	if r := h.Query(5 * time.Minute); r.Error != "" {
		t.Fatalf("an absent service is not an error: %q", r.Error)
	}
	h.Collect(context.Background(), func(context.Context) (map[string]float64, error) { return map[string]float64{"g": 1}, nil })
	c.set(t0)
	if r := h.Query(5 * time.Minute); r.Error != "" || r.Samples != 1 {
		t.Fatalf("after success: error %q samples %d", r.Error, r.Samples)
	}
}

func TestConcurrentAddAndQuery(t *testing.T) {
	h := NewHistory(Spec{Counters: []string{"c"}, Gauges: []string{"g"}, Groups: []Group{{Prefixes: []string{"g"}}}})
	var wg sync.WaitGroup
	start := time.Now()
	wg.Go(func() {
		for i := 0; i < 500; i++ {
			h.Add(start.Add(time.Duration(i)*time.Second), map[string]float64{"c": float64(i), "g": 1})
		}
	})
	for k := 0; k < 4; k++ {
		wg.Go(func() {
			for i := 0; i < 100; i++ {
				_ = h.Query(60 * time.Minute)
			}
		})
	}
	wg.Wait()
}

type countingSource struct {
	n    atomic.Int32
	errs []error
}

func (s *countingSource) Metrics(ctx context.Context) (map[string]float64, error) {
	i := int(s.n.Add(1)) - 1
	if i < len(s.errs) {
		return nil, s.errs[i]
	}
	return map[string]float64{}, nil
}

func TestSamplerRunsUntilCancelledAndReportsRealErrors(t *testing.T) {
	boom := errors.New("boom")
	src := &countingSource{errs: []error{core.ErrAbsent, ErrBusy, boom}}
	var reported []error
	var mu sync.Mutex
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		NewSampler(5*time.Millisecond).Run(ctx, "varnish", src, time.Second, func(err error) {
			mu.Lock()
			reported = append(reported, err)
			mu.Unlock()
		})
		close(done)
	}()
	deadline := time.Now().Add(2 * time.Second)
	for src.n.Load() < 5 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Run did not stop on cancel")
	}
	mu.Lock()
	defer mu.Unlock()
	if len(reported) != 1 || reported[0] != boom {
		t.Fatalf("reported = %v, want only boom", reported)
	}
}

func TestParseWindow(t *testing.T) {
	for in, want := range map[string]time.Duration{"": DefaultWindow, "5m": 5 * time.Minute, "15m": 15 * time.Minute, "60m": time.Hour, "1h": time.Hour} {
		got, err := ParseWindow(in)
		if err != nil || got != want {
			t.Fatalf("ParseWindow(%q) = %v, %v", in, got, err)
		}
	}
	for _, bad := range []string{"10m", "2h", "abc", "-5m"} {
		if _, err := ParseWindow(bad); err == nil {
			t.Fatalf("ParseWindow(%q) accepted", bad)
		}
	}
}

func TestServeHTTP(t *testing.T) {
	h, c := newTestHistory(Spec{Gauges: []string{"g"}})
	h.Add(t0, map[string]float64{"g": 4})
	c.set(t0)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics?window=5m", nil))
	if rec.Code != http.StatusOK || rec.Header().Get("Content-Type") == "" {
		t.Fatalf("status %d", rec.Code)
	}
	var body Result
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err, rec.Body.String())
	}
	if body.Window != 300 || len(body.Series["g"]) != 1 {
		t.Fatalf("body = %s", rec.Body.String())
	}

	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics?window=7m", nil))
	var e map[string]string
	json.Unmarshal(rec.Body.Bytes(), &e)
	if rec.Code != http.StatusBadRequest || e["error"] == "" || e["hint"] == "" {
		t.Fatalf("bad window: %d %s", rec.Code, rec.Body.String())
	}

	var nilHistory *History
	rec = httptest.NewRecorder()
	nilHistory.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("nil history: %d", rec.Code)
	}
}

func keys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
