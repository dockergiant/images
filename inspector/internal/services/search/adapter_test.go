package search

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dockergiant/images/inspector/internal/capture"
	"github.com/dockergiant/images/inspector/internal/core"
	"github.com/dockergiant/images/inspector/internal/docker"
	"github.com/dockergiant/images/inspector/internal/services"
)

var osContainer = docker.Container{ID: "os1", Name: "shop-opensearch-1", Service: "opensearch", Project: "shop", Running: true}

func testAdapter(t *testing.T, f *fakeOS, stores storeSet) (*adapter, *docker.Fake) {
	t.Helper()
	d := &docker.Fake{EnvName: "shop", ContainerList: []docker.Container{osContainer}}
	a := newAdapter(services.Deps{Docker: d, Capture: capture.New(slog.New(slog.DiscardHandler))})
	a.baseURL = func(*docker.Container) string { return f.srv.URL }
	a.now = func() time.Time { return fixedNow }
	a.stores = func(context.Context) storeSet { return stores }
	a.pollEvery = 10 * time.Millisecond
	a.followEvery = 20 * time.Millisecond
	return a, d
}

var magentoStores = storeSet{Magento: true, Stores: map[int]string{0: "admin", 1: "default", 3: "en"}}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

type collector struct {
	mu     sync.Mutex
	events []core.Event
}

func (c *collector) emit(e core.Event) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.events = append(c.events, e)
}

func (c *collector) of(typ string) []core.Event {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []core.Event
	for _, e := range c.events {
		if e.Type == typ {
			out = append(out, e)
		}
	}
	return out
}

func TestDetectAbsent(t *testing.T) {
	a := newAdapter(services.Deps{Docker: &docker.Fake{}})
	info, err := a.Detect(context.Background())
	if err != nil || info.Present || info.Kind != core.Search {
		t.Fatalf("%+v %v", info, err)
	}
	h, err := a.Health(context.Background())
	if err != nil || h.Status != core.ToneInfo || len(h.Warnings) != 1 {
		t.Fatalf("%+v %v", h, err)
	}
}

func TestDetectPrefersOpenSearchAndReconciles(t *testing.T) {
	f := newFakeOS(t)
	f.addIndex("magento2_product_1_v1", 1, map[string]any{queryWarn: "0ms", fetchWarn: "0ms"})
	f.markerIndex = true
	f.markers["magento2_product_1_v1"] = map[string]any{"index": "magento2_product_1_v1", "prior": map[string]any{queryWarn: nil, fetchWarn: nil}}
	a, d := testAdapter(t, f, magentoStores)
	d.ContainerList = append(d.ContainerList, docker.Container{ID: "es1", Service: "elasticsearch", Running: true})

	info, err := a.Detect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !info.Present || info.Engine != EngineOpenSearch || info.Version != "3.5.0" || !info.Tested || !info.Features["insights"] || info.Container != osContainer.Name {
		t.Errorf("info %+v", info)
	}
	if len(f.settingsOf("magento2_product_1_v1")) != 0 || len(f.markerDocs()) != 0 {
		t.Errorf("reconcile did not run: %v %v", f.settingsOf("magento2_product_1_v1"), f.markerDocs())
	}
}

func TestDetectSkipsReconcileDuringCapture(t *testing.T) {
	f := newFakeOS(t)
	f.addIndex("magento2_product_1_v1", 1, map[string]any{queryWarn: "0ms"})
	f.markerIndex = true
	f.markers["magento2_product_1_v1"] = map[string]any{"index": "magento2_product_1_v1", "prior": map[string]any{}}
	a, _ := testAdapter(t, f, magentoStores)
	if err := a.deps.Capture.Begin(captureKey, time.Minute, func(context.Context) error { return nil }); err != nil {
		t.Fatal(err)
	}
	defer a.deps.Capture.EndAll(context.Background())
	if _, err := a.Detect(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(f.markerDocs()) != 1 {
		t.Error("reconcile ran while a capture was live")
	}
}

func TestBuildHealth35(t *testing.T) {
	var ch clusterHealth
	var stats nodeStats
	var nodes []map[string]any
	json.Unmarshal(fixture(t, "cluster-health-3.5.json"), &ch)
	json.Unmarshal(fixture(t, "nodes-stats-3.5.json"), &stats)
	json.Unmarshal(fixture(t, "cat-nodes-3.5.json"), &nodes)
	ix, _ := ParseCatIndices(fixture(t, "cat-indices-3.5.json"))
	al, _ := ParseCatAliases(fixture(t, "cat-aliases-3.5.json"))
	now := time.UnixMilli(1792527280614).Add(time.Hour)
	stores := storeSet{Magento: true, Stores: fixtureStores(t, "stores-3.5.json")}
	in := healthInput{Flavor: Flavor{EngineOpenSearch, "3.5.0"}, Insights: true, Cluster: ch, Stats: &stats, Nodes: nodes,
		Views: BuildIndexViews(ix, al, stores.Stores, true, now), Stores: stores, Now: now}
	h, next := buildHealth(in)
	if h.Status != core.OK {
		t.Errorf("status %s: %+v", h.Status, h.Warnings)
	}
	if h.Cards[0].Label != "Cluster" || h.Cards[0].Value != "yellow" || h.Cards[0].Sub != "replicas unassigned (normal on one node)" {
		t.Errorf("cluster card %+v", h.Cards[0])
	}
	if h.Summary != "OpenSearch 3.5.0 · yellow, replicas unassigned (normal on one node) · 8 indices (8 Magento)" {
		t.Errorf("summary %q", h.Summary)
	}
	labels := map[string]core.Card{}
	for _, c := range h.Cards {
		labels[c.Label] = c
	}
	if labels["Heap"].Value != "15%" || labels["Searches"].Value != "101" {
		t.Errorf("cards %+v", h.Cards)
	}
	if !strings.Contains(labels["Host"].Sub, "host-wide") || labels["Host"].Value != "load 26.65" {
		t.Errorf("host card %+v", labels["Host"])
	}

	in.Prev = next
	in.Now = now.Add(10 * time.Second)
	more := stats
	more.Nodes = map[string]nodeStat{}
	for k, n := range stats.Nodes {
		n.Indices.Search.QueryTotal += 20
		n.Indices.Search.QueryTime += 600
		more.Nodes[k] = n
	}
	in.Stats = &more
	h, _ = buildHealth(in)
	for _, c := range h.Cards {
		if c.Label == "Searches" && (c.Value != "2.0/s" || c.Sub != "avg 30 ms per query") {
			t.Errorf("rate card %+v", c)
		}
	}
}

func TestBuildHealthWarnings(t *testing.T) {
	var ch clusterHealth
	json.Unmarshal(fixture(t, "cluster-health-2.0.json"), &ch)
	ix, _ := ParseCatIndices(fixture(t, "cat-indices-3.5.json"))
	al, _ := ParseCatAliases(fixture(t, "cat-aliases-3.5.json"))
	stores := storeSet{Magento: true, Stores: map[int]string{1: "default", 3: "en", 18: "outlet_nl", 19: "outlet_de"}}
	now := time.UnixMilli(1792527280614).Add(time.Hour)
	h, _ := buildHealth(healthInput{Flavor: Flavor{EngineOpenSearch, "2.0.1"}, Cluster: ch, Views: BuildIndexViews(ix, al, stores.Stores, true, now), Stores: stores, Capturing: true, Now: now})
	if h.Cards[0].Value != "green" || h.Cards[0].Tone != core.OK {
		t.Errorf("green card %+v", h.Cards[0])
	}
	ids := map[string]bool{}
	for _, w := range h.Warnings {
		ids[w.ID] = true
	}
	if !ids["search.orphans"] || !ids["search.capture"] || !ids["search.insights"] || h.Status != core.Warn {
		t.Errorf("warnings %+v status %s", h.Warnings, h.Status)
	}

	ch.Status, ch.NumberOfNodes, ch.UnassignedShards = "red", 3, 2
	h, _ = buildHealth(healthInput{Flavor: Flavor{EngineElasticsearch, "7.8.1"}, Cluster: ch})
	if h.Status != core.Error || h.Cards[0].Sub != "2 shards unassigned" || !strings.HasPrefix(h.Summary, "Elasticsearch 7.8.1") {
		t.Errorf("red: %+v", h)
	}
}

func TestHealthAgainstServer(t *testing.T) {
	f := newFakeOS(t)
	f.addIndex("magento2_product_1_v1", 1, nil)
	f.setAlias("magento2_product_1", "magento2_product_1_v1")
	a, _ := testAdapter(t, f, magentoStores)
	h, err := a.Health(context.Background())
	if err != nil || len(h.Cards) != 5 || h.Raw == nil {
		t.Fatalf("%+v %v", h, err)
	}
	if len(f.noOpaque) != 0 {
		t.Errorf("requests without X-Opaque-Id: %v", f.noOpaque)
	}
}

func browseMux(a *adapter) *http.ServeMux {
	mux := http.NewServeMux()
	for _, r := range a.Browse() {
		mux.HandleFunc(r.Method+" "+r.Path, r.Handler)
	}
	return mux
}

func call(t *testing.T, mux http.Handler, method, path string, body any) (int, map[string]any) {
	t.Helper()
	var rd *bytes.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	} else {
		rd = bytes.NewReader(nil)
	}
	req := httptest.NewRequest(method, path, rd)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	var out map[string]any
	json.Unmarshal(rec.Body.Bytes(), &out)
	return rec.Code, out
}

func TestBrowseIndices(t *testing.T) {
	f := newFakeOS(t)
	f.addIndex("magento2_product_1_v1", fixedNow.Add(-time.Hour).UnixMilli(), nil)
	f.addIndex("magento2_product_9_v1", fixedNow.Add(-time.Hour).UnixMilli(), nil)
	f.addIndex(".plugins-ml-config", 1, nil)
	f.setAlias("magento2_product_1", "magento2_product_1_v1")
	a, _ := testAdapter(t, f, magentoStores)
	code, out := call(t, browseMux(a), "GET", "/indices", nil)
	if code != 200 {
		t.Fatalf("%d %v", code, out)
	}
	views, _ := out["indices"].([]any)
	if len(views) != 3 {
		t.Fatalf("views %v", out)
	}
	first := views[0].(map[string]any)
	label := first["label"].(map[string]any)
	if first["index"] != "magento2_product_1_v1" || label["title"] != "Products · default" || first["orphan"] != false {
		t.Errorf("first %v", first)
	}
	second := views[1].(map[string]any)
	if second["index"] != "magento2_product_9_v1" || second["orphan"] != true {
		t.Errorf("second %v", second)
	}
}

func TestBrowseRejectsUnknownIndex(t *testing.T) {
	f := newFakeOS(t)
	f.addIndex("magento2_product_1_v1", 1, nil)
	a, _ := testAdapter(t, f, magentoStores)
	mux := browseMux(a)
	for _, name := range []string{"_all", "*", "magento2_product_1_v1,other", "missing"} {
		code, _ := call(t, mux, "GET", "/index/"+strings.ReplaceAll(name, "*", "%2A")+"/mapping", nil)
		if code != http.StatusNotFound {
			t.Errorf("%s: %d", name, code)
		}
	}
	code, _ := call(t, mux, "POST", "/search", map[string]any{"index": "_all", "term": "x"})
	if code != http.StatusNotFound {
		t.Errorf("search _all: %d", code)
	}
	if len(f.requests("GET", "/_all")) != 0 || len(f.requests("POST", "/_all")) != 0 || len(f.requests("GET", "/missing")) != 0 {
		t.Error("an unknown index reached the engine")
	}
}

func TestBrowseMapping(t *testing.T) {
	f := newFakeOS(t)
	f.addIndex("magento2_product_1_v1", 1, nil)
	f.setAlias("magento2_product_1", "magento2_product_1_v1")
	a, _ := testAdapter(t, f, magentoStores)
	code, out := call(t, browseMux(a), "GET", "/index/magento2_product_1/mapping?field=name", nil)
	if code != 200 {
		t.Fatalf("%d %v", code, out)
	}
	fields := out["fields"].([]any)
	if len(fields) != 2 || fields[0].(map[string]any)["field"] != "category.name" || fields[1].(map[string]any)["copyTo"].([]any)[0] != "_search" {
		t.Errorf("fields %v", fields)
	}
}

func TestBrowseSearch(t *testing.T) {
	f := newFakeOS(t)
	f.addIndex("magento2_product_1_v1", 1, nil)
	f.setAlias("magento2_product_1", "magento2_product_1_v1")
	a, _ := testAdapter(t, f, magentoStores)
	mux := browseMux(a)
	replay := `{"from":0,"size":100,"query":{"match_all":{}}}`
	code, out := call(t, mux, "POST", "/search", map[string]any{"index": "magento2_product_1", "body": replay, "profile": true, "explain": true})
	if code != 200 {
		t.Fatalf("%d %v", code, out)
	}
	reqs := f.requests("POST", "/magento2_product_1/_search")
	if len(reqs) != 1 || reqs[0].Query != "request_cache=false" {
		t.Fatalf("requests %+v", reqs)
	}
	b := reqs[0].Body
	if b["size"] != float64(20) || b["timeout"] != "5s" || b["profile"] != true || b["explain"] != true || b["from"] != float64(0) {
		t.Errorf("body %v", b)
	}
	code, _ = call(t, mux, "POST", "/search", map[string]any{"index": "magento2_product_1", "term": "jacket"})
	reqs = f.requests("POST", "/magento2_product_1/_search")
	if code != 200 || len(reqs) != 2 {
		t.Fatalf("%d %+v", code, reqs)
	}
	q, _ := json.Marshal(reqs[1].Body["query"])
	if !strings.Contains(string(q), `"_search":{"query":"jacket"}`) {
		t.Errorf("term query %s", q)
	}
	code, _ = call(t, mux, "POST", "/search", map[string]any{"index": "magento2_product_1"})
	if code != http.StatusBadRequest {
		t.Errorf("empty search: %d", code)
	}
}

func TestBuildSearch(t *testing.T) {
	cases := []struct {
		body string
		size int
		want float64
	}{
		{`{"size":100}`, 0, 20},
		{`{"size":12}`, 0, 12},
		{`{"size":0}`, 0, 0},
		{`{}`, 0, 20},
		{`{}`, 5, 5},
		{`{"size":12}`, 50, 12},
	}
	for _, c := range cases {
		q, err := buildSearch(json.RawMessage(c.body), "", c.size, false, false, true)
		if err != nil || q["size"] != c.want && q["size"] != int(c.want) {
			t.Errorf("%s size=%d: %v %v", c.body, c.size, q["size"], err)
		}
	}
	if _, err := buildSearch(json.RawMessage(`[1]`), "", 0, false, false, true); err == nil {
		t.Error("array body accepted")
	}
	q, _ := buildSearch(nil, "x", 0, false, false, false)
	if !reflect.DeepEqual(q["query"], map[string]any{"query_string": map[string]any{"query": "x"}}) {
		t.Errorf("generic term %v", q["query"])
	}
}

func TestDeleteOrphan(t *testing.T) {
	f := newFakeOS(t)
	old := fixedNow.Add(-time.Hour).UnixMilli()
	f.addIndex("magento2_product_1_v1", old, nil)
	f.addIndex("magento2_product_1_v0", old, nil)
	f.addIndex("magento2_product_9_v1", old, nil)
	f.addIndex("magento2_product_8_v1", fixedNow.Add(-time.Minute).UnixMilli(), nil)
	f.setAlias("magento2_product_1", "magento2_product_1_v1")
	a, _ := testAdapter(t, f, magentoStores)
	act := a.Actions()[0]
	if act.Name != "delete-orphan" || act.Confirm != core.ConfirmTyped || !act.Destructive {
		t.Fatalf("spec %+v", act)
	}
	ctx := context.Background()
	for _, target := range []string{"magento2_product_1_v1", "magento2_product_1_v0", "magento2_product_8_v1", "magento2_product_1", "_all", ""} {
		if _, err := act.Run(ctx, core.ActionRequest{Target: target}); err == nil {
			t.Errorf("%q deleted", target)
		}
	}
	if len(f.requests("DELETE", "/")) != 0 {
		t.Fatal("a refused delete reached the engine")
	}
	res, err := act.Run(ctx, core.ActionRequest{Target: "magento2_product_9_v1"})
	if err != nil || !strings.Contains(res.Message, "Products · store 9") {
		t.Fatalf("%+v %v", res, err)
	}
	if _, ok := f.indices["magento2_product_9_v1"]; ok {
		t.Error("orphan still there")
	}

	a.stores = func(context.Context) storeSet { return storeSet{Magento: true} }
	f.addIndex("magento2_product_7_v1", old, nil)
	if _, err := act.Run(ctx, core.ActionRequest{Target: "magento2_product_7_v1"}); err == nil {
		t.Error("deleted without a store list")
	}
}

func runStream(t *testing.T, a *adapter, name string, filter core.Filter) (*collector, context.CancelFunc, chan error) {
	t.Helper()
	var spec core.StreamSpec
	for _, s := range a.Streams() {
		if s.Name == name {
			spec = s
		}
	}
	if spec.Start == nil {
		t.Fatalf("no stream %s", name)
	}
	ctx, cancel := context.WithCancel(context.Background())
	c := &collector{}
	done := make(chan error, 1)
	go func() { done <- spec.Start(ctx, filter, c.emit) }()
	t.Cleanup(cancel)
	return c, cancel, done
}

func TestStreamSpecs(t *testing.T) {
	a := newAdapter(services.Deps{})
	var names []string
	for _, s := range a.Streams() {
		names = append(names, s.Name)
		if s.OptIn != (s.Name == "capture") {
			t.Errorf("%s opt-in %v", s.Name, s.OptIn)
		}
	}
	if strings.Join(names, ",") != "insights,slowlog,capture" {
		t.Errorf("streams %v", names)
	}
}

func TestInsightsStream(t *testing.T) {
	f := newFakeOS(t)
	f.insights = fixture(t, "insights-2.19.json")
	a, _ := testAdapter(t, f, magentoStores)
	c, cancel, done := runStream(t, a, "insights", nil)
	eventually(t, "three polls", func() bool { return len(f.requests("GET", "/_insights/top_queries")) >= 3 })
	cancel()
	<-done
	evs := c.of("search.query")
	if len(evs) != 1 {
		t.Fatalf("events %+v", c.events)
	}
	q := evs[0].Data.(core.SearchQuery)
	if q.OpaqueID != "storefront" || q.Label != "Products · default" || q.Source != "insights" || evs[0].Kind != core.Search {
		t.Errorf("query %+v", q)
	}
}

func TestInsightsStreamUnavailable(t *testing.T) {
	f := newFakeOS(t)
	f.insightsErr = http.StatusBadRequest
	a, _ := testAdapter(t, f, magentoStores)
	c, cancel, done := runStream(t, a, "insights", nil)
	eventually(t, "notice", func() bool { return len(c.of("stream.error")) == 1 })
	time.Sleep(50 * time.Millisecond)
	select {
	case err := <-done:
		t.Fatalf("stream returned early: %v", err)
	default:
	}
	if n := len(f.requests("GET", "/_insights/top_queries")); n != 1 {
		t.Errorf("polled %d times", n)
	}
	cancel()
	<-done
}

func logLines(t *testing.T, name string) []docker.LogLine {
	var out []docker.LogLine
	for _, l := range fixtureLogLines(t, name) {
		out = append(out, docker.LogLine{Time: l.Time, Stream: 1, Line: l.Line})
	}
	return out
}

func TestSlowlogStream(t *testing.T) {
	f := newFakeOS(t)
	a, d := testAdapter(t, f, storeSet{Magento: true, Stores: map[int]string{1: "default"}})
	d.LogLines = map[string][]docker.LogLine{"os1": logLines(t, "slowlog-3.5.txt")}
	c, cancel, done := runStream(t, a, "slowlog", nil)
	eventually(t, "events", func() bool { return len(c.of("search.query")) == 4 })
	cancel()
	<-done
	for _, e := range c.of("search.query") {
		q := e.Data.(core.SearchQuery)
		if q.OpaqueID == opaqueID || q.Label != "Products · default" || q.Index != "magento2_product_1_v1" {
			t.Errorf("query %+v", q)
		}
	}

	c, cancel, done = runStream(t, a, "slowlog", core.Filter{"q": "MATCH_NONE"})
	eventually(t, "filtered events", func() bool { return len(c.of("search.query")) == 2 })
	cancel()
	<-done
}

func TestSlowlogEndIsAnError(t *testing.T) {
	f := newFakeOS(t)
	a, d := testAdapter(t, f, magentoStores)
	d.LogsFunc = func(context.Context, string, time.Time, bool, func(docker.LogLine)) error { return nil }
	_, _, done := runStream(t, a, "slowlog", nil)
	select {
	case err := <-done:
		if err == nil || errors.Is(err, context.Canceled) {
			t.Fatalf("a slowlog that ends returned %v, want an error the hub restarts on", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("slowlog stream did not return after its log ended")
	}
}

func TestCaptureStream(t *testing.T) {
	f := newFakeOS(t)
	f.addIndex("magento2_product_1_v1", 1, map[string]any{queryInfo: "5s"})
	f.setAlias("magento2_product_1", "magento2_product_1_v1")
	a, d := testAdapter(t, f, magentoStores)
	d.LogLines = map[string][]docker.LogLine{"os1": logLines(t, "slowlog-2.0.txt")}

	c, cancel, done := runStream(t, a, "capture", nil)
	eventually(t, "capture on", func() bool { return len(c.of("search.capture")) == 1 })
	if !a.deps.Capture.Active(captureKey) {
		t.Error("no capture session")
	}
	if st := f.settingsOf("magento2_product_1_v1"); st[queryWarn] != "0ms" || st[fetchWarn] != "0ms" {
		t.Errorf("not captured: %v", st)
	}
	state := c.of("search.capture")[0].Data.(captureState)
	if !state.Capturing || !state.Until.Equal(fixedNow.Add(30*time.Minute)) || len(state.Indices) != 1 {
		t.Errorf("state %+v", state)
	}
	eventually(t, "slowlog events", func() bool { return len(c.of("search.query")) == 2 })

	f.addIndex("magento2_product_1_v2", 2, nil)
	f.setAlias("magento2_product_1", "magento2_product_1_v2")
	eventually(t, "alias follow", func() bool { return f.settingsOf("magento2_product_1_v2")[queryWarn] == "0ms" })

	cancel()
	if err := <-done; err != context.Canceled {
		t.Errorf("returned %v", err)
	}
	if a.deps.Capture.Active(captureKey) {
		t.Error("session still active")
	}
	if st := f.settingsOf("magento2_product_1_v1"); !reflect.DeepEqual(st, map[string]any{queryInfo: "5s"}) {
		t.Errorf("v1 not restored: %v", st)
	}
	if st := f.settingsOf("magento2_product_1_v2"); len(st) != 0 {
		t.Errorf("v2 not restored: %v", st)
	}
	if len(f.markerDocs()) != 0 {
		t.Errorf("markers left: %v", f.markerDocs())
	}
}

func TestCaptureStreamEndedByManager(t *testing.T) {
	f := newFakeOS(t)
	f.addIndex("magento2_product_1_v1", 1, nil)
	f.setAlias("magento2_product_1", "magento2_product_1_v1")
	a, _ := testAdapter(t, f, magentoStores)
	c, cancel, done := runStream(t, a, "capture", nil)
	eventually(t, "capture on", func() bool { return len(c.of("search.capture")) == 1 })

	if err := a.deps.Capture.End(context.Background(), captureKey); err != nil {
		t.Fatal(err)
	}
	eventually(t, "capture off", func() bool { return len(c.of("search.capture")) == 2 })
	if st := c.of("search.capture")[1].Data.(captureState); st.Capturing {
		t.Errorf("state %+v", st)
	}
	if len(f.settingsOf("magento2_product_1_v1")) != 0 {
		t.Error("not restored")
	}
	select {
	case err := <-done:
		t.Fatalf("stream returned and would restart a capture: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	cancel()
	<-done
}

func TestCaptureStreamBusy(t *testing.T) {
	f := newFakeOS(t)
	f.addIndex("magento2_product_1_v1", 1, nil)
	f.setAlias("magento2_product_1", "magento2_product_1_v1")
	a, _ := testAdapter(t, f, magentoStores)
	a.deps.Capture.Begin(captureKey, time.Minute, func(context.Context) error { return nil })
	defer a.deps.Capture.EndAll(context.Background())
	_, _, done := runStream(t, a, "capture", nil)
	if err := <-done; err == nil {
		t.Fatal("second capture started")
	}
	if len(f.settingsOf("magento2_product_1_v1")) != 0 || len(f.markerDocs()) != 0 {
		t.Error("refused capture left state behind")
	}
	if n := len(f.requests("PUT", "/")); n != 0 {
		t.Errorf("refused capture wrote %d requests", n)
	}
}

func TestCaptureStreamNothingToCapture(t *testing.T) {
	f := newFakeOS(t)
	f.addIndex(".plugins-ml-config", 1, nil)
	a, _ := testAdapter(t, f, magentoStores)
	c, cancel, done := runStream(t, a, "capture", nil)
	eventually(t, "notice", func() bool { return len(c.of("stream.error")) == 1 })
	select {
	case err := <-done:
		t.Fatalf("stream returned and would be restarted: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	if a.deps.Capture.Active(captureKey) {
		t.Error("session started")
	}
	cancel()
	<-done
}
