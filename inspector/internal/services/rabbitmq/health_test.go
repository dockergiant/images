package rabbitmq

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/dockergiant/images/inspector/internal/core"
)

func recordedQueues(t *testing.T) []Queue {
	t.Helper()
	qs, _, err := ParseQueues(fixture(t, "4.3-queues.json"))
	if err != nil {
		t.Fatal(err)
	}
	return qs
}

func recordedOverview(t *testing.T) *Overview {
	t.Helper()
	var o Overview
	if err := json.Unmarshal(fixture(t, "4.3-overview.json"), &o); err != nil {
		t.Fatal(err)
	}
	return &o
}

func i64(n int64) *int64 { return &n }

func TestEvaluateRecordedIdleBrokerHasConsumersFor4(t *testing.T) {
	qs := recordedQueues(t)
	h := evaluate(healthInput{overview: recordedOverview(t), queues: qs, now: time.Now()})
	if len(h.Cards) != 4 || h.Cards[0].Value != "24" {
		t.Fatalf("cards: %+v", h.Cards)
	}
	stuck := stuckQueues(qs)
	hasNoConsumers := false
	for _, w := range h.Warnings {
		if w.ID == "no-consumers" {
			hasNoConsumers = true
		}
	}
	if hasNoConsumers != (len(stuck) > 0) {
		t.Fatalf("banner and stuck queues disagree: %d stuck, warnings %+v", len(stuck), h.Warnings)
	}
}

func TestEvaluateBannerCronHints(t *testing.T) {
	qs := []Queue{
		{Name: "a", Ready: i64(5), Consumers: i64(0)},
		{Name: "b", Ready: i64(9), Consumers: i64(0)},
		{Name: "c", Ready: i64(3), Consumers: i64(2)},
		{Name: "d", Ready: i64(0), Consumers: i64(0)},
	}
	absent := evaluate(healthInput{queues: qs, cron: cronAbsent, now: time.Now()})
	w := absent.Warnings[0]
	if w.ID != "no-consumers" || !strings.Contains(w.Text, "cron is not installed") || strings.Contains(w.Text, "cron:install") || w.Fix != "roll magento cron:install" {
		t.Fatalf("absent: %+v", w)
	}
	if !strings.Contains(w.Text, "b (9), a (5)") || strings.Contains(w.Text, "c (") {
		t.Fatalf("stuck listing wrong: %s", w.Text)
	}
	installed := evaluate(healthInput{queues: qs, cron: cronInstalled, now: time.Now()})
	if !strings.Contains(installed.Warnings[0].Text, "is cron running?") || installed.Warnings[0].Fix != "" {
		t.Fatalf("installed: %+v", installed.Warnings[0])
	}
	if installed.Status != core.Warn {
		t.Fatalf("status %v", installed.Status)
	}
}

func TestEvaluateHealthyAndEmpty(t *testing.T) {
	h := evaluate(healthInput{queues: []Queue{{Name: "a", Ready: i64(0), Consumers: i64(1)}}, now: time.Now()})
	if h.Status != core.OK || len(h.Warnings) != 0 {
		t.Fatalf("%+v", h)
	}
	h = evaluate(healthInput{now: time.Now()})
	if h.Status != core.OK || h.Cards[0].Value != "0" {
		t.Fatalf("%+v", h)
	}
}

func TestEvaluateDropUnroutableAndTracing(t *testing.T) {
	o := &Overview{MessageStats: &MsgStats{DropUnroutable: i64(7)}}
	h := evaluate(healthInput{
		overview: o,
		vhost:    &VHost{Tracing: true},
		now:      time.Now(),
	})
	ids := map[string]bool{}
	for _, w := range h.Warnings {
		ids[w.ID] = true
	}
	if !ids["drop-unroutable"] || !ids["tracing-no-consumer"] {
		t.Fatalf("warnings: %+v", h.Warnings)
	}
	h = evaluate(healthInput{vhost: &VHost{Tracing: true}, traceBind: true, now: time.Now()})
	if len(h.Warnings) != 0 {
		t.Fatalf("bound trace queue should not warn: %+v", h.Warnings)
	}
	h = evaluate(healthInput{vhost: &VHost{Tracing: true}, capturing: true, now: time.Now()})
	if len(h.Warnings) != 0 {
		t.Fatalf("own capture should not warn: %+v", h.Warnings)
	}
}

func TestEvaluateOldestMessage(t *testing.T) {
	now := time.Unix(10_000, 0)
	qs := []Queue{
		{Name: "new", Ready: i64(1), Consumers: i64(1), HeadMessageTimestamp: i64(9_990)},
		{Name: "old", Ready: i64(1), Consumers: i64(1), HeadMessageTimestamp: i64(9_000)},
	}
	h := evaluate(healthInput{queues: qs, now: now})
	if !strings.Contains(h.Cards[1].Sub, "oldest 16m40s in old") {
		t.Fatalf("sub: %s", h.Cards[1].Sub)
	}
}

func TestComputeRates(t *testing.T) {
	t0 := time.Unix(100, 0)
	prev := sample{at: t0, publish: 10, deliver: 10, ack: 10, getEmpty: 100}
	cur := sample{at: t0.Add(5 * time.Second), publish: 20, deliver: 10, ack: 15, getEmpty: 150}
	r, ok := computeRates(prev, cur)
	if !ok || r.Publish != 2 || r.Deliver != 0 || r.Ack != 1 || r.GetEmpty != 10 {
		t.Fatalf("%+v %v", r, ok)
	}
	if _, ok := computeRates(sample{}, cur); ok {
		t.Fatal("first sample must not produce rates")
	}
	reset := sample{at: t0.Add(10 * time.Second), publish: 1}
	if r, _ := computeRates(cur, reset); r.Publish != 0 {
		t.Fatalf("broker restart must not give negative rates: %+v", r)
	}
}

func TestEvaluateRatesCardLabelsCronNoise(t *testing.T) {
	h := evaluate(healthInput{rates: rates{Publish: 1.5, GetEmpty: 4}, haveRates: true, now: time.Now()})
	c := h.Cards[3]
	if c.Value != "1.5/s" || !strings.Contains(c.Sub, "cron probe noise") {
		t.Fatalf("%+v", c)
	}
}

func TestPublishDeltas(t *testing.T) {
	prev := publishCounts{"a": 5, "b": 3, "gone": 1}
	cur := publishCounts{"a": 9, "b": 3, "new": 4}
	d := publishDeltas(prev, cur)
	if len(d) != 1 || d["a"] != 4 {
		t.Fatalf("%v", d)
	}
}

func TestQueueRowsSortAndNoise(t *testing.T) {
	qs := []Queue{
		{Name: "quiet", Ready: i64(0), MessageStats: &MsgStats{Get: i64(2), GetEmpty: i64(100)}},
		{Name: "busy", Ready: i64(5), Consumers: i64(0)},
	}
	rows := queueRows(qs, time.Now())
	if rows[0].Name != "busy" || !rows[0].Stuck || rows[1].CronNoise != 102 || rows[1].Stuck {
		t.Fatalf("%+v", rows)
	}
}

func TestEvaluateBannerGrammar(t *testing.T) {
	one := evaluate(healthInput{queues: []Queue{{Name: "a", Ready: i64(5), Consumers: i64(0)}}, cron: cronAbsent, now: time.Now()})
	if !strings.HasPrefix(one.Warnings[0].Text, "1 queue holds messages and has no consumer: a (5)") {
		t.Fatalf("one: %s", one.Warnings[0].Text)
	}
	if one.Cards[0].Sub != "1 holds messages without a consumer" {
		t.Fatalf("one card: %s", one.Cards[0].Sub)
	}
	two := evaluate(healthInput{queues: []Queue{{Name: "a", Ready: i64(5), Consumers: i64(0)}, {Name: "b", Ready: i64(1), Consumers: i64(0)}}, cron: cronAbsent, now: time.Now()})
	if !strings.HasPrefix(two.Warnings[0].Text, "2 queues hold messages and have no consumer: ") {
		t.Fatalf("two: %s", two.Warnings[0].Text)
	}
	if two.Cards[0].Sub != "2 hold messages without a consumer" {
		t.Fatalf("two card: %s", two.Cards[0].Sub)
	}
}
