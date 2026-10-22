package rabbitmq

import (
	"context"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"

	"github.com/dockergiant/images/inspector/internal/core"
)

func TestDecodeTracePublish(t *testing.T) {
	now := time.Unix(1, 0)
	h := amqp.Table{
		"exchange_name": "magento",
		"routing_keys":  []any{"async.operations.all"},
		"routed_queues": []any{"async.operations.all", "other"},
		"connection":    "192.168.156.10:50580 -> 192.168.156.7:5672",
		"node":          "rabbit@x",
	}
	pub, ok := decodeTrace("publish.magento", h, []byte("12345"), now)
	if !ok {
		t.Fatal("publish should decode")
	}
	if pub.Exchange != "magento" || pub.RoutingKey != "async.operations.all" || len(pub.Queues) != 2 || pub.Queues[1] != "other" {
		t.Fatalf("%+v", pub)
	}
	if pub.Size != 5 || pub.ConnIP != "192.168.156.10" || pub.ConnPort != 50580 || pub.Source != "firehose" || pub.Count != 1 || !pub.Time.Equal(now) {
		t.Fatalf("%+v", pub)
	}
}

func TestDecodeTraceSkipsDeliverAndToleratesMissingHeaders(t *testing.T) {
	if _, ok := decodeTrace("deliver.some_queue", amqp.Table{}, nil, time.Now()); ok {
		t.Fatal("deliver messages are not publishes")
	}
	pub, ok := decodeTrace("publish.magento", amqp.Table{"routing_keys": []string{"k"}}, nil, time.Now())
	if !ok || pub.Exchange != "magento" || pub.RoutingKey != "k" || pub.Queues != nil || pub.ConnIP != "" {
		t.Fatalf("%+v %v", pub, ok)
	}
}

func TestSplitConnectionName(t *testing.T) {
	ip, port := splitConnectionName("10.0.0.2:5000 -> 10.0.0.9:5672")
	if ip != "10.0.0.2" || port != 5000 {
		t.Fatalf("%s %d", ip, port)
	}
	ip, port = splitConnectionName("[fd00::2]:5000 -> [fd00::9]:5672")
	if ip != "fd00::2" || port != 5000 {
		t.Fatalf("%s %d", ip, port)
	}
	if ip, _ := splitConnectionName("garbage"); ip != "" {
		t.Fatal("garbage should not parse")
	}
}

func TestAmqpURLEscapesCredentialsAndVhost(t *testing.T) {
	got := amqpURL("10.0.0.5", 5672, "us er", "p@ss/w", "/")
	want := "amqp://us%20er:p%40ss%2Fw@10.0.0.5:5672/%2F"
	if got != want {
		t.Fatalf("got %s want %s", got, want)
	}
}

func TestTracingPlanKeepsTracingThatWasAlreadyOn(t *testing.T) {
	var puts atomic.Int32
	cl, srv := newTestClient(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPut {
			puts.Add(1)
		}
		w.Write([]byte(`{"name":"/","tracing":true}`))
	})
	defer srv.Close()
	enable, restore := tracingPlan(context.Background(), cl)
	if enable {
		t.Fatal("tracing is already on")
	}
	if err := restore(context.Background()); err != nil || puts.Load() != 0 {
		t.Fatalf("restore changed the vhost: %v, %d PUTs", err, puts.Load())
	}
}

func TestTracingPlanSwitchesOffWhatItSwitchedOn(t *testing.T) {
	var bodies []string
	cl, srv := newTestClient(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPut {
			bodies = append(bodies, "put")
			w.WriteHeader(http.StatusNoContent)
			return
		}
		w.Write([]byte(`{"name":"/","tracing":false}`))
	})
	defer srv.Close()
	enable, restore := tracingPlan(context.Background(), cl)
	if !enable {
		t.Fatal("tracing is off")
	}
	if err := restore(context.Background()); err != nil || len(bodies) != 1 {
		t.Fatalf("restore: %v %v", err, bodies)
	}
}

func TestTracingPlanFallsBackWhenVhostReadFails(t *testing.T) {
	cl, srv := newTestClient(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusInternalServerError) })
	defer srv.Close()
	if enable, _ := tracingPlan(context.Background(), cl); !enable {
		t.Fatal("a failed read must keep the old behaviour")
	}
}

func TestFirehoseCappedNoticeDoesNotRestart(t *testing.T) {
	a := &adapter{}
	parent, cancel := context.WithCancel(context.Background())
	var evs []core.Event
	done := make(chan error, 1)
	go func() { done <- a.firehoseCapped(parent, func(e core.Event) { evs = append(evs, e) }) }()
	select {
	case err := <-done:
		t.Fatalf("returned and would restart the firehose: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	cancel()
	if err := <-done; err != context.Canceled {
		t.Fatalf("returned %v", err)
	}
	if len(evs) != 1 || evs[0].Type != "stream.error" || evs[0].Kind != core.RabbitMQ {
		t.Fatalf("events %+v", evs)
	}

	done2 := make(chan error, 1)
	go func() {
		done2 <- a.firehoseCapped(parent, func(core.Event) { t.Error("emitted after the user closed the stream") })
	}()
	if err := <-done2; err != context.Canceled {
		t.Fatalf("returned %v", err)
	}
}
