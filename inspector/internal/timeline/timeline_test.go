package timeline

import (
	"context"
	"encoding/json"
	"math"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/dockergiant/images/inspector/internal/apm"
	"github.com/dockergiant/images/inspector/internal/core"
	"github.com/dockergiant/images/inspector/internal/docker"
	"github.com/dockergiant/images/inspector/internal/procmap"
)

func ts(f float64) time.Time {
	sec, frac := math.Modf(f)
	return time.Unix(int64(sec), int64(math.Round(frac*1e6))*1000).UTC()
}

const fpmIP = "192.168.156.10"

const tcpHeader = "T   sl  local_address rem_address   st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode"

func tcpRow(local, remote, inode string) string {
	return "T    0: " + local + " " + remote + " 01 00000000:00000000 00:00000000 00000000    33        0 " + inode + " 1 0000000000000000 20 4 30 10 -1"
}

// Two FPM workers in shop's php-fpm (192.168.156.10), each holding one
// connection to valkey (192.168.156.5:6379): PID 20 on port 37638, PID 21 on 37652.
var fpmWalk = strings.Join([]string{
	tcpHeader,
	tcpRow("0A9CA8C0:9306", "059CA8C0:18EB", "201"),
	tcpRow("0A9CA8C0:9314", "059CA8C0:18EB", "202"),
	"P 1 php-fpm: master process (/usr/local/etc/php-fpm.conf) ",
	"P 20 php-fpm: pool www ",
	"F 20 201",
	"P 21 php-fpm: pool www ",
	"F 21 202",
}, "\n") + "\n"

func mappedProc(t *testing.T) *procmap.Map {
	t.Helper()
	fake := &docker.Fake{
		EnvName: "shop",
		ContainerList: []docker.Container{
			{ID: "fpm", Service: "php-fpm", Running: true, IP: fpmIP},
			{ID: "valkey", Service: "redis", Running: true, IP: "192.168.156.5"},
		},
		ExecFunc: func(ctx context.Context, spec docker.ExecSpec) (docker.ExecResult, error) {
			return docker.ExecResult{Stdout: []byte(fpmWalk)}, nil
		},
	}
	m := procmap.New(fake)
	if err := m.Refresh(context.Background()); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	if p, ok := m.Lookup(fpmIP, 37638); !ok || p.PID != 20 {
		t.Fatalf("walk fixture does not map %s:37638 to PID 20: %+v %v", fpmIP, p, ok)
	}
	return m
}

// Client request 7 and backend request 8 from testdata/varnish/vsl-8.0-admin.txt.
func missReq() core.VarnishReq {
	return core.VarnishReq{
		VXID:       "7",
		BereqVXID:  "8",
		Method:     "GET",
		URL:        "/backend/admin/dashboard/index/key/REDACTED/",
		Host:       "app.shop.test",
		Status:     200,
		Handling:   "MISS",
		Start:      ts(1792526556.494798),
		TTFB:       29029408 * time.Microsecond,
		Duration:   29091115 * time.Microsecond,
		BereqStart: ts(1792526556.495530),
		BereqEnd:   ts(1792526585.450347),
		Reason:     "not in cache yet",
	}
}

func shortMiss(vxid, bereq string, start time.Time, d time.Duration) core.VarnishReq {
	return core.VarnishReq{
		VXID:       vxid,
		BereqVXID:  bereq,
		Method:     "GET",
		URL:        "/women.html",
		Host:       "app.shop.test",
		Status:     200,
		Handling:   "MISS",
		Start:      start,
		Duration:   d,
		BereqStart: start.Add(time.Millisecond),
		BereqEnd:   start.Add(d - time.Millisecond),
		Reason:     "not in cache yet",
	}
}

func cmd(at time.Time, port, db int, name string, args ...string) core.RedisCmd {
	return core.RedisCmd{
		Time: at,
		DB:   db,
		Addr: fpmIP + ":" + strconv.Itoa(port),
		IP:   fpmIP,
		Port: port,
		Cmd:  name,
		Args: args,
	}
}

func rootTrace(id, xid string, start time.Time, d time.Duration, attrs map[string]string) apm.Trace {
	return apm.Trace{
		ID:         id,
		Start:      start,
		Duration:   d,
		Method:     "GET",
		URL:        "https://app.shop.test/backend/admin/dashboard/index/key/REDACTED/",
		Route:      "adminhtml_dashboard_index",
		Status:     200,
		VarnishXID: xid,
		Spans: []apm.Span{{
			TraceID:   id,
			SpanID:    "1",
			Name:      "magento.request",
			Component: "magento",
			Start:     start,
			Duration:  d,
			Attrs:     attrs,
		}},
	}
}

func event(typ string, kind core.Kind, at time.Time, data any) core.Event {
	return core.Event{Type: typ, Time: at, Kind: kind, Data: data}
}

func newAt(proc *procmap.Map, now time.Time) *Timeline {
	tl := New(proc, nil)
	tl.now = func() time.Time { return now }
	return tl
}

func lane(t *testing.T, r Request, kind core.Kind) Lane {
	t.Helper()
	for _, l := range r.Lanes {
		if l.Kind == kind {
			return l
		}
	}
	t.Fatalf("request %s has no %s lane: %+v", r.Key, kind, r.Lanes)
	return Lane{}
}

func hasLane(r Request, kind core.Kind) bool {
	for _, l := range r.Lanes {
		if l.Kind == kind {
			return true
		}
	}
	return false
}

func only(t *testing.T, tl *Timeline) Request {
	t.Helper()
	rs := tl.Recent(0)
	if len(rs) != 1 {
		t.Fatalf("want 1 request, got %d: %+v", len(rs), rs)
	}
	return rs[0]
}

func TestExactLanes(t *testing.T) {
	v := missReq()
	tl := newAt(mappedProc(t), v.BereqEnd.Add(time.Second))

	tl.Ingest(event("redis.cmd", core.Redis, v.BereqStart, cmd(v.BereqStart.Add(2*time.Millisecond), 37638, 0, "HGET", "zc:k:69d_GLOBAL_PLUGIN_LIST", "d")))
	tl.Ingest(event("redis.cmd", core.Redis, v.BereqStart, cmd(v.BereqStart.Add(5*time.Millisecond), 37638, 1, "HGET", "zc:k:69d_FPC_KEY", "d")))
	tl.Ingest(event("varnish.req", core.Varnish, v.Start, v))
	tl.Ingest(event("trace", core.PHP, v.BereqStart, rootTrace("0000000000000000000000000000abcd", "8", v.BereqStart.Add(time.Millisecond), 28*time.Second, map[string]string{"process_id": "20"})))

	r := only(t, tl)
	if r.Key != "v:7" || r.Source != "varnish" || r.VXID != "7" || r.TraceID != "0000000000000000000000000000abcd" {
		t.Fatalf("request fields: %+v", r)
	}
	for _, k := range []core.Kind{core.Varnish, core.PHP, core.Redis} {
		if l := lane(t, r, k); l.Join != "exact" {
			t.Errorf("%s lane join %q, want exact (%s)", k, l.Join, l.Reason)
		}
	}
	php := lane(t, r, core.PHP)
	if !strings.Contains(php.Reason, "varnish.xid 8") {
		t.Errorf("php reason %q", php.Reason)
	}
	rl := lane(t, r, core.Redis)
	if rl.Count != 2 || !strings.Contains(rl.Reason, "PID 20") {
		t.Errorf("redis lane %+v", rl)
	}
}

func TestExactRedisWithoutTraceWhenOneWorkerBusy(t *testing.T) {
	start := ts(1792526600)
	v := shortMiss("100", "101", start, 300*time.Millisecond)
	tl := newAt(mappedProc(t), start.Add(time.Second))
	tl.Ingest(event("redis.cmd", core.Redis, start, cmd(start.Add(20*time.Millisecond), 37638, 0, "GET", "69d_CONFIG")))
	tl.Ingest(event("varnish.req", core.Varnish, start, v))

	r := only(t, tl)
	if l := lane(t, r, core.Redis); l.Join != "exact" {
		t.Errorf("redis lane %+v", l)
	}
	if l := lane(t, r, core.PHP); l.Join != "probable" || !strings.Contains(l.Reason, "PID 20") {
		t.Errorf("php lane %+v", l)
	}
}

func TestProbableWithoutPIDMap(t *testing.T) {
	v := missReq()
	tl := newAt(nil, v.BereqEnd.Add(time.Second))

	tl.Ingest(event("redis.cmd", core.Redis, v.BereqStart, cmd(v.BereqStart.Add(2*time.Millisecond), 37638, 0, "HGET", "zc:k:69d_GLOBAL_PLUGIN_LIST", "d")))
	tl.Ingest(event("redis.cmd", core.Redis, v.BereqStart, cmd(v.BereqStart.Add(5*time.Millisecond), 37638, 1, "HGET", "zc:k:69d_FPC_KEY", "d")))
	tl.Ingest(event("varnish.req", core.Varnish, v.Start, v))
	tl.Ingest(event("trace", core.PHP, v.BereqStart, rootTrace("0000000000000000000000000000abcd", "8", v.BereqStart.Add(time.Millisecond), 28*time.Second, nil)))

	r := only(t, tl)
	if l := lane(t, r, core.PHP); l.Join != "exact" {
		t.Errorf("php lane %+v", l)
	}
	rl := lane(t, r, core.Redis)
	if rl.Join != "probable" || rl.Count != 2 || rl.Reason == "" {
		t.Errorf("redis lane %+v", rl)
	}
}

func TestOverlappingWorkersGiveNone(t *testing.T) {
	start := ts(1792526600)
	a := shortMiss("100", "101", start, 400*time.Millisecond)
	b := shortMiss("200", "201", start.Add(100*time.Millisecond), 400*time.Millisecond)

	t.Run("unmapped", func(t *testing.T) {
		tl := newAt(nil, start.Add(2*time.Second))
		tl.Ingest(event("redis.cmd", core.Redis, start, cmd(start.Add(150*time.Millisecond), 40000, 0, "GET", "69d_A")))
		tl.Ingest(event("redis.cmd", core.Redis, start, cmd(start.Add(160*time.Millisecond), 40002, 0, "GET", "69d_B")))
		tl.Ingest(event("varnish.req", core.Varnish, a.Start, a))
		tl.Ingest(event("varnish.req", core.Varnish, b.Start, b))
		for _, r := range tl.Recent(0) {
			l := lane(t, r, core.Redis)
			if l.Join != "none" || !strings.Contains(l.Reason, "overlap") {
				t.Errorf("%s redis lane %+v", r.Key, l)
			}
		}
	})

	t.Run("two mapped workers", func(t *testing.T) {
		tl := newAt(mappedProc(t), start.Add(2*time.Second))
		tl.Ingest(event("redis.cmd", core.Redis, start, cmd(start.Add(150*time.Millisecond), 37638, 0, "GET", "69d_A")))
		tl.Ingest(event("redis.cmd", core.Redis, start, cmd(start.Add(160*time.Millisecond), 37652, 0, "GET", "69d_B")))
		tl.Ingest(event("varnish.req", core.Varnish, a.Start, a))
		tl.Ingest(event("varnish.req", core.Varnish, b.Start, b))
		for _, r := range tl.Recent(0) {
			if l := lane(t, r, core.Redis); l.Join != "none" {
				t.Errorf("%s redis lane %+v", r.Key, l)
			}
			if l := lane(t, r, core.PHP); l.Join != "none" {
				t.Errorf("%s php lane %+v", r.Key, l)
			}
		}
	})
}

func TestHitIsPoint(t *testing.T) {
	start := ts(1792526700)
	hit := core.VarnishReq{VXID: "300", Method: "GET", URL: "/", Status: 200, Handling: "HIT", Start: start, Duration: 0}
	tl := newAt(nil, start.Add(time.Second))
	tl.Ingest(event("redis.cmd", core.Redis, start, cmd(start, 40000, 0, "GET", "69d_A")))
	tl.Ingest(event("varnish.req", core.Varnish, start, hit))

	r := only(t, tl)
	if len(r.Lanes) != 1 || r.Lanes[0].Kind != core.Varnish {
		t.Fatalf("HIT lanes %+v", r.Lanes)
	}
	sp := r.Lanes[0].Spans
	if len(sp) != 1 || sp[0][0] != sp[0][1] {
		t.Fatalf("HIT spans %v, want one point", sp)
	}
	if _, err := json.Marshal(r); err != nil {
		t.Fatalf("marshal: %v", err)
	}
}

func TestGraceHitShowsBackgroundFetch(t *testing.T) {
	start := ts(1792526700)
	g := core.VarnishReq{
		VXID: "310", BereqVXID: "311", Method: "GET", URL: "/", Status: 200, Handling: "HIT", Grace: true,
		Start: start, Duration: time.Millisecond,
		BereqStart: start.Add(time.Millisecond), BereqEnd: start.Add(200 * time.Millisecond),
	}
	tl := newAt(nil, start.Add(time.Second))
	tl.Ingest(event("varnish.req", core.Varnish, start, g))
	r := only(t, tl)
	vl := lane(t, r, core.Varnish)
	if len(vl.Spans) != 2 {
		t.Fatalf("grace varnish spans %v, want hit point plus fetch", vl.Spans)
	}
	if !hasLane(r, core.PHP) {
		t.Fatalf("grace hit has no backend lane: %+v", r.Lanes)
	}
}

func TestNoSourceGivesNoRequests(t *testing.T) {
	at := ts(1792526800)
	tl := newAt(nil, at.Add(time.Second))
	tl.Ingest(event("redis.cmd", core.Redis, at, cmd(at, 40000, 0, "GET", "69d_A")))
	tl.Ingest(event("search.query", core.Search, at, core.SearchQuery{Time: at, Took: 3 * time.Millisecond, Index: "magento2_product_1_v1", Source: "insights"}))
	tl.Ingest(event("mq.pub", core.RabbitMQ, at, core.MQPub{Time: at, Exchange: "magento", RoutingKey: "async.operations.all", Source: "delta", Count: 1}))
	tl.Ingest(event("stream.error", core.Redis, at, "boom"))
	if rs := tl.Recent(10); len(rs) != 0 {
		t.Fatalf("want no requests, got %+v", rs)
	}
}

// The PURGE is request 65541 from testdata/varnish/vsl-8.0-cases.txt.
func purgeReq(at time.Time) core.VarnishReq {
	return core.VarnishReq{
		VXID: "65541", Method: "PURGE", URL: "/", Host: "app.fixture.test", Status: 200, Handling: "SYNTH", Start: at,
		Raw: []string{
			"*              << Request  >>   65541     ",
			"-        65541 ReqMethod      c PURGE",
			"-        65541 ReqURL         c /",
			"-        65541 ReqHeader      c X-Magento-Tags-Pattern: ((^|,)cat_c_41(,|$))",
			"-        65541 RespReason     c Purged",
		},
	}
}

func TestAnswerForTagBan(t *testing.T) {
	purgeAt := ts(1792528082.182390)
	missAt := purgeAt.Add(2*time.Minute + 5*time.Second)
	v := shortMiss("400", "401", missAt, 250*time.Millisecond)
	v.URL = "/gear/bags.html"
	v.Tags = []string{"cat_c", "cat_c_41", "cat_p_12", "store"}

	tl := newAt(nil, missAt.Add(time.Second))
	tl.Ingest(event("varnish.req", core.Varnish, purgeAt, purgeReq(purgeAt)))
	tl.Ingest(event("varnish.req", core.Varnish, missAt, v))

	r := only(t, tl)
	if len(r.Answers) == 0 {
		t.Fatalf("no answers")
	}
	a := r.Answers[0]
	if a.Label != "Why a MISS?" || a.Value != "Tag cat_c_41 was purged 2 min ago" {
		t.Fatalf("answer %+v", a)
	}
}

func TestAnswerForOldBanFallsBackToReason(t *testing.T) {
	purgeAt := ts(1792528082.182390)
	missAt := purgeAt.Add(11 * time.Minute)
	v := shortMiss("400", "401", missAt, 250*time.Millisecond)
	v.Tags = []string{"cat_c_41"}
	tl := New(nil, nil)
	tl.now = func() time.Time { return purgeAt }
	tl.Ingest(event("varnish.req", core.Varnish, purgeAt, purgeReq(purgeAt)))
	tl.now = func() time.Time { return missAt.Add(time.Second) }
	tl.Ingest(event("varnish.req", core.Varnish, missAt, v))
	r := only(t, tl)
	if len(r.Answers) == 0 || r.Answers[0].Value != "Not in cache yet" {
		t.Fatalf("answers %+v", r.Answers)
	}
}

func TestCacheAndSearchAnswers(t *testing.T) {
	start := ts(1792526900)
	v := shortMiss("500", "501", start, 400*time.Millisecond)
	tl := newAt(mappedProc(t), start.Add(time.Second))
	at := start.Add(10 * time.Millisecond)
	tl.Ingest(event("redis.cmd", core.Redis, at, cmd(at, 37638, 0, "HGET", "zc:k:69d_CONFIG", "d")))
	tl.Ingest(event("redis.cmd", core.Redis, at, cmd(at.Add(time.Millisecond), 37638, 0, "HGET", "zc:k:69d_LAYOUT", "d")))
	tl.Ingest(event("redis.cmd", core.Redis, at, cmd(at.Add(2*time.Millisecond), 37638, 1, "HGET", "zc:k:69d_PAGE", "d")))
	save := cmd(at.Add(300*time.Millisecond), 37638, 1, "HSET", "zc:k:69d_PAGE", "d", strings.Repeat("x", 2048), "t", "cat_c_41")
	save.Folded = 3
	tl.Ingest(event("redis.cmd", core.Redis, at, save))
	q := core.SearchQuery{
		Time: start.Add(120 * time.Millisecond), Start: start.Add(100 * time.Millisecond), Took: 14 * time.Millisecond,
		Index: "magento2_product_1_v3", Label: "Products · default", Summary: "match \"bag\" · 12 hits", Source: "slowlog",
	}
	tl.Ingest(event("search.query", core.Search, q.Time, q))
	tl.Ingest(event("varnish.req", core.Varnish, start, v))

	r := only(t, tl)
	if l := lane(t, r, core.Search); l.Join != "probable" || l.Count != 1 {
		t.Errorf("search lane %+v", l)
	}
	cards := map[string]core.Card{}
	for _, c := range r.Answers {
		cards[c.Label] = c
	}
	cache, ok := cards["Cache calls"]
	if !ok {
		t.Fatalf("no cache card: %+v", r.Answers)
	}
	if cache.Value != "db 0: 2 reads · db 1: 1 read" || cache.Sub != "3 writes · ≥2.0 KB" {
		t.Errorf("cache card %+v", cache)
	}
	search, ok := cards["Search"]
	if !ok {
		t.Fatalf("no search card: %+v", r.Answers)
	}
	if search.Value != "1 query · Products · default" || !strings.Contains(search.Sub, "14 ms") || !strings.Contains(search.Sub, "12 hits") {
		t.Errorf("search card %+v", search)
	}
	if len(r.Answers) > 3 {
		t.Errorf("more than three answers: %+v", r.Answers)
	}
}

func TestTraceBeforeVarnish(t *testing.T) {
	start := ts(1792527000)

	t.Run("varnish active parks the trace", func(t *testing.T) {
		tl := newAt(nil, start.Add(time.Second))
		earlier := shortMiss("90", "91", start.Add(-5*time.Second), 100*time.Millisecond)
		tl.Ingest(event("varnish.req", core.Varnish, earlier.Start, earlier))
		v := shortMiss("600", "601", start, 300*time.Millisecond)
		tl.Ingest(event("trace", core.PHP, start, rootTrace("00000000000000000000000000000601", "601", start.Add(2*time.Millisecond), 290*time.Millisecond, nil)))
		if n := len(tl.Recent(0)); n != 1 {
			t.Fatalf("parked trace became its own request: %d requests", n)
		}
		tl.Ingest(event("varnish.req", core.Varnish, start, v))
		r, ok := tl.Get("v:600")
		if !ok || r.TraceID != "00000000000000000000000000000601" || lane(t, r, core.PHP).Join != "exact" {
			t.Fatalf("trace not joined: %+v", r)
		}
	})

	t.Run("no varnish yet merges the trace request", func(t *testing.T) {
		tl := newAt(nil, start.Add(time.Second))
		tl.Ingest(event("trace", core.PHP, start, rootTrace("00000000000000000000000000000701", "701", start.Add(2*time.Millisecond), 290*time.Millisecond, nil)))
		r := only(t, tl)
		if r.Source != "trace" || r.Key != "t:00000000000000000000000000000701" {
			t.Fatalf("trace request %+v", r)
		}
		tl.Ingest(event("varnish.req", core.Varnish, start, shortMiss("700", "701", start, 300*time.Millisecond)))
		r = only(t, tl)
		if r.Key != "v:700" || r.TraceID == "" {
			t.Fatalf("merged request %+v", r)
		}
		if old, ok := tl.Get("t:00000000000000000000000000000701"); !ok || old.Key != "v:700" {
			t.Fatalf("old key does not resolve to the merged request: %+v %v", old, ok)
		}
	})

	t.Run("parked trace without varnish request is shown later", func(t *testing.T) {
		now := start.Add(time.Second)
		tl := New(nil, nil)
		tl.now = func() time.Time { return now }
		earlier := shortMiss("90", "91", start.Add(-5*time.Second), 100*time.Millisecond)
		tl.Ingest(event("varnish.req", core.Varnish, earlier.Start, earlier))
		tl.Ingest(event("trace", core.PHP, start, rootTrace("00000000000000000000000000000801", "801", start, 100*time.Millisecond, nil)))
		now = now.Add(10 * time.Second)
		tl.Ingest(event("redis.cmd", core.Redis, now, cmd(now, 40000, 0, "GET", "69d_A")))
		if _, ok := tl.Get("t:00000000000000000000000000000801"); !ok {
			t.Fatalf("expired parked trace was dropped")
		}
	})
}

func TestTraceSpansGiveExactLanes(t *testing.T) {
	start := ts(1792527100)
	tr := rootTrace("00000000000000000000000000000901", "", start, 200*time.Millisecond, nil)
	tr.Spans = append(tr.Spans,
		apm.Span{Name: "Redis.hGet", Component: "cache", Start: start.Add(10 * time.Millisecond), Duration: time.Millisecond},
		apm.Span{Name: "curl_exec", Component: "search", Start: start.Add(50 * time.Millisecond), Duration: 12 * time.Millisecond},
		apm.Span{Name: "PhpAmqpLib\\Channel\\AMQPChannel.basic_publish", Component: "queue", Start: start.Add(90 * time.Millisecond), Duration: 2 * time.Millisecond},
	)
	tl := newAt(nil, start.Add(time.Second))
	tl.Ingest(event("trace", core.PHP, start, &tr))
	r := only(t, tl)
	if r.Source != "trace" || r.URL == "" {
		t.Fatalf("trace request %+v", r)
	}
	for _, k := range []core.Kind{core.PHP, core.Redis, core.Search, core.RabbitMQ} {
		if l := lane(t, r, k); l.Join != "exact" || l.Count == 0 {
			t.Errorf("%s lane %+v", k, l)
		}
	}
	if hasLane(r, core.Varnish) {
		t.Errorf("trace request has a varnish lane")
	}
}

func TestNginxAccessFallback(t *testing.T) {
	at := ts(1792527200)
	tl := newAt(nil, at.Add(time.Second))
	tl.Ingest(event("redis.cmd", core.Redis, at, cmd(at.Add(-200*time.Millisecond), 40000, 0, "GET", "69d_A")))
	tl.Ingest(event("nginx.access", "", at, core.NginxAccess{Time: at, Method: "GET", URL: "/women.html", Status: 200}))
	r := only(t, tl)
	if r.Source != "nginx" || r.URL != "/women.html" {
		t.Fatalf("access request %+v", r)
	}
	for _, l := range r.Lanes {
		if l.Join == "exact" {
			t.Errorf("access request has an exact lane: %+v", l)
		}
	}
	if l := lane(t, r, core.Redis); l.Join != "probable" {
		t.Errorf("redis lane %+v", l)
	}

	tl.Ingest(event("varnish.req", core.Varnish, at, shortMiss("1000", "1001", at, 50*time.Millisecond)))
	tl.Ingest(event("nginx.access", "", at, core.NginxAccess{Time: at.Add(time.Millisecond), Method: "GET", URL: "/men.html", Status: 200}))
	for _, r := range tl.Recent(0) {
		if r.URL == "/men.html" {
			t.Errorf("access line became a request while Varnish is active")
		}
	}
}

// After 30 s without Varnish or trace events the access line of the next MISS
// arrives first; the varnish request or trace that follows takes its place.
func TestAccessLineBeforeSourceIsAbsorbed(t *testing.T) {
	start := ts(1792527250)

	t.Run("varnish", func(t *testing.T) {
		v := shortMiss("1200", "1201", start, 300*time.Millisecond)
		tl := newAt(nil, start.Add(time.Second))
		ch, cancel := tl.Subscribe()
		defer cancel()
		tl.Ingest(event("nginx.access", "", v.BereqEnd, core.NginxAccess{Time: v.BereqEnd.Add(2 * time.Millisecond), Method: "GET", URL: "/women.html", Status: 200}))
		old := only(t, tl)
		tl.Ingest(event("varnish.req", core.Varnish, start, v))
		r := only(t, tl)
		if r.Key != "v:1200" || r.Source != "varnish" {
			t.Fatalf("merged request %+v", r)
		}
		var published Request
		for len(ch) > 0 {
			if p := <-ch; p.Key == "v:1200" {
				published = p
			}
		}
		if len(published.Replaces) != 1 || published.Replaces[0] != old.Key {
			t.Fatalf("published varnish request replaces %v, want [%s]", published.Replaces, old.Key)
		}
		if got, ok := tl.Get(old.Key); !ok || got.Key != "v:1200" {
			t.Fatalf("access key %s does not resolve to the varnish request: %+v %v", old.Key, got, ok)
		}
	})

	t.Run("trace", func(t *testing.T) {
		tr := rootTrace("00000000000000000000000000001301", "", start, 200*time.Millisecond, nil)
		tr.URL = "http://app.shop.test/women.html"
		tl := newAt(nil, start.Add(time.Second))
		tl.Ingest(event("nginx.access", "", start, core.NginxAccess{Time: start.Add(205 * time.Millisecond), Method: "GET", URL: "/women.html", Status: 200}))
		tl.Ingest(event("trace", core.PHP, start, tr))
		if r := only(t, tl); r.Source != "trace" || r.TraceID != tr.ID {
			t.Fatalf("merged request %+v", r)
		}
	})

	t.Run("head miss fetched as get", func(t *testing.T) {
		v := shortMiss("1500", "1501", start, 300*time.Millisecond)
		v.Method = "HEAD"
		tl := newAt(nil, start.Add(time.Second))
		tl.Ingest(event("nginx.access", "", v.BereqEnd, core.NginxAccess{Time: v.BereqEnd, Method: "GET", URL: "/women.html", Status: 200}))
		tl.Ingest(event("varnish.req", core.Varnish, start, v))
		if r := only(t, tl); r.Key != "v:1500" {
			t.Fatalf("merged request %+v", r)
		}
	})

	t.Run("trace adopted by varnish", func(t *testing.T) {
		tr := rootTrace("00000000000000000000000000001601", "1601", start.Add(time.Millisecond), 200*time.Millisecond, nil)
		tl := newAt(nil, start.Add(time.Second))
		ch, cancel := tl.Subscribe()
		defer cancel()
		tl.Ingest(event("trace", core.PHP, start, tr))
		tl.Ingest(event("varnish.req", core.Varnish, start, shortMiss("1600", "1601", start, 300*time.Millisecond)))
		var published Request
		for len(ch) > 0 {
			if p := <-ch; p.Key == "v:1600" {
				published = p
			}
		}
		if len(published.Replaces) != 1 || published.Replaces[0] != "t:"+tr.ID {
			t.Fatalf("published varnish request replaces %v", published.Replaces)
		}
	})

	t.Run("access absorbed by a trace that varnish adopts", func(t *testing.T) {
		tr := rootTrace("00000000000000000000000000001701", "1701", start.Add(time.Millisecond), 200*time.Millisecond, nil)
		tr.URL = "http://app.shop.test/women.html"
		tl := newAt(nil, start.Add(time.Second))
		ch, cancel := tl.Subscribe()
		defer cancel()
		tl.Ingest(event("nginx.access", "", start, core.NginxAccess{Time: start.Add(205 * time.Millisecond), Method: "GET", URL: "/women.html", Status: 200}))
		access := only(t, tl)
		tl.Ingest(event("trace", core.PHP, start, tr))
		if r := only(t, tl); r.Key != "t:"+tr.ID {
			t.Fatalf("trace did not absorb the access line: %+v", r)
		}
		tl.Ingest(event("varnish.req", core.Varnish, start, shortMiss("1700", "1701", start, 300*time.Millisecond)))
		var published Request
		for len(ch) > 0 {
			if p := <-ch; p.Key == "v:1700" {
				published = p
			}
		}
		if !slices.Contains(published.Replaces, "t:"+tr.ID) || !slices.Contains(published.Replaces, access.Key) {
			t.Fatalf("published varnish request replaces %v, want the trace and %s", published.Replaces, access.Key)
		}
		if got, ok := tl.Get(access.Key); !ok || got.Key != "v:1700" {
			t.Fatalf("access key %s does not resolve to the varnish request: %+v %v", access.Key, got, ok)
		}
	})

	t.Run("other url stays", func(t *testing.T) {
		v := shortMiss("1400", "1401", start, 300*time.Millisecond)
		tl := newAt(nil, start.Add(time.Second))
		tl.Ingest(event("nginx.access", "", v.BereqEnd, core.NginxAccess{Time: v.BereqEnd, Method: "GET", URL: "/men.html", Status: 200}))
		tl.Ingest(event("varnish.req", core.Varnish, start, v))
		if n := len(tl.Recent(0)); n != 2 {
			t.Fatalf("want the unrelated access line kept, got %d requests", n)
		}
	})
}

func TestSpansStayInRange(t *testing.T) {
	start := ts(1792527300)
	v := shortMiss("1100", "1101", start, 500*time.Millisecond)
	tl := newAt(nil, start.Add(time.Second))
	for i := range 3000 {
		at := start.Add(time.Duration(i) * 150 * time.Microsecond)
		tl.Ingest(event("redis.cmd", core.Redis, at, cmd(at, 40000, 0, "GET", "69d_A")))
	}
	tl.Ingest(event("redis.cmd", core.Redis, start, cmd(start.Add(-40*time.Millisecond), 40000, 0, "GET", "69d_EARLY")))
	tl.Ingest(event("varnish.req", core.Varnish, start, v))
	r := only(t, tl)
	for _, l := range r.Lanes {
		if len(l.Spans) > maxSpans {
			t.Errorf("%s lane has %d spans", l.Kind, len(l.Spans))
		}
		for _, s := range l.Spans {
			if s[0] < 0 || s[1] > 1 || s[0] > s[1] || math.IsNaN(s[0]) || math.IsNaN(s[1]) {
				t.Errorf("%s span %v out of range", l.Kind, s)
			}
		}
	}
	if _, err := json.Marshal(r); err != nil {
		t.Fatalf("marshal: %v", err)
	}
}

func TestSideEventsExpire(t *testing.T) {
	start := ts(1792527400)
	now := start
	tl := New(nil, nil)
	tl.now = func() time.Time { return now }
	tl.Ingest(event("redis.cmd", core.Redis, start, cmd(start.Add(10*time.Millisecond), 40000, 0, "GET", "69d_A")))
	now = start.Add(45 * time.Second)
	tl.Ingest(event("varnish.req", core.Varnish, start, shortMiss("1200", "1201", start, 300*time.Millisecond)))
	r := only(t, tl)
	if hasLane(r, core.Redis) {
		t.Errorf("redis lane built from an expired command: %+v", lane(t, r, core.Redis))
	}
}

func TestSubscribeAndLateEvents(t *testing.T) {
	start := ts(1792527500)
	tl := newAt(nil, start.Add(time.Second))
	ch, cancel := tl.Subscribe()
	defer cancel()

	tl.Ingest(event("varnish.req", core.Varnish, start, shortMiss("1300", "1301", start, 300*time.Millisecond)))
	select {
	case r := <-ch:
		if r.Key != "v:1300" {
			t.Fatalf("got %+v", r)
		}
	case <-time.After(time.Second):
		t.Fatal("no request on subscribe channel")
	}

	at := start.Add(5 * time.Second)
	tl.Ingest(event("mq.pub", core.RabbitMQ, at, core.MQPub{Time: start.Add(3 * time.Second), Exchange: "magento", RoutingKey: "product_action_attribute.update", Queues: []string{"product_action_attribute.update"}, Source: "delta", Count: 2}))
	select {
	case r := <-ch:
		l := lane(t, r, core.RabbitMQ)
		if l.Join != "probable" || l.Count != 2 {
			t.Fatalf("mq lane %+v", l)
		}
	case <-time.After(time.Second):
		t.Fatal("late publish did not update the request")
	}

	cancel()
	if _, open := <-ch; open {
		t.Fatal("channel stays open after cancel")
	}
}

func TestRecentOrderAndBound(t *testing.T) {
	start := ts(1792527600)
	tl := newAt(nil, start.Add(time.Hour))
	for i := range maxRequests + 20 {
		at := start.Add(time.Duration(i) * time.Second)
		tl.Ingest(event("varnish.req", core.Varnish, at, core.VarnishReq{VXID: strconv.Itoa(i + 1), Method: "GET", URL: "/", Handling: "HIT", Start: at}))
	}
	rs := tl.Recent(0)
	if len(rs) != maxRequests {
		t.Fatalf("kept %d requests, want %d", len(rs), maxRequests)
	}
	if rs[0].VXID != strconv.Itoa(maxRequests+20) {
		t.Fatalf("newest first: got %s", rs[0].VXID)
	}
	if _, ok := tl.Get("v:1"); ok {
		t.Fatalf("oldest request was not evicted")
	}
	if got := tl.Recent(3); len(got) != 3 {
		t.Fatalf("Recent(3) gave %d", len(got))
	}
}

func TestStaticAndSequentialRequestsDoNotOverlap(t *testing.T) {
	start := ts(1792526586.976204)
	page := shortMiss("98300", "98301", start, 300*time.Millisecond)
	font := shortMiss("98312", "98313", start.Add(10*time.Millisecond), 68*time.Millisecond)
	font.Handling = "PASS"
	font.URL = "/static/version1792525861/adminhtml/MageOS/m137-admin-theme/en_US/fonts/inter/regular/inter-400.woff2"
	next := shortMiss("98320", "98321", page.BereqEnd.Add(20*time.Millisecond), 100*time.Millisecond)

	tl := newAt(nil, start.Add(2*time.Second))
	tl.Ingest(event("redis.cmd", core.Redis, start, cmd(start.Add(50*time.Millisecond), 40000, 0, "GET", "69d_A")))
	tl.Ingest(event("varnish.req", core.Varnish, font.Start, font))
	tl.Ingest(event("varnish.req", core.Varnish, page.Start, page))
	tl.Ingest(event("varnish.req", core.Varnish, next.Start, next))

	r, ok := tl.Get("v:98300")
	if !ok {
		t.Fatal("page request missing")
	}
	if l := lane(t, r, core.Redis); l.Join != "probable" {
		t.Fatalf("redis lane %+v", l)
	}
}
