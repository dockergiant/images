package api

import (
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/dockergiant/images/inspector/internal/apm"
	"github.com/dockergiant/images/inspector/internal/core"
	"github.com/dockergiant/images/inspector/internal/timeline"
)

const (
	maxV1Spans    = 500
	pageItems     = 5
	pageSpanIDs   = 50
	maxXIDs       = 4
	sinceSlack    = 2 * time.Second
	defaultV1Reqs = 100
)

// postReads are adapter routes that read but take a JSON body, so v1 offers
// them as GET with ?body=. Only these: a POST route is a write unless listed,
// which keeps the RabbitMQ peek (it requeues) off v1.
var postReads = map[core.Kind]map[string]bool{
	core.Search: {"/search": true, "/analyze": true},
}

type v1Param struct {
	Name        string
	In          string
	Description string
	Type        string
	Required    bool
}

type v1Route struct {
	Path    string
	Tag     string
	Summary string
	Params  []v1Param
	Returns string
	Stream  bool
	Handler http.HandlerFunc
}

func qparam(name, typ, desc string) v1Param {
	return v1Param{Name: name, In: "query", Type: typ, Description: desc}
}

var pathParamRe = regexp.MustCompile(`\{([A-Za-z_][A-Za-z0-9_]*)\$?\}`)

// v1Table is the single list of v1 routes: the mux, the index and the
// OpenAPI document are all built from it.
func (s *Server) v1Table() []v1Route {
	rs := []v1Route{
		{Path: "/", Tag: "inspector", Summary: "Which env this Inspector serves, and every v1 endpoint", Returns: "{env, domain, version, inspectorUrl, api: 1, endpoints: [{method, path, summary}], openapi, userscript, extension}"},
		{Path: "/openapi.json", Tag: "inspector", Summary: "This API as an OpenAPI 3.1 document", Returns: "OpenAPI 3.1 document"},
		{Path: "/overview", Tag: "inspector", Summary: "Service health tones, headlines and warnings with their copyable fixes", Returns: "{adapters: [{kind, info, health}], magento, warnings: [{id, text, fix, tone}], captures}", Handler: s.handleOverview},
		{Path: "/page", Tag: "requests", Summary: "The latest request for a URL (or exactly that Varnish xid): Varnish result and why, lanes, trace summary with cost times, top hot items and repeats, deep links", Returns: "{match: xid|url, handling, why, request, trace, hot, repeats, repeatCount, profile, links: {trace, bottlenecks, request}}; durations in request and trace are nanoseconds, analysis times are ms", Handler: s.handleV1Page, Params: []v1Param{
			qparam("url", "string", "Absolute URL of the page, as location.href gives it; the fragment is ignored. Required unless xid is set."),
			qparam("xid", "string", "The X-Varnish response header, or one of its ids. Wins over url; when no row carries it, the url decides and match says so."),
			qparam("since", "string", "Only requests that started after this time, as unix milliseconds or RFC 3339, minus 2 s for clock drift."),
		}},
		{Path: "/requests", Tag: "requests", Summary: "Recent timeline entries, newest first", Returns: "[timeline request]", Handler: s.handleV1Requests, Params: []v1Param{
			qparam("url", "string", "Only this URL: absolute, or a path with its query string."),
			qparam("xid", "string", "Only rows carrying one of these Varnish ids (X-Varnish)."),
			qparam("since", "string", "Only requests that started after this time (unix ms or RFC 3339)."),
			qparam("limit", "integer", "At most this many rows; default 100, max 500."),
		}},
		{Path: "/requests/{key}", Tag: "requests", Summary: "One timeline entry by its key", Returns: "timeline request", Handler: s.handleTimelineGet},
		{Path: "/traces", Tag: "traces", Summary: "Trace summaries with their cost times", Returns: "[{id, start, duration (ns), method, url, route, status, counts, nPlusOne, hasProfile, times: {sql, cache, search, http, php} (ms)}]", Handler: s.handleV1Traces, Params: []v1Param{
			qparam("route", "string", "Only this route, such as catalog/category/view."),
			qparam("url", "string", "Only this URL: absolute, or a path with its query string."),
			qparam("min_ms", "number", "Only traces that took at least this many milliseconds."),
			qparam("sort", "string", "slowest (default) or recent."),
			qparam("limit", "integer", "At most this many traces; default 100, max 500."),
		}},
		{Path: "/traces/metrics", Tag: "traces", Summary: "PHP request rate, latency and cost series over a window", Returns: "{step, series, latest, ...}", Handler: s.handleTraceMetrics, Params: []v1Param{qparam("window", "string", "5m, 15m or 60m.")}},
		{Path: "/traces/{id}", Tag: "traces", Summary: "One trace: its summary plus the bottleneck analysis", Returns: "{summary fields, varnishXid, spanCount, analysis, links, spans?}", Handler: s.handleV1Trace, Params: []v1Param{qparam("spans", "integer", "1 adds the spans, capped at 500 by self time and kept in trace order.")}},
		{Path: "/traces/{id}/analysis", Tag: "traces", Summary: "The bottleneck analysis of one trace", Returns: "{id, duration, unit, categories, hot, repeats, slowest, profile} in ms", Handler: s.handleAnalysis},
		{Path: "/traces/{id}/profile", Tag: "traces", Summary: "The Excimer profile of one trace, as speedscope JSON", Returns: "speedscope JSON", Handler: s.handleProfile},
		{Path: "/hotspots", Tag: "traces", Summary: "Routes, the most expensive operations and repeated calls across every stored trace", Returns: "{traces, since, routes, operations, repeats}", Handler: s.handleHotspots, Params: []v1Param{qparam("limit", "integer", "Rows per list; default 100, max 500.")}},
		{Path: "/performance", Tag: "inspector", Summary: "What Inspector costs right now: tracing state per php container, intake, sampler tick times, its own CPU and memory, and the last benchmark. Read-only; the switches stay on /api.", Returns: "{php, summary, sampler, inspector, intake, load, benchmark, defaultUrl, live}", Handler: s.handlePerformance},
		{Path: "/stream", Tag: "live", Summary: "Server-sent events for a live panel; the same subs as the UI except the opt-in captures, which change the service", Returns: "text/event-stream", Stream: true, Params: []v1Param{
			{Name: "sub", In: "query", Type: "string", Required: true, Description: "Repeatable: timeline, php:traces or <kind>:<stream>[;key=value…], at most 16. Filters: redis:monitor db and match, varnish:log q and url, search:insights and search:slowlog index and q; other streams take none. A stream runs at most 2 filtered variants started from v1 (429 beyond that), and one stopped on the Performance page stays off for v1 for 10 minutes (409). HEAD checks the subs without opening the stream."},
		}},
	}
	for _, a := range s.cfg.Adapters {
		kind := a.Kind()
		prefix := "/" + string(kind)
		rs = append(rs,
			v1Route{Path: prefix + "/info", Tag: string(kind), Summary: "What Inspector detected for " + string(kind), Returns: "{kind, present, engine, version, tested, features, container}", Handler: s.handleInfo(a)},
			v1Route{Path: prefix + "/health", Tag: string(kind), Summary: "Health of " + string(kind) + ": status, summary, cards and warnings", Returns: "{status, summary, cards, warnings}", Handler: s.handleHealth(a)},
		)
		for _, rt := range a.Browse() {
			r := v1Route{Path: prefix + rt.Path, Tag: string(kind), Returns: "the same JSON as /api/" + string(kind) + rt.Path}
			switch {
			case rt.Method == http.MethodGet:
				r.Handler = rt.Handler
				r.Summary = "GET /api/" + string(kind) + rt.Path + " mirrored; its query parameters pass through unchanged"
				for _, p := range rt.Params {
					r.Params = append(r.Params, v1Param{Name: p.Name, In: "query", Type: p.Type, Required: p.Required, Description: p.Description})
				}
			case rt.Method == http.MethodPost && postReads[kind][rt.Path]:
				r.Summary = "POST /api/" + string(kind) + rt.Path + " as a read: the JSON body goes in ?body="
				r.Handler = postAsGet(rt.Handler)
				r.Params = []v1Param{{Name: "body", In: "query", Type: "string", Required: true, Description: "The JSON body POST /api/" + string(kind) + rt.Path + " takes, URL-encoded."}}
			default:
				continue
			}
			rs = append(rs, r)
		}
	}
	for i := range rs {
		var path []v1Param
		for _, m := range pathParamRe.FindAllStringSubmatch(rs[i].Path, -1) {
			path = append(path, v1Param{Name: m[1], In: "path", Type: "string", Required: true})
		}
		rs[i].Params = append(path, rs[i].Params...)
	}
	return rs
}

// mountV1 registers every table route on mux. The index and the OpenAPI
// document read the same table, so neither can list a route the mux lacks.
func (s *Server) mountV1(mux *http.ServeMux, table []v1Route) {
	for _, rt := range table {
		switch {
		case rt.Stream:
			// routes() serves the stream outside the mux, as it does /api/stream.
		case rt.Path == "/":
			h := s.handleV1Index(table)
			mux.HandleFunc("GET "+v1Prefix+"/{$}", h)
			mux.HandleFunc("GET "+v1Prefix, h)
		case rt.Path == "/openapi.json":
			mux.HandleFunc("GET "+v1Prefix+rt.Path, s.handleOpenAPI(table))
		default:
			mux.HandleFunc("GET "+v1Prefix+rt.Path, rt.Handler)
		}
	}
	mux.HandleFunc("GET "+v1Prefix+"/", func(w http.ResponseWriter, r *http.Request) {
		writeHint(w, http.StatusNotFound, "no such v1 route: "+r.URL.Path, "GET "+v1Prefix+"/ lists every endpoint, "+v1Prefix+"/openapi.json describes them")
	})
}

func postAsGet(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		body := r.URL.Query().Get("body")
		if body == "" {
			writeHint(w, http.StatusBadRequest, "body is required", "pass the JSON body of the POST route as ?body=<url-encoded JSON>")
			return
		}
		r2 := r.Clone(r.Context())
		r2.Method = http.MethodPost
		r2.Body = io.NopCloser(strings.NewReader(body))
		r2.ContentLength = int64(len(body))
		r2.Header.Set("Content-Type", "application/json")
		h(w, r2)
	}
}

func (s *Server) inspectorURL() string {
	return s.origin
}

type v1Endpoint struct {
	Method  string `json:"method"`
	Path    string `json:"path"`
	Summary string `json:"summary"`
}

func (s *Server) handleV1Index(table []v1Route) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		eps := make([]v1Endpoint, len(table))
		for i, rt := range table {
			eps[i] = v1Endpoint{Method: http.MethodGet, Path: v1Prefix + rt.Path, Summary: rt.Summary}
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"env":          s.cfg.Env,
			"domain":       s.cfg.Domain,
			"version":      s.cfg.Version,
			"inspectorUrl": s.inspectorURL(),
			"api":          1,
			"endpoints":    eps,
			"openapi":      s.inspectorURL() + v1Prefix + "/openapi.json",
			"userscript":   s.inspectorURL() + userscriptPath,
			"extension":    s.inspectorURL() + extensionPath,
		})
	}
}

// xidTokens splits an X-Varnish value ("426144 1442055") into its ids.
func xidTokens(raw string) []string {
	f := strings.FieldsFunc(raw, func(r rune) bool { return r == ' ' || r == ',' || r == '\t' })
	if len(f) > maxXIDs {
		f = f[:maxXIDs]
	}
	return f
}

func parseSince(raw string) (time.Time, bool) {
	if raw == "" {
		return time.Time{}, true
	}
	if ms, err := strconv.ParseInt(raw, 10, 64); err == nil {
		return time.UnixMilli(ms).Add(-sinceSlack), true
	}
	if t, err := time.Parse(time.RFC3339Nano, raw); err == nil {
		return t.Add(-sinceSlack), true
	}
	return time.Time{}, false
}

type v1Links struct {
	Request     string `json:"request,omitempty"`
	Trace       string `json:"trace,omitempty"`
	Bottlenecks string `json:"bottlenecks,omitempty"`
}

func (s *Server) traceLinks(id string, l *v1Links) {
	if id == "" {
		return
	}
	l.Bottlenecks = s.inspectorURL() + "/#/traces/" + url.PathEscape(id)
	l.Trace = l.Bottlenecks + "/waterfall"
}

type v1Page struct {
	Match    string           `json:"match"`
	Handling string           `json:"handling"`
	Why      *core.Card       `json:"why"`
	Request  timeline.Request `json:"request"`
	Trace    *apm.Summary     `json:"trace"`
	Hot      []apm.HotItem    `json:"hot"`
	Repeats  []apm.Repeat     `json:"repeats"`
	// RepeatCount counts every repeat group; repeats holds the top five.
	RepeatCount int     `json:"repeatCount"`
	Profile     bool    `json:"profile"`
	Links       v1Links `json:"links"`
}

const pageHint = "Inspector sees a page once its PHP trace arrives (tracing on), or through varnishlog, which runs while the Overview is open and for 3 minutes after any " + v1Prefix + "/page or " + v1Prefix + "/requests call. The trace lands just after the page, and a HIT served before that lease started stays unknown, so retry with a short backoff."

func (s *Server) handleV1Page(w http.ResponseWriter, r *http.Request) {
	s.touchLease()
	q := r.URL.Query()
	raw, xids := q.Get("url"), xidTokens(q.Get("xid"))
	if raw == "" && len(xids) == 0 {
		writeHint(w, http.StatusBadRequest, "url or xid is required", "pass ?url=<absolute page URL>, and ?xid=<X-Varnish> when you have the response header")
		return
	}
	since, ok := parseSince(q.Get("since"))
	if !ok {
		writeHint(w, http.StatusBadRequest, "since must be unix milliseconds or an RFC 3339 time", "Date.now() in the browser gives unix milliseconds")
		return
	}
	if s.cfg.Timeline == nil {
		writeHint(w, http.StatusNotFound, "this Inspector keeps no request timeline", pageHint)
		return
	}
	page := v1Page{Hot: []apm.HotItem{}, Repeats: []apm.Repeat{}}
	var found []timeline.Request
	if len(xids) > 0 {
		found = s.cfg.Timeline.Find(timeline.Query{XIDs: xids, Since: since})
		page.Match = "xid"
	}
	if len(found) == 0 && raw != "" {
		found = s.cfg.Timeline.Find(timeline.Query{URL: raw, Since: since, Limit: 1})
		page.Match = "url"
	}
	if len(found) == 0 {
		writeHint(w, http.StatusNotFound, "no request for this page in the timeline yet", pageHint)
		return
	}
	// X-Varnish names the client request first; on a HIT the second id is
	// the fetch that stored the object, an older request.
	req := found[0]
	for _, f := range found {
		if len(xids) > 0 && f.VXID == xids[0] {
			req = f
			break
		}
	}
	page.Request = req
	page.Handling = strings.ToUpper(req.Handling)
	for i := range req.Answers {
		if strings.HasPrefix(req.Answers[i].Label, "Why ") {
			page.Why = &req.Answers[i]
			break
		}
	}
	page.Links.Request = s.inspectorURL() + "/#/overview?req=" + url.QueryEscape(req.Key)
	if req.TraceID != "" && s.cfg.Store != nil {
		if tr, ok := s.cfg.Store.Get(req.TraceID); ok {
			sum := apm.Summarize(tr)
			page.Trace = &sum
			s.traceLinks(tr.ID, &page.Links)
		}
		if a, ok := s.cfg.Store.Analysis(req.TraceID); ok {
			page.Profile = a.Profile
			page.RepeatCount = len(a.Repeats)
			for _, h := range a.Hot[:min(pageItems, len(a.Hot))] {
				h.SpanIDs = h.SpanIDs[:min(pageSpanIDs, len(h.SpanIDs))]
				page.Hot = append(page.Hot, h)
			}
			for _, rp := range a.Repeats[:min(pageItems, len(a.Repeats))] {
				rp.SpanIDs = rp.SpanIDs[:min(pageSpanIDs, len(rp.SpanIDs))]
				page.Repeats = append(page.Repeats, rp)
			}
		}
	}
	writeJSON(w, http.StatusOK, page)
}

func (s *Server) handleV1Requests(w http.ResponseWriter, r *http.Request) {
	s.touchLease()
	q := r.URL.Query()
	since, ok := parseSince(q.Get("since"))
	if !ok {
		writeHint(w, http.StatusBadRequest, "since must be unix milliseconds or an RFC 3339 time", "Date.now() in the browser gives unix milliseconds")
		return
	}
	if s.cfg.Timeline == nil {
		writeJSON(w, http.StatusOK, []any{})
		return
	}
	list := s.cfg.Timeline.Find(timeline.Query{URL: q.Get("url"), XIDs: xidTokens(q.Get("xid")), Since: since, Limit: intParam(r, "limit", defaultV1Reqs, maxTimeline)})
	if list == nil {
		list = []timeline.Request{}
	}
	writeJSON(w, http.StatusOK, list)
}

func (s *Server) handleV1Traces(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	limit := intParam(r, "limit", defaultTraces, maxHotspots)
	f := apm.ListFilter{Route: q.Get("route"), Sort: q.Get("sort")}
	if f.Sort == "slowest" {
		f.Sort = ""
	}
	if v := q.Get("min_ms"); v != "" {
		ms, err := strconv.ParseFloat(v, 64)
		if err != nil || ms < 0 {
			writeHint(w, http.StatusBadRequest, "min_ms must be a number of milliseconds", "for example ?min_ms=500")
			return
		}
		f.MinDuration = time.Duration(ms * float64(time.Millisecond))
	}
	want := q.Get("url")
	if want == "" {
		f.Limit = limit
	}
	out := []apm.Summary{}
	if s.cfg.Store != nil {
		for _, sum := range s.cfg.Store.List(f) {
			if want != "" && !sameURL(sum.URL, want) {
				continue
			}
			out = append(out, sum)
			if len(out) == limit {
				break
			}
		}
	}
	writeJSON(w, http.StatusOK, out)
}

// sameURL compares a trace's absolute URL with what a client passed: an
// absolute URL matches host and request URI, a path only the request URI.
func sameURL(traceURL, want string) bool {
	tu, err := url.Parse(traceURL)
	if err != nil {
		return traceURL == want
	}
	wu, err := url.Parse(want)
	if err != nil {
		return false
	}
	if tu.RequestURI() != wu.RequestURI() {
		return false
	}
	return wu.Host == "" || tu.Host == "" || strings.EqualFold(tu.Hostname(), wu.Hostname())
}

type v1Trace struct {
	apm.Summary
	VarnishXID string       `json:"varnishXid"`
	SpanCount  int          `json:"spanCount"`
	Analysis   apm.Analysis `json:"analysis"`
	Links      v1Links      `json:"links"`
	Spans      []apm.Span   `json:"spans,omitempty"`
}

func (s *Server) handleV1Trace(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var tr apm.Trace
	ok := false
	if s.cfg.Store != nil {
		tr, ok = s.cfg.Store.Get(id)
	}
	if !ok {
		writeHint(w, http.StatusNotFound, "trace not found", "the store keeps the last 500 traces; GET "+v1Prefix+"/traces lists them")
		return
	}
	out := v1Trace{Summary: apm.Summarize(tr), VarnishXID: tr.VarnishXID, SpanCount: len(tr.Spans)}
	out.Analysis, _ = s.cfg.Store.Analysis(id)
	s.traceLinks(tr.ID, &out.Links)
	if r.URL.Query().Get("spans") == "1" {
		out.Spans = apm.TopSpans(tr, maxV1Spans)
	}
	writeJSON(w, http.StatusOK, out)
}
