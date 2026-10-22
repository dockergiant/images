package rabbitmq

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/dockergiant/images/inspector/internal/capture"
	"github.com/dockergiant/images/inspector/internal/core"
	"github.com/dockergiant/images/inspector/internal/docker"
	"github.com/dockergiant/images/inspector/internal/magento"
	"github.com/dockergiant/images/inspector/internal/services"
)

type fakeMagento struct {
	env       *magento.Env
	consumers map[string]string
}

func (f fakeMagento) Load(context.Context) (*magento.Env, error) { return f.env, nil }
func (f fakeMagento) CacheTypeTags(context.Context) (map[string]string, error) {
	return nil, nil
}
func (f fakeMagento) FPCKey(context.Context, string) (string, error) { return "", nil }
func (f fakeMagento) ConfigShow(context.Context, []string) (map[string]string, error) {
	return nil, nil
}
func (f fakeMagento) Consumers(context.Context) (map[string]string, error) { return f.consumers, nil }
func (f fakeMagento) CacheClean(context.Context, []string) error           { return nil }

type broker struct {
	mu       sync.Mutex
	requests []string
	tracing  bool
	queues   string
	consumed int64
	qtype    string
}

func (b *broker) handler(t *testing.T) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		b.mu.Lock()
		defer b.mu.Unlock()
		b.requests = append(b.requests, r.Method+" "+r.URL.EscapedPath())
		p := r.URL.EscapedPath()
		switch {
		case r.Method == http.MethodPut && p == "/api/vhosts/%2F":
			var body map[string]bool
			json.NewDecoder(r.Body).Decode(&body)
			b.tracing = body["tracing"]
			w.WriteHeader(http.StatusNoContent)
		case p == "/api/vhosts/%2F":
			json.NewEncoder(w).Encode(map[string]any{"name": "/", "tracing": b.tracing})
		case p == "/api/overview":
			w.Write([]byte(`{"rabbitmq_version":"4.3.1","message_stats":{"publish":10}}`))
		case p == "/api/bindings/%2F":
			w.Write([]byte(`[]`))
		case strings.HasPrefix(p, "/api/queues/%2F/") && strings.HasSuffix(p, "/contents"):
			w.WriteHeader(http.StatusNoContent)
		case strings.HasPrefix(p, "/api/queues/%2F/"):
			w.Write([]byte(`{"name":"q","messages_ready":4,"consumers":` + itoa(b.consumed) + `,"type":"` + b.qtype + `"}`))
		case strings.HasPrefix(p, "/api/queues/%2F"):
			w.Write([]byte(b.queues))
		default:
			t.Errorf("unexpected request %s %s", r.Method, p)
			http.NotFound(w, r)
		}
	}
}

func itoa(n int64) string {
	b, _ := json.Marshal(n)
	return string(b)
}

func newTestAdapter(t *testing.T, b *broker) (*adapter, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(b.handler(t))
	t.Cleanup(srv.Close)
	d := services.Deps{
		Docker:  &docker.Fake{},
		Magento: fakeMagento{},
		Capture: capture.New(nil),
	}
	a := &adapter{deps: d, override: &target{base: srv.URL, user: "guest", pass: "guest", vhost: "/", amqpPort: 5672}}
	return a, srv
}

func TestTargetCredentialsFromEnvPhpAndGuestFallback(t *testing.T) {
	fk := &docker.Fake{ContainerList: []docker.Container{{ID: "r1", Service: "rabbitmq", Running: true, IP: "10.1.2.3", Name: "x-rabbitmq-1"}}}
	a := &adapter{deps: services.Deps{Docker: fk, Magento: fakeMagento{env: &magento.Env{AMQP: &magento.AMQP{User: "mq", Password: "pw", VHost: "shop", Port: 5673}}}}}
	tg, err := a.target(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if tg.user != "mq" || tg.pass != "pw" || tg.vhost != "shop" || tg.amqpPort != 5673 || tg.base != "http://10.1.2.3:15672" {
		t.Fatalf("%+v", tg)
	}
	for _, env := range []*magento.Env{nil, {}, {AMQP: &magento.AMQP{Host: "rabbitmq"}}} {
		a := &adapter{deps: services.Deps{Docker: fk, Magento: fakeMagento{env: env}}}
		tg, err := a.target(context.Background())
		if err != nil || tg.user != "guest" || tg.pass != "guest" || tg.vhost != "/" {
			t.Fatalf("env %+v: %+v %v", env, tg, err)
		}
	}
}

func TestDetectAbsentService(t *testing.T) {
	a := &adapter{deps: services.Deps{Docker: &docker.Fake{}, Magento: fakeMagento{}}}
	info, err := a.Detect(context.Background())
	if err != nil || info.Present || info.Kind != core.RabbitMQ {
		t.Fatalf("%+v %v", info, err)
	}
	h, err := a.Health(context.Background())
	if err != nil || h.Status != core.ToneInfo {
		t.Fatalf("%+v %v", h, err)
	}
}

func TestDetectVersionAndTested(t *testing.T) {
	a, _ := newTestAdapter(t, &broker{queues: `[]`})
	info, err := a.Detect(context.Background())
	if err != nil || !info.Present || info.Version != "4.3.1" || !info.Tested || !info.Features["management"] {
		t.Fatalf("%+v %v", info, err)
	}
	if isTested("3.13.7") || !isTested("4.1.8") || isTested("4.10.0") {
		t.Fatal("tested version matching is off")
	}
}

func TestDetectReportsOrphanTracing(t *testing.T) {
	a, _ := newTestAdapter(t, &broker{tracing: true, queues: `[]`})
	info, _ := a.Detect(context.Background())
	if !info.Features["tracing"] || !info.Features["tracingOrphan"] {
		t.Fatalf("%+v", info.Features)
	}
	if err := a.deps.Capture.Begin(firehoseKey, 1<<40, func(context.Context) error { return nil }); err != nil {
		t.Fatal(err)
	}
	info, _ = a.Detect(context.Background())
	if info.Features["tracingOrphan"] {
		t.Fatal("own capture is not an orphan")
	}
}

func TestDetectAuthError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(401) }))
	defer srv.Close()
	a := &adapter{deps: services.Deps{Docker: &docker.Fake{}, Magento: fakeMagento{}}, override: &target{base: srv.URL, user: "u", pass: "p", vhost: "/"}}
	info, _ := a.Detect(context.Background())
	if !info.Present || !strings.Contains(info.Error, "no retry with guest") {
		t.Fatalf("%+v", info)
	}
	h, _ := a.Health(context.Background())
	if h.Status != core.Error {
		t.Fatalf("%+v", h)
	}
}

func TestHealthCronAbsentFromExitCode(t *testing.T) {
	b := &broker{queues: `[{"name":"stuck","messages_ready":3,"consumers":0}]`}
	a, _ := newTestAdapter(t, b)
	fk := &docker.Fake{
		ContainerList: []docker.Container{{ID: "fpm", Service: "php-fpm", Running: true}},
		ExecFunc: func(_ context.Context, spec docker.ExecSpec) (docker.ExecResult, error) {
			return docker.ExecResult{ExitCode: 1, Stderr: []byte("no crontab for www-data")}, nil
		},
	}
	a.deps.Docker = fk
	h, err := a.Health(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(h.Warnings) != 1 || !strings.Contains(h.Warnings[0].Text, "cron is not installed") {
		t.Fatalf("%+v", h.Warnings)
	}
	execs := fk.Execs()
	if len(execs) != 1 || strings.Join(execs[0].Cmd, " ") != "crontab -l" || execs[0].User != magento.PHPUser {
		t.Fatalf("%+v", execs)
	}

	fk.ExecFunc = func(context.Context, docker.ExecSpec) (docker.ExecResult, error) {
		return docker.ExecResult{ExitCode: 1, Stderr: []byte("crontab: permission denied")}, nil
	}
	h, _ = a.Health(context.Background())
	for _, w := range h.Warnings {
		if strings.Contains(w.Text, "cron is not installed") {
			t.Fatalf("a permission error is not an absent crontab: %+v", h.Warnings)
		}
	}

	fk.ExecFunc = func(context.Context, docker.ExecSpec) (docker.ExecResult, error) { return docker.ExecResult{}, nil }
	h, _ = a.Health(context.Background())
	if !strings.Contains(h.Warnings[0].Text, "is cron running?") {
		t.Fatalf("%+v", h.Warnings)
	}
}

func TestHealthSecondCallProducesRates(t *testing.T) {
	a, _ := newTestAdapter(t, &broker{queues: `[]`})
	h1, _ := a.Health(context.Background())
	if h1.Cards[3].Value != "n/a" {
		t.Fatalf("%+v", h1.Cards[3])
	}
	h2, _ := a.Health(context.Background())
	if !strings.HasSuffix(h2.Cards[3].Value, "/s") {
		t.Fatalf("%+v", h2.Cards[3])
	}
}

func action(a *adapter, name string) core.ActionSpec {
	for _, s := range a.Actions() {
		if s.Name == name {
			return s
		}
	}
	panic("no action " + name)
}

func TestPurgeActionIsTypedAndStatesMagentoEffect(t *testing.T) {
	b := &broker{}
	a, _ := newTestAdapter(t, b)
	spec := action(a, "purge-queue")
	if spec.Confirm != core.ConfirmTyped || !spec.Destructive {
		t.Fatalf("%+v", spec)
	}
	res, err := spec.Run(context.Background(), core.ActionRequest{Target: "async.operations.all"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Message, "Purged 4") || !strings.Contains(res.Message, "Magento does not republish") {
		t.Fatal(res.Message)
	}
	if got := b.requests[len(b.requests)-1]; got != "DELETE /api/queues/%2F/async.operations.all/contents" {
		t.Fatal(got)
	}
	if _, err := spec.Run(context.Background(), core.ActionRequest{}); err == nil {
		t.Fatal("empty target must fail")
	}
}

func TestTracingOffAction(t *testing.T) {
	b := &broker{tracing: true}
	a, _ := newTestAdapter(t, b)
	spec := action(a, "tracing-off")
	if spec.Confirm != core.ConfirmPlain || spec.Destructive {
		t.Fatalf("%+v", spec)
	}
	if _, err := spec.Run(context.Background(), core.ActionRequest{}); err != nil {
		t.Fatal(err)
	}
	if b.tracing {
		t.Fatal("tracing should be off")
	}
}

func route(a *adapter, method, path string) core.Route {
	for _, r := range a.Browse() {
		if r.Method == method && r.Path == path {
			return r
		}
	}
	panic("no route " + path)
}

func TestPeekRefusedWhileConsumerAttached(t *testing.T) {
	b := &broker{consumed: 1}
	a, _ := newTestAdapter(t, b)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/peek", bytes.NewBufferString(`{"queue":"q"}`))
	route(a, http.MethodPost, "/peek").Handler(rec, req)
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "consumer") {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	for _, r := range b.requests {
		if strings.HasSuffix(r, "/get") {
			t.Fatal("peek must not reach the broker")
		}
	}
}

func TestPeekRefusedOnQuorumQueue(t *testing.T) {
	b := &broker{qtype: "quorum"}
	a, _ := newTestAdapter(t, b)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/peek", bytes.NewBufferString(`{"queue":"q"}`))
	route(a, http.MethodPost, "/peek").Handler(rec, req)
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "quorum") {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	for _, r := range b.requests {
		if strings.HasSuffix(r, "/get") {
			t.Fatal("peek must not reach the broker")
		}
	}
}

func TestQueuesRoute(t *testing.T) {
	a, _ := newTestAdapter(t, &broker{queues: string(fixture(t, "4.3-queues_paged.json"))})
	rec := httptest.NewRecorder()
	route(a, http.MethodGet, "/queues").Handler(rec, httptest.NewRequest(http.MethodGet, "/queues", nil))
	var out struct {
		Queues []queueRow `json:"queues"`
	}
	if rec.Code != 200 || json.Unmarshal(rec.Body.Bytes(), &out) != nil || len(out.Queues) != 24 {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
}

func TestStreamsDeclareOptInFirehose(t *testing.T) {
	a := &adapter{}
	var rates, fire *core.StreamSpec
	ss := a.Streams()
	for i := range ss {
		switch ss[i].Name {
		case "rates":
			rates = &ss[i]
		case "firehose":
			fire = &ss[i]
		}
	}
	if rates == nil || rates.OptIn || fire == nil || !fire.OptIn {
		t.Fatalf("%+v", ss)
	}
}

func TestFirehoseRestoresTracingWhenAMQPFails(t *testing.T) {
	b := &broker{queues: `[]`}
	a, _ := newTestAdapter(t, b)
	a.override.amqpAddr = "amqp://guest:guest@127.0.0.1:1/%2F"
	err := a.firehose(context.Background(), nil, func(core.Event) {})
	if err == nil {
		t.Fatal("dial to a closed port must fail")
	}
	if b.tracing {
		t.Fatal("tracing must be restored to off")
	}
	puts := 0
	for _, r := range b.requests {
		if strings.HasPrefix(r, "PUT ") {
			puts++
		}
	}
	if puts != 2 {
		t.Fatalf("want on+off, got %v", b.requests)
	}
	if a.deps.Capture.Active(firehoseKey) {
		t.Fatal("capture session leaked")
	}
}
