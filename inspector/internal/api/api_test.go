package api

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"

	"github.com/dockergiant/images/inspector/internal/core"
	"github.com/dockergiant/images/inspector/internal/docker"
)

const (
	testSecret  = "0123456789abcdef0123456789abcdef"
	testDomain  = "shop.test"
	testHost    = "inspector.shop.test"
	testOrigin  = "https://inspector.shop.test"
	traefikAddr = "172.20.0.2"
)

type fakeAdapter struct {
	kind    core.Kind
	present bool
	streams []core.StreamSpec
	actions []core.ActionSpec
	routes  []core.Route
}

func (a *fakeAdapter) Kind() core.Kind { return a.kind }
func (a *fakeAdapter) Detect(ctx context.Context) (core.Info, error) {
	return core.Info{Kind: a.kind, Present: a.present, Engine: "Fake"}, nil
}
func (a *fakeAdapter) Health(ctx context.Context) (core.Health, error) {
	return core.Health{Status: core.OK, Summary: "fine", Cards: []core.Card{{Label: "hits", Value: "1"}}}, nil
}
func (a *fakeAdapter) Browse() []core.Route       { return a.routes }
func (a *fakeAdapter) Streams() []core.StreamSpec { return a.streams }
func (a *fakeAdapter) Actions() []core.ActionSpec { return a.actions }

type runLog struct {
	mu   sync.Mutex
	reqs []core.ActionRequest
}

func (l *runLog) run(ctx context.Context, req core.ActionRequest) (core.ActionResult, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.reqs = append(l.reqs, req)
	return core.ActionResult{Message: "done " + req.Target}, nil
}

func (l *runLog) count() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.reqs)
}

func quietLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func fakeDocker(ip string) *docker.Fake {
	return &docker.Fake{EnvName: "shop", TraefikInfo: docker.TraefikInfo{Found: true, IP: ip, Bindings: []string{"127.0.0.1:443"}}}
}

func newServer(t *testing.T, d *docker.Fake, adapters ...core.Adapter) *Server {
	t.Helper()
	return New(Config{
		Env:      "shop",
		Domain:   testDomain,
		Secret:   testSecret,
		Version:  "test",
		Docker:   d,
		Adapters: adapters,
		UI:       fstest.MapFS{"index.html": {Data: []byte("<!doctype html><title>Inspector</title>")}},
		Log:      quietLog(),
	})
}

func browserReq(method, path string, body any) *http.Request {
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	r := httptest.NewRequest(method, path, rd)
	r.RemoteAddr = traefikAddr + ":41234"
	r.Host = testHost
	r.Header.Set(gateHeader, testSecret)
	return r
}

func writeReq(s *Server, path string, body any) *http.Request {
	r := browserReq(http.MethodPost, path, body)
	r.Header.Set("Origin", testOrigin)
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set(csrfHeader, s.csrf)
	return r
}

func serve(s *Server, r *http.Request) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	return w
}

func TestGateAcceptsTraefikWithSecret(t *testing.T) {
	s := newServer(t, fakeDocker(traefikAddr))
	w := serve(s, browserReq(http.MethodGet, "/api/session", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("status %d: %s", w.Code, w.Body)
	}
	var sess session
	if err := json.Unmarshal(w.Body.Bytes(), &sess); err != nil {
		t.Fatal(err)
	}
	if sess.CSRF != s.csrf || sess.Env != "shop" || sess.LAN {
		t.Fatalf("session %+v", sess)
	}
	if got := w.Header().Get("Content-Security-Policy"); got != csp {
		t.Fatalf("csp %q", got)
	}
	if w.Header().Get("Access-Control-Allow-Origin") != "" {
		t.Fatal("sent a CORS header")
	}
}

func TestGateRejectsWrongPeer(t *testing.T) {
	d := fakeDocker(traefikAddr)
	s := newServer(t, d)
	r := browserReq(http.MethodGet, "/api/session", nil)
	r.RemoteAddr = "172.20.0.9:5555"
	r.Header.Set("X-Forwarded-For", traefikAddr)
	if w := serve(s, r); w.Code != http.StatusForbidden {
		t.Fatalf("status %d", w.Code)
	}
	if d.TraefikCalls() != 2 {
		t.Fatalf("want one re-inspect on a miss, got %d calls", d.TraefikCalls())
	}
}

func TestGateFollowsTraefikMove(t *testing.T) {
	d := fakeDocker("172.20.0.7")
	s := newServer(t, d)
	calls := 0
	s.gate.docker = &movingTraefik{Fake: d, after: traefikAddr, calls: &calls}
	if w := serve(s, browserReq(http.MethodGet, "/api/session", nil)); w.Code != http.StatusOK {
		t.Fatalf("status %d: %s", w.Code, w.Body)
	}
}

type movingTraefik struct {
	*docker.Fake
	after string
	calls *int
}

func (m *movingTraefik) Traefik(ctx context.Context, refresh bool) (docker.TraefikInfo, error) {
	*m.calls++
	if refresh {
		return docker.TraefikInfo{Found: true, IP: m.after}, nil
	}
	return m.Fake.Traefik(ctx, false)
}

func TestGateRejectsWrongSecret(t *testing.T) {
	s := newServer(t, fakeDocker(traefikAddr))
	for _, v := range []string{"", "wrong", testSecret + "x", strings.ToUpper(testSecret)} {
		r := browserReq(http.MethodGet, "/api/session", nil)
		if v == "" {
			r.Header.Del(gateHeader)
		} else {
			r.Header.Set(gateHeader, v)
		}
		if w := serve(s, r); w.Code != http.StatusForbidden {
			t.Fatalf("secret %q: status %d", v, w.Code)
		}
	}
}

func TestGateRefusesEmptySecret(t *testing.T) {
	s := New(Config{Env: "shop", Domain: testDomain, Docker: fakeDocker(traefikAddr), Log: quietLog()})
	r := browserReq(http.MethodGet, "/api/session", nil)
	r.Header.Set(gateHeader, "")
	if w := serve(s, r); w.Code == http.StatusOK {
		t.Fatal("served a request without a gate secret")
	}
	r.Header.Del(gateHeader)
	if w := serve(s, r); w.Code == http.StatusOK {
		t.Fatal("served a request without a gate secret")
	}
}

func TestGateTraefikAbsent(t *testing.T) {
	for name, d := range map[string]*docker.Fake{
		"not found":  {EnvName: "shop", TraefikInfo: docker.TraefikInfo{LAN: true}},
		"no network": {EnvName: "shop", TraefikInfo: docker.TraefikInfo{Found: true}},
		"error":      {EnvName: "shop", TraefikErr: errors.New("socket gone")},
	} {
		s := newServer(t, d)
		w := serve(s, browserReq(http.MethodGet, "/api/session", nil))
		if w.Code != http.StatusServiceUnavailable {
			t.Fatalf("%s: status %d", name, w.Code)
		}
		if !strings.Contains(w.Body.String(), "Traefik is not connected to shop_default, run roll svc up") {
			t.Fatalf("%s: body %q", name, w.Body)
		}
	}
}

func TestHeaderMatrix(t *testing.T) {
	runs := &runLog{}
	a := &fakeAdapter{kind: core.Redis, present: true, actions: []core.ActionSpec{{Name: "noop", Title: "No-op", Confirm: core.ConfirmNone, Run: runs.run}}}
	s := newServer(t, fakeDocker(traefikAddr), a)
	cases := []struct {
		name   string
		build  func() *http.Request
		status int
	}{
		{"page load", func() *http.Request { return browserReq(http.MethodGet, "/", nil) }, http.StatusOK},
		{"get without origin", func() *http.Request { return browserReq(http.MethodGet, "/api/session", nil) }, http.StatusOK},
		{"get same origin", func() *http.Request {
			r := browserReq(http.MethodGet, "/api/session", nil)
			r.Header.Set("Origin", testOrigin)
			r.Header.Set("Sec-Fetch-Site", "same-origin")
			return r
		}, http.StatusOK},
		{"navigation", func() *http.Request {
			r := browserReq(http.MethodGet, "/", nil)
			r.Header.Set("Sec-Fetch-Site", "none")
			return r
		}, http.StatusOK},
		{"cross-site link to the UI", func() *http.Request {
			r := browserReq(http.MethodGet, "/", nil)
			r.Header.Set("Sec-Fetch-Site", "cross-site")
			r.Header.Set("Sec-Fetch-Mode", "navigate")
			return r
		}, http.StatusOK},
		{"same-site link to the UI", func() *http.Request {
			r := browserReq(http.MethodGet, "/", nil)
			r.Header.Set("Sec-Fetch-Site", "same-site")
			r.Header.Set("Sec-Fetch-Mode", "navigate")
			return r
		}, http.StatusOK},
		{"cross-site navigation to the api", func() *http.Request {
			r := browserReq(http.MethodGet, "/api/session", nil)
			r.Header.Set("Sec-Fetch-Site", "cross-site")
			r.Header.Set("Sec-Fetch-Mode", "navigate")
			return r
		}, http.StatusForbidden},
		{"cross-site script load of the UI", func() *http.Request {
			r := browserReq(http.MethodGet, "/app.js", nil)
			r.Header.Set("Sec-Fetch-Site", "cross-site")
			r.Header.Set("Sec-Fetch-Mode", "no-cors")
			return r
		}, http.StatusForbidden},
		{"cross-site navigation post", func() *http.Request {
			r := browserReq(http.MethodPost, "/", nil)
			r.Header.Set("Sec-Fetch-Site", "cross-site")
			r.Header.Set("Sec-Fetch-Mode", "navigate")
			return r
		}, http.StatusForbidden},
		{"cross origin get", func() *http.Request {
			r := browserReq(http.MethodGet, "/api/session", nil)
			r.Header.Set("Origin", "https://evil.example")
			return r
		}, http.StatusForbidden},
		{"store page origin", func() *http.Request {
			r := browserReq(http.MethodGet, "/api/session", nil)
			r.Header.Set("Origin", "https://app.shop.test")
			return r
		}, http.StatusForbidden},
		{"same-site fetch", func() *http.Request {
			r := browserReq(http.MethodGet, "/api/session", nil)
			r.Header.Set("Sec-Fetch-Site", "same-site")
			return r
		}, http.StatusForbidden},
		{"cross-site fetch", func() *http.Request {
			r := browserReq(http.MethodGet, "/api/stream", nil)
			r.Header.Set("Sec-Fetch-Site", "cross-site")
			return r
		}, http.StatusForbidden},
		{"wrong host", func() *http.Request {
			r := browserReq(http.MethodGet, "/api/session", nil)
			r.Host = "app.shop.test"
			return r
		}, http.StatusMisdirectedRequest},
		{"write", func() *http.Request { return writeReq(s, "/api/actions/redis/noop", map[string]any{}) }, http.StatusOK},
		{"write with charset", func() *http.Request {
			r := writeReq(s, "/api/actions/redis/noop", map[string]any{})
			r.Header.Set("Content-Type", "application/json; charset=utf-8")
			return r
		}, http.StatusOK},
		{"write without origin", func() *http.Request {
			r := writeReq(s, "/api/actions/redis/noop", map[string]any{})
			r.Header.Del("Origin")
			return r
		}, http.StatusForbidden},
		{"write cross origin", func() *http.Request {
			r := writeReq(s, "/api/actions/redis/noop", map[string]any{})
			r.Header.Set("Origin", "https://app.shop.test")
			return r
		}, http.StatusForbidden},
		{"write as form", func() *http.Request {
			r := writeReq(s, "/api/actions/redis/noop", map[string]any{})
			r.Header.Set("Content-Type", "text/plain")
			return r
		}, http.StatusUnsupportedMediaType},
		{"write without csrf", func() *http.Request {
			r := writeReq(s, "/api/actions/redis/noop", map[string]any{})
			r.Header.Del(csrfHeader)
			return r
		}, http.StatusForbidden},
		{"write wrong csrf", func() *http.Request {
			r := writeReq(s, "/api/actions/redis/noop", map[string]any{})
			r.Header.Set(csrfHeader, "nope")
			return r
		}, http.StatusForbidden},
		{"delete counts as write", func() *http.Request {
			r := browserReq(http.MethodDelete, "/api/actions/redis/noop", nil)
			return r
		}, http.StatusForbidden},
	}
	for _, c := range cases {
		w := serve(s, c.build())
		if w.Code != c.status {
			t.Errorf("%s: status %d, want %d (%s)", c.name, w.Code, c.status, strings.TrimSpace(w.Body.String()))
		}
	}
	if runs.count() != 2 {
		t.Fatalf("action ran %d times, want 2", runs.count())
	}
}

func TestLANRefusesWrites(t *testing.T) {
	runs := &runLog{}
	a := &fakeAdapter{kind: core.Redis, present: true, actions: []core.ActionSpec{{Name: "noop", Run: runs.run}}}
	d := fakeDocker(traefikAddr)
	d.TraefikInfo.LAN = true
	d.TraefikInfo.Bindings = []string{"*:443"}
	s := newServer(t, d, a)
	w := serve(s, writeReq(s, "/api/actions/redis/noop", map[string]any{}))
	if w.Code != http.StatusLocked {
		t.Fatalf("status %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "*:443") {
		t.Fatalf("reason does not name the binding: %s", w.Body)
	}
	if runs.count() != 0 {
		t.Fatal("action ran in LAN mode")
	}
	w = serve(s, browserReq(http.MethodGet, "/api/session", nil))
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"lan":true`) {
		t.Fatalf("reads must stay available: %d %s", w.Code, w.Body)
	}
}

func confirmFor(t *testing.T, s *Server, body map[string]any) (int, string) {
	t.Helper()
	w := serve(s, writeReq(s, "/api/confirm", body))
	var res struct {
		Nonce string `json:"nonce"`
		Error string `json:"error"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &res)
	return w.Code, res.Nonce
}

func TestConfirmNonces(t *testing.T) {
	runs := &runLog{}
	a := &fakeAdapter{kind: core.Redis, present: true, actions: []core.ActionSpec{
		{Name: "delete-key", Destructive: true, Confirm: core.ConfirmPlain, Run: runs.run},
		{Name: "flushdb", Destructive: true, Confirm: core.ConfirmTyped, Run: runs.run},
	}}
	s := newServer(t, fakeDocker(traefikAddr), a)
	act := func(name string, body map[string]any) int {
		return serve(s, writeReq(s, "/api/actions/redis/"+name, body)).Code
	}

	if code := act("delete-key", map[string]any{"target": "k1"}); code != http.StatusForbidden {
		t.Fatalf("no nonce: %d", code)
	}

	code, n := confirmFor(t, s, map[string]any{"kind": "redis", "action": "delete-key", "target": "k1", "params": map[string]string{"db": "0"}})
	if code != http.StatusOK || n == "" {
		t.Fatalf("confirm: %d", code)
	}
	if code := act("delete-key", map[string]any{"target": "k1", "params": map[string]string{"db": "0"}, "nonce": n}); code != http.StatusOK {
		t.Fatalf("first use: %d", code)
	}
	if code := act("delete-key", map[string]any{"target": "k1", "params": map[string]string{"db": "0"}, "nonce": n}); code != http.StatusForbidden {
		t.Fatalf("replay: %d", code)
	}

	_, n = confirmFor(t, s, map[string]any{"kind": "redis", "action": "delete-key", "target": "k1"})
	if code := act("delete-key", map[string]any{"target": "k2", "nonce": n}); code != http.StatusForbidden {
		t.Fatalf("wrong target: %d", code)
	}
	if code := act("delete-key", map[string]any{"target": "k1", "nonce": n}); code != http.StatusForbidden {
		t.Fatalf("nonce must be gone after a mismatch: %d", code)
	}

	_, n = confirmFor(t, s, map[string]any{"kind": "redis", "action": "delete-key", "target": "k1", "params": map[string]string{"db": "0"}})
	if code := act("delete-key", map[string]any{"target": "k1", "params": map[string]string{"db": "3"}, "nonce": n}); code != http.StatusForbidden {
		t.Fatalf("wrong params: %d", code)
	}

	_, n = confirmFor(t, s, map[string]any{"kind": "redis", "action": "delete-key", "target": "0"})
	if code := act("flushdb", map[string]any{"target": "0", "nonce": n}); code != http.StatusForbidden {
		t.Fatalf("nonce for another action: %d", code)
	}

	if code, _ := confirmFor(t, s, map[string]any{"kind": "redis", "action": "flushdb", "target": "0", "typed": "1"}); code != http.StatusUnprocessableEntity {
		t.Fatalf("typed mismatch: %d", code)
	}
	if code, _ := confirmFor(t, s, map[string]any{"kind": "redis", "action": "flushdb", "target": "0"}); code != http.StatusUnprocessableEntity {
		t.Fatalf("typed missing: %d", code)
	}
	code, n = confirmFor(t, s, map[string]any{"kind": "redis", "action": "flushdb", "target": "0", "typed": "0"})
	if code != http.StatusOK {
		t.Fatalf("typed match: %d", code)
	}
	if code := act("flushdb", map[string]any{"target": "0", "nonce": n}); code != http.StatusOK {
		t.Fatalf("typed action: %d", code)
	}

	if code, _ := confirmFor(t, s, map[string]any{"kind": "redis", "action": "nope", "target": "0"}); code != http.StatusNotFound {
		t.Fatalf("unknown action: %d", code)
	}
	if runs.count() != 2 {
		t.Fatalf("runs %d, want 2", runs.count())
	}
}

func TestNonceExpires(t *testing.T) {
	n := newNonces()
	now := time.Unix(1000, 0)
	n.now = func() time.Time { return now }
	tok, _ := n.issue("redis/flushdb", "0")
	now = now.Add(31 * time.Second)
	if err := n.use(tok, "redis/flushdb", "0"); !errors.Is(err, errNonceUnknown) {
		t.Fatalf("err %v", err)
	}
}

type streamProbe struct {
	started chan core.Filter
	stopped chan struct{}
}

func probeStream(name string, optIn bool) (core.StreamSpec, *streamProbe) {
	p := &streamProbe{started: make(chan core.Filter, 4), stopped: make(chan struct{})}
	return core.StreamSpec{Name: name, Title: name, OptIn: optIn, Filters: []string{"query", "x", "a", "b"}, Start: func(ctx context.Context, f core.Filter, emit core.Emit) error {
		p.started <- f
		defer close(p.stopped)
		tick := time.NewTicker(10 * time.Millisecond)
		defer tick.Stop()
		n := 0
		for {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-tick.C:
				n++
				emit(core.Event{Type: "varnish.req", Data: core.VarnishReq{VXID: "x", URL: "/p", Status: 200 + n%2}})
			}
		}
	}}, p
}

func TestStreamDeliversAndStopsProducer(t *testing.T) {
	spec, probe := probeStream("log", false)
	a := &fakeAdapter{kind: core.Varnish, present: true, streams: []core.StreamSpec{spec}}
	s := newServer(t, fakeDocker("127.0.0.1"), a)
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, ts.URL+"/api/stream?sub=varnish:log;query=ReqURL%20~%20%22%5E/p%22;x=1", nil)
	req.Host = testHost
	req.Header.Set(gateHeader, testSecret)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK || !strings.HasPrefix(res.Header.Get("Content-Type"), "text/event-stream") {
		t.Fatalf("status %d %s", res.StatusCode, res.Header.Get("Content-Type"))
	}

	select {
	case f := <-probe.started:
		if f["query"] != `ReqURL ~ "^/p"` || f["x"] != "1" {
			t.Fatalf("filter %v", f)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("producer never started")
	}

	sc := bufio.NewScanner(res.Body)
	var gotID, gotEvent, gotData bool
	deadline := time.AfterFunc(3*time.Second, cancel)
	for sc.Scan() {
		line := sc.Text()
		switch {
		case strings.HasPrefix(line, "id: "):
			gotID = true
		case line == "event: varnish.req":
			gotEvent = true
		case strings.HasPrefix(line, "data: ") && gotEvent:
			var ev struct {
				Sub  string          `json:"sub"`
				Kind core.Kind       `json:"kind"`
				Data core.VarnishReq `json:"data"`
			}
			if err := json.Unmarshal([]byte(line[6:]), &ev); err != nil {
				t.Fatal(err)
			}
			if ev.Sub != `varnish:log;query=ReqURL ~ "^/p";x=1` || ev.Kind != core.Varnish || ev.Data.URL != "/p" {
				t.Fatalf("event %+v", ev)
			}
			gotData = true
		}
		if gotID && gotData {
			break
		}
	}
	deadline.Stop()
	if !gotID || !gotData {
		t.Fatal("no event with an id arrived")
	}

	cancel()
	select {
	case <-probe.stopped:
	case <-time.After(3 * time.Second):
		t.Fatal("producer kept running after the client left")
	}
	waitFor(t, func() bool { return len(s.cfg.Hub.Active()) == 0 })
}

func waitFor(t *testing.T, ok func() bool) {
	t.Helper()
	end := time.Now().Add(2 * time.Second)
	for !ok() {
		if time.Now().After(end) {
			t.Fatal("condition not met in time")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestStreamSharesOneProducer(t *testing.T) {
	spec, probe := probeStream("log", false)
	a := &fakeAdapter{kind: core.Varnish, present: true, streams: []core.StreamSpec{spec}}
	s := newServer(t, fakeDocker("127.0.0.1"), a)
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	open := func(ctx context.Context, q string) *http.Response {
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, ts.URL+"/api/stream?"+q, nil)
		req.Host = testHost
		req.Header.Set(gateHeader, testSecret)
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return res
	}
	ctx1, cancel1 := context.WithCancel(context.Background())
	ctx2, cancel2 := context.WithCancel(context.Background())
	defer cancel1()
	defer cancel2()
	r1 := open(ctx1, "sub=varnish:log;b=2;a=1")
	defer r1.Body.Close()
	<-probe.started
	r2 := open(ctx2, "sub=varnish:log;a=1;b=2")
	defer r2.Body.Close()
	time.Sleep(50 * time.Millisecond)
	if n := len(probe.started); n != 0 {
		t.Fatalf("the same filter started %d extra producers", n)
	}
	cancel1()
	time.Sleep(50 * time.Millisecond)
	select {
	case <-probe.stopped:
		t.Fatal("producer stopped while a subscriber remained")
	default:
	}
	cancel2()
	select {
	case <-probe.stopped:
	case <-time.After(3 * time.Second):
		t.Fatal("producer kept running")
	}
}

func TestStreamRejectsBadSubs(t *testing.T) {
	spec, _ := probeStream("capture", true)
	a := &fakeAdapter{kind: core.Search, present: true, streams: []core.StreamSpec{spec}}
	d := fakeDocker(traefikAddr)
	s := newServer(t, d, a)
	for _, q := range []string{"sub=search", "sub=search:nope", "sub=mongo:log", "sub=search:capture;novalue", "sub=search:capture;bogus=1"} {
		w := serve(s, browserReq(http.MethodGet, "/api/stream?"+q, nil))
		if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), `"hint"`) {
			t.Errorf("%s: status %d %s", q, w.Code, w.Body)
		}
	}
	if w := serve(s, browserReq(http.MethodGet, "/api/stream?sub=search:capture;bogus=1", nil)); !strings.Contains(w.Body.String(), "query and x and a and b") {
		t.Errorf("unknown filter without the keys it takes: %s", w.Body)
	}
	d.SetTraefik(docker.TraefikInfo{Found: true, IP: traefikAddr, LAN: true}, nil)
	r := browserReq(http.MethodGet, "/api/stream?sub=search:capture", nil)
	r.Header.Set("Sec-Fetch-Site", "same-origin")
	w := serve(s, r)
	if w.Code != http.StatusLocked {
		t.Fatalf("opt-in capture in LAN mode: status %d", w.Code)
	}
}

func serveBriefly(s *Server, r *http.Request, d time.Duration) *httptest.ResponseRecorder {
	ctx, cancel := context.WithTimeout(r.Context(), d)
	defer cancel()
	return serve(s, r.WithContext(ctx))
}

func TestStreamOptInNeedsTheInspectorPage(t *testing.T) {
	idle := func(ctx context.Context, f core.Filter, emit core.Emit) error {
		<-ctx.Done()
		return ctx.Err()
	}
	capture := core.StreamSpec{Name: "capture", Title: "capture", OptIn: true, Start: idle}
	monitor := core.StreamSpec{Name: "monitor", Title: "monitor", Start: idle}
	search := &fakeAdapter{kind: core.Search, present: true, streams: []core.StreamSpec{capture}}
	redis := &fakeAdapter{kind: core.Redis, present: true, streams: []core.StreamSpec{monitor}}
	s := newServer(t, fakeDocker(traefikAddr), search, redis)
	cases := []struct {
		name   string
		method string
		query  string
		site   string
		status int
	}{
		{"capture without fetch metadata", http.MethodGet, "sub=search:capture", "", http.StatusForbidden},
		{"capture from a navigation", http.MethodGet, "sub=search:capture", "none", http.StatusForbidden},
		{"capture next to a plain sub", http.MethodGet, "sub=redis:monitor&sub=search:capture", "", http.StatusForbidden},
		{"capture as HEAD", http.MethodHead, "sub=search:capture", "same-origin", http.StatusMethodNotAllowed},
		{"capture from the page", http.MethodGet, "sub=search:capture", "same-origin", http.StatusOK},
		{"plain sub without fetch metadata", http.MethodGet, "sub=redis:monitor", "", http.StatusOK},
	}
	for _, c := range cases {
		r := browserReq(c.method, "/api/stream?"+c.query, nil)
		if c.site != "" {
			r.Header.Set("Sec-Fetch-Site", c.site)
		}
		w := serveBriefly(s, r, 100*time.Millisecond)
		if w.Code != c.status {
			t.Errorf("%s: status %d, want %d (%s)", c.name, w.Code, c.status, strings.TrimSpace(w.Body.String()))
		}
		if c.status == http.StatusMethodNotAllowed && w.Header().Get("Allow") != http.MethodGet {
			t.Errorf("%s: Allow %q", c.name, w.Header().Get("Allow"))
		}
	}
}

func TestStreamRefusesEveryMethodButGet(t *testing.T) {
	idle := func(ctx context.Context, f core.Filter, emit core.Emit) error {
		<-ctx.Done()
		return ctx.Err()
	}
	redis := &fakeAdapter{kind: core.Redis, present: true, streams: []core.StreamSpec{{Name: "monitor", Title: "monitor", Start: idle}}}
	s := newServer(t, fakeDocker(traefikAddr), redis)
	for _, m := range []string{http.MethodHead, http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete, http.MethodOptions} {
		r := writeReq(s, "/api/stream?sub=redis:monitor", map[string]any{})
		r.Method = m
		w := serveBriefly(s, r, 100*time.Millisecond)
		if w.Code != http.StatusMethodNotAllowed || w.Header().Get("Allow") != http.MethodGet {
			t.Errorf("%s: status %d, Allow %q (%s)", m, w.Code, w.Header().Get("Allow"), strings.TrimSpace(w.Body.String()))
		}
		if ct := w.Header().Get("Content-Type"); ct == "text/event-stream" {
			t.Errorf("%s: opened an event stream", m)
		}
	}
	if w := serveBriefly(s, browserReq(http.MethodGet, "/api/stream?sub=redis:monitor", nil), 100*time.Millisecond); w.Code != http.StatusOK {
		t.Fatalf("GET: status %d", w.Code)
	}
}

func TestStreamErrorArrivesOnce(t *testing.T) {
	spec := core.StreamSpec{Name: "insights", Title: "insights", Start: func(ctx context.Context, f core.Filter, emit core.Emit) error {
		emit(core.Event{Type: "stream.error", Data: "Query Insights needs OpenSearch 2.12"})
		<-ctx.Done()
		return ctx.Err()
	}}
	a := &fakeAdapter{kind: core.Search, present: true, streams: []core.StreamSpec{spec}}
	s := newServer(t, fakeDocker(traefikAddr), a)
	w := serveBriefly(s, browserReq(http.MethodGet, "/api/stream?sub=search:insights", nil), 400*time.Millisecond)
	if n := strings.Count(w.Body.String(), "event: stream.error\n"); n != 1 {
		t.Fatalf("stream.error sent %d times:\n%s", n, w.Body)
	}
}

func TestRingReplay(t *testing.T) {
	r := newRing(4)
	for i := 0; i < 3; i++ {
		r.add("a", core.Event{Type: "x"})
		r.add("b", core.Event{Type: "y"})
	}
	got, last, gap := r.since(1, map[string]bool{"a": true})
	if last != 6 || gap {
		t.Fatalf("last %d gap %v", last, gap)
	}
	if len(got) != 2 || got[0].id != 3 || got[1].id != 5 {
		t.Fatalf("got %+v", got)
	}
	if _, _, gap := r.since(0, map[string]bool{"a": true}); !gap {
		t.Fatal("no gap after losing a")
	}
	if _, _, gap := r.since(1, map[string]bool{"b": true}); !gap {
		t.Fatal("no gap after losing b")
	}
	got, _, gap = r.since(4, map[string]bool{"a": true, "b": true})
	if gap || len(got) != 2 || got[0].id != 5 {
		t.Fatalf("got %+v gap %v", got, gap)
	}
	if got, _, _ := r.since(6, map[string]bool{"a": true}); len(got) != 0 {
		t.Fatalf("got %+v", got)
	}
}

func TestRingGapIsPerKey(t *testing.T) {
	r := newRing(4)
	r.add("a", core.Event{Type: "x"})
	r.add("a", core.Event{Type: "x"})
	for i := 0; i < 4; i++ {
		r.add("b", core.Event{Type: "y"})
	}
	if got, _, gap := r.since(0, map[string]bool{"b": true}); gap || len(got) != 4 {
		t.Fatalf("b lost nothing: got %d gap %v", len(got), gap)
	}
	if _, _, gap := r.since(0, map[string]bool{"a": true}); !gap {
		t.Fatal("no gap after a was overwritten")
	}
	if _, _, gap := r.since(2, map[string]bool{"a": true}); gap {
		t.Fatal("gap for a client that already had every a")
	}
}

func TestParseSubsKeepsSemicolons(t *testing.T) {
	subs, err := parseSubs("sub=varnish:log;query=a%3Db&other=1&sub=redis%3Amonitor%3Bdb%3D2")
	if err != nil {
		t.Fatal(err)
	}
	if len(subs) != 2 || subs[0] != "varnish:log;query=a=b" || subs[1] != "redis:monitor;db=2" {
		t.Fatalf("subs %q", subs)
	}
}

func TestBrowseRoutesAndOverview(t *testing.T) {
	varnish := &fakeAdapter{kind: core.Varnish, present: false}
	redis := &fakeAdapter{kind: core.Redis, present: true, routes: []core.Route{{
		Method: http.MethodGet, Path: "/key/{name}",
		Handler: func(w http.ResponseWriter, r *http.Request) {
			writeJSON(w, http.StatusOK, map[string]string{"name": r.PathValue("name")})
		},
	}}}
	s := newServer(t, fakeDocker(traefikAddr), varnish, redis)

	w := serve(s, browserReq(http.MethodGet, "/api/redis/key/abc", nil))
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"name":"abc"`) {
		t.Fatalf("browse: %d %s", w.Code, w.Body)
	}
	if w := serve(s, browserReq(http.MethodGet, "/api/mongo/info", nil)); w.Code != http.StatusNotFound || !strings.Contains(w.Body.String(), "error") {
		t.Fatalf("unknown kind: %d %s", w.Code, w.Body)
	}
	if w := serve(s, browserReq(http.MethodGet, "/api/redis/info", nil)); w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"present":true`) {
		t.Fatalf("info: %d %s", w.Code, w.Body)
	}

	w = serve(s, browserReq(http.MethodGet, "/api/overview", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("overview: %d %s", w.Code, w.Body)
	}
	var ov overview
	if err := json.Unmarshal(w.Body.Bytes(), &ov); err != nil {
		t.Fatal(err)
	}
	if len(ov.Adapters) != 2 || ov.Adapters[0].Health != nil || ov.Adapters[1].Health == nil || ov.Adapters[1].Health.Summary != "fine" {
		t.Fatalf("adapters %+v", ov.Adapters)
	}
	found := false
	for _, wn := range ov.Warnings {
		found = found || wn.ID == "no-request-source"
	}
	if !found {
		t.Fatalf("warnings %+v", ov.Warnings)
	}
}

func TestTraceRoutesWithoutStore(t *testing.T) {
	s := newServer(t, fakeDocker(traefikAddr))
	if w := serve(s, browserReq(http.MethodGet, "/api/traces", nil)); w.Code != http.StatusOK || strings.TrimSpace(w.Body.String()) != "[]" {
		t.Fatalf("traces: %d %s", w.Code, w.Body)
	}
	if w := serve(s, browserReq(http.MethodGet, "/api/traces/abc/profile", nil)); w.Code != http.StatusNotFound {
		t.Fatalf("profile: %d", w.Code)
	}
	if w := serve(s, browserReq(http.MethodGet, "/api/timeline", nil)); w.Code != http.StatusOK {
		t.Fatalf("timeline: %d", w.Code)
	}
}
