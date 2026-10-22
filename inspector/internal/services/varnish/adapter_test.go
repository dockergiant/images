package varnish

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dockergiant/images/inspector/internal/core"
	"github.com/dockergiant/images/inspector/internal/docker"
	"github.com/dockergiant/images/inspector/internal/services"
)

const admHelp = "ban <field> <operator> <arg> [&& <field> <oper> <arg> ...]\nban.list [-j]\npanic.show [-j]\nvcl.show [-v] [<configname>]\n"

type execStub struct {
	mu      sync.Mutex
	replies map[string]docker.ExecResult
}

func (s *execStub) set(cmd string, res docker.ExecResult) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.replies == nil {
		s.replies = map[string]docker.ExecResult{}
	}
	s.replies[cmd] = res
}

func (s *execStub) run(ctx context.Context, spec docker.ExecSpec) (docker.ExecResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := strings.Join(spec.Cmd, " ")
	for prefix, res := range s.replies {
		if key == prefix {
			return res, nil
		}
	}
	for prefix, res := range s.replies {
		if strings.HasPrefix(key, prefix+" ") {
			return res, nil
		}
	}
	return docker.ExecResult{ExitCode: 127, Stderr: []byte("unexpected exec: " + key)}, nil
}

func newTestAdapter(t *testing.T, version string) (*adapter, *docker.Fake, *execStub) {
	t.Helper()
	stub := &execStub{}
	fake := &docker.Fake{
		EnvName:       "shop",
		ContainerList: []docker.Container{{ID: "v1", Name: "shop-varnish-1", Service: "varnish", Project: "shop", Running: true}},
		ContainerEnvs: map[string][]string{"v1": {"CACHE_SIZE=256m", "BACKEND_HOST=nginx", "BACKEND_PORT=80"}},
		ExecFunc:      stub.run,
	}
	if version != "" {
		stub.set("varnishd -V", docker.ExecResult{Stderr: fixture(t, "version-"+version+".txt")})
		stub.set("varnishlog -h", docker.ExecResult{Stderr: fixture(t, "help-"+version+"-varnishlog.txt")})
		stub.set("varnishncsa -h", docker.ExecResult{Stderr: fixture(t, "help-"+version+"-varnishncsa.txt")})
		stub.set("varnishadm help", docker.ExecResult{Stdout: []byte(admHelp)})
	}
	a := newAdapter(services.Deps{Docker: fake, Log: slog.New(slog.DiscardHandler)})
	return a, fake, stub
}

func TestDetect(t *testing.T) {
	for _, v := range []string{"8.0", "7.1"} {
		a, fake, _ := newTestAdapter(t, v)
		info, err := a.Detect(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if !info.Present || info.Engine != "Varnish" || !strings.HasPrefix(info.Version, v+".") || !info.Tested || info.Container != "shop-varnish-1" {
			t.Fatalf("%s: %+v", v, info)
		}
		for _, f := range []string{"unbuffered", "ncsaJson", "varnishadm", "admJson"} {
			if !info.Features[f] {
				t.Errorf("%s: feature %s missing", v, f)
			}
		}
		for _, spec := range fake.Execs() {
			if spec.User != "" || spec.Container != "v1" || spec.Privileged {
				t.Fatalf("%s: exec %+v must run as the image default user in the varnish container", v, spec)
			}
		}
	}
}

func TestDetectBestEffortAndAbsent(t *testing.T) {
	a, _, stub := newTestAdapter(t, "8.0")
	stub.set("varnishd -V", docker.ExecResult{Stderr: []byte("varnishd (varnish-7.6.1 revision 0)\n")})
	stub.set("varnishlog -h", docker.ExecResult{Stderr: []byte(" [-v]  Verbose record printing\n")})
	info, _ := a.Detect(context.Background())
	if info.Version != "7.6.1" || info.Tested || info.Features["unbuffered"] {
		t.Fatalf("best-effort: %+v", info)
	}

	a, fake, _ := newTestAdapter(t, "")
	fake.SetContainers(nil)
	info, err := a.Detect(context.Background())
	if err != nil || info.Present || info.Kind != core.Varnish {
		t.Fatalf("absent: %+v %v", info, err)
	}
}

func TestStreamLog(t *testing.T) {
	a, fake, _ := newTestAdapter(t, "8.0")
	lines := strings.Split(string(fixture(t, "vsl-8.0-cases.txt")), "\n")
	fake.StreamFunc = func(ctx context.Context, spec docker.ExecSpec, onLine func(string)) error {
		for _, l := range lines {
			onLine(l)
		}
		return nil
	}
	var specs []core.StreamSpec
	for _, s := range a.Streams() {
		if s.Name == "log" {
			specs = append(specs, s)
		}
	}
	if len(specs) != 1 || specs[0].OptIn {
		t.Fatalf("log stream spec: %+v", a.Streams())
	}

	var events []core.Event
	if err := specs[0].Start(context.Background(), core.Filter{}, func(e core.Event) { events = append(events, e) }); err != nil {
		t.Fatal(err)
	}
	if len(events) != 11 {
		t.Fatalf("got %d events", len(events))
	}
	for _, e := range events {
		r, ok := e.Data.(core.VarnishReq)
		if e.Type != "varnish.req" || e.Kind != core.Varnish || !ok || !e.Time.Equal(r.Start) || len(r.Raw) == 0 {
			t.Fatalf("event %+v", e)
		}
	}
	got := fake.Streams()[0]
	if !slices.Equal(got.Cmd, []string{"varnishlog", "-g", "request", "-v", "-u"}) || got.User != "" || got.Container != "v1" {
		t.Fatalf("stream exec %+v", got)
	}
	reqs, watchers := a.recent()
	if len(reqs) != 11 || watchers != 0 || reqs[0].Raw != nil {
		t.Fatalf("ring after unfiltered stream: %d rows, %d watchers", len(reqs), watchers)
	}

	err := specs[0].Start(context.Background(), core.Filter{"q": "RespStatus == 200", "url": "/catalog/product.html"}, func(core.Event) {})
	if err != nil {
		t.Fatal(err)
	}
	got = fake.Streams()[1]
	want := []string{"varnishlog", "-g", "request", "-v", "-u", "-q", `(RespStatus == 200) and ReqURL ~ "/catalog/product\\.html"`}
	if !slices.Equal(got.Cmd, want) {
		t.Fatalf("filtered cmd\n got %q\nwant %q", got.Cmd, want)
	}
	if reqs, _ := a.recent(); len(reqs) != 11 {
		t.Fatalf("filtered stream must not feed the ring, got %d rows", len(reqs))
	}

	if err := specs[0].Start(context.Background(), core.Filter{"q": `ReqURL ~ "a" && RespStatus == 200`}, func(core.Event) {}); err == nil || !strings.Contains(err.Error(), "and, or and not") {
		t.Fatalf("&& query: %v", err)
	}
}

func TestStreamCancelLeavesNoWatcher(t *testing.T) {
	a, _, _ := newTestAdapter(t, "8.0")
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- a.Streams()[0].Start(ctx, core.Filter{}, func(core.Event) {})
	}()
	deadline := time.Now().Add(2 * time.Second)
	for {
		if _, w := a.recent(); w == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("stream never started")
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	if err := <-done; err == nil {
		t.Fatal("expected the context error")
	}
	if _, w := a.recent(); w != 0 {
		t.Fatalf("watchers %d after cancel", w)
	}
}

func statJSON(values map[string]uint64) []byte {
	counters := map[string]map[string]any{}
	for k, v := range values {
		counters[k] = map[string]any{"description": k, "flag": "c", "format": "i", "value": v}
	}
	counters["VBE.boot.default.happy"] = map[string]any{"description": "", "flag": "b", "format": "b", "value": uint64(1)}
	b, _ := json.Marshal(map[string]any{"version": 1, "timestamp": "2026-10-20T20:23:04", "counters": counters})
	return b
}

func cardsByLabel(h core.Health) map[string]core.Card {
	out := map[string]core.Card{}
	for _, c := range h.Cards {
		out[c.Label] = c
	}
	return out
}

// Passes count against the hit rate: a page Varnish passes is served by PHP
// every time. Hit-for-pass is already in s_pass, so it is not added again.
func TestHitRateCountsPasses(t *testing.T) {
	a, _, stub := newTestAdapter(t, "8.0")
	clock := time.Date(2026, 10, 20, 20, 0, 0, 0, time.UTC)
	a.now = func() time.Time { return clock }
	base := map[string]uint64{"MAIN.cache_hit": 10, "MAIN.cache_miss": 5, "MAIN.s_pass": 20, "MAIN.cache_hitpass": 4, "MAIN.s_synth": 7, "MAIN.s_pipe": 1}
	stub.set("varnishstat -j", docker.ExecResult{Stdout: statJSON(base)})
	if _, err := a.Health(context.Background()); err != nil {
		t.Fatal(err)
	}
	clock = clock.Add(10 * time.Second)
	next := map[string]uint64{"MAIN.cache_hit": 13, "MAIN.cache_miss": 5, "MAIN.s_pass": 235, "MAIN.cache_hitpass": 100, "MAIN.s_synth": 900, "MAIN.s_pipe": 50}
	stub.set("varnishstat -j", docker.ExecResult{Stdout: statJSON(next)})
	h, err := a.Health(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	c := cardsByLabel(h)["Hit rate, last 5 min"]
	if c.Value != "1.4%" || c.Sub != "3 hits · 0 misses · 215 passes (not cacheable)" {
		t.Fatalf("hit card %+v", c)
	}
}

func TestHealth(t *testing.T) {
	a, _, stub := newTestAdapter(t, "8.0")
	clock := time.Date(2026, 10, 20, 20, 0, 0, 0, time.UTC)
	a.now = func() time.Time { return clock }
	stub.set("varnishadm backend.list -j", docker.ExecResult{Stdout: fixture(t, "adm-8.0-backend.list.json")})
	stub.set("varnishadm ban.list -j", docker.ExecResult{Stdout: fixture(t, "adm-8.0-ban.list-purges.json")})
	base := map[string]uint64{
		"MAIN.cache_hit": 100, "MAIN.cache_miss": 10, "MAIN.s_pass": 0, "MAIN.n_object": 1204,
		"SMA.s0.g_bytes": 118 << 20, "SMA.s0.g_space": 138 << 20, "MAIN.n_lru_nuked": 0, "MAIN.backend_fail": 0,
	}
	stub.set("varnishstat -j", docker.ExecResult{Stdout: statJSON(base)})

	h, err := a.Health(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	cards := cardsByLabel(h)
	if c := cards["Hit rate, last 5 min"]; c.Value != "90.9%" || !strings.Contains(c.Sub, "since Varnish started") {
		t.Fatalf("first hit card %+v", c)
	}

	clock = clock.Add(8 * time.Second)
	next := map[string]uint64{}
	for k, v := range base {
		next[k] = v
	}
	next["MAIN.cache_miss"] = 10 + 280
	next["MAIN.s_pass"] = 8
	next["MAIN.cache_hit"] = 100 + 1912
	stub.set("varnishstat -j", docker.ExecResult{Stdout: statJSON(next)})

	banTime := floatTime(1792528082.182585)
	for range 3 {
		a.remember(core.VarnishReq{Method: "GET", URL: "/keukenapparatuur/koffiemachines.html?p=2", Handling: "MISS", Reason: "banned by a purge", Tags: []string{"cat_c_41", "store"}, Start: banTime.Add(time.Minute)})
	}
	a.remember(core.VarnishReq{Method: "GET", URL: "/catalogsearch/result/", Handling: "PASS", Reason: "never cached (PASS in VCL)", Start: banTime})
	a.remember(core.VarnishReq{Method: "POST", URL: "/checkout/cart/add", Handling: "PASS", Reason: "POST is never cached (PASS in VCL)"})
	a.remember(core.VarnishReq{Method: "GET", URL: "/", Handling: "HIT"})
	a.remember(core.VarnishReq{Method: "GET", URL: "/a.html", Handling: "MISS", BereqVXID: "3", BereqStart: banTime, BereqEnd: banTime.Add(600 * time.Millisecond), Reason: "not in cache yet"})
	a.remember(core.VarnishReq{Method: "GET", URL: "/b.html", Handling: "MISS", BereqVXID: "5", BereqStart: banTime, BereqEnd: banTime.Add(680 * time.Millisecond), Reason: "not in cache yet"})

	h, err = a.Health(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if h.Status != core.OK || len(h.Warnings) != 0 {
		t.Fatalf("status %s warnings %+v", h.Status, h.Warnings)
	}
	if !strings.HasPrefix(h.Summary, "Varnish 8.0.2 · 86.9% hit rate · backend healthy") {
		t.Fatalf("summary %q", h.Summary)
	}
	cards = cardsByLabel(h)
	if c := cards["Hit rate, last 5 min"]; c.Value != "86.9%" || c.Sub != "1,912 hits · 280 misses · 8 passes (not cacheable)" {
		t.Fatalf("hit card %+v", c)
	}
	if c := cards["Cached pages"]; c.Value != "1,204" || c.Sub != "118 MB of 256 MB" {
		t.Fatalf("pages card %+v", c)
	}
	if c := cards["Backend (nginx:80)"]; c.Value != "Healthy" || c.Tone != core.OK || c.Sub != "avg fetch 640 ms" {
		t.Fatalf("backend card %+v", c)
	}
	if c := cards["Pushed out (LRU)"]; c.Value != "0" || c.Sub != "cache is big enough" {
		t.Fatalf("lru card %+v", c)
	}

	raw, ok := h.Raw.(HealthRaw)
	if !ok {
		t.Fatalf("raw %T", h.Raw)
	}
	if raw.Rates["MAIN.cache_hit"] != 239 {
		t.Fatalf("hit rate per second %v", raw.Rates["MAIN.cache_hit"])
	}
	if len(raw.TopMisses) != 4 {
		t.Fatalf("top misses %+v", raw.TopMisses)
	}
	top := raw.TopMisses[0]
	if top.URL != "/keukenapparatuur/koffiemachines.html" || top.Misses != 3 || top.Reason != "banned by tag cat_c_41" {
		t.Fatalf("top miss %+v", top)
	}
	if i := slices.IndexFunc(raw.TopMisses, func(m MissRow) bool { return m.URL == "/catalogsearch/result/" }); i < 0 || raw.TopMisses[i].Reason != "never cached (PASS in VCL)" {
		t.Fatalf("catalogsearch row %+v", raw.TopMisses)
	}
	if len(raw.Bans) != 2 || raw.Bans[0].Spec != `req.url ~ \.html$` || raw.Bans[0].FullFlush {
		t.Fatalf("bans %+v", raw.Bans)
	}
	if _, ok := raw.Counters["MAIN.n_object"]; !ok {
		t.Fatal("raw counters missing")
	}
}

func TestHealthWarnings(t *testing.T) {
	a, _, stub := newTestAdapter(t, "8.0")
	clock := time.Date(2026, 10, 20, 20, 0, 0, 0, time.UTC)
	a.now = func() time.Time { return clock }
	stub.set("varnishadm backend.list -j", docker.ExecResult{Stdout: []byte(`[ 3, ["backend.list", "-j"], 1.0, {"boot.default": {"type": "backend", "admin_health": "probe", "probe_message": [2, 8, "sick"], "last_change": 1.0}} ]`)})
	stub.set("varnishadm ban.list -j", docker.ExecResult{Stdout: fixture(t, "adm-8.0-ban.list.json")})
	stub.set("varnishstat -j", docker.ExecResult{Stdout: statJSON(map[string]uint64{"MAIN.n_lru_nuked": 4, "MAIN.backend_fail": 1})})
	if _, err := a.Health(context.Background()); err != nil {
		t.Fatal(err)
	}
	clock = clock.Add(6 * time.Second)
	stub.set("varnishstat -j", docker.ExecResult{Stdout: statJSON(map[string]uint64{"MAIN.n_lru_nuked": 10, "MAIN.backend_fail": 3})})
	h, err := a.Health(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if h.Status != core.Error {
		t.Fatalf("status %s", h.Status)
	}
	ids := map[string]bool{}
	for _, w := range h.Warnings {
		ids[w.ID] = true
	}
	for _, id := range []string{"varnish.backend-sick.boot.default", "varnish.backend-fail", "varnish.lru"} {
		if !ids[id] {
			t.Errorf("warning %s missing in %+v", id, h.Warnings)
		}
	}
	cards := cardsByLabel(h)
	if c := cards["Backend (nginx:80)"]; c.Value != "Sick" || c.Tone != core.Error {
		t.Fatalf("backend card %+v", c)
	}
	if c := cards["Pushed out (LRU)"]; c.Tone != core.Warn || !strings.HasPrefix(c.Sub, "6 in the last 5 min") {
		t.Fatalf("lru card %+v", c)
	}
}

func TestHealthWithoutVarnish(t *testing.T) {
	a, fake, _ := newTestAdapter(t, "")
	fake.SetContainers(nil)
	h, err := a.Health(context.Background())
	if err != nil || h.Status != core.ToneInfo || len(fake.Execs()) != 0 {
		t.Fatalf("%+v %v", h, err)
	}
}

func serve(t *testing.T, a *adapter) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	for _, r := range a.Browse() {
		mux.HandleFunc(r.Method+" /api/varnish"+r.Path, r.Handler)
	}
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func get(t *testing.T, srv *httptest.Server, path string, into any) int {
	t.Helper()
	resp, err := http.Get(srv.URL + path)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if into != nil {
		if err := json.Unmarshal(body, into); err != nil {
			t.Fatalf("%s: %v: %s", path, err, body)
		}
	}
	return resp.StatusCode
}

func TestBrowseRoutes(t *testing.T) {
	a, fake, stub := newTestAdapter(t, "8.0")
	srv := serve(t, a)
	paths := map[string]bool{}
	for _, r := range a.Browse() {
		paths[r.Path] = true
	}
	for _, p := range []string{"/bans", "/vcl", "/vcl/{name}", "/backends", "/storage", "/params", "/panic", "/search"} {
		if !paths[p] {
			t.Errorf("route %s missing", p)
		}
	}

	stub.set("varnishadm ban.list -j", docker.ExecResult{Stdout: fixture(t, "adm-8.0-ban.list-purges.json")})
	var bans []Ban
	if code := get(t, srv, "/api/varnish/bans", &bans); code != 200 || len(bans) != 3 {
		t.Fatalf("bans %d %+v", code, bans)
	}

	stub.set("varnishadm vcl.list -j", docker.ExecResult{Stdout: fixture(t, "adm-8.0-vcl.list.json")})
	stub.set(`varnishadm vcl.show -v "boot"`, docker.ExecResult{Stdout: fixture(t, "adm-8.0-vcl.show.txt")})
	var shown struct {
		Name  string    `json:"name"`
		Files []VCLFile `json:"files"`
	}
	if code := get(t, srv, "/api/varnish/vcl/boot", &shown); code != 200 || shown.Name != "boot" || len(shown.Files) != 2 {
		t.Fatalf("vcl.show %d %+v", code, shown.Name)
	}
	before := len(fake.Execs())
	if code := get(t, srv, "/api/varnish/vcl/evil%20name", nil); code != 404 {
		t.Fatalf("unknown vcl gave %d", code)
	}
	for _, spec := range fake.Execs()[before:] {
		if slices.Contains(spec.Cmd, "vcl.show") {
			t.Fatalf("vcl.show ran for an unknown name: %q", spec.Cmd)
		}
	}

	stub.set("varnishadm backend.list -j", docker.ExecResult{Stdout: fixture(t, "adm-7.1-backend.list.json")})
	var backends []Backend
	if code := get(t, srv, "/api/varnish/backends", &backends); code != 200 || len(backends) != 1 {
		t.Fatalf("backends %d %+v", code, backends)
	}
	stub.set("varnishadm storage.list -j", docker.ExecResult{Stdout: fixture(t, "adm-7.1-storage.list.json")})
	var storages []Storage
	if code := get(t, srv, "/api/varnish/storage", &storages); code != 200 || len(storages) != 2 {
		t.Fatalf("storage %d %+v", code, storages)
	}
	stub.set("varnishadm param.show -j changed", docker.ExecResult{Stdout: fixture(t, "adm-8.0-param.show.json")})
	var params []Param
	if code := get(t, srv, "/api/varnish/params", &params); code != 200 || len(params) != 2 {
		t.Fatalf("params %d %+v", code, params)
	}

	stub.set("varnishadm panic.show", docker.ExecResult{ExitCode: 1, Stdout: fixture(t, "adm-panic-none-stdout.txt"), Stderr: fixture(t, "adm-panic-none-stderr.txt")})
	var p struct {
		Panic bool   `json:"panic"`
		Text  string `json:"text"`
	}
	if code := get(t, srv, "/api/varnish/panic", &p); code != 200 || p.Panic || !strings.Contains(p.Text, "not panicked") {
		t.Fatalf("panic %d %+v", code, p)
	}
}

func TestBrowseSearch(t *testing.T) {
	a, fake, stub := newTestAdapter(t, "8.0")
	srv := serve(t, a)
	stub.set("varnishlog -d -g request -v", docker.ExecResult{Stdout: fixture(t, "vsl-8.0-cases.txt")})
	var out struct {
		Query    string            `json:"query"`
		Requests []core.VarnishReq `json:"requests"`
	}
	if code := get(t, srv, "/api/varnish/search?tag=cat_c_41", &out); code != 200 || len(out.Requests) != 11 {
		t.Fatalf("search %d %d", code, len(out.Requests))
	}
	if !out.Requests[0].Start.After(out.Requests[len(out.Requests)-1].Start) {
		t.Fatal("search results not newest first")
	}
	last := fake.Execs()[len(fake.Execs())-1]
	want := []string{"varnishlog", "-d", "-g", "request", "-v", "-k", "2000", "-q", `BerespHeader:X-Magento-Tags ~ "(^|,)cat_c_41(,|$)"`}
	if !slices.Equal(last.Cmd, want) {
		t.Fatalf("search cmd\n got %q\nwant %q", last.Cmd, want)
	}
	if code := get(t, srv, "/api/varnish/search", nil); code != 400 {
		t.Fatalf("empty search gave %d", code)
	}
	if code := get(t, srv, "/api/varnish/search?q=ReqURL%20~%20%22a%22%20%26%26%20RespStatus%20%3D%3D%20200", nil); code != 400 {
		t.Fatalf("&& search gave %d", code)
	}
	stub.set("varnishlog -d -g request -v", docker.ExecResult{ExitCode: 1, Stderr: []byte("Query expression error:\nSyntax error (Pos 14)\n")})
	var e struct {
		Error string `json:"error"`
	}
	if code := get(t, srv, "/api/varnish/search?q=ReqURL%20%3D%3D", &e); code != 400 || !strings.Contains(e.Error, "Syntax error") {
		t.Fatalf("syntax error %d %+v", code, e)
	}
}

func TestBrowseWithoutVarnish(t *testing.T) {
	a, fake, _ := newTestAdapter(t, "")
	fake.SetContainers(nil)
	srv := serve(t, a)
	if code := get(t, srv, "/api/varnish/bans", nil); code != 503 {
		t.Fatalf("got %d", code)
	}
}

func action(t *testing.T, a *adapter, name string) core.ActionSpec {
	t.Helper()
	for _, s := range a.Actions() {
		if s.Name == name {
			return s
		}
	}
	t.Fatalf("action %s missing", name)
	return core.ActionSpec{}
}

func TestActionSpecs(t *testing.T) {
	a, _, _ := newTestAdapter(t, "")
	want := map[string]core.ConfirmLevel{"purge-tag": core.ConfirmPlain, "ban-url": core.ConfirmPlain, "purge-all": core.ConfirmTyped, "ban-all": core.ConfirmTyped}
	for name, level := range want {
		s := action(t, a, name)
		if s.Confirm != level || !s.Destructive || s.Run == nil {
			t.Errorf("%s: %+v", name, s)
		}
	}
}

func TestPurgeActions(t *testing.T) {
	a, _, _ := newTestAdapter(t, "")
	var mu sync.Mutex
	var got []string
	status := http.StatusOK
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		got = append(got, r.Method+" "+r.URL.Path+" "+r.Header.Get("X-Magento-Tags-Pattern"))
		code := status
		mu.Unlock()
		w.WriteHeader(code)
		fmt.Fprint(w, "Purged")
	}))
	defer srv.Close()
	a.purgeURL = srv.URL + "/"

	res, err := action(t, a, "purge-tag").Run(context.Background(), core.ActionRequest{Target: "cat_c_41"})
	if err != nil {
		t.Fatal(err)
	}
	if got[0] != "PURGE / ((^|,)cat_c_41(,|$))" || !strings.Contains(res.Message, "cat_c_41") {
		t.Fatalf("purge-tag sent %q, %+v", got, res)
	}
	if _, err := action(t, a, "purge-tag").Run(context.Background(), core.ActionRequest{Target: "cat.c+41"}); err != nil {
		t.Fatal(err)
	}
	if got[1] != `PURGE / ((^|,)cat\.c\+41(,|$))` {
		t.Fatalf("tag not escaped: %q", got[1])
	}
	for _, bad := range []string{"", "a,b", "a b", "a\nb"} {
		if _, err := action(t, a, "purge-tag").Run(context.Background(), core.ActionRequest{Target: bad}); err == nil {
			t.Errorf("tag %q accepted", bad)
		}
	}

	if _, err := action(t, a, "purge-all").Run(context.Background(), core.ActionRequest{Target: "everything"}); err == nil {
		t.Fatal("purge-all accepted the wrong target")
	}
	if _, err := action(t, a, "purge-all").Run(context.Background(), core.ActionRequest{Target: "all-pages"}); err != nil {
		t.Fatal(err)
	}
	if got[len(got)-1] != "PURGE / .*" {
		t.Fatalf("purge-all sent %q", got[len(got)-1])
	}

	mu.Lock()
	status = http.StatusMethodNotAllowed
	mu.Unlock()
	if _, err := action(t, a, "purge-tag").Run(context.Background(), core.ActionRequest{Target: "cat_c_41"}); err == nil || !strings.Contains(err.Error(), "405") {
		t.Fatalf("405 not reported: %v", err)
	}
}

func TestBanActions(t *testing.T) {
	a, fake, stub := newTestAdapter(t, "")
	stub.set("varnishadm ban", docker.ExecResult{})

	res, err := action(t, a, "ban-url").Run(context.Background(), core.ActionRequest{Target: `\.html$`})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Message, "lurker") {
		t.Fatalf("missing lurker note: %q", res.Message)
	}
	last := fake.Execs()[len(fake.Execs())-1]
	if !slices.Equal(last.Cmd, []string{"varnishadm", "ban", "req.url", "~", `"\\.html$"`}) {
		t.Fatalf("ban-url cmd %q", last.Cmd)
	}

	if _, err := action(t, a, "ban-url").Run(context.Background(), core.ActionRequest{Target: `^/a b/"x"`}); err != nil {
		t.Fatal(err)
	}
	last = fake.Execs()[len(fake.Execs())-1]
	if last.Cmd[4] != `"^/a\\x20b/\"x\""` {
		t.Fatalf("space and quote handling: %q", last.Cmd[4])
	}

	before := len(fake.Execs())
	for _, bad := range []string{"", "(", "a\nb", `\Qa b\E`} {
		if _, err := action(t, a, "ban-url").Run(context.Background(), core.ActionRequest{Target: bad}); err == nil {
			t.Errorf("regex %q accepted", bad)
		}
	}
	if len(fake.Execs()) != before {
		t.Fatal("an invalid regex reached varnishadm")
	}

	if _, err := action(t, a, "ban-all").Run(context.Background(), core.ActionRequest{Target: "all-pages"}); err == nil {
		t.Fatal("ban-all accepted the wrong target")
	}
	if _, err := action(t, a, "ban-all").Run(context.Background(), core.ActionRequest{Target: "everything"}); err != nil {
		t.Fatal(err)
	}
	last = fake.Execs()[len(fake.Execs())-1]
	if !slices.Equal(last.Cmd, []string{"varnishadm", "ban", "req.url", "~", "."}) {
		t.Fatalf("ban-all cmd %q", last.Cmd)
	}

	stub.set("varnishadm ban", docker.ExecResult{ExitCode: 1, Stdout: []byte("Wrong number of arguments\n"), Stderr: []byte("Command failed with error code 106\n")})
	if _, err := action(t, a, "ban-all").Run(context.Background(), core.ActionRequest{Target: "everything"}); err == nil || !strings.Contains(err.Error(), "106") {
		t.Fatalf("varnishadm failure not reported: %v", err)
	}
}

func TestFormatting(t *testing.T) {
	if formatInt(0) != "0" || formatInt(999) != "999" || formatInt(1912) != "1,912" || formatInt(1234567) != "1,234,567" {
		t.Fatal("formatInt")
	}
	if n, ok := parseSize("256m"); !ok || n != 256<<20 {
		t.Fatal("parseSize 256m")
	}
	if n, ok := parseSize("1G"); !ok || n != 1<<30 {
		t.Fatal("parseSize 1G")
	}
	if _, ok := parseSize("lots"); ok {
		t.Fatal("parseSize garbage")
	}
	if formatBytes(118<<20) != "118 MB" || formatBytes(3<<29) != "1.5 GB" {
		t.Fatal("formatBytes")
	}
}

func TestMissRowsSkipAssets(t *testing.T) {
	reqs := []core.VarnishReq{
		{Method: "GET", URL: "/static/frontend/Magento/luma/en_US/mage/requirejs/mixins.js", Handling: "PASS", Reason: "never cached (PASS in VCL)"},
		{Method: "GET", URL: "/static/version1792537703/frontend/x.css", Handling: "PASS", Reason: "never cached (PASS in VCL)"},
		{Method: "GET", URL: "/media/catalog/product/cache/a/b.jpg", Handling: "PASS", Reason: "never cached (PASS in VCL)"},
		{Method: "GET", URL: "/pub/static/x.js", Handling: "MISS", Reason: "not in cache yet"},
		{Method: "GET", URL: "/pub/media/x.jpg?v=1", Handling: "MISS", Reason: "not in cache yet"},
		{Method: "GET", URL: "/category-2.html", Handling: "MISS", Reason: "not in cache yet"},
		{Method: "GET", URL: "/mediafiles.html", Handling: "MISS", Reason: "not in cache yet"},
	}
	rows := missRows(reqs, nil)
	got := []string{}
	for _, r := range rows {
		got = append(got, r.URL)
	}
	if !slices.Equal(got, []string{"/category-2.html", "/mediafiles.html"}) {
		t.Fatalf("rows %v", got)
	}
}

// Static and POST passes count too, as "not cacheable": the donut and the
// Overview tile divide by the same requests, so all three show one number.
func TestHealthHitRateShowsStaticPasses(t *testing.T) {
	a, _, stub := newTestAdapter(t, "8.0")
	stub.set("varnishadm backend.list -j", docker.ExecResult{Stdout: fixture(t, "adm-8.0-backend.list.json")})
	stub.set("varnishadm ban.list -j", docker.ExecResult{Stdout: fixture(t, "adm-8.0-ban.list-purges.json")})
	flat, err := ParseStat(fixture(t, "stat-6.0-flat.json"))
	if err != nil {
		t.Fatal(err)
	}
	if flat["MAIN.s_pass"] < 1000 {
		t.Fatalf("fixture has no static passes: %d", flat["MAIN.s_pass"])
	}
	stub.set("varnishstat -j", docker.ExecResult{Stdout: statJSON(flat)})
	h, err := a.Health(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if c := cardsByLabel(h)["Hit rate, last 5 min"]; c.Value != "4.4%" || !strings.Contains(c.Sub, "120 hits · 40 misses · 2,547 passes (not cacheable)") {
		t.Fatalf("hit card %+v", c)
	}
}

func TestHealthIsCachedAndSharesVarnishstat(t *testing.T) {
	a, fake, stub := newTestAdapter(t, "8.0")
	clock := time.Date(2026, 10, 20, 20, 0, 0, 0, time.UTC)
	a.now = func() time.Time { return clock }
	stub.set("varnishadm backend.list -j", docker.ExecResult{Stdout: fixture(t, "adm-8.0-backend.list.json")})
	stub.set("varnishadm ban.list -j", docker.ExecResult{Stdout: fixture(t, "adm-8.0-ban.list-purges.json")})
	stub.set("varnishstat -j", docker.ExecResult{Stdout: statJSON(map[string]uint64{"MAIN.cache_hit": 1})})
	stub.set("varnishadm ban", docker.ExecResult{})
	count := func(cmd string) int {
		n := 0
		for _, e := range fake.Execs() {
			if strings.Join(e.Cmd, " ") == cmd {
				n++
			}
		}
		return n
	}
	ctx := context.Background()
	for range 3 {
		if _, err := a.Health(ctx); err != nil {
			t.Fatal(err)
		}
		clock = clock.Add(time.Second)
	}
	if n := count("varnishstat -j"); n != 1 || count("varnishadm ban.list -j") != 1 {
		t.Fatalf("three polls within %s ran varnishstat %d times", healthTTL, n)
	}
	clock = clock.Add(healthTTL)
	if _, err := a.Health(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Metrics(ctx); err != nil {
		t.Fatal(err)
	}
	if n := count("varnishstat -j"); n != 2 {
		t.Fatalf("the sampler did not reuse the Health read: %d varnishstat", n)
	}
	if _, err := action(t, a, "ban-url").Run(ctx, core.ActionRequest{Target: `^/a`}); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Health(ctx); err != nil {
		t.Fatal(err)
	}
	if n := count("varnishadm ban.list -j"); n != 3 {
		t.Fatalf("Health after a ban still came from the cache: %d ban.list", n)
	}
}
