package search

import (
	"strings"
	"testing"
	"time"
)

func TestParseInsights35(t *testing.T) {
	recs, err := ParseInsights(fixture(t, "insights-3.5.json"))
	if err != nil || len(recs) != 8 {
		t.Fatalf("%d %v", len(recs), err)
	}
	r := recs[0]
	if r.key() != "02a9bf55-86c4-4eb6-bb23-9fb396ea62f1" || r.took() != 375*time.Millisecond || r.opaqueID() != "" {
		t.Errorf("got key=%s took=%s id=%q", r.key(), r.took(), r.opaqueID())
	}
	q := r.query()
	if !q.Start.Equal(time.UnixMilli(1792528344390)) || !q.Time.Equal(q.Start) || q.Took != 375*time.Millisecond {
		t.Errorf("times: %+v", q)
	}
	if q.Index != "magento2_product_1" || q.Phase != "request" || q.Source != "insights" {
		t.Errorf("fields: %+v", q)
	}
	if !strings.HasPrefix(q.Body, `{"from":0,"size":12,`) {
		t.Errorf("source string body: %.80s", q.Body)
	}
}

func TestParseInsights219(t *testing.T) {
	recs, err := ParseInsights(fixture(t, "insights-2.19.json"))
	if err != nil || len(recs) != 6 {
		t.Fatalf("%d %v", len(recs), err)
	}
	own, store := 0, 0
	for _, r := range recs {
		if r.opaqueID() == opaqueID {
			own++
		}
		if r.opaqueID() == "storefront" {
			store++
			q := r.query()
			if !strings.HasPrefix(q.Body, `{"from":0,"size":12,"query":{"bool"`) || q.Took != 88*time.Millisecond {
				t.Errorf("object source: %+v", q)
			}
		}
	}
	if own != 1 || store != 1 {
		t.Errorf("own=%d storefront=%d", own, store)
	}
}

func TestParseInsightsOldShape(t *testing.T) {
	recs, err := ParseInsights([]byte(`{"top_queries":[{"timestamp":1792528344390,"indices":["a"],"source":{"size":1},"latency":42,"labels":{}}]}`))
	if err != nil || len(recs) != 1 {
		t.Fatal(err)
	}
	if recs[0].took() != 42*time.Millisecond || recs[0].key() == "" {
		t.Errorf("took=%s key=%q", recs[0].took(), recs[0].key())
	}
}

func TestParseSlowlog(t *testing.T) {
	lines := fixtureLogLines(t, "slowlog-2.0.txt")
	var got []string
	for _, l := range lines {
		q, ok := ParseSlowlog(l.Line, l.Time)
		if !ok {
			t.Fatalf("not parsed: %s", l.Line)
		}
		got = append(got, q.Phase+" "+q.Index+" "+q.Took.String()+" "+q.OpaqueID)
		if !q.Start.Equal(l.Time.Add(-q.Took)) || !q.Time.Equal(l.Time) || q.Source != "slowlog" {
			t.Errorf("times: %+v", q)
		}
		if !strings.HasPrefix(q.Body, "{") || !strings.HasSuffix(q.Body, "}") {
			t.Errorf("body: %s", q.Body)
		}
	}
	want := []string{
		"query magento2_product_1_v1 56ms roll-inspector",
		"fetch magento2_product_1_v1 87.4ms roll-inspector",
		"query magento2_product_1_v1 21.3ms storefront",
		"fetch magento2_product_1_v1 4.5ms storefront",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("got\n%s", strings.Join(got, "\n"))
	}
}

func TestParseSlowlog35(t *testing.T) {
	lines := fixtureLogLines(t, "slowlog-3.5.txt")
	parsed := 0
	for _, l := range lines {
		q, ok := ParseSlowlog(l.Line, l.Time)
		if !ok {
			if strings.Contains(l.Line, "took_millis") {
				t.Errorf("not parsed: %s", l.Line)
			}
			continue
		}
		parsed++
		if strings.Contains(q.Body, `a]b], id[x`) {
			if q.OpaqueID != "" || !strings.HasSuffix(q.Body, `"boost":1.0}}}}`) {
				t.Errorf("bracket in source: id=%q body=%s", q.OpaqueID, q.Body)
			}
		}
	}
	if parsed != 6 {
		t.Errorf("parsed %d", parsed)
	}
	q, _ := ParseSlowlog(lines[3].Line, lines[3].Time)
	if q.Took != 633600*time.Nanosecond || q.Phase != "fetch" {
		t.Errorf("micros: %+v", q)
	}
}

func TestParseSlowlogJSON(t *testing.T) {
	tm := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	es7 := `{"type": "index_search_slowlog", "timestamp": "2026-10-20T12:00:00,000Z", "level": "WARN", "component": "i.s.s.query", "cluster.name": "docker-cluster", "node.name": "es", "message": "[magento2_product_1_v3][0]", "took": "12.5ms", "took_millis": "12", "total_hits": "3 hits", "stats": "[]", "search_type": "QUERY_THEN_FETCH", "total_shards": "1", "source": "{\"size\":10}", "id": "storefront" }`
	q, ok := ParseSlowlog(es7, tm)
	if !ok || q.Index != "magento2_product_1_v3" || q.Phase != "query" || q.Took != 12500*time.Microsecond || q.Body != `{"size":10}` || q.OpaqueID != "storefront" {
		t.Errorf("es7: %+v %v", q, ok)
	}
	es8 := `{"@timestamp":"2026-10-20T12:00:00.000Z", "log.level": "WARN", "log.logger":"index.search.slowlog.fetch","elasticsearch.slowlog.id":"x","elasticsearch.slowlog.message":"[idx][0]","elasticsearch.slowlog.search_type":"QUERY_THEN_FETCH","elasticsearch.slowlog.source":"{}","elasticsearch.slowlog.took":"1ms","elasticsearch.slowlog.took_millis":1}`
	q, ok = ParseSlowlog(es8, tm)
	if !ok || q.Index != "idx" || q.Phase != "fetch" || q.Took != time.Millisecond || q.OpaqueID != "x" {
		t.Errorf("es8: %+v %v", q, ok)
	}
	if _, ok := ParseSlowlog(`{"type":"server","message":"started"}`, tm); ok {
		t.Error("server line parsed")
	}
}

func TestSummarize(t *testing.T) {
	cases := map[string]string{
		`{"from":0,"size":12,"query":{"bool":{"must":[{"term":{"category_ids":{"value":"13","boost":1.0}}},{"terms":{"visibility":["2","4"],"boost":1.0}}]}},"sort":[{"position_category_13":{"order":"asc"}}],"aggregations":{"a":{},"b":{}}}`: `term category_ids=13, terms visibility=[2 4] · size 12 · sort position_category_13 · 2 aggs`,
		`{"query":{"match":{"_search":{"query":"jacket","operator":"OR"}}}}`:    `match _search="jacket"`,
		`{"size":2,"query":{"ids":{"values":["9076","9077"]}},"_source":false}`: `ids 2 · size 2`,
		`{"query":{"bool":{"must_not":[{"exists":{"field":"source"}}]}}}`:       `not exists source`,
		`{"query":{"match_all":{}}}`:                                            `match_all`,
		`{"size":0}`:                                                            `match_all · size 0`,
		`{"query":{"bool":{"should":[{"match":{"a":"1"}},{"match":{"b":"2"}},{"match":{"c":"3"}},{"match":{"d":"4"}},{"match":{"e":"5"}}]}}}`: `match a="1", match b="2", match c="3", match d="4" +1`,
		`{"query":{"range":{"price":{"gte":10}}}}`: `range price`,
		`not json`: `not json`,
	}
	for in, want := range cases {
		if got := Summarize(in); got != want {
			t.Errorf("%s\n got %q\nwant %q", in, got, want)
		}
	}
}
