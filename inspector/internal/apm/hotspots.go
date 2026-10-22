package apm

import (
	"math"
	"slices"
	"sort"
	"time"
)

// hotspotTTL bounds how often Hotspots walks the whole store.
const hotspotTTL = time.Second

type RouteStat struct {
	Route   string             `json:"route"`
	Count   int                `json:"count"`
	P50Ms   float64            `json:"p50Ms"`
	P95Ms   float64            `json:"p95Ms"`
	MaxMs   float64            `json:"maxMs"`
	Times   map[string]float64 `json:"times"`
	WorstID string             `json:"worstId"`
}

type Operation struct {
	Key         string  `json:"key"`
	Kind        string  `json:"kind"`
	SelfMs      float64 `json:"selfMs"`
	Calls       int     `json:"calls"`
	Traces      int     `json:"traces"`
	Share       float64 `json:"share"`
	WorstID     string  `json:"worstId"`
	WorstSpanID string  `json:"worstSpanId"`
	Example     string  `json:"example"`
}

type RepeatStat struct {
	Key          string   `json:"key"`
	Kind         string   `json:"kind"`
	PerRequest   float64  `json:"perRequest"`
	MsPerRequest float64  `json:"msPerRequest"`
	Traces       int      `json:"traces"`
	Routes       []string `json:"routes"`
	WorstID      string   `json:"worstId"`
}

// Hotspots ranks routes, operations and repeated calls across every trace
// in the store. Since is the start of the oldest trace, null when empty.
type Hotspots struct {
	Traces     int          `json:"traces"`
	Since      *time.Time   `json:"since"`
	Routes     []RouteStat  `json:"routes"`
	Operations []Operation  `json:"operations"`
	Repeats    []RepeatStat `json:"repeats"`
}

func EmptyHotspots() Hotspots {
	return Hotspots{Routes: []RouteStat{}, Operations: []Operation{}, Repeats: []RepeatStat{}}
}

// Hotspots returns at most limit rows per list; limit 0 means all. The
// result is recomputed at most once per hotspotTTL.
func (s *Store) Hotspots(limit int) Hotspots {
	s.hotMu.Lock()
	defer s.hotMu.Unlock()
	now := s.now()
	s.mu.Lock()
	gen := s.gen
	stale := s.hot == nil || (gen != s.hotGen && now.Sub(s.hotAt) >= hotspotTTL)
	var snap []hotInput
	if stale {
		snap = make([]hotInput, 0, len(s.order))
		for _, id := range s.order {
			e := s.items[id]
			snap = append(snap, hotInput{trace: e.trace, analysis: e.analysis})
		}
	}
	s.mu.Unlock()
	if stale {
		h := computeHotspots(snap)
		s.hot, s.hotGen, s.hotAt = &h, gen, now
	}
	h := *s.hot
	if limit > 0 {
		h.Routes = h.Routes[:min(limit, len(h.Routes))]
		h.Operations = h.Operations[:min(limit, len(h.Operations))]
		h.Repeats = h.Repeats[:min(limit, len(h.Repeats))]
	}
	return h
}

// hotInput holds what Hotspots reads of an entry. The trace's spans and the
// analysis are never changed after Add, so they are read outside the lock.
type hotInput struct {
	trace    Trace
	analysis *traceAnalysis
}

type routeAcc struct {
	durations []float64
	times     map[string]float64
	worst     time.Duration
	worstID   string
}

type opAcc struct {
	Operation
	worstSelf time.Duration
}

type repeatAcc struct {
	RepeatStat
	count   int
	totalMs float64
	worstMs float64
	routes  map[string]bool
}

func computeHotspots(in []hotInput) Hotspots {
	h := EmptyHotspots()
	h.Traces = len(in)
	routes := map[string]*routeAcc{}
	ops := map[callKey]*opAcc{}
	reps := map[callKey]*repeatAcc{}
	var totalMs float64
	for _, x := range in {
		t := x.trace
		if h.Since == nil || t.Start.Before(*h.Since) {
			start := t.Start
			h.Since = &start
		}
		totalMs += ms(t.Duration)

		r := routes[t.Route]
		if r == nil {
			r = &routeAcc{times: map[string]float64{}}
			routes[t.Route] = r
		}
		r.durations = append(r.durations, ms(t.Duration))
		for k, v := range costTimes(t) {
			r.times[k] += v
		}
		if r.worstID == "" || t.Duration > r.worst {
			r.worst, r.worstID = t.Duration, t.ID
		}

		if x.analysis == nil {
			continue
		}
		for _, o := range x.analysis.ops {
			k := callKey{o.kind, o.key}
			acc := ops[k]
			if acc == nil {
				acc = &opAcc{Operation: Operation{Key: o.key, Kind: o.kind}, worstSelf: -1}
				ops[k] = acc
			}
			acc.SelfMs += ms(o.self)
			acc.Calls += o.calls
			acc.Traces++
			if o.self > acc.worstSelf {
				acc.worstSelf = o.self
				acc.WorstID, acc.WorstSpanID, acc.Example = t.ID, o.worstSpan, o.example
			}
		}
		for _, rp := range x.analysis.analysis.Repeats {
			k := callKey{rp.Kind, rp.Key}
			acc := reps[k]
			if acc == nil {
				acc = &repeatAcc{RepeatStat: RepeatStat{Key: rp.Key, Kind: rp.Kind}, worstMs: -1, routes: map[string]bool{}}
				reps[k] = acc
			}
			acc.Traces++
			acc.count += rp.Count
			acc.totalMs += rp.TotalMs
			acc.routes[t.Route] = true
			if rp.TotalMs > acc.worstMs {
				acc.worstMs, acc.WorstID = rp.TotalMs, t.ID
			}
		}
	}

	for route, r := range routes {
		slices.Sort(r.durations)
		n := float64(len(r.durations))
		times := make(map[string]float64, len(categoryKeys))
		for _, k := range categoryKeys {
			times[k] = r.times[k] / n
		}
		h.Routes = append(h.Routes, RouteStat{
			Route:   route,
			Count:   len(r.durations),
			P50Ms:   nearestRank(r.durations, 0.5),
			P95Ms:   nearestRank(r.durations, 0.95),
			MaxMs:   r.durations[len(r.durations)-1],
			Times:   times,
			WorstID: r.worstID,
		})
	}
	sort.Slice(h.Routes, func(i, j int) bool {
		a, b := h.Routes[i], h.Routes[j]
		if a.P95Ms != b.P95Ms {
			return a.P95Ms > b.P95Ms
		}
		return a.Route < b.Route
	})

	for _, acc := range ops {
		acc.Share = share(acc.SelfMs, totalMs)
		h.Operations = append(h.Operations, acc.Operation)
	}
	sort.Slice(h.Operations, func(i, j int) bool {
		a, b := h.Operations[i], h.Operations[j]
		if a.SelfMs != b.SelfMs {
			return a.SelfMs > b.SelfMs
		}
		if a.Kind != b.Kind {
			return a.Kind < b.Kind
		}
		return a.Key < b.Key
	})

	totals := map[callKey]float64{}
	for k, acc := range reps {
		n := float64(acc.Traces)
		acc.PerRequest = math.Round(float64(acc.count)/n*10) / 10
		acc.MsPerRequest = acc.totalMs / n
		acc.Routes = make([]string, 0, len(acc.routes))
		for r := range acc.routes {
			acc.Routes = append(acc.Routes, r)
		}
		sort.Strings(acc.Routes)
		totals[k] = acc.totalMs
		h.Repeats = append(h.Repeats, acc.RepeatStat)
	}
	sort.Slice(h.Repeats, func(i, j int) bool {
		a, b := h.Repeats[i], h.Repeats[j]
		ta, tb := totals[callKey{a.Kind, a.Key}], totals[callKey{b.Kind, b.Key}]
		if ta != tb {
			return ta > tb
		}
		if a.Kind != b.Kind {
			return a.Kind < b.Kind
		}
		return a.Key < b.Key
	})
	return h
}

// nearestRank is the smallest value with at least p of the values at or
// below it.
func nearestRank(vals []float64, p float64) float64 {
	if len(vals) == 0 {
		return 0
	}
	if !slices.IsSorted(vals) {
		vals = slices.Sorted(slices.Values(vals))
	}
	// The epsilon keeps 0.95*20 = 19.000000000000004 at rank 19.
	rank := int(math.Ceil(p*float64(len(vals)) - 1e-9))
	return vals[min(max(rank, 1), len(vals))-1]
}
