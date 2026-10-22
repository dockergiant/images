package apm

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestNearestRank(t *testing.T) {
	ten := []float64{10, 9, 8, 7, 6, 5, 4, 3, 2, 1}
	if p := nearestRank(ten, 0.5); p != 5 {
		t.Errorf("p50 of 1..10 = %v, want 5", p)
	}
	if p := nearestRank(ten, 0.95); p != 10 {
		t.Errorf("p95 of 1..10 = %v, want 10", p)
	}
	if p := nearestRank([]float64{7}, 0.95); p != 7 {
		t.Errorf("p95 of one = %v", p)
	}
	if p := nearestRank(nil, 0.5); p != 0 {
		t.Errorf("p50 of none = %v", p)
	}
	twenty := make([]float64, 20)
	for i := range twenty {
		twenty[i] = float64(i + 1)
	}
	if p := nearestRank(twenty, 0.95); p != 19 {
		t.Errorf("p95 of 1..20 = %v, want 19", p)
	}
}

func TestSummaryTimes(t *testing.T) {
	tr := pageTrace(t)
	s := Summarize(tr)
	var sum float64
	for _, k := range categoryKeys {
		v, ok := s.Times[k]
		if !ok {
			t.Errorf("times misses %s", k)
		}
		if v < 0 {
			t.Errorf("times[%s] = %v", k, v)
		}
		sum += v
	}
	if !near(sum, 1000) || len(s.Times) != len(categoryKeys) {
		t.Errorf("times = %v", s.Times)
	}
	body, _ := json.Marshal(s)
	if !strings.Contains(string(body), `"times":{"cache":`) {
		t.Errorf("summary JSON = %s", body)
	}

	// Children that overrun the root must not push php below zero.
	over := build(t,
		sp("r", "", "magento.request", "x", 0, 10, nil),
		sp("q", "r", "PDOStatement.execute", "SELECT 1", 0, 10, map[string]string{"db.system": "mysql"}),
		sp("o", "missing", "PDOStatement.execute", "SELECT 2", 0, 30, map[string]string{"db.system": "mysql"}),
	)
	if ts := Summarize(over).Times; ts["php"] != 0 || !near(ts["sql"], 40) {
		t.Errorf("overrun times = %v", ts)
	}
}

func TestStoreAnalysis(t *testing.T) {
	s := NewStore(500, 256<<20)
	tr := pageTrace(t)
	s.Add(tr)
	a, ok := s.Analysis(tr.ID)
	if !ok || a.ID != tr.ID || len(a.Hot) == 0 || a.Profile {
		t.Fatalf("analysis = %v %+v", ok, a)
	}
	if err := s.SetProfile(tr.ID, []byte(`{}`)); err != nil {
		t.Fatal(err)
	}
	if a, _ = s.Analysis(tr.ID); !a.Profile {
		t.Error("profile flag not updated after SetProfile")
	}
	if _, ok := s.Analysis("missing"); ok {
		t.Error("analysis for a missing trace")
	}
	list := s.List(ListFilter{})
	if len(list) != 1 || !near(list[0].Times["sql"], categoryOf(a, "sql").Ms) {
		t.Errorf("list summary times = %+v", list)
	}
}

func TestStoreCountsAnalysisBytes(t *testing.T) {
	s := NewStore(500, 256<<20)
	tr := pageTrace(t)
	s.Add(tr)
	e := s.items[tr.ID]
	if e.size <= traceSize(tr) || e.size != traceSize(tr)+analysisSize(e.analysis) {
		t.Errorf("entry size %d, trace alone %d", e.size, traceSize(tr))
	}
	if s.bytes != e.size {
		t.Errorf("store bytes %d, entry %d", s.bytes, e.size)
	}
}

func repeatTrace(n int, route string, d time.Duration, repeats int) Trace {
	var sql []string
	for i := 0; i < repeats; i++ {
		sql = append(sql, fmt.Sprintf("SELECT * FROM `catalog_eav_attribute` WHERE (attribute_id = %d)", i))
	}
	sql = append(sql, "SELECT `store`.* FROM `store`")
	return synth(n, d, route, sql...)
}

func TestHotspots(t *testing.T) {
	s := NewStore(500, 256<<20)
	for i := 1; i <= 10; i++ {
		s.Add(repeatTrace(i, "catalog/category/view", time.Duration(i)*100*time.Millisecond, 12))
	}
	s.Add(repeatTrace(11, "cms/index/index", 50*time.Millisecond, 3))
	h := s.Hotspots(0)

	if h.Traces != 11 || h.Since == nil || !h.Since.Equal(t0.Add(time.Second)) {
		t.Fatalf("traces %d since %v", h.Traces, h.Since)
	}
	if len(h.Routes) != 2 {
		t.Fatalf("routes = %+v", h.Routes)
	}
	cat := h.Routes[0]
	if cat.Route != "catalog/category/view" || cat.Count != 10 || cat.P50Ms != 500 || cat.P95Ms != 1000 || cat.MaxMs != 1000 || cat.WorstID != fmt.Sprintf("%032x", 10) {
		t.Errorf("category route = %+v", cat)
	}
	var sum float64
	for _, k := range categoryKeys {
		sum += cat.Times[k]
	}
	if !near(sum, 550) || !near(cat.Times["sql"], 13) {
		t.Errorf("route average times = %v", cat.Times)
	}

	if len(h.Operations) == 0 {
		t.Fatal("no operations")
	}
	op := h.Operations[0]
	if op.Kind != "sql" || op.Key != "SELECT * FROM `catalog_eav_attribute` WHERE (attribute_id = ?)" || op.Calls != 123 || op.Traces != 11 || !near(op.SelfMs, 123) {
		t.Errorf("top operation = %+v", op)
	}
	if !near(op.Share, 123/5550.0) || op.WorstID == "" || op.WorstSpanID == "" || !strings.Contains(op.Example, "attribute_id = ") {
		t.Errorf("operation share/worst = %+v", op)
	}
	for i := 1; i < len(h.Operations); i++ {
		if h.Operations[i].SelfMs > h.Operations[i-1].SelfMs {
			t.Errorf("operations not ranked by self time: %+v", h.Operations)
		}
	}

	if len(h.Repeats) != 1 {
		t.Fatalf("repeats = %+v", h.Repeats)
	}
	r := h.Repeats[0]
	if r.Kind != "sql" || r.Traces != 10 || r.PerRequest != 12 || !near(r.MsPerRequest, 12) || len(r.Routes) != 1 || r.Routes[0] != "catalog/category/view" || r.WorstID == "" {
		t.Errorf("repeat = %+v", r)
	}

	if h := s.Hotspots(1); len(h.Routes) != 1 || len(h.Operations) != 1 || len(h.Repeats) != 1 {
		t.Errorf("limit 1 = %+v", h)
	}
}

func TestHotspotsEmpty(t *testing.T) {
	h := NewStore(0, 0).Hotspots(0)
	body, _ := json.Marshal(h)
	if string(body) != `{"traces":0,"since":null,"routes":[],"operations":[],"repeats":[]}` {
		t.Errorf("empty hotspots = %s", body)
	}
}

func TestHotspotsRecomputeAtMostOncePerSecond(t *testing.T) {
	s := NewStore(500, 256<<20)
	now := t0
	s.now = func() time.Time { return now }
	s.Add(synth(1, time.Second, "a"))
	if h := s.Hotspots(0); h.Traces != 1 {
		t.Fatalf("traces = %d", h.Traces)
	}
	s.Add(synth(2, time.Second, "a"))
	now = now.Add(999 * time.Millisecond)
	if h := s.Hotspots(0); h.Traces != 1 {
		t.Errorf("recomputed within a second: %d", h.Traces)
	}
	now = now.Add(time.Millisecond)
	if h := s.Hotspots(0); h.Traces != 2 {
		t.Errorf("not recomputed after a second: %d", h.Traces)
	}
}

func TestHotspotsDropEvictedTraces(t *testing.T) {
	s := NewStore(2, 256<<20)
	now := t0
	s.now = func() time.Time { return now }
	for i := 1; i <= 3; i++ {
		s.Add(synth(i, time.Second, fmt.Sprintf("r%d", i)))
	}
	h := s.Hotspots(0)
	if h.Traces != 2 || len(h.Routes) != 2 {
		t.Errorf("hotspots after eviction = %+v", h)
	}
	for _, r := range h.Routes {
		if r.Route == "r1" {
			t.Error("evicted trace still in hotspots")
		}
	}
}

func TestHotspotsConcurrentWithAdd(t *testing.T) {
	s := NewStore(20, 256<<20)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 1; i <= 200; i++ {
			s.Add(repeatTrace(i, "r", time.Duration(i)*time.Millisecond, 11))
		}
	}()
	for {
		select {
		case <-done:
			s.now = func() time.Time { return time.Now().Add(time.Hour) }
			if h := s.Hotspots(0); h.Traces != 20 {
				t.Errorf("traces = %d", h.Traces)
			}
			return
		default:
			s.Hotspots(5)
			s.Analysis(fmt.Sprintf("%032x", 1))
		}
	}
}
