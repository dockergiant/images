package apm

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/vmihailenco/msgpack/v5"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
)

func ddTwinOfOTLPFixture(t *testing.T) Trace {
	t.Helper()
	const root = 0x25a83c7ee41d7769
	start := int64(1792528628696313479)
	body, err := msgpack.Marshal([][]ddSpan{{
		{TraceID: root, SpanID: root, Name: "magento.request", Resource: "catalog/category/view", Service: "shop", Type: "web", Start: start, Duration: 1985730159,
			Meta:    map[string]string{"_dd.p.tid": "6ad7d4d300000000", "component": "magento", "http.method": "GET", "http.url": "https://app.shop.test/women.html", "http.status_code": "200", "magento.pathinfo": "catalog/category/view", "varnish.xid": "53"},
			Metrics: map[string]float64{"process_id": 4604}},
		{TraceID: root, SpanID: 2, ParentID: root, Name: "PDOStatement.execute", Service: "shop", Resource: "SELECT `catalog_eav_attribute`.* FROM `catalog_eav_attribute` WHERE (attribute_id = ?)", Type: "sql", Start: start + 100000000, Duration: 2000000,
			Meta: map[string]string{"db.system": "mysql"}},
		{TraceID: root, SpanID: 3, ParentID: root, Name: "Redis.mGet", Resource: "Redis.mGet", Service: "shop", Type: "redis", Start: start + 110000000, Duration: 500000,
			Meta: map[string]string{"db.system": "redis", "redis.raw_command": "mGet 69d_:SYSTEM_STORES_EN"}},
		{TraceID: root, SpanID: 4, ParentID: root, Name: "curl_exec", Resource: "http://opensearch/magento2_product_1/_search", Service: "shop", Type: "http", Start: start + 210000000, Duration: 12000000, Error: 1,
			Meta: map[string]string{"http.url": "http://opensearch/magento2_product_1/_search?track_total_hits=true", "network.destination.name": "opensearch", "network.destination.port": "9200"}},
		{TraceID: root, SpanID: 5, ParentID: root, Name: "curl_exec", Type: "http", Start: start + 1963686521, Duration: 12000000,
			Meta: map[string]string{"http.url": "http://inspector:8126/profiles/6ad7d4d30000000025a83c7ee41d7769", "network.destination.name": "inspector"}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	traces, err := decodeDD(body)
	if err != nil {
		t.Fatal(err)
	}
	return traces[0]
}

func sameShape(t *testing.T, got, want Trace) {
	t.Helper()
	if got.ID != want.ID || got.Method != want.Method || got.URL != want.URL || got.Status != want.Status ||
		got.Route != want.Route || got.VarnishXID != want.VarnishXID || !got.Start.Equal(want.Start) || got.Duration != want.Duration {
		t.Errorf("root fields differ:\n got %s %s %s %d %q %q %s %s\nwant %s %s %s %d %q %q %s %s",
			got.ID, got.Method, got.URL, got.Status, got.Route, got.VarnishXID, got.Start, got.Duration,
			want.ID, want.Method, want.URL, want.Status, want.Route, want.VarnishXID, want.Start, want.Duration)
	}
	for k, v := range want.Counts {
		if got.Counts[k] != v {
			t.Errorf("Counts[%s] = %d, want %d", k, got.Counts[k], v)
		}
	}
	for k, v := range want.Times {
		if got.Times[k] != v {
			t.Errorf("Times[%s] = %s, want %s", k, got.Times[k], v)
		}
	}
	if len(got.Spans) != len(want.Spans) {
		t.Fatalf("spans = %d, want %d", len(got.Spans), len(want.Spans))
	}
	for i := range want.Spans {
		g, w := got.Spans[i], want.Spans[i]
		if g.TraceID != w.TraceID || g.SpanID != w.SpanID || g.ParentID != w.ParentID || g.Name != w.Name ||
			g.Component != w.Component || g.Type != w.Type || g.Service != w.Service || g.Error != w.Error ||
			!g.Start.Equal(w.Start) || g.Duration != w.Duration {
			t.Errorf("span %d differs:\n got %+v\nwant %+v", i, g, w)
		}
	}
	if got.Spans[1].Resource != want.Spans[1].Resource {
		t.Errorf("sql resource = %q", got.Spans[1].Resource)
	}
	if rootOf(got).Attrs["process_id"] != "4604" {
		t.Errorf("process_id = %q", rootOf(got).Attrs["process_id"])
	}
}

func TestOTLPJSONMatchesDD(t *testing.T) {
	traces, err := decodeOTLP(fixture(t, "otlp-category.json"), "application/json")
	if err != nil {
		t.Fatal(err)
	}
	if len(traces) != 1 {
		t.Fatalf("traces = %d", len(traces))
	}
	sameShape(t, traces[0], ddTwinOfOTLPFixture(t))
	if s := spanByName(traces[0], "curl_exec"); s == nil || s.Attrs["otel.status_message"] != "slow" {
		t.Errorf("status message not kept: %+v", s)
	}
}

func TestOTLPProtobufMatchesJSON(t *testing.T) {
	data, err := parseOTLPJSON(fixture(t, "otlp-category.json"))
	if err != nil {
		t.Fatal(err)
	}
	pb, err := proto.Marshal(data)
	if err != nil {
		t.Fatal(err)
	}
	s := NewStore(500, 256<<20)
	h := NewOTLPIntake(s, allowAll)
	req := httptest.NewRequest(http.MethodPost, "/v1/traces", bytes.NewReader(pb))
	req.RemoteAddr = "127.0.0.1:1"
	req.Header.Set("Content-Type", "application/x-protobuf")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || rec.Header().Get("Content-Type") != "application/x-protobuf" {
		t.Fatalf("protobuf post = %d %s", rec.Code, rec.Header().Get("Content-Type"))
	}
	got, ok := s.Get("6ad7d4d30000000025a83c7ee41d7769")
	if !ok {
		t.Fatal("trace not stored")
	}
	sameShape(t, got, ddTwinOfOTLPFixture(t))
}

func TestOTLPIntakeHTTP(t *testing.T) {
	s := NewStore(500, 256<<20)
	h := NewOTLPIntake(s, func(ip string) bool { return ip == "127.0.0.1" })
	send := func(method, path, remote, ctype string, body []byte) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, bytes.NewReader(body))
		req.RemoteAddr = remote
		req.Header.Set("Content-Type", ctype)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}
	body := fixture(t, "otlp-category.json")
	if rec := send(http.MethodPost, "/v1/traces", "10.1.1.1:9", "application/json", body); rec.Code != http.StatusForbidden {
		t.Errorf("foreign ip = %d", rec.Code)
	}
	if rec := send(http.MethodPost, "/v1/traces", "127.0.0.1:9", "application/json; charset=utf-8", body); rec.Code != http.StatusOK || rec.Body.String() != "{}" {
		t.Errorf("json = %d %s", rec.Code, rec.Body)
	}
	if _, ok := s.Get("6ad7d4d30000000025a83c7ee41d7769"); !ok {
		t.Error("json trace not stored")
	}
	if rec := send(http.MethodGet, "/v1/traces", "127.0.0.1:9", "application/json", nil); rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET = %d", rec.Code)
	}
	if rec := send(http.MethodPost, "/v1/metrics", "127.0.0.1:9", "application/json", body); rec.Code != http.StatusNotFound {
		t.Errorf("metrics = %d", rec.Code)
	}
	if rec := send(http.MethodPost, "/v1/traces", "127.0.0.1:9", "text/plain", body); rec.Code != http.StatusUnsupportedMediaType {
		t.Errorf("text/plain = %d", rec.Code)
	}
	if rec := send(http.MethodPost, "/v1/traces", "127.0.0.1:9", "application/json", []byte("{nope")); rec.Code != http.StatusBadRequest {
		t.Errorf("bad json = %d", rec.Code)
	}
	if rec := send(http.MethodPost, "/v1/traces", "127.0.0.1:9", "application/json", make([]byte, maxBody+1)); rec.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("oversized = %d", rec.Code)
	}
}

func TestOTLPIntakeElementCeilings(t *testing.T) {
	h := NewOTLPIntake(NewStore(500, 256<<20), allowAll)
	send := func(ctype string, body []byte) int {
		req := httptest.NewRequest(http.MethodPost, "/v1/traces", bytes.NewReader(body))
		req.RemoteAddr = "127.0.0.1:9"
		req.Header.Set("Content-Type", ctype)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec.Code
	}
	spans := func(n int) []byte {
		var ss []byte
		for range n {
			ss = protowire.AppendTag(ss, 2, protowire.BytesType)
			ss = protowire.AppendBytes(ss, nil)
		}
		rs := protowire.AppendTag(nil, 2, protowire.BytesType)
		rs = protowire.AppendBytes(rs, ss)
		td := protowire.AppendTag(nil, 1, protowire.BytesType)
		return protowire.AppendBytes(td, rs)
	}
	if code := send("application/x-protobuf", spans(maxSpans+1)); code != http.StatusRequestEntityTooLarge {
		t.Errorf("%d spans = %d", maxSpans+1, code)
	}
	if code := send("application/x-protobuf", spans(10)); code != http.StatusOK {
		t.Errorf("10 spans = %d", code)
	}

	var attrs []byte
	for range maxElements + 1 {
		attrs = protowire.AppendTag(attrs, 9, protowire.BytesType)
		attrs = protowire.AppendBytes(attrs, nil)
	}
	sp := protowire.AppendTag(nil, 2, protowire.BytesType)
	sp = protowire.AppendBytes(sp, attrs)
	rs := protowire.AppendTag(nil, 2, protowire.BytesType)
	rs = protowire.AppendBytes(rs, sp)
	td := protowire.AppendTag(nil, 1, protowire.BytesType)
	td = protowire.AppendBytes(td, rs)
	if code := send("application/x-protobuf", td); code != http.StatusRequestEntityTooLarge {
		t.Errorf("%d attributes = %d", maxElements+1, code)
	}

	if code := send("application/json", make([]byte, maxJSONBody+1)); code != http.StatusRequestEntityTooLarge {
		t.Errorf("json over %d bytes = %d", maxJSONBody, code)
	}
}
