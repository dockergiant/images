//go:build integration

package inspector_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	goredis "github.com/redis/go-redis/v9"

	"github.com/dockergiant/images/inspector/internal/capture"
	"github.com/dockergiant/images/inspector/internal/core"
	"github.com/dockergiant/images/inspector/internal/docker"
	"github.com/dockergiant/images/inspector/internal/magento"
	"github.com/dockergiant/images/inspector/internal/procmap"
	"github.com/dockergiant/images/inspector/internal/services"
	"github.com/dockergiant/images/inspector/internal/services/rabbitmq"
	"github.com/dockergiant/images/inspector/internal/services/redis"
	"github.com/dockergiant/images/inspector/internal/services/search"
	"github.com/dockergiant/images/inspector/internal/services/varnish"
)

const (
	testEnv      = "inspectortest"
	searchBase   = "http://opensearch:9200"
	rabbitBase   = "http://rabbitmq:15672"
	captureIndex = "inspectortest_product_1_v1"
	queryWarn    = "index.search.slowlog.threshold.query.warn"
	fetchWarn    = "index.search.slowlog.threshold.fetch.warn"
	crashHelper  = "INSPECTOR_IT_CAPTURE_HELPER"
	helperReady  = "CAPTURING"
)

func testDeps(t *testing.T) services.Deps {
	t.Helper()
	env := os.Getenv("ROLL_ENV_NAME")
	if env != testEnv {
		t.Skipf("ROLL_ENV_NAME=%q; this test only runs against the %s compose stack (scripts/integration)", env, testEnv)
	}
	d, err := docker.New(env)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	return services.Deps{
		Docker:  d,
		Magento: magento.NewReader(d),
		Proc:    procmap.New(d),
		Capture: capture.New(log),
		Log:     log,
	}
}

type events struct {
	ch   chan core.Event
	done chan error
}

func start(t *testing.T, ctx context.Context, a core.Adapter, name string) *events {
	t.Helper()
	for _, s := range a.Streams() {
		if s.Name != name {
			continue
		}
		ev := &events{ch: make(chan core.Event, 4096), done: make(chan error, 1)}
		go func() {
			ev.done <- s.Start(ctx, core.Filter{}, func(e core.Event) {
				select {
				case ev.ch <- e:
				default:
				}
			})
		}()
		return ev
	}
	t.Fatalf("%s has no stream %s", a.Kind(), name)
	return nil
}

// wait pokes until match sees an event or the deadline passes; poke runs at
// once and then every interval, because the exec or connection behind a
// stream comes up a moment after Start.
func (ev *events) wait(t *testing.T, within, interval time.Duration, poke func(), match func(core.Event) bool) core.Event {
	t.Helper()
	deadline := time.After(within)
	tick := time.NewTicker(interval)
	defer tick.Stop()
	poke()
	for {
		select {
		case e := <-ev.ch:
			if match(e) {
				return e
			}
		case err := <-ev.done:
			t.Fatalf("stream ended early: %v", err)
		case <-tick.C:
			poke()
		case <-deadline:
			t.Fatalf("no matching event within %s", within)
		}
	}
}

func (ev *events) stop(t *testing.T, cancel context.CancelFunc) {
	t.Helper()
	cancel()
	select {
	case err := <-ev.done:
		if err != nil && !errors.Is(err, context.Canceled) {
			t.Errorf("stream returned %v", err)
		}
	case <-time.After(20 * time.Second):
		t.Error("stream did not stop after cancel")
	}
}

func httpJSON(t *testing.T, method, url string, body any, out any) int {
	t.Helper()
	var r io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		r = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, url, r)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	if strings.HasPrefix(url, rabbitBase) {
		req.SetBasicAuth("guest", "guest")
	}
	res, err := (&http.Client{Timeout: 20 * time.Second}).Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	defer res.Body.Close()
	data, _ := io.ReadAll(res.Body)
	if out != nil && len(data) > 0 {
		if err := json.Unmarshal(data, out); err != nil {
			t.Fatalf("%s %s: %v: %s", method, url, err, data)
		}
	}
	return res.StatusCode
}

func TestIntegrationDetectAndHealth(t *testing.T) {
	deps := testDeps(t)
	want := []struct {
		adapter core.Adapter
		engine  string
	}{
		{varnish.New(deps), "Varnish"},
		{redis.New(deps), "valkey"},
		{search.New(deps), search.EngineOpenSearch},
		{rabbitmq.New(deps), "RabbitMQ"},
	}
	for _, w := range want {
		t.Run(string(w.adapter.Kind()), func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			info, err := w.adapter.Detect(ctx)
			if err != nil || info.Error != "" {
				t.Fatalf("detect %+v: %v", info, err)
			}
			if !info.Present || info.Engine != w.engine || info.Version == "" || info.Container == "" {
				t.Fatalf("detect %+v, want engine %s", info, w.engine)
			}
			h, err := w.adapter.Health(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if len(h.Cards) == 0 || h.Status == core.Error {
				t.Fatalf("health %+v", h)
			}
		})
	}
}

func TestIntegrationVarnishRequestEvent(t *testing.T) {
	deps := testDeps(t)
	a := varnish.New(deps)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if _, err := a.Detect(ctx); err != nil {
		t.Fatal(err)
	}
	ev := start(t, ctx, a, "log")
	url := fmt.Sprintf("/?it=%d", time.Now().UnixNano())
	c := &http.Client{Timeout: 2 * time.Second}
	ev.wait(t, 5*time.Second, 500*time.Millisecond, func() {
		if res, err := c.Get("http://varnish" + url); err == nil {
			res.Body.Close()
		}
	}, func(e core.Event) bool {
		r, ok := e.Data.(core.VarnishReq)
		return e.Type == "varnish.req" && ok && r.URL == url && r.Status == http.StatusOK
	})
	ev.stop(t, cancel)
}

func TestIntegrationRedisCommandEvent(t *testing.T) {
	deps := testDeps(t)
	a := redis.New(deps)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if _, err := a.Detect(ctx); err != nil {
		t.Fatal(err)
	}
	key := fmt.Sprintf("inspectortest:%d", time.Now().UnixNano())
	t.Cleanup(func() {
		c := goredis.NewClient(&goredis.Options{Addr: "redis:6379", DB: 9, Protocol: 2})
		c.Del(context.Background(), key)
		c.Close()
	})
	rc, err := deps.Docker.Service(ctx, "redis")
	if err != nil || rc == nil {
		t.Fatalf("redis container: %+v %v", rc, err)
	}

	ev := start(t, ctx, a, "monitor")
	ev.wait(t, 5*time.Second, time.Hour, func() {}, func(e core.Event) bool { return e.Type == "redis.monitor" })
	got := ev.wait(t, 5*time.Second, time.Hour, func() {
		// From inside the redis container: MONITOR drops commands from
		// Inspector's own address, which this test process shares.
		res, err := deps.Docker.Exec(ctx, docker.ExecSpec{Container: rc.ID, Cmd: []string{"valkey-cli", "-n", "9", "SET", key, "v", "EX", "60"}})
		if err != nil || res.ExitCode != 0 {
			t.Errorf("set: %v %s", err, res.Stderr)
		}
	}, func(e core.Event) bool {
		cmd, ok := e.Data.(core.RedisCmd)
		return e.Type == "redis.cmd" && ok && strings.EqualFold(cmd.Cmd, "set") && len(cmd.Args) > 0 && cmd.Args[0] == key
	})
	if cmd := got.Data.(core.RedisCmd); cmd.DB != 9 {
		t.Errorf("db %d, want 9", cmd.DB)
	}
	ev.stop(t, cancel)
}

func thresholds(t *testing.T) map[string]any {
	t.Helper()
	var resp map[string]struct {
		Settings map[string]any `json:"settings"`
	}
	if code := httpJSON(t, http.MethodGet, searchBase+"/"+captureIndex+"/_settings?flat_settings=true", nil, &resp); code != http.StatusOK {
		t.Fatalf("settings: %d", code)
	}
	return map[string]any{queryWarn: resp[captureIndex].Settings[queryWarn], fetchWarn: resp[captureIndex].Settings[fetchWarn]}
}

// TestIntegrationCaptureHelper is the process TestIntegrationSearchCaptureCrash
// kills: it opens a capture and blocks until it is killed.
func TestIntegrationCaptureHelper(t *testing.T) {
	if os.Getenv(crashHelper) != "1" {
		t.Skip("runs only as the child of TestIntegrationSearchCaptureCrash")
	}
	deps := testDeps(t)
	ctx := context.Background()
	ev := start(t, ctx, search.New(deps), "capture")
	ev.wait(t, 30*time.Second, time.Hour, func() {}, func(e core.Event) bool { return e.Type == "search.capture" })
	fmt.Println(helperReady)
	select {}
}

func TestIntegrationSearchCaptureCrash(t *testing.T) {
	testDeps(t)
	cleanup := func() {
		for _, ix := range []string{captureIndex, ".roll-inspector"} {
			httpJSON(t, http.MethodDelete, searchBase+"/"+ix, nil, nil)
		}
	}
	cleanup()
	t.Cleanup(cleanup)
	settings := map[string]any{"settings": map[string]any{"number_of_replicas": 0, queryWarn: "5s"}}
	if code := httpJSON(t, http.MethodPut, searchBase+"/"+captureIndex, settings, nil); code != http.StatusOK {
		t.Fatalf("create index: %d", code)
	}
	before := thresholds(t)
	if before[queryWarn] != "5s" || before[fetchWarn] != nil {
		t.Fatalf("prior thresholds %v", before)
	}

	child := exec.Command(os.Args[0], "-test.run=^TestIntegrationCaptureHelper$", "-test.v")
	child.Env = append(os.Environ(), crashHelper+"=1")
	out, err := child.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	child.Stderr = os.Stderr
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { child.Process.Kill(); child.Wait() })
	ready := make(chan bool, 1)
	go func() {
		sc := bufio.NewScanner(out)
		for sc.Scan() {
			if sc.Text() == helperReady {
				ready <- true
				break
			}
		}
		io.Copy(io.Discard, out)
		close(ready)
	}()
	select {
	case ok := <-ready:
		if !ok {
			t.Fatal("capture helper exited before it captured")
		}
	case <-time.After(45 * time.Second):
		t.Fatal("capture helper did not report a capture")
	}

	if got := thresholds(t); got[queryWarn] != "0ms" || got[fetchWarn] != "0ms" {
		t.Fatalf("thresholds during capture %v", got)
	}
	if err := child.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	child.Wait()
	if got := thresholds(t); got[queryWarn] != "0ms" {
		t.Fatalf("the killed capture should leave its thresholds behind, got %v", got)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	info, err := search.New(testDeps(t)).Detect(ctx)
	if err != nil || info.Error != "" {
		t.Fatalf("detect after crash %+v: %v", info, err)
	}
	if got := thresholds(t); got[queryWarn] != before[queryWarn] || got[fetchWarn] != before[fetchWarn] {
		t.Fatalf("thresholds after reconcile %v, want %v", got, before)
	}
	var marker struct {
		Found bool `json:"found"`
	}
	httpJSON(t, http.MethodGet, searchBase+"/.roll-inspector/_doc/"+captureIndex, nil, &marker)
	if marker.Found {
		t.Error("reconcile left the capture marker")
	}
}

func tracing(t *testing.T) bool {
	t.Helper()
	var v struct {
		Tracing bool `json:"tracing"`
	}
	if code := httpJSON(t, http.MethodGet, rabbitBase+"/api/vhosts/%2F", nil, &v); code != http.StatusOK {
		t.Fatalf("vhost: %d", code)
	}
	return v.Tracing
}

func TestIntegrationRabbitFirehose(t *testing.T) {
	deps := testDeps(t)
	exchange := rabbitBase + "/api/exchanges/%2F/magento"
	if code := httpJSON(t, http.MethodPut, exchange, map[string]any{"type": "topic", "durable": false}, nil); code/100 != 2 {
		t.Fatalf("declare exchange: %d", code)
	}
	t.Cleanup(func() { httpJSON(t, http.MethodDelete, exchange, nil, nil) })
	if tracing(t) {
		t.Fatal("tracing already on before the firehose started")
	}

	a := rabbitmq.New(deps)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if _, err := a.Detect(ctx); err != nil {
		t.Fatal(err)
	}
	ev := start(t, ctx, a, "firehose")
	key := fmt.Sprintf("inspectortest.%d", time.Now().UnixNano())
	publish := map[string]any{"properties": map[string]any{}, "routing_key": key, "payload": "hello", "payload_encoding": "string"}
	got := ev.wait(t, 15*time.Second, 500*time.Millisecond, func() {
		httpJSON(t, http.MethodPost, exchange+"/publish", publish, nil)
	}, func(e core.Event) bool {
		p, ok := e.Data.(core.MQPub)
		return e.Type == "mq.pub" && ok && p.RoutingKey == key
	})
	if p := got.Data.(core.MQPub); p.Exchange != "magento" || p.Source != "firehose" || p.Size != len("hello") {
		t.Errorf("publish event %+v", p)
	}
	if !tracing(t) {
		t.Error("tracing off while the firehose runs")
	}
	ev.stop(t, cancel)
	if tracing(t) {
		t.Error("tracing still on after the firehose stopped")
	}
}
