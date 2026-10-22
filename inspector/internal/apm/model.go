package apm

import (
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

type Span struct {
	TraceID   string            `json:"traceId"`
	SpanID    string            `json:"spanId"`
	ParentID  string            `json:"parentId"`
	Name      string            `json:"name"`
	Resource  string            `json:"resource"`
	Service   string            `json:"service"`
	Type      string            `json:"type"`
	Component string            `json:"component"`
	Start     time.Time         `json:"start"`
	Duration  time.Duration     `json:"duration"`
	Error     bool              `json:"error"`
	Attrs     map[string]string `json:"attrs"`
}

type Trace struct {
	ID         string                   `json:"id"`
	Start      time.Time                `json:"start"`
	Duration   time.Duration            `json:"duration"`
	Method     string                   `json:"method"`
	URL        string                   `json:"url"`
	Route      string                   `json:"route"`
	Status     int                      `json:"status"`
	VarnishXID string                   `json:"varnishXid"`
	Cached     bool                     `json:"cached"`
	Counts     map[string]int           `json:"counts"`
	Times      map[string]time.Duration `json:"times"`
	NPlusOne   []string                 `json:"nPlusOne"`
	HasProfile bool                     `json:"hasProfile"`
	Spans      []Span                   `json:"spans"`
	// Benchmark marks a request the Performance page's benchmark sent.
	Benchmark bool `json:"-"`

	// pageKey is host and path. taught marks a route the Magento integration
	// set; urlRoute one that is only "METHOD /path", as in lean traces.
	pageKey  string
	taught   bool
	urlRoute bool
}

// BenchmarkUserAgent is what the benchmark sends, so the store can keep its
// synthetic load out of the traces and the cost figures.
const BenchmarkUserAgent = "Roll-Inspector-Benchmark"

type Summary struct {
	ID         string         `json:"id"`
	Start      time.Time      `json:"start"`
	Duration   time.Duration  `json:"duration"`
	Method     string         `json:"method"`
	URL        string         `json:"url"`
	Route      string         `json:"route"`
	Status     int            `json:"status"`
	Counts     map[string]int `json:"counts"`
	NPlusOne   bool           `json:"nPlusOne"`
	HasProfile bool           `json:"hasProfile"`
	// Times holds ms per cost category (sql, cache, search, http, php), so a
	// list row can draw the cost strip without fetching the trace.
	Times map[string]float64 `json:"times"`
}

type ListFilter struct {
	Route       string
	MinDuration time.Duration
	Sort        string
	Limit       int
}

const maxAttr = 4 << 10

var countedKinds = []string{"sql", "cache", "search", "queue", "http"}

func cut(s string) string {
	if len(s) <= maxAttr {
		return s
	}
	i := maxAttr
	for i > 0 && !utf8.RuneStart(s[i]) {
		i--
	}
	return s[:i]
}

func assemble(spans []Span) (Trace, bool) {
	spans = dropProfilerUploads(spans)
	if len(spans) == 0 {
		return Trace{}, false
	}
	for i := range spans {
		spans[i].Component = classify(spans[i])
	}
	sort.SliceStable(spans, func(i, j int) bool { return spans[i].Start.Before(spans[j].Start) })
	ri := rootIndex(spans)
	if ri > 0 {
		root := spans[ri]
		copy(spans[1:ri+1], spans[:ri])
		spans[0] = root
	}
	root := spans[0]
	tr := Trace{
		ID:       root.TraceID,
		Start:    root.Start,
		Duration: root.Duration,
		Spans:    spans,
	}
	a := root.Attrs
	tr.Method = first(a, "http.method", "http.request.method")
	tr.URL = first(a, "http.url", "url.full")
	tr.Status, _ = strconv.Atoi(first(a, "http.status_code", "http.response.status_code"))
	tr.Route = first(a, "magento.pathinfo")
	tr.taught = tr.Route != ""
	if tr.Route == "" {
		tr.Route = root.Resource
		tr.urlRoute = urlResource(root.Resource)
	}
	if tr.Route == "" {
		tr.Route = first(a, "magento.route", "http.route")
	}
	tr.pageKey = pageKey(tr.URL)
	tr.VarnishXID = a["varnish.xid"]
	tr.Benchmark = first(a, "http.useragent", "user_agent.original", "http.user_agent") == BenchmarkUserAgent
	switch strings.ToLower(a["magento.cached"]) {
	case "1", "true", "yes":
		tr.Cached = true
	}
	tr.Counts, tr.Times = tally(spans)
	tr.NPlusOne = nPlusOne(spans)
	return tr, true
}

// urlResource reports a resource the tracer built from the request line,
// "GET /women.html", because no integration named the route.
func urlResource(r string) bool {
	method, path, ok := strings.Cut(r, " ")
	if !ok || method == "" || !strings.HasPrefix(path, "/") {
		return false
	}
	for _, c := range method {
		if c < 'A' || c > 'Z' {
			return false
		}
	}
	return true
}

func pageKey(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return ""
	}
	return strings.ToLower(u.Hostname()) + u.EscapedPath()
}

func first(a map[string]string, keys ...string) string {
	for _, k := range keys {
		if v := a[k]; v != "" {
			return v
		}
	}
	return ""
}

func rootIndex(spans []Span) int {
	ids := make(map[string]bool, len(spans))
	for _, s := range spans {
		ids[s.SpanID] = true
	}
	orphan := -1
	for i, s := range spans {
		if s.ParentID == "" {
			return i
		}
		if orphan < 0 && !ids[s.ParentID] {
			orphan = i
		}
	}
	if orphan < 0 {
		return 0
	}
	return orphan
}

func dropProfilerUploads(spans []Span) []Span {
	parentOf := map[string]string{}
	for _, s := range spans {
		if profilerUpload(s) {
			parentOf[s.SpanID] = s.ParentID
		}
	}
	if len(parentOf) == 0 {
		return spans
	}
	out := spans[:0]
	for _, s := range spans {
		if _, drop := parentOf[s.SpanID]; drop {
			continue
		}
		for depth := 0; depth < len(parentOf); depth++ {
			p, dropped := parentOf[s.ParentID]
			if !dropped {
				break
			}
			s.ParentID = p
		}
		out = append(out, s)
	}
	return out
}

func tally(spans []Span) (map[string]int, map[string]time.Duration) {
	counts := make(map[string]int, len(countedKinds))
	for _, k := range countedKinds {
		counts[k] = 0
	}
	self, _ := selfTimes(spans)
	times := map[string]time.Duration{}
	for i, s := range spans {
		if counted(s) {
			counts[s.Component]++
		}
		if self[i] > 0 {
			times[s.Component] += self[i]
		}
	}
	return counts, times
}
