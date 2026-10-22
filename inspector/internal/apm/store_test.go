package apm

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

var t0 = time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)

func synth(n int, d time.Duration, route string, sql ...string) Trace {
	id := fmt.Sprintf("%032x", n)
	spans := []Span{{TraceID: id, SpanID: "0000000000000001", Name: "magento.request", Type: "web", Resource: route, Start: t0.Add(time.Duration(n) * time.Second), Duration: d, Attrs: map[string]string{"http.method": "GET", "http.url": "https://app.test/" + route}}}
	for i, q := range sql {
		spans = append(spans, Span{TraceID: id, SpanID: fmt.Sprintf("%016x", i+2), ParentID: "0000000000000001", Name: "PDOStatement.execute", Type: "sql", Resource: q, Start: spans[0].Start, Duration: time.Millisecond})
	}
	tr, ok := assemble(spans)
	if !ok {
		panic("assemble failed")
	}
	return tr
}

func TestStoreAddGetList(t *testing.T) {
	s := NewStore(500, 256<<20)
	s.Add(synth(1, 300*time.Millisecond, "catalog/category/view"))
	s.Add(synth(2, 100*time.Millisecond, "cms/index/index"))
	s.Add(synth(3, 900*time.Millisecond, "catalog/category/view"))

	tr, ok := s.Get(fmt.Sprintf("%032x", 2))
	if !ok || tr.Route != "cms/index/index" || tr.Method != "GET" {
		t.Fatalf("Get = %v %+v", ok, tr)
	}
	if _, ok := s.Get("missing"); ok {
		t.Error("Get(missing) found something")
	}

	list := s.List(ListFilter{})
	if len(list) != 3 || list[0].Duration != 900*time.Millisecond || list[2].Duration != 100*time.Millisecond {
		t.Errorf("default sort is not slowest first: %+v", list)
	}
	list = s.List(ListFilter{Sort: "recent"})
	if list[0].ID != fmt.Sprintf("%032x", 3) || list[2].ID != fmt.Sprintf("%032x", 1) {
		t.Errorf("recent sort: %v %v", list[0].ID, list[2].ID)
	}
	list = s.List(ListFilter{Route: "catalog/category/view", MinDuration: 500 * time.Millisecond})
	if len(list) != 1 || list[0].Duration != 900*time.Millisecond {
		t.Errorf("filter: %+v", list)
	}
	if list = s.List(ListFilter{Limit: 2}); len(list) != 2 {
		t.Errorf("limit: %d", len(list))
	}
	if list[0].Counts == nil {
		t.Error("summary without counts")
	}
}

func TestStoreReplacesSameID(t *testing.T) {
	s := NewStore(500, 256<<20)
	s.Add(synth(1, time.Second, "a"))
	s.Add(synth(1, 2*time.Second, "b"))
	list := s.List(ListFilter{})
	if len(list) != 1 || list[0].Route != "b" {
		t.Errorf("list = %+v", list)
	}
}

func TestStoreEvictsByCount(t *testing.T) {
	s := NewStore(3, 256<<20)
	for i := 1; i <= 5; i++ {
		s.Add(synth(i, time.Second, "r"))
	}
	if n := len(s.List(ListFilter{})); n != 3 {
		t.Fatalf("holds %d, want 3", n)
	}
	for i := 1; i <= 2; i++ {
		if _, ok := s.Get(fmt.Sprintf("%032x", i)); ok {
			t.Errorf("trace %d not evicted", i)
		}
	}
	if _, ok := s.Get(fmt.Sprintf("%032x", 5)); !ok {
		t.Error("newest evicted")
	}
}

func TestStoreEvictsByBudget(t *testing.T) {
	q := strings.Repeat("x", 1000)
	first := synth(1, time.Second, "r", q, q, q)
	one := traceSize(first) + analysisSize(analyze(first))
	s := NewStore(500, one*3+one/2)
	for i := 1; i <= 6; i++ {
		s.Add(synth(i, time.Second, "r", q, q, q))
	}
	list := s.List(ListFilter{Sort: "recent"})
	if len(list) != 3 {
		t.Fatalf("holds %d, want 3", len(list))
	}
	if list[0].ID != fmt.Sprintf("%032x", 6) || list[2].ID != fmt.Sprintf("%032x", 4) {
		t.Errorf("kept %s..%s, want the newest three", list[2].ID, list[0].ID)
	}
	if s.bytes > s.maxBytes {
		t.Errorf("bytes %d over budget %d", s.bytes, s.maxBytes)
	}

	huge := synth(7, time.Second, "r", strings.Repeat("y", int(s.maxBytes)))
	s.Add(huge)
	if _, ok := s.Get(huge.ID); ok {
		t.Error("trace larger than the whole budget was stored")
	}
	if len(s.List(ListFilter{})) != 3 {
		t.Error("oversized trace evicted the others")
	}
}

func TestStoreProfileEitherOrder(t *testing.T) {
	s := NewStore(500, 256<<20)
	a, b := synth(1, time.Second, "r"), synth(2, time.Second, "r")

	if err := s.SetProfile(a.ID, []byte(`{"a":1}`)); err != nil {
		t.Fatal(err)
	}
	s.Add(a)
	if tr, _ := s.Get(a.ID); !tr.HasProfile {
		t.Error("profile before trace not attached")
	}

	s.Add(b)
	ch, cancel := s.Subscribe()
	defer cancel()
	if err := s.SetProfile(b.ID, []byte(`{"b":1}`)); err != nil {
		t.Fatal(err)
	}
	if tr, _ := s.Get(b.ID); !tr.HasProfile {
		t.Error("profile after trace not attached")
	}
	select {
	case tr := <-ch:
		if tr.ID != b.ID || !tr.HasProfile {
			t.Errorf("update = %s %v", tr.ID, tr.HasProfile)
		}
	case <-time.After(time.Second):
		t.Error("no update published for the profile")
	}
	if data, ok := s.Profile(b.ID); !ok || string(data) != `{"b":1}` {
		t.Errorf("Profile = %s %v", data, ok)
	}
	if list := s.List(ListFilter{}); !list[0].HasProfile || !list[1].HasProfile {
		t.Error("summary misses HasProfile")
	}

	if err := s.SetProfile("NOT-A-KEY", []byte("x")); err == nil {
		t.Error("invalid id accepted")
	}
	if err := s.SetProfile(fmt.Sprintf("%032x", 9), make([]byte, maxProfile+1)); err == nil {
		t.Error("oversized profile accepted")
	}
}

func TestStorePendingProfilesBounded(t *testing.T) {
	s := NewStore(500, 256<<20)
	for i := 0; i < maxPending+10; i++ {
		if err := s.SetProfile(fmt.Sprintf("%032x", 1000+i), []byte("{}")); err != nil {
			t.Fatal(err)
		}
	}
	if len(s.pending) != maxPending {
		t.Errorf("pending = %d, want %d", len(s.pending), maxPending)
	}
	if _, ok := s.Profile(fmt.Sprintf("%032x", 1000)); ok {
		t.Error("oldest pending profile kept")
	}
	big := make([]byte, maxProfile)
	for i := 0; i < 20; i++ {
		_ = s.SetProfile(fmt.Sprintf("%032x", 5000+i), big)
	}
	if s.pendingBytes > maxPendingBytes {
		t.Errorf("pending bytes %d over %d", s.pendingBytes, maxPendingBytes)
	}
}

func TestStoreSubscribe(t *testing.T) {
	s := NewStore(500, 256<<20)
	ch, cancel := s.Subscribe()
	s.Add(synth(1, time.Second, "r"))
	select {
	case tr := <-ch:
		if tr.ID != fmt.Sprintf("%032x", 1) || len(tr.Spans) == 0 {
			t.Errorf("got %+v", tr)
		}
	case <-time.After(time.Second):
		t.Fatal("no trace published")
	}
	cancel()
	cancel()
	if _, open := <-ch; open {
		t.Error("channel open after cancel")
	}
	for i := 0; i < 1000; i++ {
		s.Add(synth(i+2, time.Second, "r"))
	}
}

func TestNPlusOne(t *testing.T) {
	var nine, ten []string
	for i := 0; i < 10; i++ {
		q := fmt.Sprintf("SELECT `e`.* FROM `catalog_product_entity` AS `e` WHERE (entity_id = %d) AND sku = 'SKU-%d'", i, i)
		if i < 9 {
			nine = append(nine, q)
		}
		ten = append(ten, q)
	}
	if tr := synth(1, time.Second, "r", nine...); len(tr.NPlusOne) != 0 {
		t.Errorf("9 repeats flagged: %q", tr.NPlusOne)
	}
	tr := synth(2, time.Second, "r", ten...)
	if len(tr.NPlusOne) != 1 {
		t.Fatalf("10 repeats not flagged: %q", tr.NPlusOne)
	}
	if want := "SELECT `e`.* FROM `catalog_product_entity` AS `e` WHERE (entity_id = ?) AND sku = ?"; tr.NPlusOne[0] != want {
		t.Errorf("normalised = %q", tr.NPlusOne[0])
	}

	var prepared []Span
	id := fmt.Sprintf("%032x", 3)
	prepared = append(prepared, Span{TraceID: id, SpanID: "1", Name: "magento.request", Start: t0, Duration: time.Second})
	for i := 0; i < 6; i++ {
		for _, n := range []string{"PDO.prepare", "PDOStatement.execute"} {
			prepared = append(prepared, Span{TraceID: id, SpanID: fmt.Sprintf("%d%s", i, n), ParentID: "1", Name: n, Type: "sql", Resource: "SELECT 1 FROM t WHERE id = ?", Start: t0, Duration: time.Millisecond})
		}
	}
	tr, _ = assemble(prepared)
	if len(tr.NPlusOne) != 0 || tr.Counts["sql"] != 6 {
		t.Errorf("prepare counted as execution: n+1 %q, sql %d", tr.NPlusOne, tr.Counts["sql"])
	}
}

func TestNormalizeSQL(t *testing.T) {
	cases := map[string]string{
		"SELECT * FROM t WHERE id IN (1, 2, 3)":              "SELECT * FROM t WHERE id IN (?)",
		"SELECT * FROM t WHERE id IN (?,?,?)":                "SELECT * FROM t WHERE id IN (?)",
		"SELECT  *\n FROM `t1`  WHERE a = 'it''s' AND b=1.5": "SELECT * FROM `t1` WHERE a = ? AND b=?",
		"INSERT INTO t (a, b) VALUES (1, 'x'), (2, 'y')":     "INSERT INTO t (a, b) VALUES (?)",
		"SELECT * FROM t WHERE x = 0xff AND s = \"q\"":       "SELECT * FROM t WHERE x = ? AND s = ?",
	}
	for in, want := range cases {
		if got := normalizeSQL(in); got != want {
			t.Errorf("normalizeSQL(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestStoreSkipsBenchmarkRequests(t *testing.T) {
	s := NewStore(500, 256<<20)
	bench := synth(1, time.Millisecond, "cms/index/index")
	bench.Benchmark = true
	if s.Add(bench) {
		t.Fatal("kept a benchmark trace")
	}
	if !s.Add(synth(2, time.Millisecond, "cms/index/index")) || len(s.List(ListFilter{})) != 1 {
		t.Fatal("a shop trace was not kept")
	}
	// Its profile is dropped, before or after the trace.
	if err := s.SetProfile(bench.ID, []byte("{}")); err != nil || len(s.pending) != 0 {
		t.Fatalf("a benchmark profile after its trace was parked: %v %d", err, len(s.pending))
	}
	early := synth(3, time.Millisecond, "cms/index/index")
	early.Benchmark = true
	if err := s.SetProfile(early.ID, []byte("{}")); err != nil {
		t.Fatal(err)
	}
	s.Add(early)
	if len(s.pending) != 0 || s.pendingBytes != 0 {
		t.Fatalf("a benchmark profile before its trace stayed parked: %d", len(s.pending))
	}
}

func TestStoreSkipsOwnHost(t *testing.T) {
	s := NewStore(500, 256<<20)
	s.SkipHost("Inspector.shop.test")
	own := synth(1, time.Millisecond, "cms/index/index")
	own.URL = "http://inspector.shop.test/api/stream?sub=php%3Atraces"
	s.Add(own)
	s.Add(synth(2, time.Millisecond, "cms/index/index"))
	if list := s.List(ListFilter{}); len(list) != 1 || list[0].ID != fmt.Sprintf("%032x", 2) {
		t.Fatalf("own-host trace kept: %+v", list)
	}
}

func TestAssembleMarksBenchmarkRequests(t *testing.T) {
	for ua, want := range map[string]bool{BenchmarkUserAgent: true, "Mozilla/5.0": false} {
		tr, ok := assemble([]Span{{TraceID: "t1", SpanID: "s1", Name: "web.request", Start: time.Unix(1, 0), Duration: time.Millisecond, Attrs: map[string]string{"http.useragent": ua}}})
		if !ok || tr.Benchmark != want {
			t.Errorf("%s: benchmark %v", ua, tr.Benchmark)
		}
	}
}
