package timeline

import (
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/dockergiant/images/inspector/internal/apm"
	"github.com/dockergiant/images/inspector/internal/core"
)

func bigTrace(start time.Time, n int) apm.Trace {
	tr := rootTrace("00000000000000000000000000002001", "", start, 2*time.Second, map[string]string{"process_id": "20"})
	comps := []string{"cache", "search", "queue", "sql"}
	seed := uint64(42)
	for i := range n {
		seed = seed*6364136223846793005 + 1442695040888963407
		off := time.Duration(seed>>33) % (2 * time.Second)
		d := time.Duration(seed>>13) % (3 * time.Millisecond)
		tr.Spans = append(tr.Spans, apm.Span{
			SpanID:    fmt.Sprintf("%x", i+2),
			ParentID:  "1",
			Name:      "span",
			Component: comps[i%len(comps)],
			Start:     start.Add(off),
			Duration:  d,
			Attrs:     map[string]string{"k": "v"},
		})
	}
	return tr
}

func expectLane(sc span, spans []apm.Span, comp string) (int, [][2]float64, time.Duration) {
	var pairs [][2]time.Time
	var total time.Duration
	for _, s := range spans {
		if component(s) == comp {
			pairs = append(pairs, [2]time.Time{s.Start, s.Start.Add(s.Duration)})
			total += s.Duration
		}
	}
	return len(pairs), coalesce(sc, pairs), total
}

func TestBigTraceLanesMatchSpans(t *testing.T) {
	start := ts(1792528000)
	tr := bigTrace(start, 10000)
	tl := newAt(nil, start.Add(3*time.Second))
	tl.Ingest(event("trace", core.PHP, start, tr))
	r := only(t, tl)

	sc := span{start, start.Add(tr.Duration)}
	for _, c := range []struct {
		kind core.Kind
		comp string
	}{{core.Redis, "cache"}, {core.Search, "search"}, {core.RabbitMQ, "queue"}} {
		n, pairs, total := expectLane(sc, tr.Spans, c.comp)
		l := lane(t, r, c.kind)
		if l.Count != n || !reflect.DeepEqual(l.Spans, pairs) {
			t.Errorf("%s lane count %d spans %v, want %d %v", c.kind, l.Count, l.Spans, n, pairs)
		}
		if c.comp != "queue" && l.Summary != fmt.Sprintf("%d calls · %s", n, fmtDur(total)) {
			t.Errorf("%s summary %q", c.kind, l.Summary)
		}
	}
	php := lane(t, r, core.PHP)
	if php.Count != len(tr.Spans) || php.Join != "exact" {
		t.Errorf("php lane %+v", php)
	}
	_, _, cacheTotal := expectLane(sc, tr.Spans, "cache")
	nSearch, _, searchTotal := expectLane(sc, tr.Spans, "search")
	nCache, _, _ := expectLane(sc, tr.Spans, "cache")
	want := []core.Card{
		{Label: "Cache calls", Tone: core.ToneInfo, Value: fmt.Sprintf("%d cache calls in the trace", nCache), Sub: fmtDur(cacheTotal) + " in cache calls"},
		{Label: "Search", Tone: core.ToneInfo, Value: fmt.Sprintf("%d search calls in the trace", nSearch), Sub: "took " + fmtDur(searchTotal)},
	}
	if !reflect.DeepEqual(r.Answers, want) {
		t.Errorf("answers %+v, want %+v", r.Answers, want)
	}
}

// The timeline holds up to maxRequests entries outside the trace store's byte
// budget, so an entry must not keep a trace's spans alive.
func TestEntriesHoldNoSpans(t *testing.T) {
	spanType := reflect.TypeOf(apm.Span{})
	var holds func(reflect.Type, map[reflect.Type]bool) bool
	holds = func(ty reflect.Type, seen map[reflect.Type]bool) bool {
		if ty == spanType {
			return true
		}
		if seen[ty] {
			return false
		}
		seen[ty] = true
		switch ty.Kind() {
		case reflect.Pointer, reflect.Slice, reflect.Array, reflect.Chan:
			return holds(ty.Elem(), seen)
		case reflect.Map:
			return holds(ty.Key(), seen) || holds(ty.Elem(), seen)
		case reflect.Struct:
			for i := range ty.NumField() {
				if holds(ty.Field(i).Type, seen) {
					return true
				}
			}
		}
		return false
	}
	for _, v := range []any{entry{}, parkedTrace{}} {
		if holds(reflect.TypeOf(v), map[reflect.Type]bool{}) {
			t.Errorf("%T can reach apm.Span", v)
		}
	}
}
