package rabbitmq

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func fixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "..", "testdata", "rabbitmq", name))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestParseQueuesPagedAndBareAgree(t *testing.T) {
	bare, pages, err := ParseQueues(fixture(t, "4.3-queues.json"))
	if err != nil || pages != 1 {
		t.Fatalf("bare: %v pages=%d", err, pages)
	}
	paged, _, err := ParseQueues(fixture(t, "4.3-queues_paged.json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(bare) != 24 || len(paged) != len(bare) {
		t.Fatalf("bare=%d paged=%d", len(bare), len(paged))
	}
	if bare[0].Name != paged[0].Name {
		t.Fatalf("order differs: %s vs %s", bare[0].Name, paged[0].Name)
	}
}

func TestParseQueuesOptionalFieldsAbsent(t *testing.T) {
	qs, _, err := ParseQueues([]byte(`[{"name":"bare"},{"name":"idle","messages_ready":0}]`))
	if err != nil {
		t.Fatal(err)
	}
	q := qs[0]
	if q.ReadyCount() != 0 || q.ConsumerCount() != 0 || q.UnackedCount() != 0 {
		t.Fatalf("absent counts should read as zero: %+v", q)
	}
	if q.Stat(func(m *MsgStats) *int64 { return m.Publish }) != 0 {
		t.Fatal("absent message_stats should read as zero")
	}
	if q.HeadMessageTimestamp != nil {
		t.Fatal("head timestamp should stay nil")
	}
}

func TestParseQueuesPageCount(t *testing.T) {
	_, pages, err := ParseQueues([]byte(`{"items":[],"page":1,"page_count":3}`))
	if err != nil || pages != 3 {
		t.Fatalf("pages=%d err=%v", pages, err)
	}
}

func TestRecordedOverviewAndTopology(t *testing.T) {
	var o Overview
	if err := json.Unmarshal(fixture(t, "4.3-overview.json"), &o); err != nil {
		t.Fatal(err)
	}
	if o.Version() != "4.3.1" || o.ObjectTotals == nil || val(o.ObjectTotals.Queues) != 24 {
		t.Fatalf("overview: %+v", o)
	}
	var cs []Consumer
	if err := json.Unmarshal(fixture(t, "4.3-consumers.json"), &cs); err != nil || len(cs) != 4 {
		t.Fatalf("consumers %d %v", len(cs), err)
	}
	if cs[0].ChannelDetails.PeerHost == "" || cs[0].ChannelDetails.PeerPort == 0 || cs[0].Queue.Name == "" {
		t.Fatalf("consumer channel details lost: %+v", cs[0])
	}
	var conns []Connection
	if err := json.Unmarshal(fixture(t, "4.3-connections.json"), &conns); err != nil || len(conns) != 4 {
		t.Fatalf("connections %d %v", len(conns), err)
	}
	if conns[0].ClientProperties.Product != "AMQPLib" {
		t.Fatalf("client props: %+v", conns[0].ClientProperties)
	}
	var v VHost
	if err := json.Unmarshal(fixture(t, "4.3-vhost.json"), &v); err != nil || v.Tracing {
		t.Fatalf("vhost: %+v %v", v, err)
	}
}

func TestOverviewVersionFor3x(t *testing.T) {
	var o Overview
	if err := json.Unmarshal([]byte(`{"management_version":"3.13.7","rabbitmq_version":"3.13.7"}`), &o); err != nil {
		t.Fatal(err)
	}
	if o.Version() != "3.13.7" || o.MessageStats != nil {
		t.Fatalf("%+v", o)
	}
}

func newTestClient(h http.HandlerFunc) (*Client, *httptest.Server) {
	srv := httptest.NewServer(h)
	return NewClient(srv.URL, "u", "p", "/"), srv
}

func TestClientEscapesVhostAndQueueName(t *testing.T) {
	var uris []string
	cl, srv := newTestClient(func(w http.ResponseWriter, r *http.Request) {
		uris = append(uris, r.Method+" "+r.RequestURI)
		if u, p, ok := r.BasicAuth(); !ok || u != "u" || p != "p" {
			t.Errorf("basic auth missing: %v %v %v", u, p, ok)
		}
		w.Write([]byte(`{"name":"x"}`))
	})
	defer srv.Close()
	ctx := context.Background()
	if _, err := cl.Queue(ctx, "a b/c"); err != nil {
		t.Fatal(err)
	}
	if err := cl.Purge(ctx, "q.1"); err != nil {
		t.Fatal(err)
	}
	if _, err := cl.VHost(ctx); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"GET /api/queues/%2F/a%20b%2Fc",
		"DELETE /api/queues/%2F/q.1/contents",
		"GET /api/vhosts/%2F",
	}
	for i, w := range want {
		if uris[i] != w {
			t.Errorf("uri %d = %q, want %q", i, uris[i], w)
		}
	}
}

func TestClientQueuesWalksPages(t *testing.T) {
	var seen []string
	cl, srv := newTestClient(func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.URL.Query().Get("page"))
		switch r.URL.Query().Get("page") {
		case "1":
			w.Write([]byte(`{"items":[{"name":"a"}],"page":1,"page_count":2}`))
		default:
			w.Write([]byte(`{"items":[{"name":"b"}],"page":2,"page_count":2}`))
		}
	})
	defer srv.Close()
	qs, err := cl.Queues(context.Background())
	if err != nil || len(qs) != 2 || strings.Join(seen, ",") != "1,2" {
		t.Fatalf("qs=%v seen=%v err=%v", qs, seen, err)
	}
}

func TestClientAuthAndStatusErrors(t *testing.T) {
	cl, srv := newTestClient(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/overview" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		http.Error(w, "nope", http.StatusNotFound)
	})
	defer srv.Close()
	if _, err := cl.Overview(context.Background()); !errors.Is(err, ErrAuth) {
		t.Fatalf("want ErrAuth, got %v", err)
	}
	_, err := cl.Queue(context.Background(), "missing")
	var se *StatusError
	if !errors.As(err, &se) || se.Code != 404 {
		t.Fatalf("want StatusError 404, got %v", err)
	}
}

func TestClientSetTracingAndPeekBodies(t *testing.T) {
	var bodies []map[string]any
	cl, srv := newTestClient(func(w http.ResponseWriter, r *http.Request) {
		var m map[string]any
		json.NewDecoder(r.Body).Decode(&m)
		bodies = append(bodies, m)
		if strings.HasSuffix(r.URL.Path, "/get") {
			w.Write([]byte(`[{"payload_bytes":2,"redelivered":false,"exchange":"magento","routing_key":"k","message_count":0,"payload":"{}","payload_encoding":"string"}]`))
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	defer srv.Close()
	if err := cl.SetTracing(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	msgs, err := cl.Peek(context.Background(), "q", 3)
	if err != nil || len(msgs) != 1 || msgs[0].RoutingKey != "k" {
		t.Fatalf("%v %v", msgs, err)
	}
	if bodies[0]["tracing"] != true {
		t.Fatalf("tracing body: %v", bodies[0])
	}
	if bodies[1]["ackmode"] != "ack_requeue_true" || bodies[1]["count"] != float64(3) {
		t.Fatalf("peek body: %v", bodies[1])
	}
}
