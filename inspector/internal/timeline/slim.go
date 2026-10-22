package timeline

import (
	"sort"
	"time"

	"github.com/dockergiant/images/inspector/internal/apm"
)

// A Magento page trace holds thousands of spans, and the timeline keeps up to
// maxRequests entries outside the trace store's byte budget. An entry keeps
// only what its lanes and answers read, so evicted traces are really freed.
type traceInfo struct {
	ID, Method, URL, Route, VarnishXID string
	Start                              time.Time
	Duration                           time.Duration
	Status, SpanCount, PID             int
	Comp                               map[string]compInfo
}

// Ivals stay in absolute time because the scale changes when a varnish
// request adopts the trace.
type compInfo struct {
	Count int
	Total time.Duration
	Ivals [][2]time.Time
}

const maxIvals = 4 * maxSpans

var laneComps = map[string]bool{"cache": true, "search": true, "queue": true}

func slim(tr apm.Trace) *traceInfo {
	ti := &traceInfo{
		ID:         tr.ID,
		Method:     tr.Method,
		URL:        tr.URL,
		Route:      tr.Route,
		VarnishXID: tr.VarnishXID,
		Start:      tr.Start,
		Duration:   tr.Duration,
		Status:     tr.Status,
		SpanCount:  len(tr.Spans),
		PID:        tracePID(&tr),
	}
	for _, s := range tr.Spans {
		c := component(s)
		if !laneComps[c] {
			continue
		}
		if ti.Comp == nil {
			ti.Comp = map[string]compInfo{}
		}
		ci := ti.Comp[c]
		ci.Count++
		ci.Total += s.Duration
		ci.Ivals = append(ci.Ivals, [2]time.Time{s.Start, s.Start.Add(s.Duration)})
		ti.Comp[c] = ci
	}
	for c, ci := range ti.Comp {
		ci.Ivals = premerge(ci.Ivals, tr.Duration)
		ti.Comp[c] = ci
	}
	return ti
}

// premerge runs coalesce's doubling gap in absolute time, starting from the
// gap coalesce uses on the trace's own scale, so coalesce later picks the
// same intervals it would have from the raw spans.
func premerge(in [][2]time.Time, d time.Duration) [][2]time.Time {
	if len(in) <= maxIvals {
		return in
	}
	sort.Slice(in, func(a, b int) bool { return in[a][0].Before(in[b][0]) })
	gap := time.Duration(float64(d) * coalesceGap)
	for {
		out := mergeAbs(in, gap)
		if len(out) <= maxIvals {
			return out
		}
		gap = max(2*gap, time.Microsecond)
	}
}

func mergeAbs(in [][2]time.Time, gap time.Duration) [][2]time.Time {
	var out [][2]time.Time
	for _, p := range in {
		if n := len(out); n > 0 && p[0].Sub(out[n-1][1]) <= gap {
			if p[1].After(out[n-1][1]) {
				out[n-1][1] = p[1]
			}
			continue
		}
		out = append(out, p)
	}
	return out
}
