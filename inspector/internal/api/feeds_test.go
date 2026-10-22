package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"
	"testing/fstest"
	"time"

	"github.com/dockergiant/images/inspector/internal/core"
	"github.com/dockergiant/images/inspector/internal/docker"
	"github.com/dockergiant/images/inspector/internal/perf"
	"github.com/dockergiant/images/inspector/internal/timeline"
)

type feedRig struct {
	s       *Server
	ts      *httptest.Server
	box     *phpBox
	varnish *streamProbe
	redis   *streamProbe
}

func newFeedRig(t *testing.T, tracing bool) *feedRig {
	t.Helper()
	varnishSpec, varnishProbe := probeStream("log", false)
	redisSpec, redisProbe := probeStream("monitor", false)
	box := &phpBox{on: tracing}
	d := fakeDocker("127.0.0.1")
	d.ContainerList = []docker.Container{{ID: "fpm", Service: "php-fpm", Running: true}}
	d.ContainerEnvs = map[string][]string{"fpm": {"ROLL_INSPECTOR_TRACE=1"}}
	d.ExecFunc = box.exec
	s := New(Config{
		Env:    "shop",
		Domain: testDomain,
		Secret: testSecret,
		Docker: d,
		Perf:   perf.New(perf.Options{Docker: d, Env: "shop", Domain: testDomain}),
		Adapters: []core.Adapter{
			&fakeAdapter{kind: core.Varnish, present: true, streams: []core.StreamSpec{varnishSpec}},
			&fakeAdapter{kind: core.Redis, present: true, streams: []core.StreamSpec{redisSpec}},
		},
		Timeline: timeline.New(nil, nil),
		UI:       fstest.MapFS{"index.html": {Data: []byte("x")}},
		Log:      quietLog(),
	})
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	return &feedRig{s: s, ts: ts, box: box, varnish: varnishProbe, redis: redisProbe}
}

func started(t *testing.T, p *streamProbe, what string) {
	t.Helper()
	select {
	case <-p.started:
	case <-time.After(2 * time.Second):
		t.Fatalf("%s never started", what)
	}
}

func notStarted(t *testing.T, p *streamProbe, what string) {
	t.Helper()
	select {
	case <-p.started:
		t.Fatalf("%s started", what)
	case <-time.After(150 * time.Millisecond):
	}
}

func stopped(t *testing.T, p *streamProbe, what string) {
	t.Helper()
	select {
	case <-p.stopped:
	case <-time.After(3 * time.Second):
		t.Fatalf("%s kept running", what)
	}
}

func (r *feedRig) v1(t *testing.T, path string) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, r.ts.URL+path, nil)
	req.Host = testHost
	req.Header.Set(gateHeader, testSecret)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
}

func holders(s *Server, key string) []string {
	for _, src := range s.liveSources() {
		if src.Key == key {
			return src.Holders
		}
	}
	return nil
}

func TestTimelineLeavesMonitorOffWhileTracing(t *testing.T) {
	r := newFeedRig(t, true)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	res := openStream(t, ctx, r.ts.URL, "sub=timeline")
	defer res.Body.Close()

	started(t, r.varnish, "varnishlog")
	notStarted(t, r.redis, "MONITOR")
	if got := r.s.cfg.Timeline.CacheSource(); got != timeline.SourceTrace {
		t.Fatalf("cache source %q, want %q", got, timeline.SourceTrace)
	}
}

func TestTimelineRunsMonitorWithTracingOff(t *testing.T) {
	r := newFeedRig(t, false)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	res := openStream(t, ctx, r.ts.URL, "sub=timeline")
	defer res.Body.Close()

	started(t, r.redis, "MONITOR")
	if got := r.s.cfg.Timeline.CacheSource(); got != timeline.SourceMonitor {
		t.Fatalf("cache source %q, want %q", got, timeline.SourceMonitor)
	}
	if h := holders(r.s, "redis:monitor"); !slices.Contains(h, "timeline") {
		t.Fatalf("redis:monitor holders %v", h)
	}
}

func TestTimelineDropsMonitorOnceTracingIsOn(t *testing.T) {
	r := newFeedRig(t, false)
	r.s.recheck = 50 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	res := openStream(t, ctx, r.ts.URL, "sub=timeline")
	defer res.Body.Close()
	started(t, r.redis, "MONITOR")

	on := true
	if _, err := r.s.cfg.Perf.Controller().Apply(context.Background(), "php-fpm", perf.Change{Tracing: &on}); err != nil {
		t.Fatal(err)
	}
	stopped(t, r.redis, "MONITOR after tracing went on")
	waitFor(t, func() bool { return r.s.cfg.Timeline.CacheSource() == timeline.SourceTrace })
	select {
	case <-r.varnish.stopped:
		t.Fatal("varnishlog stopped with MONITOR")
	default:
	}
}

func TestV1CallLeasesVarnishLog(t *testing.T) {
	r := newFeedRig(t, false)
	r.s.leaseFor = 300 * time.Millisecond
	r.v1(t, "/api/v1/requests")

	started(t, r.varnish, "varnishlog for the lease")
	notStarted(t, r.redis, "MONITOR for the lease")
	waitFor(t, func() bool { return len(r.s.cfg.Timeline.Recent(0)) > 0 })
	if h := holders(r.s, "varnish:log"); !slices.Equal(h, []string{"api lease"}) {
		t.Fatalf("varnish:log holders %v, want [api lease]", h)
	}

	// Each call moves the end of the lease.
	for range 4 {
		time.Sleep(100 * time.Millisecond)
		r.v1(t, "/api/v1/page?url=https://app.shop.test/p")
	}
	select {
	case <-r.varnish.stopped:
		t.Fatal("the lease ended while calls kept coming")
	default:
	}
	stopped(t, r.varnish, "varnishlog after the lease ran out")
	waitFor(t, func() bool { return len(r.s.cfg.Hub.Active()) == 0 })
}

func TestLeaseJoinsTheOverviewSource(t *testing.T) {
	r := newFeedRig(t, true)
	r.s.leaseFor = 200 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	res := openStream(t, ctx, r.ts.URL, "sub=timeline")
	defer res.Body.Close()
	started(t, r.varnish, "varnishlog")

	r.v1(t, "/api/v1/requests")
	waitFor(t, func() bool {
		h := holders(r.s, "varnish:log")
		return slices.Contains(h, "timeline") && slices.Contains(h, "api lease")
	})
	notStarted(t, r.varnish, "a second varnishlog")

	cancel()
	time.Sleep(400 * time.Millisecond)
	stopped(t, r.varnish, "varnishlog once the viewer and the lease are gone")
}

func TestLeaseRespectsAStoppedSource(t *testing.T) {
	r := newFeedRig(t, false)
	r.s.wantMu.Lock()
	r.s.stopped["varnish:log"] = time.Now()
	r.s.wantMu.Unlock()
	r.v1(t, "/api/v1/requests")
	notStarted(t, r.varnish, "varnishlog stopped on the Performance page")
}
