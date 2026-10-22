package apm

import (
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	defaultMaxTraces = 500
	defaultMaxBytes  = 256 << 20
	subBuffer        = 64
)

type entry struct {
	trace    Trace
	analysis *traceAnalysis
	profile  []byte
	size     int64
}

type Store struct {
	mu        sync.Mutex
	maxTraces int
	maxBytes  int64
	bytes     int64
	order     []string
	items     map[string]*entry

	pending      map[string][]byte
	pendingOrder []string
	pendingBytes int64

	subs map[chan Trace]struct{}

	skipHost string
	// benchIDs remembers the benchmark's skipped traces for a while, so
	// their profiles are dropped too, whichever of the two arrives first.
	benchIDs   map[string]bool
	benchOrder []string

	// routes maps a page (host and path) to the route a full trace gave it,
	// so lean traces of that page group under the same route.
	routes     map[string]string
	routeOrder []string

	intake     []intakeEvent
	evicted    uint64
	dropped    uint64
	rejected   uint64
	lastReject string
	skipped    IntakeSkips

	now func() time.Time
	// gen changes whenever the set of traces does, so Hotspots knows when
	// its cached result is out of date.
	gen uint64

	hotMu  sync.Mutex
	hot    *Hotspots
	hotGen uint64
	hotAt  time.Time
}

func NewStore(maxTraces int, maxBytes int64) *Store {
	if maxTraces <= 0 {
		maxTraces = defaultMaxTraces
	}
	if maxBytes <= 0 {
		maxBytes = defaultMaxBytes
	}
	return &Store{
		maxTraces: maxTraces,
		maxBytes:  maxBytes,
		items:     map[string]*entry{},
		pending:   map[string][]byte{},
		subs:      map[chan Trace]struct{}{},
		routes:    map[string]string{},
		now:       time.Now,
	}
}

// SkipHost drops traces for this host: while the inspector container restarts, Traefik routes
// inspector.<domain> to the nginx fallback, and Magento's 302 for the UI's own requests would fill the list.
func (s *Store) SkipHost(host string) {
	s.mu.Lock()
	s.skipHost = strings.ToLower(host)
	s.mu.Unlock()
}

// Add reports whether the store kept the trace.
func (s *Store) Add(t Trace) bool {
	if t.ID == "" {
		return false
	}
	// The benchmark's requests would evict the traces being investigated
	// and skew the cost figures for five minutes; the tracer still sends
	// them, which is the cost the benchmark times.
	if t.Benchmark {
		s.mu.Lock()
		s.skipBench(t.ID)
		s.skipped.Benchmark++
		s.mu.Unlock()
		return false
	}
	s.mu.Lock()
	skip := s.skipHost
	s.mu.Unlock()
	if skip != "" {
		if u, err := url.Parse(t.URL); err == nil && strings.EqualFold(u.Hostname(), skip) {
			s.mu.Lock()
			s.skipped.Inspector++
			s.mu.Unlock()
			return false
		}
	}
	// Analysed before taking the lock: a large trace takes milliseconds.
	analysis := analyze(t)
	s.mu.Lock()
	defer s.mu.Unlock()
	e := &entry{trace: t, analysis: analysis}
	s.route(&e.trace)
	if old, ok := s.items[t.ID]; ok {
		e.profile = old.profile
		s.remove(t.ID)
	}
	if e.profile == nil {
		e.profile, _ = s.unpark(t.ID)
	}
	e.trace.HasProfile = e.profile != nil
	e.size = traceSize(e.trace) + analysisSize(e.analysis) + int64(len(e.profile))
	if e.size > s.maxBytes {
		s.dropped++
		return false
	}
	s.items[t.ID] = e
	s.note(intakeTrace, int64(len(t.Spans)))
	s.order = append(s.order, t.ID)
	s.gen++
	s.bytes += e.size
	s.evict()
	s.publish(e.trace)
	return true
}

const (
	benchMemory = 256
	maxRoutes   = 4096
)

// route gives a lean trace the route a full trace of the same page carried,
// and moves the stored lean traces of a page once a full one names it.
// Lean tracing drops the Magento integration that sets magento.pathinfo.
func (s *Store) route(t *Trace) {
	if t.pageKey == "" {
		return
	}
	if t.urlRoute {
		if r, ok := s.routes[t.pageKey]; ok {
			t.Route = r
		}
		return
	}
	if !t.taught || s.routes[t.pageKey] == t.Route {
		return
	}
	if _, ok := s.routes[t.pageKey]; !ok {
		s.routeOrder = append(s.routeOrder, t.pageKey)
		if len(s.routeOrder) > maxRoutes {
			delete(s.routes, s.routeOrder[0])
			s.routeOrder = s.routeOrder[1:]
		}
	}
	s.routes[t.pageKey] = t.Route
	for _, e := range s.items {
		if e.trace.urlRoute && e.trace.pageKey == t.pageKey && e.trace.Route != t.Route {
			d := int64(len(t.Route) - len(e.trace.Route))
			e.trace.Route = t.Route
			e.size += d
			s.bytes += d
			s.gen++
		}
	}
}

func (s *Store) skipBench(id string) {
	if s.benchIDs == nil {
		s.benchIDs = map[string]bool{}
	}
	if s.benchIDs[id] {
		return
	}
	s.benchIDs[id] = true
	s.benchOrder = append(s.benchOrder, id)
	if len(s.benchOrder) > benchMemory {
		delete(s.benchIDs, s.benchOrder[0])
		s.benchOrder = s.benchOrder[1:]
	}
	if data, ok := s.pending[id]; ok {
		s.pendingBytes -= int64(len(data))
		delete(s.pending, id)
		s.dropPendingOrder(id)
	}
}

func (s *Store) List(f ListFilter) []Summary {
	s.mu.Lock()
	out := make([]Summary, 0, len(s.order))
	for i := len(s.order) - 1; i >= 0; i-- {
		t := s.items[s.order[i]].trace
		if f.Route != "" && t.Route != f.Route {
			continue
		}
		if t.Duration < f.MinDuration {
			continue
		}
		out = append(out, Summarize(t))
	}
	s.mu.Unlock()
	switch f.Sort {
	case "recent", "start":
		sort.SliceStable(out, func(i, j int) bool { return out[i].Start.After(out[j].Start) })
	default:
		sort.SliceStable(out, func(i, j int) bool { return out[i].Duration > out[j].Duration })
	}
	if f.Limit > 0 && len(out) > f.Limit {
		out = out[:f.Limit]
	}
	return out
}

func (s *Store) Get(id string) (Trace, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.items[id]
	if !ok {
		return Trace{}, false
	}
	return e.trace, true
}

// Analysis is the bottleneck analysis computed when the trace was added.
func (s *Store) Analysis(id string) (Analysis, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.items[id]
	if !ok || e.analysis == nil {
		return Analysis{}, false
	}
	a := e.analysis.analysis
	a.Profile = e.trace.HasProfile
	return a, true
}

func (s *Store) SetProfile(id string, data []byte) error {
	if !validKey(id) {
		return ErrBadTraceID
	}
	if len(data) > maxProfile {
		return ErrProfileTooLarge
	}
	data = append([]byte(nil), data...)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.benchIDs[id] {
		return nil
	}
	s.note(intakeProfile, int64(len(data)))
	e, ok := s.items[id]
	if !ok {
		s.park(id, data)
		return nil
	}
	s.bytes += int64(len(data) - len(e.profile))
	e.size += int64(len(data) - len(e.profile))
	e.profile = data
	e.trace.HasProfile = true
	s.evict()
	if _, kept := s.items[id]; kept {
		s.publish(e.trace)
	}
	return nil
}

func (s *Store) Profile(id string) ([]byte, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if e, ok := s.items[id]; ok && e.profile != nil {
		return e.profile, true
	}
	data, ok := s.pending[id]
	return data, ok
}

func (s *Store) Subscribe() (<-chan Trace, func()) {
	ch := make(chan Trace, subBuffer)
	s.mu.Lock()
	s.subs[ch] = struct{}{}
	s.mu.Unlock()
	var once sync.Once
	return ch, func() {
		once.Do(func() {
			s.mu.Lock()
			delete(s.subs, ch)
			s.mu.Unlock()
			close(ch)
		})
	}
}

func (s *Store) publish(t Trace) {
	for ch := range s.subs {
		select {
		case ch <- t:
		default:
		}
	}
}

func (s *Store) evict() {
	for len(s.order) > 1 && (len(s.order) > s.maxTraces || s.bytes > s.maxBytes) {
		s.remove(s.order[0])
		s.evicted++
	}
}

func (s *Store) remove(id string) {
	e, ok := s.items[id]
	if !ok {
		return
	}
	delete(s.items, id)
	s.bytes -= e.size
	s.gen++
	for i, o := range s.order {
		if o == id {
			s.order = append(s.order[:i], s.order[i+1:]...)
			break
		}
	}
}

func Summarize(t Trace) Summary {
	return Summary{
		ID:         t.ID,
		Start:      t.Start,
		Duration:   t.Duration,
		Method:     t.Method,
		URL:        t.URL,
		Route:      t.Route,
		Status:     t.Status,
		Counts:     t.Counts,
		NPlusOne:   len(t.NPlusOne) > 0,
		HasProfile: t.HasProfile,
		Times:      costTimes(t),
	}
}

const (
	traceOverhead = 512
	spanOverhead  = 256
	attrOverhead  = 64
)

func traceSize(t Trace) int64 {
	n := int64(traceOverhead + len(t.ID) + len(t.Method) + len(t.URL) + len(t.Route) + len(t.VarnishXID))
	for _, q := range t.NPlusOne {
		n += int64(len(q))
	}
	for _, sp := range t.Spans {
		n += int64(spanOverhead + len(sp.TraceID) + len(sp.SpanID) + len(sp.ParentID) + len(sp.Name) +
			len(sp.Resource) + len(sp.Service) + len(sp.Type) + len(sp.Component))
		for k, v := range sp.Attrs {
			n += int64(attrOverhead + len(k) + len(v))
		}
	}
	return n
}
