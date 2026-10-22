package rabbitmq

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"

	"github.com/dockergiant/images/inspector/internal/core"
	"github.com/dockergiant/images/inspector/internal/docker"
	"github.com/dockergiant/images/inspector/internal/metrics"
	"github.com/dockergiant/images/inspector/internal/services"
)

func TestMetricsFromOverviewAndQueues(t *testing.T) {
	var o Overview
	if err := json.Unmarshal(fixture(t, "4.3-overview.json"), &o); err != nil {
		t.Fatal(err)
	}
	qs, _, err := ParseQueues(fixture(t, "4.3-queues.json"))
	if err != nil {
		t.Fatal(err)
	}
	m := brokerMetrics(&o, qs)
	if m["publish"] != float64(val(o.MessageStats.Publish)) || m["deliver"] != float64(val(o.MessageStats.Deliver)) || m["ack"] != float64(val(o.MessageStats.Ack)) {
		t.Fatalf("rates = %v", m)
	}
	if m["connections"] != 4 {
		t.Fatalf("connections = %v", m["connections"])
	}
	for _, q := range qs {
		if m["ready."+q.Name] != float64(q.ReadyCount()) || m["unacked."+q.Name] != float64(q.UnackedCount()) || m["consumers."+q.Name] != float64(q.ConsumerCount()) {
			t.Fatalf("queue %s: %v %v %v", q.Name, m["ready."+q.Name], m["unacked."+q.Name], m["consumers."+q.Name])
		}
	}
	if m["ready.catalog_website_attribute_value_sync"] != 4 {
		t.Fatalf("ready = %v", m["ready.catalog_website_attribute_value_sync"])
	}
}

func TestMetricsLeaveRatesOutBeforeTheFirstMessage(t *testing.T) {
	m := brokerMetrics(&Overview{ObjectTotals: &ObjectTotals{}}, nil)
	for _, k := range []string{"publish", "deliver", "ack"} {
		if _, ok := m[k]; ok {
			t.Errorf("%s reported without message_stats; a 0 here would spike the first real rate", k)
		}
	}
}

func TestMetricsRecordsAndServes(t *testing.T) {
	qs := `[{"name":"a","messages_ready":3,"messages_unacknowledged":1,"consumers":0},{"name":"b","messages_ready":0,"consumers":1}]`
	a, _ := newTestAdapter(t, &broker{queues: qs})
	for i := 0; i < 2; i++ {
		if _, err := a.Metrics(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	mux := http.NewServeMux()
	for _, rt := range a.Browse() {
		mux.HandleFunc(rt.Method+" "+rt.Path, rt.Handler)
	}
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics?window=15m", nil))
	var r metrics.Result
	if err := json.Unmarshal(rec.Body.Bytes(), &r); err != nil || rec.Code != 200 {
		t.Fatalf("%d %s %v", rec.Code, rec.Body, err)
	}
	if r.Latest["ready.a"] != 3 || r.Latest["unacked.a"] != 1 || r.Latest["consumers.b"] != 1 {
		t.Fatalf("latest = %v", r.Latest)
	}
	for _, name := range []string{"publish", "deliver", "ack", "connections"} {
		if _, ok := r.Series[name]; !ok {
			t.Errorf("series %s missing", name)
		}
	}
	if !slices.ContainsFunc(a.Browse(), func(rt core.Route) bool { return rt.Path == "/metrics" && rt.Method == http.MethodGet }) {
		t.Fatal("no GET /metrics route")
	}
}

func TestMetricsWithoutRabbitMQ(t *testing.T) {
	a := &adapter{deps: services.Deps{Docker: &docker.Fake{}, Magento: fakeMagento{}}}
	if _, err := a.Metrics(context.Background()); !errors.Is(err, core.ErrAbsent) {
		t.Fatalf("err = %v, want ErrAbsent", err)
	}
}
