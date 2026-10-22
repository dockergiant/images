package api

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"testing/fstest"
	"time"

	"github.com/dockergiant/images/inspector/clients"
	"github.com/dockergiant/images/inspector/internal/apm"
	"github.com/dockergiant/images/inspector/internal/core"
	"github.com/dockergiant/images/inspector/internal/timeline"
)

// The origins a userscript, a DevTools panel, a sandboxed frame and curl send.
var foreignOrigins = []string{"https://evil.test", "null", "chrome-extension://abc", "https://app.shop.test", ""}

func v1Server(t *testing.T, adapters ...core.Adapter) *Server {
	t.Helper()
	return New(Config{
		Env:      "shop",
		Domain:   testDomain,
		Secret:   testSecret,
		Version:  "test",
		Docker:   fakeDocker(traefikAddr),
		Adapters: adapters,
		Clients:  testClients(),
		UI:       testUI(),
		Log:      quietLog(),
	})
}

func testClients() fstest.MapFS {
	return fstest.MapFS{
		"userscript/roll-inspector.user.js": {Data: []byte("// ==UserScript==\n// @match https://*.__ROLL_DOMAIN__/*\n// @connect inspector.__ROLL_DOMAIN__\n// ==/UserScript==\nconst DOMAIN = '__ROLL_DOMAIN__';\n")},
		"extension/manifest.json":           {Data: []byte(`{"manifest_version":3,"name":"Roll Inspector","version":"1.0.0"}`)},
		"extension/panel.html":              {Data: []byte("<!doctype html><link rel=stylesheet href=tokens.css>")},
		"extension/lib/core.js":             {Data: []byte("// core")},
	}
}

func testUI() fstest.MapFS {
	return fstest.MapFS{
		"index.html":                {Data: []byte("<!doctype html><title>Inspector</title>")},
		"app.css":                   {Data: []byte("*,:before{box-sizing:border-box}:root{color-scheme:dark;--ground:#101418;--chalk:#e4e8ec}.x{color:red}")},
		"fonts/plex-400.woff2":      {Data: []byte("wOF2plex")},
		"fonts/jetbrains-400.woff2": {Data: []byte("wOF2mono")},
		"fonts/OFL.txt":             {Data: []byte("licence")},
	}
}

func fromOrigin(method, path, origin string) *http.Request {
	r := browserReq(method, path, nil)
	if origin != "" {
		r.Header.Set("Origin", origin)
		r.Header.Set("Sec-Fetch-Site", "cross-site")
	}
	return r
}

func checkCORS(t *testing.T, name string, h http.Header) {
	t.Helper()
	if h.Get("Access-Control-Allow-Origin") != "*" {
		t.Errorf("%s: Access-Control-Allow-Origin %q", name, h.Get("Access-Control-Allow-Origin"))
	}
	if h.Get("Access-Control-Expose-Headers") != "*" {
		t.Errorf("%s: Access-Control-Expose-Headers %q", name, h.Get("Access-Control-Expose-Headers"))
	}
	if v := h.Get("Access-Control-Allow-Credentials"); v != "" {
		t.Errorf("%s: Access-Control-Allow-Credentials %q", name, v)
	}
}

func TestV1AnswersEveryOrigin(t *testing.T) {
	s := v1Server(t)
	for _, origin := range foreignOrigins {
		for _, path := range []string{"/api/v1/", "/api/v1", "/api/v1/openapi.json", "/api/v1/hotspots", "/api/v1/traces", "/api/v1/requests", "/api/v1/overview"} {
			w := serve(s, fromOrigin(http.MethodGet, path, origin))
			name := path + " from " + origin
			if w.Code != http.StatusOK {
				t.Errorf("%s: status %d (%s)", name, w.Code, strings.TrimSpace(w.Body.String()))
				continue
			}
			checkCORS(t, name, w.Header())
			if w.Header().Get("Cache-Control") != "no-store" {
				t.Errorf("%s: Cache-Control %q", name, w.Header().Get("Cache-Control"))
			}
		}
	}
}

func TestV1Preflight(t *testing.T) {
	s := v1Server(t)
	for _, origin := range foreignOrigins {
		r := fromOrigin(http.MethodOptions, "/api/v1/page?url=x", origin)
		r.Header.Set("Access-Control-Request-Method", "GET")
		r.Header.Set("Access-Control-Request-Headers", "x-requested-with")
		w := serve(s, r)
		if w.Code != http.StatusNoContent {
			t.Fatalf("preflight from %q: status %d (%s)", origin, w.Code, w.Body)
		}
		checkCORS(t, "preflight from "+origin, w.Header())
		h := w.Header()
		if h.Get("Access-Control-Allow-Methods") != "GET, HEAD, OPTIONS" || h.Get("Access-Control-Allow-Headers") != "*" || h.Get("Access-Control-Max-Age") != "600" {
			t.Fatalf("preflight headers %v", h)
		}
	}
}

func TestV1RefusesEveryOtherMethod(t *testing.T) {
	runs := &runLog{}
	peeked := atomic.Int32{}
	redis := &fakeAdapter{kind: core.Redis, present: true, actions: []core.ActionSpec{{Name: "noop", Run: runs.run}}}
	mq := &fakeAdapter{kind: core.RabbitMQ, present: true, routes: []core.Route{{Method: http.MethodPost, Path: "/peek", Handler: func(w http.ResponseWriter, r *http.Request) {
		peeked.Add(1)
		writeJSON(w, http.StatusOK, map[string]string{"peeked": "yes"})
	}}}}
	s := v1Server(t, redis, mq)
	for _, m := range []string{http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete, "PURGE"} {
		for _, path := range []string{"/api/v1/", "/api/v1/actions/redis/noop", "/api/v1/rabbitmq/peek", "/api/v1/confirm"} {
			// Even a request that carries everything /api asks of a write.
			r := writeReq(s, path, map[string]any{"queue": "q"})
			r.Method = m
			w := serve(s, r)
			if w.Code != http.StatusMethodNotAllowed {
				t.Errorf("%s %s: status %d", m, path, w.Code)
			}
			if w.Header().Get("Allow") != "GET, HEAD, OPTIONS" {
				t.Errorf("%s %s: Allow %q", m, path, w.Header().Get("Allow"))
			}
			checkCORS(t, m+" "+path, w.Header())
		}
	}
	if runs.count() != 0 || peeked.Load() != 0 {
		t.Fatalf("a v1 request ran an action (%d) or a peek (%d)", runs.count(), peeked.Load())
	}
	if w := serve(s, browserReq(http.MethodGet, "/api/v1/rabbitmq/peek", nil)); w.Code != http.StatusNotFound || peeked.Load() != 0 {
		t.Fatalf("GET peek on v1: status %d, peeked %d", w.Code, peeked.Load())
	}
	if w := serve(s, browserReq(http.MethodHead, "/api/v1/", nil)); w.Code != http.StatusOK {
		t.Fatalf("HEAD: status %d", w.Code)
	}
}

func TestV1LeavesTheRestOfAPIClosed(t *testing.T) {
	runs := &runLog{}
	redis := &fakeAdapter{kind: core.Redis, present: true, actions: []core.ActionSpec{{Name: "noop", Run: runs.run}}}
	s := v1Server(t, redis)
	for _, origin := range []string{"https://evil.test", "null", "chrome-extension://abc", "https://app.shop.test"} {
		w := serve(s, fromOrigin(http.MethodGet, "/api/session", origin))
		if w.Code != http.StatusForbidden || w.Header().Get("Access-Control-Allow-Origin") != "" {
			t.Errorf("GET /api/session from %s: status %d, ACAO %q", origin, w.Code, w.Header().Get("Access-Control-Allow-Origin"))
		}
		r := writeReq(s, "/api/actions/redis/noop", map[string]any{})
		r.Header.Set("Origin", origin)
		if w := serve(s, r); w.Code != http.StatusForbidden {
			t.Errorf("write from %s: status %d", origin, w.Code)
		}
		r = fromOrigin(http.MethodOptions, "/api/actions/redis/noop", origin)
		if w := serve(s, r); w.Code == http.StatusNoContent || w.Header().Get("Access-Control-Allow-Origin") != "" {
			t.Errorf("preflight on /api from %s answered with CORS: %d", origin, w.Code)
		}
	}
	// A path that only looks like v1 until it is cleaned stays under the /api rules.
	r := fromOrigin(http.MethodGet, "/api/session", "https://evil.test")
	r.URL.Path = "/api/v1/../session"
	if w := serve(s, r); w.Header().Get("Access-Control-Allow-Origin") != "" {
		t.Fatalf("dot-dot path got CORS: %d", w.Code)
	}
	r = writeReq(s, "/api/actions/redis/noop", map[string]any{})
	r.URL.Path = "/api/v1/../actions/redis/noop"
	r.Header.Set("Origin", "https://evil.test")
	serve(s, r)
	if runs.count() != 0 {
		t.Fatal("a dot-dot path ran a write cross-origin")
	}
	// The page's own writes still work.
	if w := serve(s, writeReq(s, "/api/actions/redis/noop", map[string]any{})); w.Code != http.StatusOK {
		t.Fatalf("same-origin write: status %d (%s)", w.Code, w.Body)
	}
}

func TestV1StillChecksHostAndGate(t *testing.T) {
	s := v1Server(t)
	r := fromOrigin(http.MethodGet, "/api/v1/", "https://evil.test")
	r.Host = "app.shop.test"
	if w := serve(s, r); w.Code != http.StatusMisdirectedRequest {
		t.Fatalf("wrong host: status %d", w.Code)
	}
	r = fromOrigin(http.MethodGet, "/api/v1/", "https://evil.test")
	r.Header.Del(gateHeader)
	if w := serve(s, r); w.Code != http.StatusForbidden {
		t.Fatalf("no gate header: status %d", w.Code)
	}
}

type v1Index struct {
	Env          string `json:"env"`
	Domain       string `json:"domain"`
	Version      string `json:"version"`
	InspectorURL string `json:"inspectorUrl"`
	API          int    `json:"api"`
	Endpoints    []struct {
		Method  string `json:"method"`
		Path    string `json:"path"`
		Summary string `json:"summary"`
	} `json:"endpoints"`
}

func mirrorAdapters(searched *atomic.Value) []core.Adapter {
	ok := func(name string) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			writeJSON(w, http.StatusOK, map[string]string{"route": name, "name": r.PathValue("name"), "q": r.URL.Query().Get("q")})
		}
	}
	redis := &fakeAdapter{kind: core.Redis, present: true, routes: []core.Route{
		{Method: http.MethodGet, Path: "/keys", Handler: ok("keys")},
		{Method: http.MethodGet, Path: "/key", Handler: ok("key")},
	}}
	search := &fakeAdapter{kind: core.Search, present: true, routes: []core.Route{
		{Method: http.MethodGet, Path: "/index/{name}/mapping", Handler: ok("mapping")},
		{Method: http.MethodPost, Path: "/search", Handler: func(w http.ResponseWriter, r *http.Request) {
			body, _ := io.ReadAll(r.Body)
			searched.Store(r.Method + " " + r.Header.Get("Content-Type") + " " + string(body))
			writeJSON(w, http.StatusOK, map[string]any{"hits": []string{"doc"}})
		}},
	}}
	mq := &fakeAdapter{kind: core.RabbitMQ, present: true, routes: []core.Route{
		{Method: http.MethodGet, Path: "/queues", Handler: ok("queues")},
		{Method: http.MethodPost, Path: "/peek", Handler: ok("peek")},
	}}
	return []core.Adapter{redis, search, mq}
}

func TestV1MirrorsTheAdapterReads(t *testing.T) {
	var searched atomic.Value
	s := v1Server(t, mirrorAdapters(&searched)...)
	for _, c := range []struct{ v1, api string }{
		{"/api/v1/redis/keys?q=cat", "/api/redis/keys?q=cat"},
		{"/api/v1/redis/info", "/api/redis/info"},
		{"/api/v1/redis/health", "/api/redis/health"},
		{"/api/v1/search/index/magento2_product_1/mapping", "/api/search/index/magento2_product_1/mapping"},
		{"/api/v1/rabbitmq/queues", "/api/rabbitmq/queues"},
	} {
		w := serve(s, fromOrigin(http.MethodGet, c.v1, "https://evil.test"))
		ref := serve(s, browserReq(http.MethodGet, c.api, nil))
		if w.Code != http.StatusOK || w.Body.String() != ref.Body.String() {
			t.Errorf("%s: %d %q, /api gave %q", c.v1, w.Code, w.Body, ref.Body)
		}
	}
	body := `{"index":"magento2_product_1","term":"shirt"}`
	w := serve(s, fromOrigin(http.MethodGet, "/api/v1/search/search?body="+url.QueryEscape(body), "https://evil.test"))
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "doc") {
		t.Fatalf("search read: %d %s", w.Code, w.Body)
	}
	if got := searched.Load(); got != "POST application/json "+body {
		t.Fatalf("search handler saw %q", got)
	}
	if w := serve(s, fromOrigin(http.MethodGet, "/api/v1/search/search", "https://evil.test")); w.Code != http.StatusBadRequest {
		t.Fatalf("search without body: %d", w.Code)
	}

	w = serve(s, browserReq(http.MethodGet, "/api/v1/", nil))
	var idx v1Index
	if err := json.Unmarshal(w.Body.Bytes(), &idx); err != nil {
		t.Fatal(err)
	}
	if idx.Env != "shop" || idx.Domain != testDomain || idx.Version != "test" || idx.API != 1 || idx.InspectorURL != testOrigin {
		t.Fatalf("index %+v", idx)
	}
	paths := map[string]bool{}
	for _, e := range idx.Endpoints {
		if e.Method != http.MethodGet || e.Summary == "" {
			t.Errorf("endpoint %+v", e)
		}
		paths[e.Path] = true
	}
	for _, p := range []string{"/api/v1/", "/api/v1/openapi.json", "/api/v1/page", "/api/v1/stream", "/api/v1/traces/{id}", "/api/v1/hotspots", "/api/v1/redis/keys", "/api/v1/search/index/{name}/mapping", "/api/v1/search/search", "/api/v1/rabbitmq/queues", "/api/v1/rabbitmq/health"} {
		if !paths[p] {
			t.Errorf("index lacks %s", p)
		}
	}
	if paths["/api/v1/rabbitmq/peek"] {
		t.Error("index lists the peek, which requeues messages")
	}
}

func TestV1OpenAPIComesFromTheRouteTable(t *testing.T) {
	var searched atomic.Value
	s := v1Server(t, mirrorAdapters(&searched)...)
	w := serve(s, fromOrigin(http.MethodGet, "/api/v1/openapi.json", "null"))
	if w.Code != http.StatusOK {
		t.Fatalf("status %d", w.Code)
	}
	var doc struct {
		OpenAPI string `json:"openapi"`
		Info    struct {
			Title   string `json:"title"`
			Version string `json:"version"`
		} `json:"info"`
		Servers []struct {
			URL string `json:"url"`
		} `json:"servers"`
		Paths map[string]map[string]struct {
			Summary     string `json:"summary"`
			OperationID string `json:"operationId"`
			Parameters  []struct {
				Name     string `json:"name"`
				In       string `json:"in"`
				Required bool   `json:"required"`
			} `json:"parameters"`
			Responses map[string]json.RawMessage `json:"responses"`
		} `json:"paths"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &doc); err != nil {
		t.Fatal(err)
	}
	if doc.OpenAPI != "3.1.0" || doc.Info.Title == "" || len(doc.Servers) != 1 || doc.Servers[0].URL != testOrigin {
		t.Fatalf("header %+v %+v", doc.Info, doc.Servers)
	}
	idx := v1Index{}
	_ = json.Unmarshal(serve(s, browserReq(http.MethodGet, "/api/v1/", nil)).Body.Bytes(), &idx)
	if len(doc.Paths) != len(idx.Endpoints) {
		t.Fatalf("openapi has %d paths, the index %d endpoints", len(doc.Paths), len(idx.Endpoints))
	}
	ops := map[string]bool{}
	for _, e := range idx.Endpoints {
		op, ok := doc.Paths[e.Path]["get"]
		if !ok {
			t.Errorf("openapi lacks GET %s", e.Path)
			continue
		}
		if op.Summary == "" || op.OperationID == "" || len(op.Responses) == 0 {
			t.Errorf("%s: %+v", e.Path, op)
		}
		if ops[op.OperationID] {
			t.Errorf("operationId %s is not unique", op.OperationID)
		}
		ops[op.OperationID] = true
		if len(doc.Paths[e.Path]) != 1 {
			t.Errorf("%s documents methods besides GET", e.Path)
		}
	}
	mapping := doc.Paths["/api/v1/search/index/{name}/mapping"]["get"]
	if len(mapping.Parameters) == 0 || mapping.Parameters[0].Name != "name" || mapping.Parameters[0].In != "path" || !mapping.Parameters[0].Required {
		t.Fatalf("path parameter %+v", mapping.Parameters)
	}
	page := doc.Paths["/api/v1/page"]["get"]
	names := map[string]bool{}
	for _, p := range page.Parameters {
		names[p.Name] = p.In == "query"
	}
	if !names["url"] || !names["xid"] {
		t.Fatalf("page parameters %+v", page.Parameters)
	}
	if _, ok := doc.Paths["/api/v1/rabbitmq/peek"]; ok {
		t.Fatal("openapi documents the peek")
	}
}

func TestV1StreamRefusesOptInCaptures(t *testing.T) {
	idle := func(ctx context.Context, f core.Filter, emit core.Emit) error {
		<-ctx.Done()
		return ctx.Err()
	}
	search := &fakeAdapter{kind: core.Search, present: true, streams: []core.StreamSpec{{Name: "capture", Title: "capture", OptIn: true, Start: idle}}}
	redis := &fakeAdapter{kind: core.Redis, present: true, streams: []core.StreamSpec{{Name: "monitor", Title: "monitor", Start: idle}}}
	mq := &fakeAdapter{kind: core.RabbitMQ, present: true, streams: []core.StreamSpec{{Name: "firehose", Title: "firehose", OptIn: true, Start: idle}}}
	s := v1Server(t, search, redis, mq)
	for _, sub := range []string{"sub=search:capture", "sub=rabbitmq:firehose", "sub=redis:monitor&sub=search:capture"} {
		r := browserReq(http.MethodGet, "/api/v1/stream?"+sub, nil)
		// Even with the Fetch Metadata the Inspector page itself sends.
		r.Header.Set("Sec-Fetch-Site", "same-origin")
		w := serveBriefly(s, r, 100*time.Millisecond)
		if w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), "hint") {
			t.Errorf("%s: status %d (%s)", sub, w.Code, strings.TrimSpace(w.Body.String()))
		}
	}
	w := serveBriefly(s, fromOrigin(http.MethodGet, "/api/v1/stream?sub=redis:monitor", "https://evil.test"), 100*time.Millisecond)
	if w.Code != http.StatusOK || w.Header().Get("Content-Type") != "text/event-stream" {
		t.Fatalf("plain sub: status %d, type %q", w.Code, w.Header().Get("Content-Type"))
	}
	checkCORS(t, "stream", w.Header())
	// The UI's own stream keeps its rule: captures from the page are fine.
	r := browserReq(http.MethodGet, "/api/stream?sub=search:capture", nil)
	r.Header.Set("Sec-Fetch-Site", "same-origin")
	if w := serveBriefly(s, r, 100*time.Millisecond); w.Code != http.StatusOK {
		t.Fatalf("/api/stream capture from the page: %d", w.Code)
	}
}

func pageServer(t *testing.T) (*Server, apm.Trace, *timeline.Timeline) {
	t.Helper()
	s, store := traceServer(t)
	tr, ok := store.Get(batchTrace)
	if !ok {
		t.Fatal("fixture trace missing")
	}
	tl := timeline.New(nil, store)
	tl.Ingest(core.Event{Type: "trace", Kind: core.PHP, Time: tr.Start, Data: tr})
	s.cfg.Timeline = tl
	s.cfg.Clients = testClients()
	return s, tr, tl
}

type v1PageBody struct {
	Match   string `json:"match"`
	Request struct {
		Key     string `json:"key"`
		URL     string `json:"url"`
		TraceID string `json:"traceId"`
	} `json:"request"`
	Handling string `json:"handling"`
	Trace    *struct {
		ID    string             `json:"id"`
		Times map[string]float64 `json:"times"`
	} `json:"trace"`
	Hot         []apm.HotItem `json:"hot"`
	Repeats     []apm.Repeat  `json:"repeats"`
	RepeatCount int           `json:"repeatCount"`
	Links       struct {
		Trace       string `json:"trace"`
		Bottlenecks string `json:"bottlenecks"`
		Request     string `json:"request"`
	} `json:"links"`
	Error string `json:"error"`
	Hint  string `json:"hint"`
}

func getPage(t *testing.T, s *Server, query string) (int, v1PageBody) {
	t.Helper()
	w := serve(s, fromOrigin(http.MethodGet, "/api/v1/page?"+query, "https://app.shop2.test"))
	checkCORS(t, "page", w.Header())
	var body v1PageBody
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("page %s: %v (%s)", query, err, w.Body)
	}
	return w.Code, body
}

func TestV1PageByURL(t *testing.T) {
	s, tr, _ := pageServer(t)
	code, p := getPage(t, s, "url="+url.QueryEscape(tr.URL))
	if code != http.StatusOK || p.Match != "url" || p.Request.TraceID != tr.ID || p.Trace == nil || p.Trace.ID != tr.ID {
		t.Fatalf("page: %d %+v", code, p)
	}
	for _, k := range []string{"sql", "cache", "search", "http", "php"} {
		if _, ok := p.Trace.Times[k]; !ok {
			t.Errorf("times lacks %s", k)
		}
	}
	if len(p.Hot) == 0 || len(p.Hot) > 5 || len(p.Repeats) > 5 || p.RepeatCount < len(p.Repeats) || p.Hot[0].Rank != 1 {
		t.Fatalf("hot %d, repeats %d", len(p.Hot), len(p.Repeats))
	}
	for _, h := range p.Hot {
		if len(h.SpanIDs) > 50 {
			t.Fatalf("hot item carries %d span ids", len(h.SpanIDs))
		}
	}
	if p.Links.Bottlenecks != testOrigin+"/#/traces/"+tr.ID || p.Links.Trace != testOrigin+"/#/traces/"+tr.ID+"/waterfall" || !strings.HasPrefix(p.Links.Request, testOrigin+"/#/overview?req=") {
		t.Fatalf("links %+v", p.Links)
	}
	// What location.href holds, fragment included.
	if code, p := getPage(t, s, "url="+url.QueryEscape(tr.URL+"#reviews")); code != http.StatusOK || p.Request.TraceID != tr.ID {
		t.Fatalf("with fragment: %d %+v", code, p)
	}
}

func TestV1PageByXID(t *testing.T) {
	s, tr, tl := pageServer(t)
	if tr.VarnishXID == "" {
		t.Fatal("fixture trace has no varnish.xid")
	}
	code, p := getPage(t, s, "xid="+url.QueryEscape(tr.VarnishXID))
	if code != http.StatusOK || p.Match != "xid" || p.Request.TraceID != tr.ID {
		t.Fatalf("by xid: %d %+v", code, p)
	}
	// A varnish row for the same page: X-Varnish names its client request.
	u, err := url.Parse(tr.URL)
	if err != nil {
		t.Fatal(err)
	}
	v := core.VarnishReq{VXID: "424242", BereqVXID: tr.VarnishXID, Method: "GET", URL: u.RequestURI(), Host: u.Host, Status: 200, Handling: "MISS", Start: tr.Start.Add(-time.Millisecond), Duration: tr.Duration + 2*time.Millisecond, Reason: "not in cache yet"}
	tl.Ingest(core.Event{Type: "varnish.req", Kind: core.Varnish, Time: v.Start, Data: v})
	code, p = getPage(t, s, "xid=424242&url="+url.QueryEscape("https://app.shop2.test/elsewhere"))
	if code != http.StatusOK || p.Match != "xid" || p.Request.Key != "v:424242" || p.Handling != "MISS" || p.Trace == nil {
		t.Fatalf("xid wins over url: %d %+v", code, p)
	}
	// An xid nobody knows falls back to the URL, and says so.
	code, p = getPage(t, s, "xid=999&url="+url.QueryEscape(tr.URL))
	if code != http.StatusOK || p.Match != "url" {
		t.Fatalf("fallback: %d %+v", code, p)
	}
}

func TestV1PageMisses(t *testing.T) {
	s, tr, _ := pageServer(t)
	for _, q := range []string{
		"url=" + url.QueryEscape("https://app.shop2.test/nothing-here"),
		"xid=999",
		"url=" + url.QueryEscape(tr.URL) + "&since=" + url.QueryEscape(tr.Start.Add(time.Hour).Format(time.RFC3339)),
	} {
		code, p := getPage(t, s, q)
		if code != http.StatusNotFound || p.Error == "" || p.Hint == "" {
			t.Errorf("%s: %d %+v", q, code, p)
		}
	}
	code, p := getPage(t, s, "")
	if code != http.StatusBadRequest || p.Error == "" || p.Hint == "" {
		t.Fatalf("no url: %d %+v", code, p)
	}
	// since takes unix milliseconds, as Date.now() gives them, with slack for clock drift.
	ms := tr.Start.Add(time.Second).UnixMilli()
	if code, _ := getPage(t, s, "url="+url.QueryEscape(tr.URL)+"&since="+strconv.FormatInt(ms, 10)); code != http.StatusOK {
		t.Fatalf("since within the slack: %d", code)
	}
}

func TestV1RequestsAndTraces(t *testing.T) {
	s, tr, _ := pageServer(t)
	w := serve(s, fromOrigin(http.MethodGet, "/api/v1/requests?url="+url.QueryEscape(tr.URL)+"&limit=5", "null"))
	var reqs []timeline.Request
	if err := json.Unmarshal(w.Body.Bytes(), &reqs); err != nil || w.Code != http.StatusOK || len(reqs) != 1 || reqs[0].TraceID != tr.ID {
		t.Fatalf("requests: %d %v %s", w.Code, err, w.Body)
	}
	w = serve(s, fromOrigin(http.MethodGet, "/api/v1/requests/"+url.PathEscape(reqs[0].Key), "null"))
	if w.Code != http.StatusOK {
		t.Fatalf("request by key: %d %s", w.Code, w.Body)
	}

	var list []apm.Summary
	w = serve(s, fromOrigin(http.MethodGet, "/api/v1/traces?url="+url.QueryEscape(tr.URL), "null"))
	if err := json.Unmarshal(w.Body.Bytes(), &list); err != nil || len(list) != 1 || list[0].ID != tr.ID {
		t.Fatalf("traces by url: %v %s", err, w.Body)
	}
	w = serve(s, fromOrigin(http.MethodGet, "/api/v1/traces?min_ms=100000", "null"))
	if err := json.Unmarshal(w.Body.Bytes(), &list); err != nil || len(list) != 0 {
		t.Fatalf("min_ms: %v %s", err, w.Body)
	}
	if w := serve(s, fromOrigin(http.MethodGet, "/api/v1/traces?min_ms=soon", "null")); w.Code != http.StatusBadRequest {
		t.Fatalf("bad min_ms: %d", w.Code)
	}

	var one struct {
		ID        string             `json:"id"`
		Times     map[string]float64 `json:"times"`
		Analysis  apm.Analysis       `json:"analysis"`
		Spans     []apm.Span         `json:"spans"`
		SpanCount int                `json:"spanCount"`
	}
	w = serve(s, fromOrigin(http.MethodGet, "/api/v1/traces/"+tr.ID, "null"))
	if err := json.Unmarshal(w.Body.Bytes(), &one); err != nil || one.ID != tr.ID || one.Analysis.ID != tr.ID || one.Spans != nil || one.SpanCount != len(tr.Spans) || len(one.Times) != 5 {
		t.Fatalf("trace: %v id %q spans %d count %d", err, one.ID, len(one.Spans), one.SpanCount)
	}
	w = serve(s, fromOrigin(http.MethodGet, "/api/v1/traces/"+tr.ID+"?spans=1", "null"))
	if err := json.Unmarshal(w.Body.Bytes(), &one); err != nil || len(one.Spans) != min(len(tr.Spans), maxV1Spans) {
		t.Fatalf("trace with spans: %v %d of %d", err, len(one.Spans), len(tr.Spans))
	}
	for _, p := range []string{"/api/v1/traces/" + tr.ID + "/analysis", "/api/v1/hotspots?limit=2", "/api/v1/traces/metrics?window=5m"} {
		if w := serve(s, fromOrigin(http.MethodGet, p, "null")); w.Code != http.StatusOK {
			t.Errorf("%s: %d %s", p, w.Code, w.Body)
		}
	}
	w = serve(s, fromOrigin(http.MethodGet, "/api/v1/traces/0000", "null"))
	var miss struct{ Error, Hint string }
	if err := json.Unmarshal(w.Body.Bytes(), &miss); err != nil || w.Code != http.StatusNotFound || miss.Error == "" || miss.Hint == "" {
		t.Fatalf("unknown trace: %d %s", w.Code, w.Body)
	}
	w = serve(s, fromOrigin(http.MethodGet, "/api/v1/nothing", "null"))
	if err := json.Unmarshal(w.Body.Bytes(), &miss); err != nil || w.Code != http.StatusNotFound || miss.Hint == "" {
		t.Fatalf("unknown route: %d %s", w.Code, w.Body)
	}
}

func TestUserscriptIsServedWithTheDomain(t *testing.T) {
	s := v1Server(t)
	for _, origin := range foreignOrigins {
		r := fromOrigin(http.MethodGet, "/userscript/roll-inspector.user.js", origin)
		if origin == "chrome-extension://abc" {
			r.Header.Set("Sec-Fetch-Site", "none")
		}
		w := serve(s, r)
		if w.Code != http.StatusOK {
			t.Fatalf("from %q: status %d (%s)", origin, w.Code, w.Body)
		}
		if ct := w.Header().Get("Content-Type"); ct != "text/javascript; charset=utf-8" {
			t.Fatalf("content type %q", ct)
		}
		body := w.Body.String()
		if strings.Contains(body, "__ROLL_") || !strings.Contains(body, "@match https://*."+testDomain+"/*") || !strings.Contains(body, "@connect inspector."+testDomain) {
			t.Fatalf("body %q", body)
		}
	}
	if w := serve(s, writeReq(s, "/userscript/roll-inspector.user.js", map[string]any{})); w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST: %d", w.Code)
	}
}

func TestExtensionZipIsReproducible(t *testing.T) {
	get := func() []byte {
		s := v1Server(t)
		w := serve(s, fromOrigin(http.MethodGet, "/extension/roll-inspector.zip", "chrome-extension://abc"))
		if w.Code != http.StatusOK || w.Header().Get("Content-Type") != "application/zip" {
			t.Fatalf("zip: %d %q %s", w.Code, w.Header().Get("Content-Type"), w.Body)
		}
		if !strings.Contains(w.Header().Get("Content-Disposition"), "roll-inspector.zip") {
			t.Fatalf("disposition %q", w.Header().Get("Content-Disposition"))
		}
		return w.Body.Bytes()
	}
	a, b := get(), get()
	if !bytes.Equal(a, b) {
		t.Fatal("two builds of the zip differ")
	}
	zr, err := zip.NewReader(bytes.NewReader(a), int64(len(a)))
	if err != nil {
		t.Fatal(err)
	}
	files := map[string]string{}
	for _, f := range zr.File {
		rc, err := f.Open()
		if err != nil {
			t.Fatal(err)
		}
		data, _ := io.ReadAll(rc)
		rc.Close()
		files[f.Name] = string(data)
		if f.Modified.Year() < 1980 || f.Modified.Year() > 2000 {
			t.Errorf("%s: modified %v", f.Name, f.Modified)
		}
	}
	for _, name := range []string{"roll-inspector/manifest.json", "roll-inspector/panel.html", "roll-inspector/lib/core.js", "roll-inspector/fonts/plex-400.woff2", "roll-inspector/fonts/jetbrains-400.woff2", "roll-inspector/tokens.css"} {
		if _, ok := files[name]; !ok {
			t.Errorf("zip lacks %s (has %v)", name, keysOf(files))
		}
	}
	if _, ok := files["roll-inspector/fonts/OFL.txt"]; ok {
		t.Error("zip carries a non-font file from the fonts folder")
	}
	tokens := files["roll-inspector/tokens.css"]
	if !strings.HasPrefix(tokens, ":root{") || !strings.Contains(tokens, "--ground:#101418") || strings.Contains(tokens, ".x{") {
		t.Fatalf("tokens.css %q", tokens)
	}
}

func keysOf(m map[string]string) []string {
	out := []string{}
	for k := range m {
		out = append(out, k)
	}
	return out
}

// The zip from the real extension files and the last UI build: every page
// the manifest names and every font panel.css asks for must be in it.
func TestRealExtensionZip(t *testing.T) {
	dist := os.DirFS(filepath.Join("..", "..", "ui", "dist"))
	if _, err := fs.Stat(dist, "app.css"); err != nil {
		t.Skip("ui/dist is not built; run scripts/ui")
	}
	data, err := buildExtensionZip(clients.Files, dist)
	if err != nil {
		t.Fatal(err)
	}
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatal(err)
	}
	have := map[string]bool{}
	for _, f := range zr.File {
		have[strings.TrimPrefix(f.Name, zipRoot+"/")] = true
	}
	css, _ := fs.ReadFile(clients.Files, "extension/panel.css")
	for _, m := range regexp.MustCompile(`url\(([^)]+)\)`).FindAllStringSubmatch(string(css), -1) {
		if !have[m[1]] {
			t.Errorf("panel.css needs %s, the zip lacks it", m[1])
		}
	}
	for _, f := range []string{"manifest.json", "devtools.html", "devtools.js", "panel.html", "panel.js", "panel.css", "options.html", "options.js", "lib/core.js", "tokens.css"} {
		if !have[f] {
			t.Errorf("zip lacks %s", f)
		}
	}
}

func idleStream(name string, filters ...string) core.StreamSpec {
	return core.StreamSpec{Name: name, Title: name, Filters: filters, Start: func(ctx context.Context, f core.Filter, emit core.Emit) error {
		<-ctx.Done()
		return ctx.Err()
	}}
}

// liveStream opens a stream on a live server and returns its response once
// the headers arrived; the caller closes the body.
func liveStream(t *testing.T, s *Server, path string, header http.Header) *http.Response {
	t.Helper()
	h := s.Handler()
	// As if through Traefik, like the requests browserReq builds.
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.RemoteAddr = traefikAddr + ":41234"
		h.ServeHTTP(w, r)
	}))
	t.Cleanup(ts.Close)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, ts.URL+path, nil)
	req.Host = testHost
	req.Header.Set(gateHeader, testSecret)
	for k, v := range header {
		req.Header[k] = v
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { res.Body.Close() })
	return res
}

func TestStreamBoundsTheVariantsOfAStream(t *testing.T) {
	redis := &fakeAdapter{kind: core.Redis, present: true, streams: []core.StreamSpec{idleStream("monitor", "db", "match")}}
	s := v1Server(t, redis)
	res := liveStream(t, s, "/api/stream?sub=redis:monitor;match=a&sub=redis:monitor;match=b&sub=redis:monitor;match=c&sub=redis:monitor;match=d", nil)
	if res.StatusCode != http.StatusOK || len(s.cfg.Hub.Active()) != 4 {
		t.Fatalf("four variants: %d %v", res.StatusCode, s.cfg.Hub.Active())
	}
	w := serveBriefly(s, browserReq(http.MethodGet, "/api/stream?sub=redis:monitor;match=e", nil), 100*time.Millisecond)
	if w.Code != http.StatusTooManyRequests || !strings.Contains(w.Body.String(), "hint") {
		t.Fatalf("a fifth variant: %d %s", w.Code, w.Body)
	}
	// Joining a running variant starts nothing and is fine, from any origin.
	w = serveBriefly(s, fromOrigin(http.MethodGet, "/api/v1/stream?sub=redis:monitor;match=a", "https://evil.test"), 100*time.Millisecond)
	if w.Code != http.StatusOK {
		t.Fatalf("joining from v1: %d %s", w.Code, w.Body)
	}
	w = serveBriefly(s, fromOrigin(http.MethodGet, "/api/v1/stream?sub=redis:monitor;match=x", "https://evil.test"), 100*time.Millisecond)
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("a new variant from v1: %d %s", w.Code, w.Body)
	}
	if n := len(s.cfg.Hub.Active()); n != 4 {
		t.Fatalf("refused subs left producers: %v", s.cfg.Hub.Active())
	}
}

func TestV1StreamRefusesUnknownFilters(t *testing.T) {
	redis := &fakeAdapter{kind: core.Redis, present: true, streams: []core.StreamSpec{idleStream("monitor", "db", "match")}}
	mq := &fakeAdapter{kind: core.RabbitMQ, present: true, streams: []core.StreamSpec{idleStream("rates")}}
	s := v1Server(t, redis, mq)
	for _, q := range []string{"sub=redis:monitor;x=1", "sub=rabbitmq:rates;q=a"} {
		w := serveBriefly(s, fromOrigin(http.MethodGet, "/api/v1/stream?"+q, "https://evil.test"), 100*time.Millisecond)
		if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "hint") {
			t.Errorf("%s: %d %s", q, w.Code, w.Body)
		}
	}
	if len(s.cfg.Hub.Active()) != 0 {
		t.Fatalf("producers started: %v", s.cfg.Hub.Active())
	}
}

func TestV1StreamAnswersHeadWithoutStarting(t *testing.T) {
	redis := &fakeAdapter{kind: core.Redis, present: true, streams: []core.StreamSpec{idleStream("monitor", "db", "match")}}
	s := v1Server(t, redis)
	w := serveBriefly(s, fromOrigin(http.MethodHead, "/api/v1/stream?sub=redis:monitor", "https://evil.test"), 100*time.Millisecond)
	if w.Code != http.StatusOK || w.Header().Get("Content-Type") != "text/event-stream" || w.Body.Len() != 0 {
		t.Fatalf("head: %d %q", w.Code, w.Header().Get("Content-Type"))
	}
	if len(s.cfg.Hub.Active()) != 0 {
		t.Fatalf("HEAD started %v", s.cfg.Hub.Active())
	}
	if w := serveBriefly(s, fromOrigin(http.MethodHead, "/api/v1/stream?sub=redis:monitor;x=1", "https://evil.test"), 100*time.Millisecond); w.Code != http.StatusBadRequest {
		t.Fatalf("head with a bad sub: %d", w.Code)
	}
	w = serveBriefly(s, fromOrigin(http.MethodPost, "/api/v1/stream?sub=redis:monitor", "https://evil.test"), 100*time.Millisecond)
	if w.Code != http.StatusMethodNotAllowed || w.Header().Get("Allow") != "GET, HEAD, OPTIONS" {
		t.Fatalf("post: %d, Allow %q", w.Code, w.Header().Get("Allow"))
	}
}
