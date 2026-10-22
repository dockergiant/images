package rabbitmq

import (
	"context"
	"errors"
	"fmt"

	"github.com/dockergiant/images/inspector/internal/core"
	"github.com/dockergiant/images/inspector/internal/metrics"
)

var metricSpec = metrics.Spec{
	Counters: []string{"publish", "deliver", "ack"},
	Gauges:   []string{"connections", "ready.", "unacked.", "consumers."},
	Groups:   []metrics.Group{{Prefixes: []string{"ready.", "unacked.", "consumers."}, Rank: []string{"ready.", "unacked."}}},
}

var _ core.MetricsSource = (*adapter)(nil)

// hist is created on first use, because tests build the adapter as a literal.
func (a *adapter) hist() *metrics.History {
	a.histOnce.Do(func() {
		if a.history == nil {
			a.history = metrics.NewHistory(metricSpec)
		}
	})
	return a.history
}

func (a *adapter) Metrics(ctx context.Context) (map[string]float64, error) {
	return a.hist().Collect(ctx, func(ctx context.Context) (map[string]float64, error) {
		cl, _, err := a.client(ctx)
		if errors.Is(err, errAbsent) {
			return nil, fmt.Errorf("%w: %w", core.ErrAbsent, err)
		}
		if err != nil {
			return nil, err
		}
		o, err := cl.Overview(ctx)
		if err != nil {
			return nil, errors.New(authMessage(err))
		}
		qs, err := cl.Queues(ctx)
		if err != nil {
			return nil, errors.New(authMessage(err))
		}
		return brokerMetrics(o, qs), nil
	})
}

// brokerMetrics leaves a rate out while the broker has no message_stats for
// it yet: a 0 followed by the real total would show as a burst.
func brokerMetrics(o *Overview, qs []Queue) map[string]float64 {
	m := map[string]float64{}
	if o != nil && o.MessageStats != nil {
		for name, p := range map[string]*int64{"publish": o.MessageStats.Publish, "deliver": o.MessageStats.Deliver, "ack": o.MessageStats.Ack} {
			if p != nil {
				m[name] = float64(*p)
			}
		}
	}
	if o != nil && o.ObjectTotals != nil {
		m["connections"] = float64(val(o.ObjectTotals.Connections))
	}
	for i := range qs {
		q := &qs[i]
		m["ready."+q.Name] = float64(q.ReadyCount())
		m["unacked."+q.Name] = float64(q.UnackedCount())
		m["consumers."+q.Name] = float64(q.ConsumerCount())
	}
	return m
}
