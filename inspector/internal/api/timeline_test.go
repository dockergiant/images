package api

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/dockergiant/images/inspector/internal/core"
	"github.com/dockergiant/images/inspector/internal/timeline"
)

func timelineServer(t *testing.T, feeds []Feed, adapters ...core.Adapter) *Server {
	t.Helper()
	return New(Config{
		Env:      "shop",
		Domain:   testDomain,
		Secret:   testSecret,
		Docker:   fakeDocker("127.0.0.1"),
		Adapters: adapters,
		Feeds:    feeds,
		Timeline: timeline.New(nil, nil),
		UI:       fstest.MapFS{"index.html": {Data: []byte("x")}},
		Log:      quietLog(),
	})
}

func openStream(t *testing.T, ctx context.Context, url, query string) *http.Response {
	t.Helper()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url+"/api/stream?"+query, nil)
	req.Host = testHost
	req.Header.Set(gateHeader, testSecret)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if res.StatusCode != http.StatusOK {
		t.Fatalf("stream %s: status %d", query, res.StatusCode)
	}
	return res
}

func waitEvent(t *testing.T, res *http.Response, typ string, cancel context.CancelFunc) json.RawMessage {
	t.Helper()
	deadline := time.AfterFunc(3*time.Second, cancel)
	defer deadline.Stop()
	sc := bufio.NewScanner(res.Body)
	seen := false
	for sc.Scan() {
		line := sc.Text()
		if line == "event: "+typ {
			seen = true
			continue
		}
		if seen && strings.HasPrefix(line, "data: ") {
			return json.RawMessage(line[6:])
		}
	}
	t.Fatalf("no %s event arrived", typ)
	return nil
}

func TestTimelineStreamFeedsSourcesAndStops(t *testing.T) {
	varnishSpec, varnishProbe := probeStream("log", false)
	redisSpec, redisProbe := probeStream("monitor", false)
	nginxSpec, nginxProbe := probeStream("access", false)
	searchSpec, searchProbe := probeStream("insights", false)
	varnish := &fakeAdapter{kind: core.Varnish, present: true, streams: []core.StreamSpec{varnishSpec}}
	redis := &fakeAdapter{kind: core.Redis, present: false, streams: []core.StreamSpec{redisSpec}}
	search := &fakeAdapter{kind: core.Search, present: true, streams: []core.StreamSpec{searchSpec}}
	s := timelineServer(t, []Feed{{Kind: "nginx", Spec: nginxSpec}}, varnish, redis, search)
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	res := openStream(t, ctx, ts.URL, "sub=timeline")
	defer res.Body.Close()

	for name, p := range map[string]*streamProbe{"varnish": varnishProbe, "nginx": nginxProbe} {
		select {
		case <-p.started:
		case <-time.After(2 * time.Second):
			t.Fatalf("%s source never started", name)
		}
	}

	var ev struct {
		Sub  string           `json:"sub"`
		Data timeline.Request `json:"data"`
	}
	if err := json.Unmarshal(waitEvent(t, res, "timeline", cancel), &ev); err != nil {
		t.Fatal(err)
	}
	if ev.Sub != "timeline" || ev.Data.Key != "v:x" || ev.Data.URL != "/p" {
		t.Fatalf("timeline event %+v", ev)
	}
	if got := s.cfg.Timeline.Recent(0); len(got) == 0 {
		t.Fatal("varnish events were not ingested")
	}

	select {
	case <-redisProbe.started:
		t.Fatal("MONITOR started for an absent redis")
	case <-searchProbe.started:
		t.Fatal("insights started without the insights feature")
	default:
	}

	cancel()
	for name, p := range map[string]*streamProbe{"varnish": varnishProbe, "nginx": nginxProbe} {
		select {
		case <-p.stopped:
		case <-time.After(3 * time.Second):
			t.Fatalf("%s source kept running after the last viewer left", name)
		}
	}
	waitFor(t, func() bool { return len(s.cfg.Hub.Active()) == 0 })
}

func TestTimelineSharesSourceWithDrawer(t *testing.T) {
	spec, probe := probeStream("log", false)
	varnish := &fakeAdapter{kind: core.Varnish, present: true, streams: []core.StreamSpec{spec}}
	s := timelineServer(t, nil, varnish)
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	ctx1, cancel1 := context.WithCancel(context.Background())
	defer cancel1()
	r1 := openStream(t, ctx1, ts.URL, "sub=timeline")
	defer r1.Body.Close()
	<-probe.started

	ctx2, cancel2 := context.WithCancel(context.Background())
	defer cancel2()
	r2 := openStream(t, ctx2, ts.URL, "sub=varnish:log")
	defer r2.Body.Close()
	waitEvent(t, r2, "varnish.req", cancel2)
	if n := len(probe.started); n != 0 {
		t.Fatalf("the drawer started %d extra varnishlog producers", n)
	}

	cancel1()
	time.Sleep(50 * time.Millisecond)
	select {
	case <-probe.stopped:
		t.Fatal("varnishlog stopped while the drawer still watched it")
	default:
	}
	cancel2()
	select {
	case <-probe.stopped:
	case <-time.After(3 * time.Second):
		t.Fatal("varnishlog kept running")
	}
}

func TestTimelineEventsNeedTheTimelineSub(t *testing.T) {
	spec, probe := probeStream("log", false)
	varnish := &fakeAdapter{kind: core.Varnish, present: true, streams: []core.StreamSpec{spec}}
	s := timelineServer(t, []Feed{{Kind: "nginx", Spec: core.StreamSpec{Name: "access"}}}, varnish)
	if _, err := s.resolveSub("nginx:access"); err != nil {
		t.Fatalf("feed not resolvable: %v", err)
	}
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	res := openStream(t, ctx, ts.URL, "sub=varnish:log")
	defer res.Body.Close()
	<-probe.started
	waitEvent(t, res, "varnish.req", cancel)
	if got := s.cfg.Timeline.Recent(0); len(got) != 0 {
		t.Fatalf("a drawer alone fed the timeline: %d requests", len(got))
	}
}

func TestTimelineOnlyViewerSkipsSourceEvents(t *testing.T) {
	done := make(chan struct{})
	spec := core.StreamSpec{Name: "monitor", Title: "monitor", Start: func(ctx context.Context, f core.Filter, emit core.Emit) error {
		for i := 0; i < 5000; i++ {
			emit(core.Event{Type: "redis.cmd", Data: core.RedisCmd{Cmd: "GET", Args: []string{"k"}}})
		}
		close(done)
		<-ctx.Done()
		return ctx.Err()
	}}
	redis := &fakeAdapter{kind: core.Redis, present: true, streams: []core.StreamSpec{spec}}
	s := timelineServer(t, nil, redis)
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	res := openStream(t, ctx, ts.URL, "sub=timeline")
	defer res.Body.Close()
	gaps := make(chan bool, 1)
	go func() {
		sc := bufio.NewScanner(res.Body)
		gap := false
		for sc.Scan() {
			gap = gap || sc.Text() == "event: gap"
		}
		gaps <- gap
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("redis source never finished")
	}
	time.Sleep(3 * batchEvery)
	cancel()
	if <-gaps {
		t.Fatal("the timeline viewer got a gap from redis traffic it never asked for")
	}
	s.ring.mu.Lock()
	defer s.ring.mu.Unlock()
	for _, e := range s.ring.items {
		if e.key == "redis:monitor" {
			t.Fatal("redis events went into the ring with nobody watching them")
		}
	}
}

func TestDrawerReconnectGetsMissedEvents(t *testing.T) {
	spec, probe := probeStream("log", false)
	varnish := &fakeAdapter{kind: core.Varnish, present: true, streams: []core.StreamSpec{spec}}
	s := timelineServer(t, nil, varnish)
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	ctx1, cancel1 := context.WithCancel(context.Background())
	defer cancel1()
	r1 := openStream(t, ctx1, ts.URL, "sub=timeline")
	defer r1.Body.Close()
	<-probe.started

	ctx2, cancel2 := context.WithCancel(context.Background())
	r2 := openStream(t, ctx2, ts.URL, "sub=varnish:log")
	deadline := time.AfterFunc(3*time.Second, cancel2)
	sc := bufio.NewScanner(r2.Body)
	var seen uint64
	for seen == 0 && sc.Scan() {
		if v, ok := strings.CutPrefix(sc.Text(), "id: "); ok {
			seen, _ = strconv.ParseUint(v, 10, 64)
		}
	}
	deadline.Stop()
	cancel2()
	r2.Body.Close()
	if seen == 0 {
		t.Fatal("the drawer got no event id")
	}
	left := time.Now()
	time.Sleep(400 * time.Millisecond)

	ctx3, cancel3 := context.WithCancel(context.Background())
	defer cancel3()
	req, _ := http.NewRequestWithContext(ctx3, http.MethodGet, ts.URL+"/api/stream?sub=varnish:log", nil)
	req.Host = testHost
	req.Header.Set(gateHeader, testSecret)
	req.Header.Set("Last-Event-ID", strconv.FormatUint(seen, 10))
	r3, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer r3.Body.Close()
	deadline = time.AfterFunc(3*time.Second, cancel3)
	defer deadline.Stop()
	sc = bufio.NewScanner(r3.Body)
	for sc.Scan() {
		data, ok := strings.CutPrefix(sc.Text(), "data: ")
		if !ok {
			continue
		}
		var ev struct {
			Type string    `json:"type"`
			Time time.Time `json:"time"`
		}
		if json.Unmarshal([]byte(data), &ev) == nil && ev.Type == "varnish.req" && ev.Time.After(left.Add(200*time.Millisecond)) && ev.Time.Before(left.Add(400*time.Millisecond)) {
			return
		}
	}
	t.Fatal("the reconnect did not replay events from while the drawer was away")
}
