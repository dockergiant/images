package timeline

import (
	"strings"
	"testing"
	"time"

	"github.com/dockergiant/images/inspector/internal/apm"
	"github.com/dockergiant/images/inspector/internal/core"
)

func cacheTrace(id, xid string, start time.Time, d time.Duration, pid string, calls int) apm.Trace {
	tr := rootTrace(id, xid, start, d, map[string]string{"process_id": pid})
	for i := range calls {
		tr.Spans = append(tr.Spans, apm.Span{Name: "Redis.hGet", Component: "cache", Start: start.Add(time.Duration(10+i) * time.Millisecond), Duration: time.Millisecond})
	}
	return tr
}

func TestTraceSpansBeatMonitorForValkey(t *testing.T) {
	start := ts(1792528000)
	v := shortMiss("2000", "2001", start, 300*time.Millisecond)
	tl := newAt(mappedProc(t), start.Add(time.Second))
	at := start.Add(20 * time.Millisecond)
	tl.Ingest(event("redis.cmd", core.Redis, at, cmd(at, 37638, 0, "GET", "69d_A")))
	tl.Ingest(event("varnish.req", core.Varnish, start, v))
	tl.Ingest(event("trace", core.PHP, start, cacheTrace("00000000000000000000000000002001", "2001", start.Add(2*time.Millisecond), 290*time.Millisecond, "20", 3)))

	l := lane(t, only(t, tl), core.Redis)
	if l.Join != joinExact || l.Count != 3 || l.Source != SourceTrace {
		t.Fatalf("redis lane %+v, want the 3 trace spans as source %q", l, SourceTrace)
	}
}

func TestMonitorLaneNamesItsSource(t *testing.T) {
	start := ts(1792528100)
	v := shortMiss("2100", "2101", start, 300*time.Millisecond)
	tl := newAt(mappedProc(t), start.Add(time.Second))
	tl.SetCacheSource(SourceMonitor)
	at := start.Add(20 * time.Millisecond)
	tl.Ingest(event("redis.cmd", core.Redis, at, cmd(at, 37638, 0, "GET", "69d_A")))
	tl.Ingest(event("varnish.req", core.Varnish, start, v))

	if l := lane(t, only(t, tl), core.Redis); l.Source != SourceMonitor || l.Count != 1 {
		t.Fatalf("redis lane %+v, want source %q", l, SourceMonitor)
	}
}

func TestTraceSourceWithoutMonitor(t *testing.T) {
	start := ts(1792528200)

	t.Run("a trace without cache spans made no cache calls", func(t *testing.T) {
		tl := newAt(nil, start.Add(time.Second))
		tl.SetCacheSource(SourceTrace)
		tl.Ingest(event("trace", core.PHP, start, cacheTrace("00000000000000000000000000002201", "", start, 100*time.Millisecond, "20", 0)))
		l := lane(t, only(t, tl), core.Redis)
		if l.Join != joinExact || l.Count != 0 || l.Source != SourceTrace || !strings.Contains(l.Reason, "no cache spans") {
			t.Fatalf("redis lane %+v", l)
		}
	})

	t.Run("a request without its trace says why the lane is empty", func(t *testing.T) {
		tl := newAt(nil, start.Add(time.Second))
		tl.SetCacheSource(SourceTrace)
		tl.Ingest(event("varnish.req", core.Varnish, start, shortMiss("2300", "2301", start, 300*time.Millisecond)))
		l := lane(t, only(t, tl), core.Redis)
		if l.Join != joinNone || l.Source != SourceTrace || !strings.Contains(l.Reason, "MONITOR") {
			t.Fatalf("redis lane %+v", l)
		}
	})

	t.Run("no Valkey source gives no lane", func(t *testing.T) {
		tl := newAt(nil, start.Add(time.Second))
		tl.Ingest(event("trace", core.PHP, start, cacheTrace("00000000000000000000000000002401", "", start, 100*time.Millisecond, "20", 0)))
		if r := only(t, tl); hasLane(r, core.Redis) {
			t.Fatalf("redis lane without any source: %+v", lane(t, r, core.Redis))
		}
	})
}
