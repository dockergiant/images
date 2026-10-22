package apm

import (
	"testing"
	"time"
)

func TestTopSpansKeepsTheMostSelfTimeInOrder(t *testing.T) {
	at := time.Unix(1792526556, 0)
	span := func(id, parent string, start, d int) Span {
		return Span{SpanID: id, ParentID: parent, Start: at.Add(time.Duration(start) * time.Millisecond), Duration: time.Duration(d) * time.Millisecond}
	}
	tr := Trace{Spans: []Span{
		span("root", "", 0, 100),
		span("a", "root", 0, 60),
		span("a1", "a", 0, 50),
		span("b", "root", 70, 5),
		span("c", "root", 80, 15),
	}}
	got := TopSpans(tr, 3)
	ids := []string{}
	for _, s := range got {
		ids = append(ids, s.SpanID)
	}
	// Self times: root 20, a 10, a1 50, b 5, c 15.
	if len(ids) != 3 || ids[0] != "root" || ids[1] != "a1" || ids[2] != "c" {
		t.Fatalf("got %v, want [root a1 c]", ids)
	}
	if n := len(TopSpans(tr, 0)); n != 5 {
		t.Fatalf("limit 0 kept %d spans, want all 5", n)
	}
	if n := len(TopSpans(tr, 10)); n != 5 {
		t.Fatalf("limit above the count kept %d spans", n)
	}
	if got := TopSpans(Trace{}, 5); got == nil || len(got) != 0 {
		t.Fatalf("empty trace gave %#v, want an empty slice", got)
	}
}
