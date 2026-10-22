package rabbitmq

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/dockergiant/images/inspector/internal/core"
)

const (
	cronMissing = "cron is not installed"
	cronUnknown = "is cron running?"
)

type sample struct {
	at       time.Time
	publish  int64
	deliver  int64
	ack      int64
	getEmpty int64
}

func sampleOf(o *Overview, at time.Time) sample {
	s := sample{at: at}
	if o != nil && o.MessageStats != nil {
		s.publish = val(o.MessageStats.Publish)
		s.deliver = val(o.MessageStats.Deliver)
		s.ack = val(o.MessageStats.Ack)
		s.getEmpty = val(o.MessageStats.GetEmpty)
	}
	return s
}

type rates struct {
	Publish  float64 `json:"publish"`
	Deliver  float64 `json:"deliver"`
	Ack      float64 `json:"ack"`
	GetEmpty float64 `json:"getEmpty"`
}

func computeRates(prev, cur sample) (rates, bool) {
	dt := cur.at.Sub(prev.at).Seconds()
	if prev.at.IsZero() || dt <= 0 {
		return rates{}, false
	}
	per := func(a, b int64) float64 {
		if b < a {
			return 0
		}
		return float64(b-a) / dt
	}
	return rates{
		Publish:  per(prev.publish, cur.publish),
		Deliver:  per(prev.deliver, cur.deliver),
		Ack:      per(prev.ack, cur.ack),
		GetEmpty: per(prev.getEmpty, cur.getEmpty),
	}, true
}

type cronState int

const (
	cronUnchecked cronState = iota
	cronInstalled
	cronAbsent
)

type healthInput struct {
	overview  *Overview
	queues    []Queue
	rates     rates
	haveRates bool
	cron      cronState
	vhost     *VHost
	traceBind bool
	capturing bool
	now       time.Time
}

func stuckQueues(qs []Queue) []Queue {
	var out []Queue
	for _, q := range qs {
		if q.ReadyCount() > 0 && q.ConsumerCount() == 0 {
			out = append(out, q)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ReadyCount() > out[j].ReadyCount() })
	return out
}

func oldestMessage(qs []Queue, now time.Time) (string, time.Duration) {
	var best *Queue
	for i := range qs {
		q := &qs[i]
		if q.HeadMessageTimestamp == nil || q.ReadyCount() == 0 {
			continue
		}
		if best == nil || *q.HeadMessageTimestamp < *best.HeadMessageTimestamp {
			best = q
		}
	}
	if best == nil {
		return "", 0
	}
	age := now.Sub(time.Unix(*best.HeadMessageTimestamp, 0))
	if age < 0 {
		age = 0
	}
	return best.Name, age.Round(time.Second)
}

func tracingOrphan(v *VHost, traceBind, capturing bool) bool {
	return v != nil && v.Tracing && !traceBind && !capturing
}

func evaluate(in healthInput) core.Health {
	var queueTotal, ready, unacked, consumers int64
	for _, q := range in.queues {
		queueTotal++
		ready += q.ReadyCount()
		unacked += q.UnackedCount()
		consumers += q.ConsumerCount()
	}
	if in.overview != nil && in.overview.QueueTotals != nil && len(in.queues) == 0 {
		ready = val(in.overview.QueueTotals.Ready)
		unacked = val(in.overview.QueueTotals.Unacknowledge)
	}

	h := core.Health{Status: core.OK, Raw: in.overview}
	stuck := stuckQueues(in.queues)

	readyCard := core.Card{Label: "Messages ready", Value: fmt.Sprint(ready), Sub: fmt.Sprintf("%d unacknowledged", unacked), Tone: core.OK}
	if len(stuck) > 0 {
		readyCard.Tone = core.Warn
	}
	if name, age := oldestMessage(in.queues, in.now); name != "" {
		readyCard.Sub += fmt.Sprintf(", oldest %s in %s", age, name)
	}

	consumerCard := core.Card{Label: "Consumers", Value: fmt.Sprint(consumers), Sub: "attached to queues", Tone: core.OK}
	if consumers == 0 {
		consumerCard.Tone = core.ToneInfo
	}

	pubCard := core.Card{Label: "Publish rate", Value: "n/a", Sub: "needs two samples", Tone: core.ToneInfo}
	if in.haveRates {
		pubCard.Value = fmt.Sprintf("%.1f/s", in.rates.Publish)
		pubCard.Sub = fmt.Sprintf("deliver %.1f/s, ack %.1f/s; %.1f/s get_empty is cron probe noise", in.rates.Deliver, in.rates.Ack, in.rates.GetEmpty)
		pubCard.Tone = core.OK
	}

	h.Cards = []core.Card{
		{Label: "Queues", Value: fmt.Sprint(queueTotal), Sub: stuckSub(len(stuck)), Tone: core.OK},
		readyCard,
		consumerCard,
		pubCard,
	}

	if len(stuck) > 0 {
		names := make([]string, 0, 3)
		for i, q := range stuck {
			if i == 3 {
				break
			}
			names = append(names, fmt.Sprintf("%s (%d)", q.Name, q.ReadyCount()))
		}
		text := fmt.Sprintf("%d queues hold messages and have no consumer: %s", len(stuck), strings.Join(names, ", "))
		if len(stuck) == 1 {
			text = "1 queue holds messages and has no consumer: " + names[0]
		}
		if len(stuck) > 3 {
			text += fmt.Sprintf(" and %d more", len(stuck)-3)
		}
		w := core.Warning{ID: "no-consumers", Tone: core.Warn}
		switch in.cron {
		case cronAbsent:
			w.Text = text + ". " + cronMissing
			w.Fix = "roll magento cron:install"
		default:
			w.Text = text + ". " + cronUnknown
		}
		h.Warnings = append(h.Warnings, w)
	}

	if in.overview != nil && in.overview.MessageStats != nil && val(in.overview.MessageStats.DropUnroutable) > 0 {
		h.Warnings = append(h.Warnings, core.Warning{
			ID:   "drop-unroutable",
			Text: fmt.Sprintf("%d messages were dropped as unroutable since the broker started: an exchange has no binding for their routing key", val(in.overview.MessageStats.DropUnroutable)),
			Tone: core.Warn,
		})
	}

	if tracingOrphan(in.vhost, in.traceBind, in.capturing) {
		h.Warnings = append(h.Warnings, core.Warning{
			ID:   "tracing-no-consumer",
			Text: "Tracing is on with no consumer: the broker copies every message to amq.rabbitmq.trace for nobody",
			Tone: core.Warn,
		})
	}

	if len(h.Warnings) > 0 {
		h.Status = core.Warn
	}
	switch {
	case len(stuck) > 0:
		h.Summary = fmt.Sprintf("%d queues, %d messages waiting without a consumer", queueTotal, sumReady(stuck))
	default:
		h.Summary = fmt.Sprintf("%d queues, %d messages ready, %d consumers", queueTotal, ready, consumers)
	}
	return h
}

func sumReady(qs []Queue) int64 {
	var n int64
	for _, q := range qs {
		n += q.ReadyCount()
	}
	return n
}

type queueRow struct {
	Name      string `json:"name"`
	State     string `json:"state"`
	Type      string `json:"type"`
	Ready     int64  `json:"ready"`
	Unacked   int64  `json:"unacked"`
	Consumers int64  `json:"consumers"`
	Published int64  `json:"published"`
	Delivered int64  `json:"delivered"`
	CronNoise int64  `json:"cronNoise"`
	Stuck     bool   `json:"stuck"`
	Oldest    string `json:"oldest,omitempty"`
}

func queueRows(qs []Queue, now time.Time) []queueRow {
	rows := make([]queueRow, 0, len(qs))
	for _, q := range qs {
		r := queueRow{
			Name:      q.Name,
			State:     q.State,
			Type:      q.Type,
			Ready:     q.ReadyCount(),
			Unacked:   q.UnackedCount(),
			Consumers: q.ConsumerCount(),
			Published: q.Stat(func(m *MsgStats) *int64 { return m.Publish }),
			Delivered: q.Stat(func(m *MsgStats) *int64 { return m.Deliver }),
			CronNoise: q.Stat(func(m *MsgStats) *int64 { return m.Get }) + q.Stat(func(m *MsgStats) *int64 { return m.GetEmpty }),
			Stuck:     q.ReadyCount() > 0 && q.ConsumerCount() == 0,
		}
		if q.HeadMessageTimestamp != nil && r.Ready > 0 {
			age := now.Sub(time.Unix(*q.HeadMessageTimestamp, 0)).Round(time.Second)
			if age < 0 {
				age = 0
			}
			r.Oldest = age.String()
		}
		rows = append(rows, r)
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].Ready != rows[j].Ready {
			return rows[i].Ready > rows[j].Ready
		}
		return rows[i].Name < rows[j].Name
	})
	return rows
}

type publishCounts map[string]int64

func queuePublishCounts(qs []Queue) publishCounts {
	out := publishCounts{}
	for _, q := range qs {
		out[q.Name] = q.Stat(func(m *MsgStats) *int64 { return m.Publish })
	}
	return out
}

func publishDeltas(prev, cur publishCounts) map[string]int64 {
	out := map[string]int64{}
	for name, n := range cur {
		p, ok := prev[name]
		if ok && n > p {
			out[name] = n - p
		}
	}
	return out
}

func stuckSub(n int) string {
	if n == 1 {
		return "1 holds messages without a consumer"
	}
	return fmt.Sprintf("%d hold messages without a consumer", n)
}
