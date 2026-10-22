// Package metrics keeps a short history of service metrics, so the Health
// screens can draw "over time" charts from what a single snapshot cannot show.
package metrics

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/dockergiant/images/inspector/internal/core"
)

const (
	Step          = 10 * time.Second
	Keep          = 60 * time.Minute
	Capacity      = int(Keep / Step)
	DefaultWindow = 15 * time.Minute
	Top           = 8
	// SampleTimeout stays under Step, so one sampler never overlaps itself.
	SampleTimeout = 8 * time.Second
	maxNames      = 1024
	other         = "other"
)

var ErrBusy = errors.New("a metrics sample is already running")

var windows = map[string]time.Duration{"5m": 5 * time.Minute, "15m": 15 * time.Minute, "60m": time.Hour, "1h": time.Hour}

// Value encodes NaN as null: a ratio without a denominator has no value, and
// 0 would read as a real one.
type Value float64

func (v Value) MarshalJSON() ([]byte, error) {
	f := float64(v)
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return []byte("null"), nil
	}
	if f != math.Trunc(f) {
		f = math.Round(f*1e4) / 1e4
	}
	return strconv.AppendFloat(nil, f, 'f', -1, 64), nil
}

func (v *Value) UnmarshalJSON(b []byte) error {
	if string(b) == "null" {
		*v = Value(math.NaN())
		return nil
	}
	f, err := strconv.ParseFloat(string(b), 64)
	*v = Value(f)
	return err
}

// Point is [unix seconds, value].
type Point [2]Value

type Ratio struct {
	Name string
	Num  []string
	Den  []string
}

// Group folds a family of series, such as one per queue, into the Top members
// plus "other". Members are ranked by the Rank prefixes (all prefixes when
// empty): counters by their increase over the window, gauges by their latest
// value.
type Group struct {
	Prefixes []string
	Rank     []string
}

// Spec names what a service reports. An entry ending in "." matches every
// name with that prefix. Names that match neither Counters nor Gauges are
// kept only as inputs for Ratios.
type Spec struct {
	Counters []string
	Gauges   []string
	Ratios   []Ratio
	Groups   []Group
}

func matches(list []string, name string) bool {
	for _, p := range list {
		if p == name || strings.HasSuffix(p, ".") && strings.HasPrefix(name, p) {
			return true
		}
	}
	return false
}

func (s Spec) kind(name string) string {
	switch {
	case matches(s.Counters, name):
		return "rate"
	case matches(s.Gauges, name):
		return "gauge"
	}
	return ""
}

type sample struct {
	at     time.Time
	values map[string]float64
}

type History struct {
	spec Spec
	now  func() time.Time
	busy sync.Mutex

	mu      sync.Mutex
	ring    []sample
	next    int
	lastErr string
}

func NewHistory(spec Spec) *History {
	return &History{spec: spec, now: time.Now, ring: make([]sample, 0, Capacity)}
}

// WithClock makes the history stamp samples and windows with the adapter's
// clock, so tests that move one move both.
func (h *History) WithClock(now func() time.Time) *History {
	h.now = now
	return h
}

// Collect runs one fetch and records it. A fetch that is still running makes
// the call return ErrBusy instead of starting a second one.
func (h *History) Collect(ctx context.Context, fetch func(context.Context) (map[string]float64, error)) (map[string]float64, error) {
	if !h.busy.TryLock() {
		return nil, ErrBusy
	}
	defer h.busy.Unlock()
	v, err := fetch(ctx)
	h.mu.Lock()
	switch {
	case err == nil:
		h.lastErr = ""
	case errors.Is(err, core.ErrAbsent):
		h.lastErr = ""
	default:
		h.lastErr = err.Error()
	}
	h.mu.Unlock()
	if err != nil {
		return nil, err
	}
	h.Add(h.now(), v)
	return v, nil
}

func (h *History) Add(at time.Time, values map[string]float64) {
	if len(values) > maxNames {
		names := make([]string, 0, len(values))
		for k := range values {
			names = append(names, k)
		}
		sort.Strings(names)
		kept := make(map[string]float64, maxNames)
		for _, k := range names[:maxNames] {
			kept[k] = values[k]
		}
		values = kept
	} else {
		values = cloneMap(values)
	}
	s := sample{at: at, values: values}
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.ring) < Capacity {
		h.ring = append(h.ring, s)
		return
	}
	h.ring[h.next] = s
	h.next = (h.next + 1) % Capacity
}

func cloneMap(m map[string]float64) map[string]float64 {
	out := make(map[string]float64, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

func (h *History) Len() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.ring)
}

func (h *History) ordered() ([]sample, string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]sample, 0, len(h.ring))
	out = append(out, h.ring[h.next:]...)
	out = append(out, h.ring[:h.next]...)
	return out, h.lastErr
}

type Result struct {
	Step    int                `json:"step"`
	Window  int                `json:"window"`
	Samples int                `json:"samples"`
	Series  map[string][]Point `json:"series"`
	Latest  map[string]Value   `json:"latest"`
	Totals  map[string]Value   `json:"totals"`
	Kinds   map[string]string  `json:"kinds"`
	Error   string             `json:"error,omitempty"`
}

func (h *History) Query(window time.Duration) Result {
	r := Result{
		Step:   int(Step / time.Second),
		Window: int(window / time.Second),
		Series: map[string][]Point{},
		Latest: map[string]Value{},
		Totals: map[string]Value{},
		Kinds:  map[string]string{},
	}
	if h == nil {
		return r
	}
	all, lastErr := h.ordered()
	r.Error = lastErr
	now := h.now()
	from := now.Add(-window)
	if oldest := now.Add(-Keep); from.Before(oldest) {
		from = oldest
	}
	start := len(all)
	for i, s := range all {
		if !s.at.Before(from) {
			start = i
			break
		}
	}
	in := all[start:]
	r.Samples = len(in)
	r.Step = spacing(in)
	var base *sample
	if start > 0 && len(in) > 0 && in[0].at.Sub(all[start-1].at) <= 2*Keep {
		base = &all[start-1]
	}
	prevOf := func(i int) *sample {
		if i > 0 {
			return &in[i-1]
		}
		return base
	}

	for _, n := range h.spec.Counters {
		if !strings.HasSuffix(n, ".") {
			r.Series[n] = []Point{}
			r.Kinds[n] = "rate"
		}
	}
	for _, n := range h.spec.Gauges {
		if !strings.HasSuffix(n, ".") {
			r.Series[n] = []Point{}
			r.Kinds[n] = "gauge"
		}
	}
	names := map[string]bool{}
	for _, s := range in {
		for n := range s.values {
			names[n] = true
		}
	}
	for n := range names {
		kind := h.spec.kind(n)
		if kind == "" {
			continue
		}
		r.Kinds[n] = kind
		pts := []Point{}
		total, haveTotal := 0.0, false
		for i, s := range in {
			v, ok := s.values[n]
			if !ok {
				continue
			}
			t := Value(s.at.Unix())
			if kind == "gauge" {
				pts = append(pts, Point{t, Value(v)})
				continue
			}
			p := prevOf(i)
			if p == nil {
				continue
			}
			pv, ok := p.values[n]
			dt := s.at.Sub(p.at).Seconds()
			if !ok || dt <= 0 {
				continue
			}
			d := v - pv
			if d < 0 {
				d = 0
			}
			total += d
			haveTotal = true
			pts = append(pts, Point{t, Value(d / dt)})
		}
		r.Series[n] = pts
		if haveTotal {
			r.Totals[n] = Value(total)
		}
	}

	for _, ra := range h.spec.Ratios {
		r.Kinds[ra.Name] = "ratio"
		pts := []Point{}
		var num, den float64
		for i, s := range in {
			p := prevOf(i)
			if p == nil {
				continue
			}
			dn, ok1 := delta(p.values, s.values, ra.Num)
			dd, ok2 := delta(p.values, s.values, ra.Den)
			if !ok1 || !ok2 {
				continue
			}
			v := math.NaN()
			if dn >= 0 && dd > 0 {
				v = dn / dd
				num += dn
				den += dd
			}
			pts = append(pts, Point{Value(s.at.Unix()), Value(v)})
		}
		r.Series[ra.Name] = pts
		if den > 0 {
			r.Totals[ra.Name] = Value(num / den)
		} else if len(pts) > 0 {
			r.Totals[ra.Name] = Value(math.NaN())
		}
	}

	if len(in) > 0 {
		last := in[len(in)-1].values
		for n, v := range last {
			switch h.spec.kind(n) {
			case "rate":
				r.Latest[n] = Value(max(v, 0))
			case "gauge":
				r.Latest[n] = Value(v)
			}
		}
		for _, ra := range h.spec.Ratios {
			n, ok1 := sum(last, ra.Num)
			d, ok2 := sum(last, ra.Den)
			if !ok1 || !ok2 {
				continue
			}
			if d > 0 {
				r.Latest[ra.Name] = Value(n / d)
			} else {
				r.Latest[ra.Name] = Value(math.NaN())
			}
		}
	}

	for _, g := range h.spec.Groups {
		fold(&r, g)
	}
	return r
}

// spacing is the median gap between samples in whole seconds, so a sampler
// that runs every 30 s, or was paused, is not reported as 10 s.
func spacing(in []sample) int {
	if len(in) < 2 {
		return int(Step / time.Second)
	}
	gaps := make([]float64, 0, len(in)-1)
	for i := 1; i < len(in); i++ {
		gaps = append(gaps, in[i].at.Sub(in[i-1].at).Seconds())
	}
	sort.Float64s(gaps)
	if n := int(math.Round(gaps[len(gaps)/2])); n > 0 {
		return n
	}
	return int(Step / time.Second)
}

// sum needs one of the names, not all: an older engine may lack a counter,
// such as hitpass, which Varnish 6.0 does not report.
func sum(m map[string]float64, names []string) (float64, bool) {
	total, found := 0.0, false
	for _, n := range names {
		if v, ok := m[n]; ok {
			total += v
			found = true
		}
	}
	return total, found
}

func delta(prev, cur map[string]float64, names []string) (float64, bool) {
	a, ok1 := sum(prev, names)
	b, ok2 := sum(cur, names)
	if !ok1 || !ok2 {
		return 0, false
	}
	return b - a, true
}

type member struct {
	name          string
	rank, overall float64
}

func fold(r *Result, g Group) {
	rank := g.Rank
	if len(rank) == 0 {
		rank = g.Prefixes
	}
	byName := map[string]*member{}
	for _, p := range g.Prefixes {
		for n := range r.Kinds {
			if !strings.HasPrefix(n, p) {
				continue
			}
			m := strings.TrimPrefix(n, p)
			if byName[m] == nil {
				byName[m] = &member{name: m}
			}
			score := scoreOf(r, n)
			byName[m].overall += score
			if slices.Contains(rank, p) {
				byName[m].rank += score
			}
		}
	}
	members := make([]*member, 0, len(byName))
	for _, m := range byName {
		if m.name != other {
			members = append(members, m)
		}
	}
	if len(members) <= Top && byName[other] == nil {
		return
	}
	sort.Slice(members, func(i, j int) bool {
		a, b := members[i], members[j]
		if a.rank != b.rank {
			return a.rank > b.rank
		}
		if a.overall != b.overall {
			return a.overall > b.overall
		}
		return a.name < b.name
	})
	folded := []string{}
	if len(members) > Top {
		for _, m := range members[Top:] {
			folded = append(folded, m.name)
		}
	}
	if byName[other] != nil {
		folded = append(folded, other)
	}
	for _, p := range g.Prefixes {
		kind := ""
		var series [][]Point
		latest, haveLatest := 0.0, false
		total, haveTotal := 0.0, false
		for _, m := range folded {
			n := p + m
			k, ok := r.Kinds[n]
			if !ok {
				continue
			}
			kind = k
			series = append(series, r.Series[n])
			if v, ok := r.Latest[n]; ok {
				latest += float64(v)
				haveLatest = true
			}
			if v, ok := r.Totals[n]; ok {
				total += float64(v)
				haveTotal = true
			}
			delete(r.Series, n)
			delete(r.Latest, n)
			delete(r.Totals, n)
			delete(r.Kinds, n)
		}
		if kind == "" {
			continue
		}
		n := p + other
		r.Kinds[n] = kind
		r.Series[n] = sumSeries(series)
		if haveLatest {
			r.Latest[n] = Value(latest)
		}
		if haveTotal {
			r.Totals[n] = Value(total)
		}
	}
}

func scoreOf(r *Result, name string) float64 {
	var v Value
	if r.Kinds[name] == "rate" {
		v = r.Totals[name]
	} else {
		v = r.Latest[name]
	}
	if math.IsNaN(float64(v)) {
		return 0
	}
	return float64(v)
}

func sumSeries(list [][]Point) []Point {
	byT := map[Value]float64{}
	for _, s := range list {
		for _, p := range s {
			if !math.IsNaN(float64(p[1])) {
				byT[p[0]] += float64(p[1])
			} else if _, ok := byT[p[0]]; !ok {
				byT[p[0]] = 0
			}
		}
	}
	out := make([]Point, 0, len(byT))
	for t, v := range byT {
		out = append(out, Point{t, Value(v)})
	}
	sort.Slice(out, func(i, j int) bool { return out[i][0] < out[j][0] })
	return out
}

func ParseWindow(s string) (time.Duration, error) {
	if s == "" {
		return DefaultWindow, nil
	}
	if d, ok := windows[s]; ok {
		return d, nil
	}
	return 0, errors.New("unknown window " + strconv.Quote(s))
}

const windowHint = "use window=5m, 15m or 60m"

func (h *History) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	window, err := ParseWindow(r.URL.Query().Get("window"))
	if err != nil {
		WindowError(w, err)
		return
	}
	WriteJSON(w, http.StatusOK, h.Query(window))
}

// WindowError answers a bad ?window= the same way on every metrics route.
func WindowError(w http.ResponseWriter, err error) {
	WriteJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error(), "hint": windowHint})
}

func WriteJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
