package timeline

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/dockergiant/images/inspector/internal/apm"
	"github.com/dockergiant/images/inspector/internal/core"
)

const (
	joinExact    = "exact"
	joinProbable = "probable"
	joinNone     = "none"
)

type span struct {
	from, to time.Time
}

func (s span) overlaps(o span) bool {
	return !s.to.Before(o.from) && !o.to.Before(s.from)
}

func (s span) frac(at time.Time) float64 {
	d := s.to.Sub(s.from)
	if d <= 0 {
		return 0
	}
	f := float64(at.Sub(s.from)) / float64(d)
	switch {
	case f < 0:
		return 0
	case f > 1:
		return 1
	}
	return f
}

func (s span) pair(from, to time.Time) [2]float64 {
	a, b := s.frac(from), s.frac(to)
	if b < a {
		b = a
	}
	return [2]float64{a, b}
}

// window is the stretch in which the backend worked on the request, widened by
// the 50 ms slack the spec allows for clock and logging jitter.
func (e *entry) window() (span, bool) {
	switch {
	case e.varnish != nil:
		v := e.varnish
		if v.BereqVXID == "" {
			return span{}, false
		}
		from, to := v.BereqStart, v.BereqEnd
		if from.IsZero() {
			from, to = v.Start, v.Start.Add(v.Duration)
		}
		if to.Before(from) {
			to = from
		}
		return span{from.Add(-slack), to.Add(slack)}, true
	case e.trace != nil:
		return span{e.trace.Start.Add(-slack), e.trace.Start.Add(e.trace.Duration).Add(slack)}, true
	case e.access != nil:
		return span{e.access.Time.Add(-accessLookback), e.access.Time.Add(slack)}, true
	}
	return span{}, false
}

func (e *entry) busy() (span, bool) {
	w, ok := e.window()
	if !ok {
		return span{}, false
	}
	if e.access != nil {
		return span{w.from, w.to.Add(-slack)}, true
	}
	return span{w.from.Add(slack), w.to.Add(-slack)}, true
}

// Static files reach nginx without PHP, so they never compete for a worker.
func (e *entry) static() bool {
	p := e.req.URL
	return strings.HasPrefix(p, "/static/") || strings.HasPrefix(p, "/media/")
}

func (e *entry) windowText() string {
	switch {
	case e.varnish != nil:
		return "between backend start and end ±50 ms"
	case e.trace != nil:
		return "in the trace window ±50 ms"
	}
	return "in the second before the access line"
}

func (e *entry) scale() span {
	switch {
	case e.varnish != nil:
		v := e.varnish
		end := v.Start.Add(v.Duration)
		if v.BereqEnd.After(end) {
			end = v.BereqEnd
		}
		return span{v.Start, end}
	case e.trace != nil:
		return span{e.trace.Start, e.trace.Start.Add(e.trace.Duration)}
	case e.access != nil:
		return span{e.access.Time.Add(-accessLookback), e.access.Time}
	}
	return span{}
}

type resolvedCmd struct {
	cmd       core.RedisCmd
	pid       int
	container string
}

type joined struct {
	window   span
	scale    span
	overlap  int
	cmds     []resolvedCmd
	workers  []int
	unmapped int
	worker   int
	byTrace  bool

	cache       []resolvedCmd
	cacheSpans  compInfo
	queries     []core.SearchQuery
	searchSpans compInfo
	cacheJoin   string
}

func (t *Timeline) build(e *entry) {
	sc := e.scale()
	lanes := []Lane{}
	if e.varnish != nil {
		lanes = append(lanes, varnishLane(e, sc))
	}
	j := &joined{scale: sc}
	if w, ok := e.window(); ok {
		j.window = w
		t.collect(e, j)
		lanes = append(lanes, t.phpLane(e, j))
		if l, ok := t.redisLane(e, j); ok {
			lanes = append(lanes, l)
		}
		if l, ok := t.searchLane(e, j); ok {
			lanes = append(lanes, l)
		}
		if l, ok := t.mqLane(e, j); ok {
			lanes = append(lanes, l)
		}
	}
	e.req.Lanes = lanes
	e.req.Answers = t.answers(e, j)
}

func (t *Timeline) collect(e *entry, j *joined) {
	if mine, ok := e.busy(); ok {
		for _, other := range t.entries {
			if other == e || other.static() {
				continue
			}
			if ob, ok := other.busy(); ok && ob.overlaps(mine) {
				j.overlap++
			}
		}
	}

	seen := map[int]bool{}
	for _, c := range t.redis.between(j.window.from, j.window.to) {
		rc, ok := t.resolve(c)
		if !ok {
			continue
		}
		j.cmds = append(j.cmds, rc)
		if rc.pid == 0 {
			j.unmapped++
			continue
		}
		if !seen[rc.pid] {
			seen[rc.pid] = true
			j.workers = append(j.workers, rc.pid)
		}
	}
	sort.Ints(j.workers)

	if e.trace != nil && e.trace.PID > 0 {
		j.worker, j.byTrace = e.trace.PID, true
	} else if j.overlap == 0 && len(j.workers) == 1 {
		j.worker = j.workers[0]
	}
}

func (t *Timeline) resolve(c core.RedisCmd) (resolvedCmd, bool) {
	if c.Addr == "lua" {
		return resolvedCmd{}, false
	}
	rc := resolvedCmd{cmd: c, pid: c.PID, container: c.Container}
	role := ""
	if t.proc != nil && c.IP != "" {
		if p, ok := t.proc.Lookup(c.IP, c.Port); ok {
			rc.pid, rc.container, role = p.PID, p.Container, p.Role
		} else if rc.container == "" {
			if svc, ok := t.proc.ContainerOf(c.IP); ok {
				rc.container = svc
			}
		}
	}
	if rc.container != "" && rc.container != "php-fpm" && rc.container != "php-debug" {
		return resolvedCmd{}, false
	}
	switch role {
	case "fpm-master", "consumer", "cron", "cli":
		return resolvedCmd{}, false
	}
	return rc, true
}

func tracePID(tr *apm.Trace) int {
	root := rootSpan(tr)
	if root == nil {
		return 0
	}
	for _, k := range []string{"process_id", "pid"} {
		if v, ok := root.Attrs[k]; ok {
			if f, err := strconv.ParseFloat(strings.TrimSpace(v), 64); err == nil && f > 0 {
				return int(f)
			}
		}
	}
	return 0
}

func rootSpan(tr *apm.Trace) *apm.Span {
	if tr == nil || len(tr.Spans) == 0 {
		return nil
	}
	for i := range tr.Spans {
		if p := tr.Spans[i].ParentID; p == "" || p == "0" {
			return &tr.Spans[i]
		}
	}
	first := &tr.Spans[0]
	for i := range tr.Spans {
		if tr.Spans[i].Start.Before(first.Start) {
			first = &tr.Spans[i]
		}
	}
	return first
}

func component(s apm.Span) string {
	if c := strings.ToLower(s.Component); c != "" {
		return c
	}
	switch strings.ToLower(s.Type) {
	case "redis", "memcached", "cache":
		return "cache"
	case "elasticsearch", "opensearch", "search":
		return "search"
	case "queue", "amqp", "rabbitmq":
		return "queue"
	case "sql", "db", "mysql":
		return "sql"
	}
	return ""
}

func traceComp(tr *traceInfo, comp string) compInfo {
	if tr == nil {
		return compInfo{}
	}
	return tr.Comp[comp]
}

func varnishLane(e *entry, sc span) Lane {
	v := e.varnish
	l := Lane{
		Kind:    core.Varnish,
		Join:    joinExact,
		Reason:  "varnishlog request group " + v.VXID,
		Count:   1,
		Summary: varnishSummary(v),
	}
	switch {
	case v.BereqVXID == "":
		l.Spans = [][2]float64{{0, 0}}
	case v.Grace:
		hit := sc.frac(v.Start.Add(v.Duration))
		l.Spans = [][2]float64{{0, hit}, sc.pair(v.BereqStart, v.BereqEnd)}
		l.Reason += ", background fetch " + v.BereqVXID
	default:
		l.Spans = [][2]float64{{0, 1}}
		l.Reason += ", backend request " + v.BereqVXID
	}
	return l
}

func varnishSummary(v *core.VarnishReq) string {
	parts := []string{}
	if v.Handling != "" {
		h := v.Handling
		if v.Grace {
			h += " (grace)"
		}
		parts = append(parts, h)
	}
	if v.Status != 0 {
		parts = append(parts, strconv.Itoa(v.Status))
	}
	parts = append(parts, fmtDur(v.Duration))
	return strings.Join(parts, " · ")
}

func (t *Timeline) phpLane(e *entry, j *joined) Lane {
	l := Lane{Kind: core.PHP, Spans: [][2]float64{}}
	if tr := e.trace; tr != nil {
		l.Join = joinExact
		if e.varnish != nil {
			l.Reason = fmt.Sprintf("varnish.xid %s on the root span matches backend request %s", tr.VarnishXID, e.varnish.BereqVXID)
		} else {
			l.Reason = "PHP trace " + tr.ID
		}
		l.Count = tr.SpanCount
		l.Spans = [][2]float64{j.scale.pair(tr.Start, tr.Start.Add(tr.Duration))}
		name := tr.Route
		if name == "" {
			name = e.req.URL
		}
		l.Summary = strings.Join([]string{name, fmtDur(tr.Duration), plural(tr.SpanCount, "span", "spans")}, " · ")
		return l
	}
	if j.worker > 0 {
		l.Join = joinProbable
		l.Reason = fmt.Sprintf("PID %d is the only php-fpm worker with Redis traffic %s", j.worker, e.windowText())
		l.Summary = fmt.Sprintf("PID %d", j.worker)
		var first, last time.Time
		for _, c := range j.cmds {
			if c.pid != j.worker {
				continue
			}
			if first.IsZero() || c.cmd.Time.Before(first) {
				first = c.cmd.Time
			}
			if c.cmd.Time.After(last) {
				last = c.cmd.Time
			}
		}
		if !first.IsZero() {
			l.Spans = [][2]float64{j.scale.pair(first, last)}
		}
		return l
	}
	l.Join = joinNone
	switch {
	case j.overlap > 0:
		l.Reason = fmt.Sprintf("%s overlap this window and no trace ties a worker to it", plural(j.overlap, "other request", "other requests"))
	case len(j.workers) > 1:
		l.Reason = fmt.Sprintf("%d php-fpm workers were busy in the window", len(j.workers))
	default:
		l.Reason = "no trace and no mapped worker in the window; roll inspector trace on gives exact joins"
	}
	return l
}

func (t *Timeline) redisLane(e *entry, j *joined) (Lane, bool) {
	spans := traceComp(e.trace, "cache")
	if spans.Count > 0 {
		l := Lane{Kind: core.Redis, Join: joinExact, Source: SourceTrace}
		l.Reason = plural(spans.Count, "cache span", "cache spans") + " in the trace"
		j.cacheSpans = spans
		l.Count = spans.Count
		l.Spans = coalesce(j.scale, spans.Ivals)
		l.Summary = fmt.Sprintf("%s · %s", plural(spans.Count, "call", "calls"), fmtDur(spans.Total))
		j.cacheJoin = l.Join
		return l, true
	}
	if t.cacheSource == SourceTrace && len(t.redis.items) == 0 {
		l := Lane{Kind: core.Redis, Spans: [][2]float64{}, Source: SourceTrace}
		if e.trace != nil {
			l.Join = joinExact
			l.Reason = "the trace has no cache spans"
			l.Summary = "no calls"
		} else {
			l.Join = joinNone
			l.Reason = "no trace for this request; tracing is on, so Valkey calls come from trace spans and MONITOR stays off"
		}
		return l, true
	}
	if len(t.redis.items) == 0 {
		return Lane{}, false
	}
	l := Lane{Kind: core.Redis, Spans: [][2]float64{}, Source: SourceMonitor}

	var mine []resolvedCmd
	if j.worker > 0 {
		for _, c := range j.cmds {
			if c.pid == j.worker {
				mine = append(mine, c)
			}
		}
	}
	switch {
	case len(mine) > 0:
		l.Join = joinExact
		if j.byTrace {
			l.Reason = fmt.Sprintf("connections mapped to PID %d, the worker in the trace", j.worker)
		} else {
			l.Reason = fmt.Sprintf("connections mapped to PID %d, the only worker busy in the window", j.worker)
		}
		j.cache = mine
	case len(j.cmds) == 0:
		l.Join = joinNone
		l.Reason = "no commands from PHP in the request window"
		return l, true
	case j.overlap > 0:
		l.Join = joinNone
		l.Reason = fmt.Sprintf("%s overlap this window and the PID map cannot tell their workers apart", plural(j.overlap, "other request", "other requests"))
		return l, true
	case len(j.workers) > 1:
		l.Join = joinNone
		l.Reason = fmt.Sprintf("%d php-fpm workers were busy in the window", len(j.workers))
		return l, true
	default:
		l.Join = joinProbable
		l.Reason = fmt.Sprintf("commands from %s %s; the connection is not in the PID map", phpSource(j.cmds), e.windowText())
		j.cache = j.cmds
	}
	j.cacheJoin = l.Join
	l.Count = cmdCount(j.cache)
	points := make([][2]time.Time, len(j.cache))
	for i, c := range j.cache {
		points[i] = [2]time.Time{c.cmd.Time, c.cmd.Time}
	}
	l.Spans = coalesce(j.scale, points)
	reads, writes, _, _ := cacheStats(j.cache)
	l.Summary = fmt.Sprintf("%s · %s", plural(reads, "read", "reads"), plural(writes, "write", "writes"))
	return l, true
}

func phpSource(cmds []resolvedCmd) string {
	for _, c := range cmds {
		if c.container != "" {
			if c.cmd.IP != "" {
				return fmt.Sprintf("%s (%s)", c.container, c.cmd.IP)
			}
			return c.container
		}
	}
	if len(cmds) > 0 && cmds[0].cmd.IP != "" {
		return cmds[0].cmd.IP
	}
	return "PHP"
}

func cmdCount(cmds []resolvedCmd) int {
	n := 0
	for _, c := range cmds {
		n += max(1, c.cmd.Folded)
	}
	return n
}

func (t *Timeline) searchLane(e *entry, j *joined) (Lane, bool) {
	spans := traceComp(e.trace, "search")
	if len(t.search.items) == 0 && spans.Count == 0 {
		return Lane{}, false
	}
	l := Lane{Kind: core.Search, Spans: [][2]float64{}}
	if spans.Count > 0 {
		l.Join = joinExact
		l.Reason = plural(spans.Count, "search span", "search spans") + " in the trace"
		l.Count = spans.Count
		l.Spans = coalesce(j.scale, spans.Ivals)
		l.Summary = fmt.Sprintf("%s · %s", plural(spans.Count, "call", "calls"), fmtDur(spans.Total))
		j.searchSpans = spans
		return l, true
	}
	qs := t.search.between(j.window.from, j.window.to)
	if len(qs) == 0 {
		l.Join = joinNone
		l.Reason = "no queries started in the request window"
		return l, true
	}
	j.queries = append([]core.SearchQuery(nil), qs...)
	l.Join = joinProbable
	l.Reason = "query start (log time minus took) " + e.windowText()
	if j.overlap > 0 {
		l.Reason += fmt.Sprintf("; %s overlap it", plural(j.overlap, "other request", "other requests"))
	}
	l.Count = len(qs)
	pairs := make([][2]time.Time, len(qs))
	for i, q := range qs {
		pairs[i] = [2]time.Time{q.Start, q.Start.Add(q.Took)}
	}
	l.Spans = coalesce(j.scale, pairs)
	l.Summary = fmt.Sprintf("%s · %s", plural(len(qs), "query", "queries"), strings.Join(indexLabels(qs), ", "))
	return l, true
}

func indexLabels(qs []core.SearchQuery) []string {
	var out []string
	seen := map[string]bool{}
	for _, q := range qs {
		name := q.Label
		if name == "" {
			name = q.Index
		}
		if name == "" || seen[name] {
			continue
		}
		seen[name] = true
		out = append(out, name)
	}
	return out
}

func (t *Timeline) mqLane(e *entry, j *joined) (Lane, bool) {
	spans := traceComp(e.trace, "queue")
	if len(t.mq.items) == 0 && spans.Count == 0 {
		return Lane{}, false
	}
	l := Lane{Kind: core.RabbitMQ, Spans: [][2]float64{}}
	if spans.Count > 0 {
		l.Join = joinExact
		l.Reason = plural(spans.Count, "AMQP span", "AMQP spans") + " in the trace"
		l.Count = spans.Count
		l.Spans = coalesce(j.scale, spans.Ivals)
		l.Summary = plural(spans.Count, "call", "calls")
		return l, true
	}

	var mine, fire, delta []core.MQPub
	for _, p := range t.mq.between(j.window.from, j.window.to.Add(mqSampleDelay)) {
		if p.Source == "delta" {
			delta = append(delta, p)
			continue
		}
		if p.Time.After(j.window.to) {
			continue
		}
		pid, ok := t.publisher(p)
		if !ok {
			continue
		}
		if j.worker > 0 && pid == j.worker {
			mine = append(mine, p)
		}
		fire = append(fire, p)
	}

	var pubs []core.MQPub
	switch {
	case len(mine) > 0:
		l.Join = joinExact
		l.Reason = fmt.Sprintf("publishes on the AMQP connection of PID %d", j.worker)
		pubs = mine
	case len(fire) > 0:
		l.Join = joinProbable
		l.Reason = "firehose publishes " + e.windowText()
		if j.overlap > 0 {
			l.Reason += fmt.Sprintf("; %s overlap it", plural(j.overlap, "other request", "other requests"))
		}
		pubs = fire
	case len(delta) > 0:
		l.Join = joinProbable
		l.Reason = "per-queue publish deltas, which RabbitMQ samples every 5 s; switch on the firehose for exact publishes"
		pubs = delta
	default:
		l.Join = joinNone
		l.Reason = "no publishes in the request window"
		return l, true
	}
	n := 0
	var points [][2]time.Time
	for _, p := range pubs {
		n += max(1, p.Count)
		if p.Source != "delta" {
			points = append(points, [2]time.Time{p.Time, p.Time})
		}
	}
	l.Count = n
	l.Spans = coalesce(j.scale, points)
	l.Summary = fmt.Sprintf("%s · %s", plural(n, "publish", "publishes"), strings.Join(routingKeys(pubs), ", "))
	return l, true
}

func (t *Timeline) publisher(p core.MQPub) (int, bool) {
	if t.proc == nil || p.ConnIP == "" {
		return p.PID, true
	}
	if proc, ok := t.proc.Lookup(p.ConnIP, p.ConnPort); ok {
		switch proc.Role {
		case "consumer", "cron", "cli", "fpm-master":
			return 0, false
		}
		return proc.PID, true
	}
	if svc, ok := t.proc.ContainerOf(p.ConnIP); ok && svc != "php-fpm" && svc != "php-debug" {
		return 0, false
	}
	return p.PID, true
}

func routingKeys(pubs []core.MQPub) []string {
	var out []string
	seen := map[string]bool{}
	for _, p := range pubs {
		k := p.RoutingKey
		if k == "" && len(p.Queues) > 0 {
			k = p.Queues[0]
		}
		if k == "" || seen[k] {
			continue
		}
		seen[k] = true
		out = append(out, k)
	}
	return out
}

const coalesceGap = 0.005

func coalesce(sc span, pairs [][2]time.Time) [][2]float64 {
	in := make([][2]float64, len(pairs))
	for i, p := range pairs {
		in[i] = sc.pair(p[0], p[1])
	}
	sort.Slice(in, func(a, b int) bool { return in[a][0] < in[b][0] })
	for gap := coalesceGap; ; gap *= 2 {
		out := merge(in, gap)
		if len(out) <= maxSpans {
			return out
		}
	}
}

func merge(in [][2]float64, gap float64) [][2]float64 {
	out := [][2]float64{}
	for _, p := range in {
		if n := len(out); n > 0 && p[0]-out[n-1][1] <= gap {
			out[n-1][1] = max(out[n-1][1], p[1])
			continue
		}
		out = append(out, p)
	}
	return out
}
