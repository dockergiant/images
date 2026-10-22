package apm

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/vmihailenco/msgpack/v5"
)

func TestTraceKey(t *testing.T) {
	cases := []struct {
		tid  string
		id   uint64
		want string
	}{
		{"6ad7cccc00000000", 0x5af7bd4902325a6c, "6ad7cccc000000005af7bd4902325a6c"},
		{"6AD7CCCC00000000", 0x5af7bd4902325a6c, "6ad7cccc000000005af7bd4902325a6c"},
		{"", 1, "00000000000000000000000000000001"},
		{"abc", 0xffffffffffffffff, "0000000000000abcffffffffffffffff"},
	}
	for _, c := range cases {
		if got := traceKey(c.tid, c.id); got != c.want {
			t.Errorf("traceKey(%q, %x) = %s, want %s", c.tid, c.id, got, c.want)
		}
	}
}

func TestDecodeCategoryPage(t *testing.T) {
	traces, err := decodeDD(fixture(t, "dd-category.msgpack"))
	if err != nil {
		t.Fatal(err)
	}
	if len(traces) != 1 {
		t.Fatalf("got %d traces, want 1", len(traces))
	}
	tr := traces[0]
	if tr.ID != "6ad7d4d30000000025a83c7ee41d7769" {
		t.Errorf("ID = %s", tr.ID)
	}
	if len(tr.Spans) != 2430 {
		t.Errorf("spans = %d, want 2430 (2431 minus the profiler upload)", len(tr.Spans))
	}
	if tr.Method != "GET" || tr.URL != "https://app.shop.test/women.html" || tr.Status != 200 {
		t.Errorf("root fields = %s %s %d", tr.Method, tr.URL, tr.Status)
	}
	if tr.Route != "catalog/category/view" {
		t.Errorf("Route = %q", tr.Route)
	}
	if tr.VarnishXID != "53" {
		t.Errorf("VarnishXID = %q", tr.VarnishXID)
	}
	if tr.Cached {
		t.Error("Cached without magento.cached")
	}
	if tr.Duration != 1985730159*time.Nanosecond {
		t.Errorf("Duration = %s", tr.Duration)
	}
	want := map[string]int{"sql": 324, "cache": 134, "search": 4, "queue": 0, "http": 0}
	for k, v := range want {
		if tr.Counts[k] != v {
			t.Errorf("Counts[%s] = %d, want %d", k, tr.Counts[k], v)
		}
	}
	root := rootOf(tr)
	if root == nil || root.Name != "magento.request" || root.Component != "magento" {
		t.Fatalf("root = %+v", root)
	}
	if root.Attrs["process_id"] != "4604" {
		t.Errorf("process_id attr = %q", root.Attrs["process_id"])
	}
	if tr.Spans[0].ParentID != "" {
		t.Errorf("first span is not the root: %+v", tr.Spans[0].Name)
	}
	ids := map[string]bool{}
	for _, s := range tr.Spans {
		ids[s.SpanID] = true
		if s.TraceID != tr.ID {
			t.Fatalf("span %s TraceID = %s", s.Name, s.TraceID)
		}
		if strings.HasPrefix(s.Attrs["http.url"], "http://inspector:8126/") {
			t.Fatal("profiler upload span kept")
		}
	}
	for _, s := range tr.Spans {
		if s.ParentID != "" && !ids[s.ParentID] {
			t.Fatalf("span %s has unknown parent %s", s.Name, s.ParentID)
		}
	}
	found := false
	for _, q := range tr.NPlusOne {
		if strings.Contains(q, "FROM `catalog_eav_attribute` WHERE (attribute_id = ?)") {
			found = true
		}
	}
	if !found {
		t.Errorf("NPlusOne misses the catalog_eav_attribute query: %q", tr.NPlusOne)
	}
	for _, s := range tr.Spans {
		if s.Name == "curl_exec" && s.Component != "search" {
			t.Errorf("curl_exec to %s classified %s", s.Resource, s.Component)
		}
	}
	var total time.Duration
	for _, d := range tr.Times {
		total += d
	}
	if total <= 0 || total > tr.Duration {
		t.Errorf("self times sum to %s, root %s", total, tr.Duration)
	}
}

func TestDecodeBatchOfThree(t *testing.T) {
	traces, err := decodeDD(fixture(t, "dd-batch3.msgpack"))
	if err != nil {
		t.Fatal(err)
	}
	keys := []string{
		"6ad7d78100000000d4c01c89ecced149",
		"6ad7d78100000000d57803a2d0f6699c",
		"6ad7d78100000000bee34c393d4d3d8e",
	}
	if len(traces) != len(keys) {
		t.Fatalf("got %d traces, want 3", len(traces))
	}
	for i, tr := range traces {
		if tr.ID != keys[i] {
			t.Errorf("trace %d ID = %s, want %s", i, tr.ID, keys[i])
		}
		if tr.Status != 500 {
			t.Errorf("trace %d Status = %d", i, tr.Status)
		}
		if tr.Route != "GET /gear" {
			t.Errorf("trace %d Route = %q", i, tr.Route)
		}
		root := rootOf(tr)
		if root == nil || !root.Error || !strings.Contains(root.Attrs["error.message"], "PDOException") {
			t.Errorf("trace %d root error not carried: %+v", i, root)
		}
	}
}

func TestDecodeErrorPage(t *testing.T) {
	traces, err := decodeDD(fixture(t, "dd-error.msgpack"))
	if err != nil {
		t.Fatal(err)
	}
	if len(traces) != 1 {
		t.Fatalf("got %d traces", len(traces))
	}
	tr := traces[0]
	if tr.ID != "6ad7d7330000000034151c295b19649f" || tr.Status != 500 {
		t.Errorf("ID %s status %d", tr.ID, tr.Status)
	}
	errs := 0
	for _, s := range tr.Spans {
		if s.Error {
			errs++
		}
		if s.Name == "PDOStatement.execute" && s.Attrs["db.user"] != "REDACTED" {
			t.Errorf("fixture not redacted: db.user = %q", s.Attrs["db.user"])
		}
	}
	if errs != 8 {
		t.Errorf("error spans = %d, want 8", errs)
	}
	if tr.Counts["cache"] == 0 {
		t.Error("no cache spans counted")
	}
	if s := spanByName(tr, "Redis.clearLastError"); s == nil || s.Component != "cache" {
		t.Errorf("Cm noise span missing or misclassified: %+v", s)
	}
}

func TestDecodeRawCLI(t *testing.T) {
	traces, err := decodeDD(fixture(t, "dd-cli.msgpack"))
	if err != nil {
		t.Fatal(err)
	}
	if len(traces) != 1 || len(traces[0].Spans) != 1 {
		t.Fatalf("got %+v", traces)
	}
	tr := traces[0]
	if tr.ID != "6ad7ccac00000000307a5c0388989a65" {
		t.Errorf("ID = %s", tr.ID)
	}
	s := tr.Spans[0]
	if s.Type != "cli" || s.Component != "php" || s.Error {
		t.Errorf("span = %+v", s)
	}
	if tr.Method != "" || tr.URL != "" || tr.Status != 0 {
		t.Errorf("CLI trace has http fields: %s %s %d", tr.Method, tr.URL, tr.Status)
	}
	if tr.Route != "Standard input code" {
		t.Errorf("Route = %q", tr.Route)
	}
	if s.Attrs["process_id"] != "3080" {
		t.Errorf("process_id = %q", s.Attrs["process_id"])
	}
}

func TestDecodeCutsAttributes(t *testing.T) {
	long := strings.Repeat("é", 3000)
	body, _ := msgpack.Marshal([][]ddSpan{{{
		TraceID:  7,
		SpanID:   7,
		Name:     "PDO.query",
		Resource: long,
		Type:     "sql",
		Meta:     map[string]string{"_dd.p.tid": "6ad7cccc00000000", "db.statement": long, "_dd.code_origin.frames.1.file": "/x.php"},
		Start:    1,
		Duration: 2,
	}}})
	traces, err := decodeDD(body)
	if err != nil {
		t.Fatal(err)
	}
	s := traces[0].Spans[0]
	if len(s.Attrs["db.statement"]) > maxAttr || len(s.Resource) > maxAttr {
		t.Errorf("not cut: %d %d", len(s.Attrs["db.statement"]), len(s.Resource))
	}
	if !strings.HasSuffix(s.Resource, "é") {
		t.Error("cut inside a rune")
	}
	if _, ok := s.Attrs["_dd.code_origin.frames.1.file"]; ok {
		t.Error("code origin frames kept")
	}
}

func TestDecodeReparentsAroundDroppedSpan(t *testing.T) {
	body, _ := msgpack.Marshal([][]ddSpan{{
		{TraceID: 1, SpanID: 1, Name: "magento.request", Type: "web", Start: 10, Duration: 100},
		{TraceID: 1, SpanID: 2, ParentID: 1, Name: "curl_exec", Type: "http", Start: 20, Duration: 10, Meta: map[string]string{"network.destination.name": "inspector"}},
		{TraceID: 1, SpanID: 3, ParentID: 2, Name: "child", Start: 21, Duration: 1},
	}})
	traces, err := decodeDD(body)
	if err != nil {
		t.Fatal(err)
	}
	tr := traces[0]
	if len(tr.Spans) != 2 {
		t.Fatalf("spans = %d", len(tr.Spans))
	}
	if c := spanByName(tr, "child"); c == nil || c.ParentID != tr.Spans[0].SpanID {
		t.Errorf("child not re-parented: %+v", c)
	}
}

func post(t *testing.T, h http.Handler, method, path, remote string, body []byte) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, bytes.NewReader(body))
	req.RemoteAddr = remote
	req.Header.Set("Content-Type", "application/msgpack")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestDDIntakeRoutes(t *testing.T) {
	s := NewStore(500, 256<<20)
	h := NewDDIntake(s, allowAll)

	rec := post(t, h, http.MethodGet, "/info", "127.0.0.1:5000", nil)
	var info struct {
		Endpoints []string `json:"endpoints"`
	}
	if rec.Code != 200 || json.Unmarshal(rec.Body.Bytes(), &info) != nil || len(info.Endpoints) != 1 || info.Endpoints[0] != "/v0.4/traces" {
		t.Errorf("/info = %d %s", rec.Code, rec.Body)
	}

	body := fixture(t, "dd-batch3.msgpack")
	for _, m := range []string{http.MethodPut, http.MethodPost} {
		rec = post(t, h, m, "/v0.4/traces", "172.20.0.5:40000", body)
		if rec.Code != 200 || strings.TrimSpace(rec.Body.String()) != `{"rate_by_service":{}}` {
			t.Errorf("%s /v0.4/traces = %d %s", m, rec.Code, rec.Body)
		}
	}
	if n := len(s.List(ListFilter{})); n != 3 {
		t.Errorf("store holds %d traces, want 3", n)
	}

	for path, code := range map[string]int{
		"/telemetry/proxy/api/v2/apmtelemetry": http.StatusAccepted,
		"/v0.7/config":                         http.StatusNotFound,
		"/v0.6/stats":                          http.StatusNotFound,
		"/v0.5/traces":                         http.StatusNotFound,
		"/nope":                                http.StatusNotFound,
	} {
		if rec = post(t, h, http.MethodPost, path, "127.0.0.1:1", []byte("{}")); rec.Code != code {
			t.Errorf("%s = %d, want %d", path, rec.Code, code)
		}
	}

	if rec = post(t, h, http.MethodPost, "/v0.4/traces", "127.0.0.1:1", []byte{0xc1}); rec.Code != http.StatusBadRequest {
		t.Errorf("garbage body = %d", rec.Code)
	}
}

func TestDDIntakeRejectsForeignIP(t *testing.T) {
	s := NewStore(500, 256<<20)
	var asked []string
	h := NewDDIntake(s, func(ip string) bool {
		asked = append(asked, ip)
		return ip == "127.0.0.1"
	})
	for _, path := range []string{"/v0.4/traces", "/profiles/6ad7d4d30000000025a83c7ee41d7769", "/info"} {
		rec := post(t, h, http.MethodPost, path, "10.0.0.9:1234", fixture(t, "dd-cli.msgpack"))
		if rec.Code != http.StatusForbidden {
			t.Errorf("%s from foreign ip = %d", path, rec.Code)
		}
	}
	rec := post(t, h, http.MethodPost, "/v0.4/traces", "[fe80::1%eth0]:1234", fixture(t, "dd-cli.msgpack"))
	if rec.Code != http.StatusForbidden {
		t.Errorf("ipv6 = %d", rec.Code)
	}
	if asked[len(asked)-1] != "fe80::1" {
		t.Errorf("allow got %q", asked[len(asked)-1])
	}
	if len(s.List(ListFilter{})) != 0 {
		t.Error("rejected trace stored")
	}
}

func TestDDIntakeBodyCap(t *testing.T) {
	s := NewStore(500, 256<<20)
	h := NewDDIntake(s, allowAll)
	big := make([]byte, maxBody+1)
	if rec := post(t, h, http.MethodPost, "/v0.4/traces", "127.0.0.1:1", big); rec.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("oversized trace body = %d", rec.Code)
	}
	req := httptest.NewRequest(http.MethodPost, "/v0.4/traces", io.NopCloser(io.MultiReader(bytes.NewReader([]byte{0x91}), bytes.NewReader(big))))
	req.RemoteAddr = "127.0.0.1:1"
	req.ContentLength = -1
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("oversized chunked body = %d", rec.Code)
	}
}

func TestDDIntakeProfiles(t *testing.T) {
	s := NewStore(500, 256<<20)
	h := NewDDIntake(s, allowAll)
	profile := fixture(t, "profile-6ad7d4d30000000025a83c7ee41d7769.json")

	rec := post(t, h, http.MethodPost, "/profiles/6AD7D4D30000000025A83C7EE41D7769", "127.0.0.1:1", profile)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("profile = %d %s", rec.Code, rec.Body)
	}
	rec = post(t, h, http.MethodPost, "/v0.4/traces", "127.0.0.1:1", fixture(t, "dd-category.msgpack"))
	if rec.Code != 200 {
		t.Fatalf("traces = %d", rec.Code)
	}
	tr, ok := s.Get("6ad7d4d30000000025a83c7ee41d7769")
	if !ok || !tr.HasProfile {
		t.Fatalf("trace %v hasProfile %v", ok, tr.HasProfile)
	}
	if got, ok := s.Profile("6ad7d4d30000000025a83c7ee41d7769"); !ok || !bytes.Equal(got, profile) {
		t.Error("profile not returned")
	}

	for id, code := range map[string]int{
		"6ad7d4d3":                                http.StatusBadRequest,
		"zzd7d4d30000000025a83c7ee41d7769":        http.StatusBadRequest,
		"6ad7d4d30000000025a83c7ee41d7769/../etc": http.StatusBadRequest,
	} {
		if rec = post(t, h, http.MethodPost, "/profiles/"+id, "127.0.0.1:1", profile); rec.Code != code {
			t.Errorf("profile id %q = %d, want %d", id, rec.Code, code)
		}
	}
	big := make([]byte, maxProfile+1)
	if rec = post(t, h, http.MethodPost, "/profiles/00000000000000000000000000000001", "127.0.0.1:1", big); rec.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("oversized profile = %d", rec.Code)
	}
	if rec = post(t, h, http.MethodGet, "/profiles/6ad7d4d30000000025a83c7ee41d7769", "127.0.0.1:1", nil); rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET profile on intake = %d", rec.Code)
	}
}

func emptySpans(n int) []byte {
	b := []byte{0x91, 0xdd, byte(n >> 24), byte(n >> 16), byte(n >> 8), byte(n)}
	return append(b, bytes.Repeat([]byte{0x80}, n)...)
}

func TestDDIntakeElementCeilings(t *testing.T) {
	h := NewDDIntake(NewStore(500, 256<<20), allowAll)
	if rec := post(t, h, http.MethodPut, "/v0.4/traces", "127.0.0.1:1", emptySpans(maxSpans+1)); rec.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("%d spans = %d %s", maxSpans+1, rec.Code, rec.Body)
	}
	if rec := post(t, h, http.MethodPut, "/v0.4/traces", "127.0.0.1:1", emptySpans(10)); rec.Code != http.StatusOK {
		t.Errorf("10 spans = %d %s", rec.Code, rec.Body)
	}

	n := maxElements + 1
	meta := []byte{0x91, 0x91, 0x81, 0xa4, 'm', 'e', 't', 'a', 0xdf, byte(n >> 24), byte(n >> 16), byte(n >> 8), byte(n)}
	meta = append(meta, bytes.Repeat([]byte{0xa0, 0xa0}, n)...)
	if rec := post(t, h, http.MethodPut, "/v0.4/traces", "127.0.0.1:1", meta); rec.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("%d meta entries = %d %s", n, rec.Code, rec.Body)
	}

	chunks := []byte{0xdd, byte(n >> 24), byte(n >> 16), byte(n >> 8), byte(n)}
	chunks = append(chunks, bytes.Repeat([]byte{0x90}, n)...)
	if rec := post(t, h, http.MethodPut, "/v0.4/traces", "127.0.0.1:1", chunks); rec.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("%d empty chunks = %d %s", n, rec.Code, rec.Body)
	}

	deep := append(bytes.Repeat([]byte{0x91}, 1000), 0x80)
	if rec := post(t, h, http.MethodPut, "/v0.4/traces", "127.0.0.1:1", deep); rec.Code != http.StatusBadRequest {
		t.Errorf("deep nesting = %d %s", rec.Code, rec.Body)
	}
}
