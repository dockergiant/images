package apm

import (
	"fmt"
	"net/http"
	"net/url"
	"testing"
	"time"
)

func TestIntakeCostCountsTracesSpansAndBytes(t *testing.T) {
	s := NewStore(500, 256<<20)
	h := NewDDIntake(s, allowAll)
	body := fixture(t, "dd-batch3.msgpack")
	if rec := post(t, h, http.MethodPost, "/v0.4/traces", "127.0.0.1:1", body); rec.Code != 200 {
		t.Fatalf("traces = %d", rec.Code)
	}
	profile := fixture(t, "profile-6ad7d4d30000000025a83c7ee41d7769.json")
	if rec := post(t, h, http.MethodPost, "/profiles/6ad7d4d30000000025a83c7ee41d7769", "127.0.0.1:1", profile); rec.Code != http.StatusAccepted {
		t.Fatalf("profile = %d", rec.Code)
	}

	c := s.Cost(5 * time.Minute)
	if c.Traces != 3 || c.Window != 300 {
		t.Fatalf("cost %+v", c)
	}
	spans := 0
	for _, sum := range s.List(ListFilter{}) {
		tr, _ := s.Get(sum.ID)
		spans += len(tr.Spans)
	}
	if c.Spans != spans || c.AvgSpans != float64(spans)/3 {
		t.Fatalf("spans %d avg %v, want %d", c.Spans, c.AvgSpans, spans)
	}
	if c.Bytes != int64(len(body)) || c.AvgBytes != float64(len(body))/3 {
		t.Fatalf("bytes %d avg %v, want %d", c.Bytes, c.AvgBytes, len(body))
	}
	if c.Profiles != 1 || c.ProfileBytes != int64(len(profile)) {
		t.Fatalf("profiles %d bytes %d", c.Profiles, c.ProfileBytes)
	}
	wantMB := float64(len(body)+len(profile)) / (1 << 20) / 5
	if diff := c.MBPerMin - wantMB; diff > 1e-9 || diff < -1e-9 {
		t.Fatalf("MB per min %v, want %v", c.MBPerMin, wantMB)
	}
	if c.Stored != 3 || c.StoredBytes <= 0 || c.MaxTraces != 500 || c.MaxBytes != 256<<20 {
		t.Fatalf("store %+v", c)
	}
	if len(c.Series.Traces) == 0 || len(c.Series.Traces) != len(c.Series.MB) {
		t.Fatalf("series %+v", c.Series)
	}
	total := 0.0
	for _, p := range c.Series.Traces {
		total += p[1]
	}
	if total != 3 {
		t.Fatalf("series sums to %v traces", total)
	}
}

func TestIntakeCostWindowAndEvictions(t *testing.T) {
	s := NewStore(2, 256<<20)
	clock := time.Unix(1_792_000_000, 0)
	s.now = func() time.Time { return clock }
	for i := range 4 {
		s.Add(Trace{ID: fmt.Sprintf("%032x", i+1), Spans: []Span{{Name: "a"}}})
		s.noteBody(100, 1)
		clock = clock.Add(2 * time.Minute)
	}
	c := s.Cost(5 * time.Minute)
	if c.Traces != 2 || c.Evicted != 2 {
		t.Fatalf("window traces %d evicted %d", c.Traces, c.Evicted)
	}
	s.noteRejected("test")
	if s.Cost(5*time.Minute).Rejected != 1 {
		t.Fatal("rejected body not counted")
	}
}

func TestIntakeCostRejectsOversizeBody(t *testing.T) {
	s := NewStore(500, 256<<20)
	h := NewDDIntake(s, allowAll)
	if rec := post(t, h, http.MethodPost, "/v0.4/traces", "127.0.0.1:1", make([]byte, maxBody+1)); rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("code %d", rec.Code)
	}
	if c := s.Cost(5 * time.Minute); c.Rejected != 1 || c.Traces != 0 {
		t.Fatalf("cost %+v", c)
	}
}

func TestIntakeCostEmpty(t *testing.T) {
	c := NewStore(0, 0).Cost(5 * time.Minute)
	if c.Traces != 0 || c.AvgSpans != 0 || c.MBPerMin != 0 || c.Series.Traces == nil {
		t.Fatalf("empty %+v", c)
	}
}

func TestIntakeCostLeavesOutSkippedTraces(t *testing.T) {
	body := fixture(t, "dd-batch3.msgpack")
	traces, err := decodeDD(body)
	if err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(traces[0].URL)
	if err != nil || u.Hostname() == "" {
		t.Fatalf("fixture url %q", traces[0].URL)
	}
	s := NewStore(500, 256<<20)
	s.SkipHost(u.Hostname())
	if rec := post(t, NewDDIntake(s, allowAll), http.MethodPost, "/v0.4/traces", "127.0.0.1:1", body); rec.Code != 200 {
		t.Fatalf("traces = %d", rec.Code)
	}
	kept := len(s.List(ListFilter{}))
	c := s.Cost(5 * time.Minute)
	if kept >= len(traces) || c.Traces != kept || c.Bytes != int64(len(body)*kept/len(traces)) {
		t.Fatalf("kept %d of %d, cost %+v", kept, len(traces), c)
	}
}

// Traces the store leaves out on purpose are counted, so a short count after
// a load test can be told apart from traces that never arrived.
func TestIntakeCostCountsSkippedTracesAndTheLastReject(t *testing.T) {
	s := NewStore(500, 256<<20)
	s.SkipHost("inspector.shop.test")
	s.Add(Trace{ID: fmt.Sprintf("%032x", 1), Benchmark: true, Spans: []Span{{Name: "a"}}})
	s.Add(Trace{ID: fmt.Sprintf("%032x", 2), Benchmark: true, Spans: []Span{{Name: "a"}}})
	s.Add(Trace{ID: fmt.Sprintf("%032x", 3), URL: "https://inspector.shop.test/api/session", Spans: []Span{{Name: "a"}}})
	s.Add(Trace{ID: fmt.Sprintf("%032x", 4), URL: "https://app.shop.test/", Spans: []Span{{Name: "a"}}})

	h := NewDDIntake(s, allowAll)
	if rec := post(t, h, http.MethodPost, "/v0.4/traces", "127.0.0.1:1", []byte{0xc1}); rec.Code != http.StatusBadRequest {
		t.Fatalf("garbage body = %d", rec.Code)
	}
	c := s.Cost(5 * time.Minute)
	if c.Skipped.Benchmark != 2 || c.Skipped.Inspector != 1 || c.Stored != 1 {
		t.Fatalf("skipped %+v stored %d", c.Skipped, c.Stored)
	}
	if c.Rejected != 1 || c.LastReject == "" {
		t.Fatalf("rejected %d, last reject %q", c.Rejected, c.LastReject)
	}
}
