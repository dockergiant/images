package apm

import "time"

const (
	intakeKeep   = 15 * time.Minute
	intakeBucket = time.Minute
	intakeEvents = 20000
)

type intakeKind uint8

const (
	intakeTrace intakeKind = iota
	intakeBody
	intakeProfile
)

type intakeEvent struct {
	at   time.Time
	kind intakeKind
	// n is spans for a trace and bytes for a body or a profile.
	n int64
}

// IntakeCost is what tracing cost Inspector over a window: what arrived, and
// what the store had to let go of.
type IntakeCost struct {
	Window          int     `json:"window"`
	Traces          int     `json:"traces"`
	Spans           int     `json:"spans"`
	AvgSpans        float64 `json:"avgSpans"`
	Bytes           int64   `json:"bytes"`
	AvgBytes        float64 `json:"avgBytes"`
	Profiles        int     `json:"profiles"`
	ProfileBytes    int64   `json:"profileBytes"`
	AvgProfileBytes float64 `json:"avgProfileBytes"`
	MBPerMin        float64 `json:"mbPerMin"`
	Evicted         uint64  `json:"evicted"`
	Dropped         uint64  `json:"dropped"`
	Rejected        uint64  `json:"rejected"`
	// LastReject is why the last refused payload was refused.
	LastReject  string       `json:"lastReject,omitempty"`
	Skipped     IntakeSkips  `json:"skipped"`
	Stored      int          `json:"stored"`
	StoredBytes int64        `json:"storedBytes"`
	MaxTraces   int          `json:"maxTraces"`
	MaxBytes    int64        `json:"maxBytes"`
	Series      IntakeSeries `json:"series"`
}

// IntakeSkips counts the traces the store leaves out on purpose since
// Inspector started: the benchmark's own requests, and Inspector's host
// while Traefik routes it to the shop's nginx during a restart.
type IntakeSkips struct {
	Benchmark uint64 `json:"benchmark"`
	Inspector uint64 `json:"inspector"`
}

// IntakeSeries holds one point per minute, [unix seconds of the minute, value],
// over the last 15 minutes.
type IntakeSeries struct {
	Step   int          `json:"step"`
	Traces [][2]float64 `json:"traces"`
	MB     [][2]float64 `json:"mb"`
}

// note expects s.mu held.
func (s *Store) note(kind intakeKind, n int64) {
	now := s.now()
	s.intake = append(s.intake, intakeEvent{at: now, kind: kind, n: n})
	cut := 0
	for cut < len(s.intake) && (now.Sub(s.intake[cut].at) > intakeKeep || len(s.intake)-cut > intakeEvents) {
		cut++
	}
	if cut > 0 {
		s.intake = append(s.intake[:0], s.intake[cut:]...)
	}
}

// noteBody records a decoded intake body; its traces are counted by Add.
func (s *Store) noteBody(bytes, traces int) {
	if traces == 0 {
		return
	}
	s.mu.Lock()
	s.note(intakeBody, int64(bytes))
	s.mu.Unlock()
}

func (s *Store) noteRejected(why string) {
	s.mu.Lock()
	s.rejected++
	s.lastReject = why
	s.mu.Unlock()
}

func (s *Store) Cost(window time.Duration) IntakeCost {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	c := IntakeCost{
		Window:      int(window / time.Second),
		Evicted:     s.evicted,
		Dropped:     s.dropped,
		Rejected:    s.rejected,
		LastReject:  s.lastReject,
		Skipped:     s.skipped,
		Stored:      len(s.order),
		StoredBytes: s.bytes,
		MaxTraces:   s.maxTraces,
		MaxBytes:    s.maxBytes,
	}
	span := max(window, intakeKeep)
	start := now.Add(-span).Truncate(intakeBucket).Add(intakeBucket)
	buckets := int(now.Sub(start)/intakeBucket) + 1
	traces := make([]float64, buckets)
	mb := make([]float64, buckets)
	from := now.Add(-window)
	for _, e := range s.intake {
		in := !e.at.Before(from)
		if b := int(e.at.Sub(start) / intakeBucket); !e.at.Before(start) && b < buckets {
			switch e.kind {
			case intakeTrace:
				traces[b]++
			default:
				mb[b] += float64(e.n) / (1 << 20)
			}
		}
		if !in {
			continue
		}
		switch e.kind {
		case intakeTrace:
			c.Traces++
			c.Spans += int(e.n)
		case intakeBody:
			c.Bytes += e.n
		case intakeProfile:
			c.Profiles++
			c.ProfileBytes += e.n
		}
	}
	if c.Traces > 0 {
		c.AvgSpans = float64(c.Spans) / float64(c.Traces)
		c.AvgBytes = float64(c.Bytes) / float64(c.Traces)
	}
	if c.Profiles > 0 {
		c.AvgProfileBytes = float64(c.ProfileBytes) / float64(c.Profiles)
	}
	if minutes := window.Minutes(); minutes > 0 {
		c.MBPerMin = float64(c.Bytes+c.ProfileBytes) / (1 << 20) / minutes
	}
	c.Series = IntakeSeries{Step: int(intakeBucket / time.Second), Traces: make([][2]float64, buckets), MB: make([][2]float64, buckets)}
	for i := range buckets {
		t := float64(start.Add(time.Duration(i) * intakeBucket).Unix())
		c.Series.Traces[i] = [2]float64{t, traces[i]}
		c.Series.MB[i] = [2]float64{t, mb[i]}
	}
	return c
}
