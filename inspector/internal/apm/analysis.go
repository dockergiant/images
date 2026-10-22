package apm

import (
	"slices"
	"sort"
	"strings"
	"time"
)

const (
	maxHot     = 25
	maxSlowest = 5
	maxPath    = 8
	// A cache pattern can run thousands of times in one request; the UI only
	// needs enough ids to mark the group in the waterfall.
	maxGroupIDs = 500
)

var (
	categoryKeys = []string{"sql", "cache", "search", "http", "php"}
	callKinds    = []string{"sql", "cache", "search", "http"}
	opKinds      = map[string]bool{"sql": true, "cache": true, "search": true, "http": true, "plugin": true, "observer": true, "block": true}

	// Cm_Cache wraps every save in multi and exec, so these repeat in every
	// request without saying anything about the data it reads.
	cacheControl = map[string]bool{"multi": true, "exec": true, "discard": true, "watch": true, "unwatch": true, "select": true, "ping": true, "auth": true, "connect": true, "pconnect": true, "close": true, "isconnected": true}
)

type Category struct {
	Key   string  `json:"key"`
	Ms    float64 `json:"ms"`
	Share float64 `json:"share"`
	Count int     `json:"count"`
}

type HotItem struct {
	Rank     int      `json:"rank"`
	SpanID   string   `json:"spanId"`
	SpanIDs  []string `json:"spanIds"`
	Name     string   `json:"name"`
	Resource string   `json:"resource"`
	Key      string   `json:"key"`
	SelfMs   float64  `json:"selfMs"`
	TotalMs  float64  `json:"totalMs"`
	Share    float64  `json:"share"`
	Calls    int      `json:"calls"`
	Path     []string `json:"path"`
	Kind     string   `json:"kind"`
}

type Repeat struct {
	Key     string   `json:"key"`
	Kind    string   `json:"kind"`
	Count   int      `json:"count"`
	TotalMs float64  `json:"totalMs"`
	AvgMs   float64  `json:"avgMs"`
	SpanIDs []string `json:"spanIds"`
	Example string   `json:"example"`
}

type SlowCall struct {
	Kind     string  `json:"kind"`
	SpanID   string  `json:"spanId"`
	Resource string  `json:"resource"`
	Ms       float64 `json:"ms"`
}

// Analysis answers where one request spent its time. All times are in ms.
type Analysis struct {
	ID         string     `json:"id"`
	Duration   float64    `json:"duration"`
	Unit       string     `json:"unit"`
	Categories []Category `json:"categories"`
	Hot        []HotItem  `json:"hot"`
	Repeats    []Repeat   `json:"repeats"`
	Slowest    []SlowCall `json:"slowest"`
	Profile    bool       `json:"profile"`
}

// opStat is one operation's cost within a trace, the input for Hotspots.
type opStat struct {
	kind, key string
	calls     int
	self      time.Duration
	worstSpan string
	worstSelf time.Duration
	example   string
}

type traceAnalysis struct {
	analysis Analysis
	ops      []opStat
}

func ms(d time.Duration) float64 { return float64(d) / float64(time.Millisecond) }

func share(part, whole float64) float64 {
	if whole <= 0 {
		return 0
	}
	return part / whole
}

// selfTimes gives each span its duration minus the union of its children's
// intervals, clipped to its own window, so overlapping or overrunning
// children never push it below zero. parent is -1 for roots and orphans.
func selfTimes(spans []Span) ([]time.Duration, []int) {
	n := len(spans)
	idx := make(map[string]int, n)
	for i := range spans {
		if _, dup := idx[spans[i].SpanID]; !dup {
			idx[spans[i].SpanID] = i
		}
	}
	parent := make([]int, n)
	kids := make([]int, n+1)
	for i := range spans {
		p, ok := idx[spans[i].ParentID]
		if !ok || spans[i].ParentID == "" || p == i {
			p = -1
		} else {
			kids[p+1]++
		}
		parent[i] = p
	}
	for i := 1; i <= n; i++ {
		kids[i] += kids[i-1]
	}
	child := make([]int, kids[n])
	fill := slices.Clone(kids[:n])
	for i, p := range parent {
		if p >= 0 {
			child[fill[p]] = i
			fill[p]++
		}
	}
	start := make([]int64, n)
	for i := range spans {
		start[i] = spans[i].Start.UnixNano()
	}
	self := make([]time.Duration, n)
	for p := range spans {
		self[p] = max(spans[p].Duration, 0)
		cs := child[kids[p]:kids[p+1]]
		if len(cs) == 0 {
			continue
		}
		if !slices.IsSortedFunc(cs, func(a, b int) int { return cmpInt64(start[a], start[b]) }) {
			slices.SortFunc(cs, func(a, b int) int { return cmpInt64(start[a], start[b]) })
		}
		lo, hi := start[p], start[p]+int64(spans[p].Duration)
		var covered, curLo, curHi int64
		open := false
		for _, c := range cs {
			s, e := max(start[c], lo), min(start[c]+int64(spans[c].Duration), hi)
			if e <= s {
				continue
			}
			switch {
			case !open:
				curLo, curHi, open = s, e, true
			case s > curHi:
				covered += curHi - curLo
				curLo, curHi = s, e
			case e > curHi:
				curHi = e
			}
		}
		if open {
			covered += curHi - curLo
		}
		self[p] = max(spans[p].Duration-time.Duration(covered), 0)
	}
	return self, parent
}

func cmpInt64(a, b int64) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

// costTimes splits a trace's duration into the cost categories. Everything
// that is not a query, cache, search or outbound call is PHP time.
func costTimes(t Trace) map[string]float64 {
	out := make(map[string]float64, len(categoryKeys))
	var calls time.Duration
	for _, k := range callKinds {
		out[k] = ms(t.Times[k])
		calls += t.Times[k]
	}
	out["php"] = ms(max(t.Duration-calls, 0))
	return out
}

func categoryOfKind(kind string) string {
	switch kind {
	case "sql", "cache", "search", "http":
		return kind
	}
	return "php"
}

type hotKey struct {
	parent    int
	name, key string
}

type hotGroup struct {
	first, worst int
	calls        int
	self, total  time.Duration
}

type callKey struct{ kind, key string }

type repeatGroup struct {
	worst int
	count int
	total time.Duration
}

func analyze(t Trace) *traceAnalysis {
	spans := t.Spans
	n := len(spans)
	self, parent := selfTimes(spans)
	durMs := ms(t.Duration)

	kinds := make([]string, n)
	keys := make([]string, n)
	memo := map[string]string{}
	for i := range spans {
		kinds[i] = kindOf(spans[i])
		keys[i] = spanKey(spans[i], kinds[i], memo)
	}

	times := costTimes(t)
	counts := map[string]int{}
	for i := range spans {
		if counted(spans[i]) {
			counts[categoryOfKind(kinds[i])]++
		}
	}
	a := Analysis{ID: t.ID, Duration: durMs, Unit: "ms", Categories: make([]Category, 0, len(categoryKeys))}
	for _, k := range categoryKeys {
		a.Categories = append(a.Categories, Category{Key: k, Ms: times[k], Share: share(times[k], durMs), Count: counts[k]})
	}

	a.Hot = hotItems(spans, self, parent, kinds, keys, durMs)
	repeats, ops := calls(spans, self, kinds, keys)
	a.Repeats = repeats
	a.Slowest = slowest(spans, kinds, keys)
	return &traceAnalysis{analysis: a, ops: ops}
}

func spanKey(s Span, kind string, memo map[string]string) string {
	var raw string
	switch kind {
	case "sql":
		raw = sqlText(s)
	case "cache":
		raw = cacheCall(s)
	case "search", "http":
		return requestURL(s)
	default:
		if s.Resource == "" {
			return s.Name
		}
		return s.Resource
	}
	if v, ok := memo[raw]; ok {
		return v
	}
	var v string
	if kind == "sql" {
		v = normalizeSQL(raw)
	} else {
		v = normalizeCache(raw)
	}
	memo[raw] = v
	return v
}

func hotItems(spans []Span, self []time.Duration, parent []int, kinds, keys []string, durMs float64) []HotItem {
	index := make(map[hotKey]int, len(spans)/2)
	var groups []hotGroup
	member := make([]int, len(spans))
	for i := range spans {
		k := hotKey{parent: parent[i], name: spans[i].Name, key: keys[i]}
		g, ok := index[k]
		if !ok {
			g = len(groups)
			index[k] = g
			groups = append(groups, hotGroup{first: i, worst: i})
		}
		member[i] = g
		gr := &groups[g]
		gr.calls++
		gr.self += self[i]
		gr.total += spans[i].Duration
		if self[i] > self[gr.worst] {
			gr.worst = i
		}
	}
	order := make([]int, len(groups))
	for i := range order {
		order[i] = i
	}
	sort.Slice(order, func(x, y int) bool {
		a, b := groups[order[x]], groups[order[y]]
		if a.self != b.self {
			return a.self > b.self
		}
		return a.first < b.first
	})
	if len(order) > maxHot {
		order = order[:maxHot]
	}
	slot := map[int]int{}
	out := make([]HotItem, len(order))
	for r, g := range order {
		gr := groups[g]
		w := gr.worst
		slot[g] = r
		out[r] = HotItem{
			Rank:     r + 1,
			SpanID:   spans[w].SpanID,
			SpanIDs:  make([]string, 0, min(gr.calls, maxGroupIDs)),
			Name:     spans[w].Name,
			Resource: display(spans[w], kinds[w]),
			Key:      keys[w],
			SelfMs:   ms(gr.self),
			TotalMs:  ms(gr.total),
			Share:    share(ms(gr.self), durMs),
			Calls:    gr.calls,
			Path:     path(spans, parent, w),
			Kind:     kinds[w],
		}
	}
	for i, g := range member {
		if r, ok := slot[g]; ok && len(out[r].SpanIDs) < maxGroupIDs {
			out[r].SpanIDs = append(out[r].SpanIDs, spans[i].SpanID)
		}
	}
	return out
}

// path names the ancestors of span i from the top down, without the root.
// Deep chains keep their head and tail.
func path(spans []Span, parent []int, i int) []string {
	var up []string
	for p, steps := parent[i], 0; p >= 0 && steps < len(spans); p, steps = parent[p], steps+1 {
		if parent[p] < 0 && p == 0 {
			break
		}
		up = append(up, pathLabel(spans[p]))
	}
	out := make([]string, 0, min(len(up), maxPath))
	if len(up) > maxPath {
		head, tail := 3, maxPath-4
		for k := len(up) - 1; k >= len(up)-head; k-- {
			out = append(out, up[k])
		}
		out = append(out, "…")
		for k := tail - 1; k >= 0; k-- {
			out = append(out, up[k])
		}
		return out
	}
	for k := len(up) - 1; k >= 0; k-- {
		out = append(out, up[k])
	}
	return out
}

func pathLabel(s Span) string {
	switch {
	case s.Name == "magento.block.render":
		if b := s.Attrs["magento.block.name"]; b != "" {
			return "block " + b
		}
		return "block " + s.Resource
	case s.Name == "magento.template.render":
		return "template " + s.Resource
	case s.Name == "magento.event.execute":
		return "observer " + shortClass(s.Resource)
	case strings.HasPrefix(s.Name, "magento.plugin"):
		return "plugin " + shortClass(s.Resource)
	case strings.HasPrefix(s.Name, "magento."):
		return strings.TrimPrefix(s.Name, "magento.")
	}
	return s.Name
}

func shortClass(c string) string {
	c = strings.TrimSuffix(c, "\\Interceptor")
	if i := strings.LastIndexByte(c, '\\'); i >= 0 {
		return c[i+1:]
	}
	return c
}

// calls groups the counted query, cache, search and outbound calls by their
// normalised form: groups of nPlusOneMin or more are repeats, and every
// group of a kind Hotspots ranks becomes an op.
func calls(spans []Span, self []time.Duration, kinds, keys []string) ([]Repeat, []opStat) {
	repIndex := map[callKey]int{}
	var reps []repeatGroup
	repOf := make([]int, len(spans))
	opIndex := map[callKey]int{}
	var ops []opStat
	for i := range spans {
		repOf[i] = -1
		s := spans[i]
		kind := kinds[i]
		if !opKinds[kind] || !counted(s) {
			continue
		}
		k := callKey{kind, keys[i]}
		o, ok := opIndex[k]
		if !ok {
			o = len(ops)
			opIndex[k] = o
			ops = append(ops, opStat{kind: kind, key: keys[i], worstSpan: s.SpanID, worstSelf: -1})
		}
		op := &ops[o]
		op.calls++
		op.self += self[i]
		if self[i] > op.worstSelf {
			op.worstSelf = self[i]
			op.worstSpan = s.SpanID
			op.example = display(s, kind)
		}
		if categoryOfKind(kind) != kind || framing(kind, keys[i]) {
			continue
		}
		g, ok := repIndex[k]
		if !ok {
			g = len(reps)
			repIndex[k] = g
			reps = append(reps, repeatGroup{worst: i})
		}
		repOf[i] = g
		rg := &reps[g]
		rg.count++
		rg.total += s.Duration
		if s.Duration > spans[rg.worst].Duration {
			rg.worst = i
		}
	}
	out := []Repeat{}
	slot := map[int]int{}
	for g, rg := range reps {
		if rg.count < nPlusOneMin {
			continue
		}
		w := rg.worst
		slot[g] = len(out)
		out = append(out, Repeat{
			Key:     keys[w],
			Kind:    kinds[w],
			Count:   rg.count,
			TotalMs: ms(rg.total),
			AvgMs:   ms(rg.total) / float64(rg.count),
			SpanIDs: make([]string, 0, min(rg.count, maxGroupIDs)),
			Example: display(spans[w], kinds[w]),
		})
	}
	if len(out) > 0 {
		for i, g := range repOf {
			if r, ok := slot[g]; ok && g >= 0 && len(out[r].SpanIDs) < maxGroupIDs {
				out[r].SpanIDs = append(out[r].SpanIDs, spans[i].SpanID)
			}
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].TotalMs != out[j].TotalMs {
			return out[i].TotalMs > out[j].TotalMs
		}
		return out[i].Key < out[j].Key
	})
	if ops == nil {
		ops = []opStat{}
	}
	return out, ops
}

func framing(kind, key string) bool {
	return kind == "cache" && cacheControl[strings.ToLower(cacheCommand(key))]
}

func slowest(spans []Span, kinds, keys []string) []SlowCall {
	top := map[string][]int{}
	for i := range spans {
		kind := kinds[i]
		if categoryOfKind(kind) != kind || kind == "php" || !counted(spans[i]) || framing(kind, keys[i]) {
			continue
		}
		list := top[kind]
		if len(list) == maxSlowest && spans[i].Duration <= spans[list[maxSlowest-1]].Duration {
			continue
		}
		pos := sort.Search(len(list), func(k int) bool { return spans[list[k]].Duration < spans[i].Duration })
		list = slices.Insert(list, pos, i)
		if len(list) > maxSlowest {
			list = list[:maxSlowest]
		}
		top[kind] = list
	}
	out := []SlowCall{}
	for _, kind := range callKinds {
		for _, i := range top[kind] {
			out = append(out, SlowCall{Kind: kind, SpanID: spans[i].SpanID, Resource: display(spans[i], kind), Ms: ms(spans[i].Duration)})
		}
	}
	return out
}

const (
	itemOverhead = 96
	idOverhead   = 32
)

func analysisSize(d *traceAnalysis) int64 {
	if d == nil {
		return 0
	}
	a := d.analysis
	n := int64(traceOverhead + len(a.ID) + len(a.Categories)*itemOverhead)
	for _, h := range a.Hot {
		n += int64(itemOverhead + len(h.SpanID) + len(h.Name) + len(h.Resource) + len(h.Key) + len(h.Kind))
		for _, p := range h.Path {
			n += int64(idOverhead + len(p))
		}
		n += int64(len(h.SpanIDs) * (idOverhead + 16))
	}
	for _, r := range a.Repeats {
		n += int64(itemOverhead+len(r.Key)+len(r.Kind)+len(r.Example)) + int64(len(r.SpanIDs)*(idOverhead+16))
	}
	for _, s := range a.Slowest {
		n += int64(itemOverhead + len(s.Kind) + len(s.SpanID) + len(s.Resource))
	}
	for _, o := range d.ops {
		n += int64(itemOverhead + len(o.kind) + len(o.key) + len(o.worstSpan) + len(o.example))
	}
	return n
}
