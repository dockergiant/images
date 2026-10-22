package metrics

import (
	"maps"
	"math"
	"sort"
	"time"

	"github.com/dockergiant/images/inspector/internal/apm"
)

const (
	phpStep   = time.Minute
	topRoutes = 8
)

var (
	histogramBounds = []float64{50, 100, 200, 500, 1000, 2000, 5000}
	costKeys        = []string{"sql", "cache", "search", "http", "php"}
	phpKinds        = map[string]string{"requests": "count", "p50": "ms", "p95": "ms", "max": "ms", "sql_p50": "count", "sql_p95": "count"}
)

type Bucket struct {
	From  Value `json:"from"`
	To    Value `json:"to"`
	Count int   `json:"count"`
}

type RouteCost struct {
	Route string             `json:"route"`
	Count int                `json:"count"`
	AvgMs float64            `json:"avgMs"`
	Times map[string]float64 `json:"times"`
}

// PHPResult is the trace store as charts: per-minute series plus the latency
// histogram and per-route cost, which have no time axis.
type PHPResult struct {
	Step      int                `json:"step"`
	Window    int                `json:"window"`
	Samples   int                `json:"samples"`
	Since     *Value             `json:"since"`
	Series    map[string][]Point `json:"series"`
	Latest    map[string]Value   `json:"latest"`
	Totals    map[string]Value   `json:"totals"`
	Kinds     map[string]string  `json:"kinds"`
	Histogram []Bucket           `json:"histogram"`
	Routes    []RouteCost        `json:"routes"`
}

type phpStats struct {
	ms  []float64
	sql []float64
}

func (s *phpStats) add(t apm.Summary) {
	s.ms = append(s.ms, float64(t.Duration)/float64(time.Millisecond))
	s.sql = append(s.sql, float64(t.Counts["sql"]))
}

func (s *phpStats) values() map[string]Value {
	sort.Float64s(s.ms)
	sort.Float64s(s.sql)
	out := map[string]Value{"requests": Value(len(s.ms))}
	for name, v := range map[string]float64{
		"p50":     nearestRank(s.ms, 0.5),
		"p95":     nearestRank(s.ms, 0.95),
		"max":     nearestRank(s.ms, 1),
		"sql_p50": nearestRank(s.sql, 0.5),
		"sql_p95": nearestRank(s.sql, 0.95),
	} {
		out[name] = Value(v)
	}
	return out
}

func nearestRank(sorted []float64, p float64) float64 {
	if len(sorted) == 0 {
		return math.NaN()
	}
	i := int(math.Ceil(p*float64(len(sorted)))) - 1
	return sorted[min(max(i, 0), len(sorted)-1)]
}

// PHP aggregates web requests only: CLI and cron traces carry no HTTP method,
// and a minutes-long cron run would swamp the request latency.
func PHP(list []apm.Summary, window time.Duration, now time.Time) PHPResult {
	r := PHPResult{
		Step:   int(phpStep / time.Second),
		Window: int(window / time.Second),
		Series: map[string][]Point{},
		Latest: map[string]Value{},
		Totals: map[string]Value{},
		Kinds:  maps.Clone(phpKinds),
		Routes: []RouteCost{},
	}
	end := now.Truncate(phpStep).Add(phpStep)
	from := end.Add(-window)
	buckets := int(window / phpStep)
	perMinute := make([]phpStats, buckets)
	var all, recent phpStats
	hist := make([]int, len(histogramBounds)+1)
	type routeAcc struct {
		count int
		ms    float64
		times map[string]float64
	}
	routes := map[string]*routeAcc{}

	for _, t := range list {
		if t.Method == "" {
			continue
		}
		if r.Since == nil || Value(t.Start.Unix()) < *r.Since {
			v := Value(t.Start.Unix())
			r.Since = &v
		}
		if t.Start.Before(from) || !t.Start.Before(end) {
			continue
		}
		r.Samples++
		all.add(t)
		perMinute[int(t.Start.Sub(from)/phpStep)].add(t)
		if !t.Start.Before(now.Add(-phpStep)) {
			recent.add(t)
		}
		ms := float64(t.Duration) / float64(time.Millisecond)
		b := sort.SearchFloat64s(histogramBounds, ms)
		hist[b]++
		acc := routes[t.Route]
		if acc == nil {
			acc = &routeAcc{times: map[string]float64{}}
			routes[t.Route] = acc
		}
		acc.count++
		acc.ms += ms
		for _, k := range costKeys {
			acc.times[k] += t.Times[k]
		}
	}

	for name := range phpKinds {
		r.Series[name] = make([]Point, 0, buckets)
	}
	for i := range perMinute {
		at := Value(from.Add(time.Duration(i) * phpStep).Unix())
		v := perMinute[i].values()
		for name := range phpKinds {
			r.Series[name] = append(r.Series[name], Point{at, v[name]})
		}
	}
	r.Totals = all.values()
	r.Latest = recent.values()

	lo := 0.0
	for i, n := range hist {
		hi := math.NaN()
		if i < len(histogramBounds) {
			hi = histogramBounds[i]
		}
		r.Histogram = append(r.Histogram, Bucket{From: Value(lo), To: Value(hi), Count: n})
		lo = hi
	}

	for route, acc := range routes {
		rc := RouteCost{Route: route, Count: acc.count, AvgMs: acc.ms / float64(acc.count), Times: map[string]float64{}}
		for _, k := range costKeys {
			rc.Times[k] = acc.times[k] / float64(acc.count)
		}
		r.Routes = append(r.Routes, rc)
	}
	sort.Slice(r.Routes, func(i, j int) bool {
		a, b := r.Routes[i], r.Routes[j]
		ta, tb := a.AvgMs*float64(a.Count), b.AvgMs*float64(b.Count)
		if ta != tb {
			return ta > tb
		}
		return a.Route < b.Route
	})
	if len(r.Routes) > topRoutes {
		r.Routes = r.Routes[:topRoutes]
	}
	return r
}
