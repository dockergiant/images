package metrics

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestParseInterval(t *testing.T) {
	for in, want := range map[string]time.Duration{"": Step, "10": 10 * time.Second, "30": 30 * time.Second, "60": time.Minute, "off": 0, "paused": 0, "0": 0} {
		got, err := ParseInterval(in)
		if err != nil || got != want {
			t.Errorf("ParseInterval(%q) = %v, %v", in, got, err)
		}
	}
	for _, bad := range []string{"5", "15", "abc", "-10", "10s"} {
		if _, err := ParseInterval(bad); err == nil {
			t.Errorf("ParseInterval(%q) accepted", bad)
		}
	}
}

func TestSetIntervalRejectsOtherValues(t *testing.T) {
	s := NewSampler(Step)
	if err := s.SetInterval(15 * time.Second); err == nil {
		t.Fatal("15 s accepted")
	}
	if err := s.SetInterval(0); err != nil || s.Interval() != 0 {
		t.Fatalf("pause: %v %v", err, s.Interval())
	}
}

type timedSource struct {
	n     atomic.Int32
	sleep time.Duration
	err   error
}

func (s *timedSource) Metrics(ctx context.Context) (map[string]float64, error) {
	s.n.Add(1)
	time.Sleep(s.sleep)
	return nil, s.err
}

func waitFor(t *testing.T, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for " + what)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

func TestSamplerPausesAndResumes(t *testing.T) {
	s := NewSampler(5 * time.Millisecond)
	s.allowAny = true
	src := &timedSource{}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		s.Run(ctx, "varnish", src, time.Second, nil)
		close(done)
	}()
	waitFor(t, "first samples", func() bool { return src.n.Load() >= 3 })

	if err := s.SetInterval(0); err != nil {
		t.Fatal(err)
	}
	time.Sleep(20 * time.Millisecond)
	paused := src.n.Load()
	time.Sleep(40 * time.Millisecond)
	if src.n.Load() != paused {
		t.Fatalf("sampled while paused: %d -> %d", paused, src.n.Load())
	}

	if err := s.SetInterval(5 * time.Millisecond); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "resume", func() bool { return src.n.Load() > paused+2 })
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Run did not stop on cancel")
	}
}

func TestSamplerRecordsTickTimes(t *testing.T) {
	s := NewSampler(5 * time.Millisecond)
	s.allowAny = true
	boom := errors.New("boom")
	fast, slow := &timedSource{}, &timedSource{sleep: 15 * time.Millisecond, err: boom}
	var reported atomic.Int32
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	wg.Go(func() { s.Run(ctx, "redis", fast, time.Second, nil) })
	wg.Go(func() { s.Run(ctx, "search", slow, time.Second, func(error) { reported.Add(1) }) })
	waitFor(t, "ticks", func() bool {
		ts := s.Ticks(time.Minute)
		return len(ts) == 2 && ts[0].Ticks >= 3 && ts[1].Ticks >= 3
	})
	cancel()
	wg.Wait()

	ticks := s.Ticks(time.Minute)
	if len(ticks) != 2 || ticks[0].Kind != "redis" || ticks[1].Kind != "search" {
		t.Fatalf("ticks %+v", ticks)
	}
	sl := ticks[1]
	if sl.Ticks < 3 || sl.Errors != sl.Ticks || sl.AvgMs < 15 || sl.MaxMs < sl.AvgMs || sl.LastError != "boom" {
		t.Fatalf("slow %+v", sl)
	}
	if ticks[0].AvgMs >= sl.AvgMs || ticks[0].Errors != 0 {
		t.Fatalf("fast %+v", ticks[0])
	}
	if reported.Load() == 0 {
		t.Fatal("real errors still reach onErr")
	}
}

func TestSamplerKeepsSpacingAcrossAChange(t *testing.T) {
	// A change takes effect without a burst: a sample that just ran is not
	// repeated because the interval moved.
	s := NewSampler(time.Hour)
	s.allowAny = true
	src := &timedSource{}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.Run(ctx, "varnish", src, time.Second, nil)
	waitFor(t, "first sample", func() bool { return src.n.Load() == 1 })
	s.SetInterval(30 * time.Minute)
	time.Sleep(30 * time.Millisecond)
	if n := src.n.Load(); n != 1 {
		t.Fatalf("a change sampled again: %d", n)
	}
}

func TestQueryStepFollowsSampleSpacing(t *testing.T) {
	h, c := newTestHistory(Spec{Gauges: []string{"g"}})
	for i := range 4 {
		h.Add(t0.Add(time.Duration(i)*30*time.Second), map[string]float64{"g": 1})
	}
	c.set(t0.Add(90 * time.Second))
	if r := h.Query(15 * time.Minute); r.Step != 30 {
		t.Fatalf("step %d, want 30", r.Step)
	}
	empty, _ := newTestHistory(Spec{Gauges: []string{"g"}})
	if r := empty.Query(15 * time.Minute); r.Step != 10 {
		t.Fatalf("empty step %d", r.Step)
	}
}
