package apm

import (
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"testing"
	"time"
)

const testTrace = "0000000000000000000000000000abcd"

// sp builds a span at start..start+dur milliseconds after t0.
func sp(id, parent, name, resource string, start, dur float64, attrs map[string]string) Span {
	typ := ""
	switch {
	case strings.HasPrefix(name, "PDO"):
		typ = "sql"
	case strings.HasPrefix(name, "Redis."):
		typ = "redis"
	}
	return Span{
		TraceID: testTrace, SpanID: id, ParentID: parent, Name: name, Resource: resource, Type: typ,
		Start: t0.Add(time.Duration(start * float64(time.Millisecond))), Duration: time.Duration(dur * float64(time.Millisecond)),
		Attrs: attrs,
	}
}

func build(t testing.TB, spans ...Span) Trace {
	t.Helper()
	tr, ok := assemble(spans)
	if !ok {
		t.Fatal("assemble failed")
	}
	return tr
}

func near(a, b float64) bool { return math.Abs(a-b) < 0.0011 }

func TestSelfTimesUnionClipped(t *testing.T) {
	spans := []Span{
		sp("p", "", "root", "", 0, 100, nil),
		sp("a", "p", "child", "", 10, 20, nil),
		sp("b", "p", "child", "", 20, 30, nil),
		sp("c", "p", "child", "", 90, 30, nil),
		sp("d", "a", "grandchild", "", 0, 50, nil),
		sp("e", "missing", "orphan", "", 0, 5, nil),
	}
	self, parent := selfTimes(spans)
	want := []time.Duration{50 * time.Millisecond, 0, 30 * time.Millisecond, 30 * time.Millisecond, 50 * time.Millisecond, 5 * time.Millisecond}
	for i, w := range want {
		if self[i] != w {
			t.Errorf("self[%s] = %s, want %s", spans[i].SpanID, self[i], w)
		}
	}
	if parent[1] != 0 || parent[4] != 1 || parent[0] != -1 || parent[5] != -1 {
		t.Errorf("parents = %v", parent)
	}
}

func TestSelfTimesSelfParent(t *testing.T) {
	spans := []Span{sp("a", "a", "loop", "", 0, 10, nil)}
	self, parent := selfTimes(spans)
	if self[0] != 10*time.Millisecond || parent[0] != -1 {
		t.Errorf("self %v parent %v", self, parent)
	}
}

func categoryOf(a Analysis, key string) Category {
	for _, c := range a.Categories {
		if c.Key == key {
			return c
		}
	}
	return Category{}
}

func pageTrace(t testing.TB) Trace {
	spans := []Span{
		sp("r", "", "magento.request", "catalog/category/view", 0, 1000, map[string]string{"http.method": "GET", "http.url": "https://app.test/c.html"}),
		sp("l", "r", "magento.launch", "Magento\\Framework\\App\\Http\\Interceptor", 10, 900, nil),
		sp("b", "l", "magento.block.render", "Magento_Catalog:product_list", 100, 600, map[string]string{"magento.block.name": "category.products.list"}),
		sp("tp", "b", "magento.template.render", "Magento_Catalog::product/list.phtml", 100, 500, nil),
		sp("o", "l", "magento.event.execute", "Vendor\\Module\\Observer\\Slow", 20, 50, map[string]string{"magento.event.name": "controller_action_predispatch"}),
		sp("h", "l", "curl_exec", "https://api.example.com/rates", 75, 20, map[string]string{"http.url": "https://api.example.com/rates?x=1"}),
		sp("c1", "l", "Redis.setEx", "Redis.setEx", 96, 2, map[string]string{"db.system": "redis", "redis.raw_command": "setEx 69d_:EAV_3 7200 SECRET-CACHE-VALUE"}),
		sp("n1", "l", "Redis.clearLastError", "Redis.clearLastError", 98, 0.5, map[string]string{"db.system": "redis"}),
	}
	for i := 0; i < 12; i++ {
		start := 110 + float64(i)*10
		q := fmt.Sprintf("SELECT `e`.* FROM `catalog_product_entity` AS `e` WHERE (entity_id = %d)", i)
		spans = append(spans,
			sp(fmt.Sprintf("p%d", i), "tp", "PDO.prepare", q, start, 0.5, map[string]string{"db.system": "mysql"}),
			sp(fmt.Sprintf("x%d", i), "tp", "PDOStatement.execute", q, start+1, 1+float64(i)/10, map[string]string{"db.system": "mysql"}),
		)
	}
	for i := 0; i < 30; i++ {
		spans = append(spans, sp(fmt.Sprintf("n%d", i+2), "l", "Redis.clearLastError", "Redis.clearLastError", 300+float64(i)*0.1, 0.01, map[string]string{"db.system": "redis"}))
	}
	for i := 0; i < 12; i++ {
		raw := map[string]string{"db.system": "redis", "redis.raw_command": "multi 2"}
		spans = append(spans, sp(fmt.Sprintf("m%d", i), "l", "Redis.multi", "Redis.multi", 400+float64(i)*0.1, 0.01, raw))
		raw = map[string]string{"db.system": "redis", "redis.raw_command": "exec "}
		spans = append(spans, sp(fmt.Sprintf("e%d", i), "l", "Redis.exec", "Redis.exec", 400.05+float64(i)*0.1, 0.01, raw))
	}
	return build(t, spans...)
}

func TestAnalyzePage(t *testing.T) {
	tr := pageTrace(t)
	a := analyze(tr).analysis

	if a.ID != tr.ID || a.Unit != "ms" || !near(a.Duration, 1000) {
		t.Fatalf("header = %s %s %v", a.ID, a.Unit, a.Duration)
	}
	keys := []string{}
	var sum float64
	for _, c := range a.Categories {
		keys = append(keys, c.Key)
		sum += c.Ms
	}
	if strings.Join(keys, ",") != "sql,cache,search,http,php" {
		t.Errorf("category order = %v", keys)
	}
	if !near(sum, 1000) {
		t.Errorf("categories sum to %v ms", sum)
	}
	sql := categoryOf(a, "sql")
	// 12 prepares of 0.5 ms and executes of 1.0..2.1 ms.
	if !near(sql.Ms, 12*0.5+12*1+6.6) || sql.Count != 12 || !near(sql.Share, sql.Ms/1000) {
		t.Errorf("sql category = %+v", sql)
	}
	if c := categoryOf(a, "cache"); c.Count != 25 {
		t.Errorf("cache count = %d, want 25 (clearLastError is noise)", c.Count)
	}
	if c := categoryOf(a, "http"); !near(c.Ms, 20) || c.Count != 1 {
		t.Errorf("http category = %+v", c)
	}
	times := Summarize(tr).Times
	for _, c := range a.Categories {
		if !near(times[c.Key], c.Ms) {
			t.Errorf("summary times[%s] = %v, analysis %v", c.Key, times[c.Key], c.Ms)
		}
	}

	if len(a.Hot) == 0 || a.Hot[0].Rank != 1 {
		t.Fatalf("hot = %+v", a.Hot)
	}
	for i := 1; i < len(a.Hot); i++ {
		if a.Hot[i].SelfMs > a.Hot[i-1].SelfMs || a.Hot[i].Rank != i+1 {
			t.Fatalf("hot not ranked by self time at %d: %+v", i, a.Hot)
		}
	}
	var exec *HotItem
	for i := range a.Hot {
		if a.Hot[i].Name == "PDOStatement.execute" {
			exec = &a.Hot[i]
		}
	}
	if exec == nil {
		t.Fatalf("no execute group in hot: %+v", a.Hot)
	}
	if exec.Calls != 12 || len(exec.SpanIDs) != 12 || exec.SpanID != "x11" || exec.Kind != "sql" {
		t.Errorf("execute group = %+v", exec)
	}
	if exec.Key != "SELECT `e`.* FROM `catalog_product_entity` AS `e` WHERE (entity_id = ?)" {
		t.Errorf("group key = %q", exec.Key)
	}
	if !strings.Contains(exec.Resource, "entity_id = 11") {
		t.Errorf("group resource is not the slowest call's: %q", exec.Resource)
	}
	if strings.Join(exec.Path, " › ") != "launch › block category.products.list › template Magento_Catalog::product/list.phtml" {
		t.Errorf("path = %q", exec.Path)
	}
	if !near(exec.Share, exec.SelfMs/1000) || !near(exec.TotalMs, exec.SelfMs) {
		t.Errorf("execute share/total = %v %v %v", exec.Share, exec.SelfMs, exec.TotalMs)
	}
	var tpl *HotItem
	for i := range a.Hot {
		if a.Hot[i].Kind == "template" {
			tpl = &a.Hot[i]
		}
	}
	if tpl == nil || !near(tpl.SelfMs, 500-12*0.5-12*1-6.6) || !near(tpl.TotalMs, 500) {
		t.Errorf("template self = %+v", tpl)
	}

	if len(a.Repeats) != 1 {
		t.Fatalf("repeats = %+v (multi and exec are transaction framing, not repeats)", a.Repeats)
	}
	r := a.Repeats[0]
	if r.Kind != "sql" || r.Count != 12 || len(r.SpanIDs) != 12 || !near(r.TotalMs, 12+6.6) || !near(r.AvgMs*float64(r.Count), r.TotalMs) {
		t.Errorf("repeat = %+v", r)
	}
	if !strings.Contains(r.Example, "entity_id = 11") {
		t.Errorf("repeat example = %q", r.Example)
	}

	perKind := map[string]int{}
	for _, s := range a.Slowest {
		perKind[s.Kind]++
	}
	if perKind["sql"] != 5 || perKind["cache"] != 1 || perKind["http"] != 1 {
		t.Errorf("slowest per kind = %v", perKind)
	}
	if a.Slowest[0].Kind != "sql" || a.Slowest[0].SpanID != "x11" {
		t.Errorf("slowest sql first = %+v", a.Slowest[0])
	}

	body, _ := json.Marshal(a)
	if strings.Contains(string(body), "SECRET-CACHE-VALUE") {
		t.Error("analysis carries a cache value")
	}
	for _, field := range []string{`"categories"`, `"hot"`, `"repeats"`, `"slowest"`, `"profile"`, `"selfMs"`, `"totalMs"`, `"spanIds"`, `"path"`, `"avgMs"`, `"example"`} {
		if !strings.Contains(string(body), field) {
			t.Errorf("analysis JSON misses %s", field)
		}
	}
}

func TestAnalyzeOpsSkipNoiseAndPrepare(t *testing.T) {
	d := analyze(pageTrace(t))
	byKey := map[string]opStat{}
	for _, o := range d.ops {
		byKey[o.kind+" "+o.key] = o
	}
	sql := byKey["sql SELECT `e`.* FROM `catalog_product_entity` AS `e` WHERE (entity_id = ?)"]
	if sql.calls != 12 || sql.worstSpan != "x11" {
		t.Errorf("sql op = %+v", sql)
	}
	for k := range byKey {
		if strings.Contains(k, "clearLastError") {
			t.Errorf("noise op kept: %s", k)
		}
	}
	if o := byKey["cache setEx *_:EAV_*"]; o.calls != 1 || strings.Contains(o.example, "SECRET") {
		t.Errorf("cache op = %+v", o)
	}
	if o := byKey["observer Vendor\\Module\\Observer\\Slow"]; o.calls != 1 {
		t.Errorf("observer op missing: %v", byKey)
	}
	if o := byKey["block Magento_Catalog:product_list"]; o.calls != 1 {
		t.Errorf("block op missing: %v", byKey)
	}
	if o := byKey["http https://api.example.com/rates"]; o.calls != 1 {
		t.Errorf("http op missing: %v", byKey)
	}
	for k := range byKey {
		if strings.HasPrefix(k, "template ") || strings.HasPrefix(k, "php ") {
			t.Errorf("op of a kind Hotspots does not rank: %s", k)
		}
	}
}

func TestAnalyzeHotCapAndGrouping(t *testing.T) {
	spans := []Span{sp("r", "", "magento.request", "x", 0, 1000, nil)}
	for i := 0; i < 40; i++ {
		dur := 10.0
		if i < 2 {
			dur = 20
		}
		spans = append(spans, sp(fmt.Sprintf("b%d", i), "r", "magento.block.render", fmt.Sprintf("Mod:block%d", i), float64(i)*20, dur, nil))
	}
	for i := 0; i < 3; i++ {
		spans = append(spans, sp(fmt.Sprintf("s%d", i), "b0", "magento.block.render", "Mod:same", float64(i)*6, 5, nil))
		spans = append(spans, sp(fmt.Sprintf("u%d", i), "b1", "magento.block.render", "Mod:same", 20+float64(i)*6, 5, nil))
	}
	a := analyze(build(t, spans...)).analysis
	if len(a.Hot) != maxHot {
		t.Fatalf("hot holds %d, want %d", len(a.Hot), maxHot)
	}
	groups := 0
	for _, h := range a.Hot {
		if h.Resource == "Mod:same" {
			groups++
			if h.Calls != 3 || len(h.SpanIDs) != 3 || !near(h.SelfMs, 15) {
				t.Errorf("same block under one parent not grouped: %+v", h)
			}
		}
	}
	if groups != 2 {
		t.Errorf("want one group per parent, got %d", groups)
	}
	if a.Repeats == nil || a.Slowest == nil {
		t.Error("empty lists must be [] not null")
	}
}

func TestAnalyzeEmptyListsNotNull(t *testing.T) {
	a := analyze(build(t, sp("r", "", "magento.request", "x", 0, 10, nil))).analysis
	body, _ := json.Marshal(a)
	if strings.Contains(string(body), "null") {
		t.Errorf("null in %s", body)
	}
	if len(a.Hot) != 1 || a.Hot[0].Path == nil || len(a.Hot[0].Path) != 0 {
		t.Errorf("root hot item = %+v", a.Hot)
	}
}

func TestAnalyzeDeepPathCapped(t *testing.T) {
	spans := []Span{sp("s0", "", "magento.request", "x", 0, 100, nil)}
	for i := 1; i <= 40; i++ {
		spans = append(spans, sp(fmt.Sprintf("s%d", i), fmt.Sprintf("s%d", i-1), fmt.Sprintf("magento.level%d", i), "", 0, 100-float64(i), nil))
	}
	a := analyze(build(t, spans...)).analysis
	for _, h := range a.Hot {
		if len(h.Path) > maxPath {
			t.Fatalf("path of %d entries: %q", len(h.Path), h.Path)
		}
		if h.Name == "magento.level40" && (h.Path[0] != "level1" || h.Path[len(h.Path)-1] != "level39") {
			t.Errorf("capped path = %q", h.Path)
		}
	}
}

func TestAnalyzeFixture(t *testing.T) {
	traces, err := decodeDD(fixture(t, "dd-category.msgpack"))
	if err != nil {
		t.Fatal(err)
	}
	tr := traces[0]
	a := analyze(tr).analysis
	var sum float64
	for _, c := range a.Categories {
		sum += c.Ms
	}
	if !near(sum, a.Duration) {
		t.Errorf("categories sum %v, duration %v", sum, a.Duration)
	}
	if c := categoryOf(a, "sql"); c.Count != tr.Counts["sql"] {
		t.Errorf("sql count %d, trace counts %d", c.Count, tr.Counts["sql"])
	}
	if len(a.Hot) != maxHot {
		t.Errorf("hot = %d", len(a.Hot))
	}
	found := false
	for _, r := range a.Repeats {
		if r.Count < nPlusOneMin {
			t.Errorf("repeat below threshold: %+v", r)
		}
		if r.Kind == "sql" && strings.Contains(r.Key, "FROM `catalog_eav_attribute` WHERE (attribute_id = ?)") {
			found = true
		}
	}
	if !found {
		t.Errorf("repeats miss the catalog_eav_attribute query")
	}
	for _, s := range a.Slowest {
		if s.Kind == "cache" && strings.Count(s.Resource, " ") > 1 {
			t.Errorf("cache resource carries more than command and key: %q", s.Resource)
		}
	}
}

// bigTrace is a synthetic Magento page of n spans: blocks with templates,
// prepared statements with literals, cache calls with values, and observers.
func bigTrace(n int) Trace {
	spans := []Span{
		sp("root", "", "magento.request", "catalog/category/view", 0, 2000, nil),
		sp("launch", "root", "magento.launch", "Http", 1, 1990, nil),
	}
	value := strings.Repeat("v", 600)
	i := 0
	for len(spans) < n {
		blk := fmt.Sprintf("b%d", i)
		at := 2 + float64(i)*0.25
		spans = append(spans,
			sp(blk, "launch", "magento.block.render", fmt.Sprintf("Mod:block_%d", i%80), at, 0.2, map[string]string{"magento.block.name": fmt.Sprintf("block.%d", i%80)}),
			sp(blk+"t", blk, "magento.template.render", fmt.Sprintf("Mod::t%d.phtml", i%80), at, 0.19, nil),
			sp(blk+"p", blk+"t", "PDO.prepare", fmt.Sprintf("SELECT `e`.*, `price_index`.`price` FROM `catalog_product_entity` AS `e` INNER JOIN `catalog_product_index_price` AS `price_index` ON price_index.entity_id = e.entity_id AND price_index.website_id = '1' WHERE (e.entity_id IN (%d, %d, %d, %d)) AND (`e`.`sku` = 'SKU-%d') LIMIT 20", i, i+1, i+2, i+3, i), at, 0.01, map[string]string{"db.system": "mysql"}),
			sp(blk+"x", blk+"t", "PDOStatement.execute", fmt.Sprintf("SELECT `e`.*, `price_index`.`price` FROM `catalog_product_entity` AS `e` INNER JOIN `catalog_product_index_price` AS `price_index` ON price_index.entity_id = e.entity_id AND price_index.website_id = '1' WHERE (e.entity_id IN (%d, %d, %d, %d)) AND (`e`.`sku` = 'SKU-%d') LIMIT 20", i, i+1, i+2, i+3, i), at+0.01, 0.05, map[string]string{"db.system": "mysql"}),
			sp(blk+"c", blk+"t", "Redis.setEx", "Redis.setEx", at+0.07, 0.02, map[string]string{"db.system": "redis", "redis.raw_command": fmt.Sprintf("setEx 69d_:BLOCK_HTML_%X 7200 %s", i*7919, value)}),
			sp(blk+"o", "launch", "magento.event.execute", fmt.Sprintf("Vendor\\Observer\\O%d", i%30), at+0.2, 0.02, nil),
		)
		i++
	}
	tr, _ := assemble(spans[:n])
	return tr
}

func BenchmarkAnalyze7000(b *testing.B) {
	tr := bigTrace(7000)
	if len(tr.Spans) != 7000 {
		b.Fatalf("spans = %d", len(tr.Spans))
	}
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		analyze(tr)
	}
}

func BenchmarkAnalyzeFixture(b *testing.B) {
	traces, err := decodeDD(fixture(b, "dd-category.msgpack"))
	if err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		analyze(traces[0])
	}
}
