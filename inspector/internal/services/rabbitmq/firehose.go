package rabbitmq

import (
	"context"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"

	"github.com/dockergiant/images/inspector/internal/core"
)

const (
	traceExchange   = "amq.rabbitmq.trace"
	firehoseKey     = "rabbitmq:firehose"
	firehoseMaxTime = 30 * time.Minute
)

func stringList(v any) []string {
	switch t := v.(type) {
	case []string:
		return t
	case []any:
		out := make([]string, 0, len(t))
		for _, e := range t {
			if s, ok := e.(string); ok {
				out = append(out, s)
			}
		}
		return out
	case string:
		return []string{t}
	}
	return nil
}

func splitConnectionName(name string) (string, int) {
	left, _, _ := strings.Cut(name, " -> ")
	host, port, err := net.SplitHostPort(strings.TrimSpace(left))
	if err != nil {
		return "", 0
	}
	n, _ := strconv.Atoi(port)
	return host, n
}

func decodeTrace(routingKey string, headers amqp.Table, body []byte, now time.Time) (core.MQPub, bool) {
	if !strings.HasPrefix(routingKey, "publish.") {
		return core.MQPub{}, false
	}
	pub := core.MQPub{
		Time:   now,
		Size:   len(body),
		Source: "firehose",
		Count:  1,
	}
	pub.Exchange, _ = headers["exchange_name"].(string)
	if pub.Exchange == "" {
		pub.Exchange = strings.TrimPrefix(routingKey, "publish.")
	}
	if keys := stringList(headers["routing_keys"]); len(keys) > 0 {
		pub.RoutingKey = keys[0]
	}
	pub.Queues = stringList(headers["routed_queues"])
	if conn, ok := headers["connection"].(string); ok {
		pub.ConnIP, pub.ConnPort = splitConnectionName(conn)
	}
	return pub, true
}

func amqpURL(host string, port int, user, pass, vhost string) string {
	u := url.URL{
		Scheme:  "amqp",
		User:    url.UserPassword(user, pass),
		Host:    net.JoinHostPort(host, strconv.Itoa(port)),
		Path:    "/" + vhost,
		RawPath: "/" + url.PathEscape(vhost),
	}
	return u.String()
}

// tracingPlan reports whether the firehose has to switch tracing on and
// what undoing it means: tracing that was already on is the developer's.
func tracingPlan(ctx context.Context, cl *Client) (enable bool, restore func(context.Context) error) {
	if v, err := cl.VHost(ctx); err == nil && v.Tracing {
		return false, func(context.Context) error { return nil }
	}
	return true, func(rctx context.Context) error { return cl.SetTracing(rctx, false) }
}

func (a *adapter) firehoseCapped(parent context.Context, emit core.Emit) error {
	if parent.Err() != nil {
		return parent.Err()
	}
	emit(core.Event{Type: "stream.error", Time: time.Now(), Kind: core.RabbitMQ, Data: "firehose stopped after 30 minutes and tracing is off: close and reopen the lane to start it again"})
	<-parent.Done()
	return parent.Err()
}

func (a *adapter) firehose(parent context.Context, _ core.Filter, emit core.Emit) error {
	t, err := a.target(parent)
	if err != nil {
		return err
	}
	cl := a.clientFor(t)
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	enable, undo := tracingPlan(parent, cl)
	restore := func(rctx context.Context) error {
		cancel()
		return undo(rctx)
	}
	if err := a.deps.Capture.Begin(firehoseKey, firehoseMaxTime, restore); err != nil {
		return err
	}
	defer func() {
		rctx, rcancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer rcancel()
		if err := a.deps.Capture.End(rctx, firehoseKey); err != nil && a.deps.Log != nil {
			a.deps.Log.Error("rabbitmq tracing restore failed", "err", err)
		}
	}()

	if enable {
		if err := cl.SetTracing(ctx, true); err != nil {
			return fmt.Errorf("enable tracing: %w", err)
		}
	}
	addr := t.amqpAddr
	if addr == "" {
		addr = amqpURL(t.host, t.amqpPort, t.user, t.pass, t.vhost)
	}
	conn, err := amqp.DialConfig(addr, amqp.Config{Dial: amqp.DefaultDial(5 * time.Second)})
	if err != nil {
		return fmt.Errorf("amqp dial: %w", err)
	}
	defer conn.Close()
	ch, err := conn.Channel()
	if err != nil {
		return err
	}
	q, err := ch.QueueDeclare("", false, true, true, false, nil)
	if err != nil {
		return err
	}
	for _, key := range []string{"publish.magento", "deliver.*"} {
		if err := ch.QueueBind(q.Name, key, traceExchange, false, nil); err != nil {
			return fmt.Errorf("bind %s: %w", key, err)
		}
	}
	msgs, err := ch.Consume(q.Name, "", true, true, false, false, nil)
	if err != nil {
		return err
	}
	closed := conn.NotifyClose(make(chan *amqp.Error, 1))
	for {
		select {
		case <-ctx.Done():
			return a.firehoseCapped(parent, emit)
		case e := <-closed:
			if e != nil {
				return e
			}
			return fmt.Errorf("amqp connection closed")
		case m, ok := <-msgs:
			if !ok {
				return fmt.Errorf("firehose consumer closed")
			}
			pub, ok := decodeTrace(m.RoutingKey, m.Headers, m.Body, time.Now())
			if !ok {
				continue
			}
			if a.deps.Proc != nil && pub.ConnIP != "" {
				if p, found := a.deps.Proc.Lookup(pub.ConnIP, pub.ConnPort); found {
					pub.PID = p.PID
				}
			}
			emit(core.Event{Type: "mq.pub", Time: pub.Time, Kind: core.RabbitMQ, Data: pub})
		}
	}
}
