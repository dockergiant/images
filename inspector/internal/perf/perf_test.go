package perf

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/dockergiant/images/inspector/internal/apm"
	"github.com/dockergiant/images/inspector/internal/docker"
	"github.com/dockergiant/images/inspector/internal/metrics"
)

func newTestPerf(t *testing.T) (*Perf, *fakePHP, *apm.Store) {
	t.Helper()
	c, php, d := newTestController(t)
	d.SelfStatsFunc = func(context.Context) (docker.Stats, error) {
		return docker.Stats{Read: time.Now(), MemBytes: 50 << 20, MemLimit: 768 << 20}, nil
	}
	store := apm.NewStore(0, 0)
	p := New(Options{
		Docker: d, Env: "shop", Domain: "shop.test", Store: store,
		Sampler: metrics.NewSampler(30 * time.Second), SampleDefault: 10 * time.Second,
		Bench: BenchConfig{Base: "http://127.0.0.1:1"},
	})
	p.ctl = c
	p.Bench.ctl = c
	return p, php, store
}

func TestSnapshot(t *testing.T) {
	p, php, store := newTestPerf(t)
	php.set("fpm", docker.IniState{Script: true, DetailSupport: true, Template: true, Loaded: true, Settings: true})
	store.Add(apm.Trace{ID: fmt.Sprintf("%032x", 1), Method: "GET", Status: 200, Route: "cms/index/index", URL: "https://app.shop.test/", Duration: 100 * time.Millisecond, Spans: []apm.Span{{Name: "a"}}})
	store.Add(apm.Trace{ID: fmt.Sprintf("%032x", 2), Method: "GET", Status: 200, Route: "catalog/category/view", URL: "https://app.shop.test/women.html", Duration: 900 * time.Millisecond, Spans: []apm.Span{{Name: "a"}}})
	store.Add(apm.Trace{ID: fmt.Sprintf("%032x", 3), Method: "POST", Status: 200, Route: "checkout/cart/add", URL: "https://app.shop.test/checkout/cart/add", Duration: 5 * time.Second, Spans: []apm.Span{{Name: "a"}}})

	s := p.Snapshot(context.Background())
	if len(s.PHP) != 2 || !s.PHP[0].Tracing || s.Summary.Label != "Tracing · full" {
		t.Fatalf("php %+v summary %+v", s.PHP, s.Summary)
	}
	if s.Sampler.Interval != 30 || s.Sampler.Paused || s.Sampler.Default != 10 || s.Sampler.Permanent != "ROLL_INSPECTOR_SAMPLE=30" {
		t.Fatalf("sampler %+v", s.Sampler)
	}
	if len(s.Sampler.Options) != 4 {
		t.Fatalf("options %+v", s.Sampler.Options)
	}
	if s.Inspector.MemBytes != 50<<20 || s.Intake.Traces != 3 || s.Load.CPUs == 0 {
		t.Fatalf("inspector %+v intake %+v load %+v", s.Inspector, s.Intake, s.Load)
	}
	if s.DefaultURL != "https://app.shop.test/women.html" {
		t.Fatalf("default url %q: the slowest GET page that answered 200", s.DefaultURL)
	}
	if s.Benchmark.Running || s.Benchmark.Arms == nil {
		t.Fatalf("benchmark %+v", s.Benchmark)
	}
}

func TestSetSample(t *testing.T) {
	p, _, _ := newTestPerf(t)
	if err := p.SetSample("off"); err != nil {
		t.Fatal(err)
	}
	s := p.Snapshot(context.Background())
	if !s.Sampler.Paused || s.Sampler.Interval != 0 || s.Sampler.Permanent != "ROLL_INSPECTOR_SAMPLE=off" {
		t.Fatalf("paused %+v", s.Sampler)
	}
	if err := p.SetSample("15"); err == nil {
		t.Fatal("15 accepted")
	}
	if err := p.SetSample("60"); err != nil || p.Sampler.Interval() != time.Minute {
		t.Fatal(err)
	}
}
