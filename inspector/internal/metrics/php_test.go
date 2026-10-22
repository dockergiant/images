package metrics

import (
	"encoding/json"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/dockergiant/images/inspector/internal/apm"
)

func web(at time.Time, route string, ms float64, sql int, times map[string]float64) apm.Summary {
	return apm.Summary{
		ID:       route + at.String(),
		Start:    at,
		Duration: time.Duration(ms * float64(time.Millisecond)),
		Method:   "GET",
		URL:      "https://app.test/" + route,
		Route:    route,
		Counts:   map[string]int{"sql": sql},
		Times:    times,
	}
}

func TestPHPPerMinuteSeries(t *testing.T) {
	// 12:04:30, so the 5 minute window is the buckets 12:00 to 12:04.
	now := time.Date(2026, 10, 21, 12, 4, 30, 0, time.UTC)
	m := func(min, sec int) time.Time { return time.Date(2026, 10, 21, 12, min, sec, 0, time.UTC) }
	list := []apm.Summary{
		web(m(0, 5), "a", 100, 10, nil),
		web(m(0, 50), "a", 300, 30, nil),
		web(m(0, 55), "a", 200, 20, nil),
		web(m(2, 0), "b", 900, 90, nil),
		web(m(4, 10), "b", 50, 5, nil),
		web(time.Date(2026, 10, 21, 11, 59, 59, 0, time.UTC), "old", 7000, 1, nil),
		{ID: "cli", Start: m(1, 0), Duration: 9 * time.Second, Route: "cron:run"},
	}
	r := PHP(list, 5*time.Minute, now)

	if r.Step != 60 || r.Window != 300 || r.Samples != 5 {
		t.Fatalf("step %d window %d samples %d", r.Step, r.Window, r.Samples)
	}
	req := r.Series["requests"]
	if len(req) != 5 {
		t.Fatalf("requests = %v", req)
	}
	if int64(req[0][0]) != m(0, 0).Unix() || int64(req[4][0]) != m(4, 0).Unix() {
		t.Fatalf("buckets start %v end %v", req[0][0], req[4][0])
	}
	if got := values(req); got[0] != 3 || got[1] != 0 || got[2] != 1 || got[3] != 0 || got[4] != 1 {
		t.Fatalf("requests per minute = %v (cli traces are not requests)", got)
	}
	p50 := r.Series["p50"]
	if p50[0][1] != 200 || !math.IsNaN(float64(p50[1][1])) || p50[2][1] != 900 {
		t.Fatalf("p50 = %v", p50)
	}
	if r.Series["p95"][0][1] != 300 || r.Series["max"][0][1] != 300 {
		t.Fatalf("p95 %v max %v", r.Series["p95"][0], r.Series["max"][0])
	}
	if r.Series["sql_p50"][0][1] != 20 || r.Series["sql_p95"][0][1] != 30 {
		t.Fatalf("sql p50 %v p95 %v", r.Series["sql_p50"][0], r.Series["sql_p95"][0])
	}
	if r.Totals["requests"] != 5 || r.Totals["max"] != 900 || r.Totals["p50"] != 200 {
		t.Fatalf("totals = %v", r.Totals)
	}
	if r.Latest["requests"] != 1 || r.Latest["p50"] != 50 {
		t.Fatalf("latest (last 60 s) = %v", r.Latest)
	}
	if r.Since == nil || int64(*r.Since) != time.Date(2026, 10, 21, 11, 59, 59, 0, time.UTC).Unix() {
		t.Fatalf("since = %v, the oldest web trace in the store", r.Since)
	}
	if r.Kinds["requests"] != "count" || r.Kinds["p95"] != "ms" || r.Kinds["sql_p95"] != "count" {
		t.Fatalf("kinds = %v", r.Kinds)
	}
}

func TestPHPHistogram(t *testing.T) {
	now := time.Date(2026, 10, 21, 12, 0, 30, 0, time.UTC)
	var list []apm.Summary
	for _, ms := range []float64{10, 50, 51, 150, 499, 999, 1500, 4000, 5000, 6000, 60000} {
		list = append(list, web(now.Add(-time.Second), "r", ms, 0, nil))
	}
	r := PHP(list, 15*time.Minute, now)
	if len(r.Histogram) != 8 {
		t.Fatalf("buckets = %d", len(r.Histogram))
	}
	want := []int{2, 1, 1, 1, 1, 1, 2, 2}
	for i, b := range r.Histogram {
		if b.Count != want[i] {
			t.Fatalf("bucket %d (%v-%v) = %d, want %d", i, b.From, b.To, b.Count, want[i])
		}
	}
	if r.Histogram[0].From != 0 || r.Histogram[0].To != 50 || r.Histogram[6].To != 5000 {
		t.Fatalf("bounds %+v", r.Histogram)
	}
	last := r.Histogram[7]
	if last.From != 5000 || !math.IsNaN(float64(last.To)) {
		t.Fatalf("last bucket %+v", last)
	}
	b, _ := json.Marshal(last)
	if !strings.Contains(string(b), `"to":null`) {
		t.Fatalf("open bucket json %s", b)
	}
}

func TestPHPRouteCost(t *testing.T) {
	now := time.Date(2026, 10, 21, 12, 0, 30, 0, time.UTC)
	cost := func(sql, php float64) map[string]float64 {
		return map[string]float64{"sql": sql, "cache": 0, "search": 0, "http": 0, "php": php}
	}
	list := []apm.Summary{
		web(now.Add(-10*time.Second), "cat", 400, 0, cost(100, 300)),
		web(now.Add(-20*time.Second), "cat", 600, 0, cost(300, 300)),
		web(now.Add(-30*time.Second), "home", 100, 0, cost(10, 90)),
	}
	for i := 0; i < 10; i++ {
		list = append(list, web(now.Add(-time.Second), "r"+string(rune('a'+i)), 1, 0, cost(0, 1)))
	}
	r := PHP(list, 5*time.Minute, now)
	if len(r.Routes) != 8 {
		t.Fatalf("routes = %d, want the top 8", len(r.Routes))
	}
	top := r.Routes[0]
	if top.Route != "cat" || top.Count != 2 || top.AvgMs != 500 || top.Times["sql"] != 200 || top.Times["php"] != 300 {
		t.Fatalf("top route %+v", top)
	}
	if r.Routes[1].Route != "home" {
		t.Fatalf("routes rank by total time: %+v", r.Routes[1])
	}
	for _, k := range []string{"sql", "cache", "search", "http", "php"} {
		if _, ok := r.Routes[1].Times[k]; !ok {
			t.Fatalf("times lacks %s", k)
		}
	}
}

func TestPHPEmpty(t *testing.T) {
	r := PHP(nil, 5*time.Minute, time.Date(2026, 10, 21, 12, 0, 30, 0, time.UTC))
	if r.Samples != 0 || r.Since != nil || len(r.Routes) != 0 || r.Routes == nil {
		t.Fatalf("empty = %+v", r)
	}
	if got := values(r.Series["requests"]); len(got) != 5 || got[0] != 0 {
		t.Fatalf("requests = %v, an idle shop has 0 per minute", got)
	}
	if len(r.Histogram) != 8 {
		t.Fatalf("histogram = %v", r.Histogram)
	}
	if _, err := json.Marshal(r); err != nil {
		t.Fatal(err)
	}
}
