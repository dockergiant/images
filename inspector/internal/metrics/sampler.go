package metrics

import (
	"context"
	"errors"
	"math"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/dockergiant/images/inspector/internal/core"
)

const tickKeep = 360

// Intervals are the choices the Performance page offers; 0 is paused.
var Intervals = []time.Duration{10 * time.Second, 30 * time.Second, time.Minute}

// ParseInterval reads ROLL_INSPECTOR_SAMPLE: seconds, or off to pause.
func ParseInterval(s string) (time.Duration, error) {
	switch s {
	case "":
		return Step, nil
	case "off", "paused", "0":
		return 0, nil
	}
	n, err := strconv.Atoi(s)
	if err == nil {
		d := time.Duration(n) * time.Second
		for _, ok := range Intervals {
			if d == ok {
				return d, nil
			}
		}
	}
	return 0, errors.New("sample interval " + strconv.Quote(s) + " is not 10, 30, 60 or off")
}

type tick struct {
	at  time.Time
	dur time.Duration
	err string
}

type tickLog struct {
	items []tick
	next  int
}

func (l *tickLog) add(t tick) {
	if len(l.items) < tickKeep {
		l.items = append(l.items, t)
		return
	}
	l.items[l.next] = t
	l.next = (l.next + 1) % tickKeep
}

// Sampler runs the metrics samplers on one interval that the Performance page
// can change or pause while Inspector runs, and times every sample it takes.
type Sampler struct {
	mu       sync.Mutex
	interval time.Duration
	changed  chan struct{}
	ticks    map[string]*tickLog
	now      func() time.Time
	// allowAny lets tests use millisecond intervals.
	allowAny bool
}

func NewSampler(interval time.Duration) *Sampler {
	return &Sampler{interval: interval, changed: make(chan struct{}), ticks: map[string]*tickLog{}, now: time.Now}
}

func (s *Sampler) Interval() time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.interval
}

func (s *Sampler) SetInterval(d time.Duration) error {
	ok := d == 0 || s.allowAny
	for _, v := range Intervals {
		ok = ok || d == v
	}
	if !ok {
		return errors.New("sample interval " + d.String() + " is not 10 s, 30 s, 60 s or paused")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if d == s.interval {
		return nil
	}
	s.interval = d
	close(s.changed)
	s.changed = make(chan struct{})
	return nil
}

func (s *Sampler) current() (time.Duration, <-chan struct{}) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.interval, s.changed
}

func (s *Sampler) record(kind string, t tick) {
	s.mu.Lock()
	defer s.mu.Unlock()
	l := s.ticks[kind]
	if l == nil {
		l = &tickLog{}
		s.ticks[kind] = l
	}
	l.add(t)
}

// Run samples src until ctx ends. Paused, it waits for the next change; a
// change keeps the spacing from the last sample instead of sampling at once.
// Absent services and a sample that is still running are expected, so only
// other errors reach onErr.
func (s *Sampler) Run(ctx context.Context, kind string, src core.MetricsSource, timeout time.Duration, onErr func(error)) {
	var last time.Time
	timer := time.NewTimer(0)
	defer timer.Stop()
	for {
		every, changed := s.current()
		if every > 0 {
			wait := time.Duration(0)
			if !last.IsZero() {
				wait = time.Until(last.Add(every))
			}
			if wait <= 0 {
				last = time.Now()
				s.sample(ctx, kind, src, timeout, onErr)
				wait = every
			}
			timer.Reset(wait)
		}
		var fire <-chan time.Time
		if every > 0 {
			fire = timer.C
		}
		select {
		case <-ctx.Done():
			return
		case <-changed:
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
		case <-fire:
		}
	}
}

func (s *Sampler) sample(ctx context.Context, kind string, src core.MetricsSource, timeout time.Duration, onErr func(error)) {
	start := time.Now()
	sctx, cancel := context.WithTimeout(ctx, timeout)
	_, err := src.Metrics(sctx)
	cancel()
	if ctx.Err() != nil || errors.Is(err, ErrBusy) {
		return
	}
	t := tick{at: s.now(), dur: time.Since(start)}
	if err != nil && !errors.Is(err, core.ErrAbsent) {
		t.err = err.Error()
		if onErr != nil {
			onErr(err)
		}
	}
	s.record(kind, t)
}

type TickStats struct {
	Kind      string  `json:"kind"`
	Ticks     int     `json:"ticks"`
	Errors    int     `json:"errors"`
	AvgMs     float64 `json:"avgMs"`
	MaxMs     float64 `json:"maxMs"`
	LastMs    float64 `json:"lastMs"`
	LastAt    int64   `json:"lastAt"`
	LastError string  `json:"lastError,omitempty"`
}

// Ticks sums up the samples of the last window per service, sorted by kind.
func (s *Sampler) Ticks(window time.Duration) []TickStats {
	s.mu.Lock()
	defer s.mu.Unlock()
	from := s.now().Add(-window)
	out := make([]TickStats, 0, len(s.ticks))
	for kind, l := range s.ticks {
		st := TickStats{Kind: kind}
		var total time.Duration
		var lastAt time.Time
		for _, t := range l.items {
			if t.at.Before(from) {
				continue
			}
			st.Ticks++
			total += t.dur
			ms := float64(t.dur) / float64(time.Millisecond)
			st.MaxMs = math.Max(st.MaxMs, ms)
			if t.err != "" {
				st.Errors++
			}
			if t.at.After(lastAt) {
				lastAt = t.at
				st.LastMs = ms
				st.LastError = t.err
			}
		}
		if st.Ticks > 0 {
			st.AvgMs = float64(total) / float64(st.Ticks) / float64(time.Millisecond)
			st.LastAt = lastAt.Unix()
		}
		out = append(out, st)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Kind < out[j].Kind })
	return out
}
