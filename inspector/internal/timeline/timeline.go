package timeline

import (
	"fmt"
	"net/url"
	"reflect"
	"regexp"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/dockergiant/images/inspector/internal/apm"
	"github.com/dockergiant/images/inspector/internal/core"
	"github.com/dockergiant/images/inspector/internal/procmap"
)

const (
	sideKeep      = 30 * time.Second
	banKeep       = 10 * time.Minute
	parkFor       = 5 * time.Second
	maxRequests   = 500
	maxSide       = 50000
	maxSpans      = 64
	slack         = 50 * time.Millisecond
	mqSampleDelay = 5 * time.Second
	subBuffer     = 256
)

// The nginx main log format has no $request_time: an access line marks the end
// of a request, so the window reaches back this far before it.
const accessLookback = time.Second

type Lane struct {
	Kind    core.Kind    `json:"kind"`
	Join    string       `json:"join"`
	Reason  string       `json:"reason"`
	Count   int          `json:"count"`
	Spans   [][2]float64 `json:"spans"`
	Summary string       `json:"summary"`
	// Source says where the Valkey lane's calls come from: the trace's cache
	// spans or Valkey MONITOR.
	Source string `json:"source,omitempty"`
}

// The Valkey lane's sources. MONITOR slows every command on the shop's
// Valkey, so it only runs while no trace can stand in for it.
const (
	SourceTrace   = "trace"
	SourceMonitor = "monitor"
)

type Request struct {
	Key      string        `json:"key"`
	Method   string        `json:"method"`
	URL      string        `json:"url"`
	Host     string        `json:"host"`
	Status   int           `json:"status"`
	Handling string        `json:"handling"`
	Start    time.Time     `json:"start"`
	Duration time.Duration `json:"duration"`
	VXID     string        `json:"vxid,omitempty"`
	TraceID  string        `json:"traceId,omitempty"`
	Source   string        `json:"source"`
	Lanes    []Lane        `json:"lanes"`
	Answers  []core.Card   `json:"answers"`
	// Replaces names the rows this request absorbed, which were removed
	// without an event of their own.
	Replaces []string `json:"replaces,omitempty"`
}

type entry struct {
	seq     uint64
	req     Request
	varnish *core.VarnishReq
	trace   *traceInfo
	access  *core.NginxAccess
}

type ban struct {
	at      time.Time
	pattern string
	re      *regexp.Regexp
}

type parkedTrace struct {
	trace *traceInfo
	since time.Time
}

type Timeline struct {
	proc  *procmap.Map
	store *apm.Store
	now   func() time.Time

	mu          sync.Mutex
	entries     []*entry
	byKey       map[string]*entry
	byBereq     map[string]*entry
	byTrace     map[string]*entry
	aliases     map[string]string
	parked      []parkedTrace
	redis       ring[core.RedisCmd]
	search      ring[core.SearchQuery]
	mq          ring[core.MQPub]
	bans        []ban
	frontends   map[int]string
	cacheSource string
	lastVarnish time.Time
	lastTrace   time.Time
	seq         uint64
	subs        map[int]chan Request
	nextSub     int
}

func New(proc *procmap.Map, store *apm.Store) *Timeline {
	return &Timeline{
		proc:    proc,
		store:   store,
		now:     time.Now,
		byKey:   map[string]*entry{},
		byBereq: map[string]*entry{},
		byTrace: map[string]*entry{},
		aliases: map[string]string{},
		redis:   ring[core.RedisCmd]{at: func(c core.RedisCmd) time.Time { return c.Time }},
		search:  ring[core.SearchQuery]{at: func(q core.SearchQuery) time.Time { return q.Start }},
		mq:      ring[core.MQPub]{at: func(p core.MQPub) time.Time { return p.Time }},
		subs:    map[int]chan Request{},
	}
}

// SetFrontends names the Redis databases after their Magento cache frontends
// for the cache answer card; without it the card says "db <n>".
func (t *Timeline) SetFrontends(names map[int]string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.frontends = map[int]string{}
	for db, name := range names {
		t.frontends[db] = name
	}
}

// SetCacheSource says which source feeds the Valkey lane: SourceTrace while
// tracing is on and MONITOR stays off, SourceMonitor while MONITOR runs, or
// "" without Valkey. Requests already in the timeline keep their lanes.
func (t *Timeline) SetCacheSource(src string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.cacheSource = src
}

func (t *Timeline) CacheSource() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.cacheSource
}

func (t *Timeline) Ingest(ev core.Event) {
	t.mu.Lock()
	defer t.mu.Unlock()
	now := t.now()
	t.expire(now)

	switch ev.Type {
	case "varnish.req":
		if v, ok := as[core.VarnishReq](ev.Data); ok {
			t.addVarnish(v, now)
		}
	case "trace":
		if tr, ok := t.traceOf(ev.Data); ok {
			t.addTrace(slim(tr), now)
		}
	case "nginx.access":
		if a, ok := as[core.NginxAccess](ev.Data); ok {
			t.addAccess(a, now)
		}
	case "redis.cmd":
		if c, ok := as[core.RedisCmd](ev.Data); ok && !c.Time.Before(now.Add(-sideKeep)) {
			t.redis.add(c)
			t.touch(c.Time, 0)
		}
	case "search.query":
		if q, ok := as[core.SearchQuery](ev.Data); ok {
			if q.Start.IsZero() {
				q.Start = q.Time.Add(-q.Took)
			}
			if !q.Start.Before(now.Add(-sideKeep)) {
				t.search.add(q)
				t.touch(q.Start, 0)
			}
		}
	case "mq.pub":
		if p, ok := as[core.MQPub](ev.Data); ok && !p.Time.Before(now.Add(-sideKeep)) {
			t.mq.add(p)
			delay := time.Duration(0)
			if p.Source == "delta" {
				delay = mqSampleDelay
			}
			t.touch(p.Time, delay)
		}
	}
}

func as[T any](v any) (T, bool) {
	switch x := v.(type) {
	case T:
		return x, true
	case *T:
		if x != nil {
			return *x, true
		}
	}
	var zero T
	return zero, false
}

func (t *Timeline) traceOf(v any) (apm.Trace, bool) {
	if tr, ok := as[apm.Trace](v); ok {
		return tr, tr.ID != ""
	}
	if s, ok := as[apm.Summary](v); ok && t.store != nil {
		return t.store.Get(s.ID)
	}
	return apm.Trace{}, false
}

func (t *Timeline) expire(now time.Time) {
	t.redis.prune(now.Add(-sideKeep))
	t.search.prune(now.Add(-sideKeep))
	t.mq.prune(now.Add(-sideKeep))

	cut := 0
	for cut < len(t.bans) && t.bans[cut].at.Before(now.Add(-banKeep)) {
		cut++
	}
	t.bans = t.bans[cut:]

	var keep []parkedTrace
	for _, p := range t.parked {
		if now.Sub(p.since) < parkFor {
			keep = append(keep, p)
			continue
		}
		t.refresh(t.newTraceEntry(p.trace), true)
	}
	t.parked = keep
}

func (t *Timeline) addVarnish(v core.VarnishReq, now time.Time) {
	t.lastVarnish = now
	if strings.EqualFold(v.Method, "PURGE") || strings.EqualFold(v.Method, "BAN") {
		t.addBans(v)
		return
	}
	if v.VXID == "" {
		return
	}
	key := "v:" + v.VXID
	e := t.byKey[key]
	if e == nil {
		e = t.insert(key)
	} else if e.varnish != nil && t.byBereq[e.varnish.BereqVXID] == e {
		delete(t.byBereq, e.varnish.BereqVXID)
	}
	e.varnish = &v
	if v.BereqVXID != "" {
		t.byBereq[v.BereqVXID] = e
		t.adoptTrace(e, v.BereqVXID)
	}
	t.fill(e)
	t.absorbAccess(e)
	t.refresh(e, true)
	t.refreshNeighbours(e)
}

func (t *Timeline) adoptTrace(e *entry, bereq string) {
	if e.trace != nil {
		return
	}
	for i, p := range t.parked {
		if p.trace.VarnishXID == bereq {
			tr := p.trace
			e.trace = tr
			t.byTrace[tr.ID] = e
			t.parked = slices.Delete(t.parked, i, i+1)
			return
		}
	}
	for _, other := range t.entries {
		if other == e || other.varnish != nil || other.trace == nil || other.trace.VarnishXID != bereq {
			continue
		}
		e.trace = other.trace
		t.byTrace[other.trace.ID] = e
		t.remove(other)
		// The trace row may have absorbed an access line; its key has to
		// follow, or the client keeps that row and a Get on it misses.
		for _, k := range append([]string{other.req.Key}, other.req.Replaces...) {
			t.aliases[k] = e.req.Key
			e.req.Replaces = append(e.req.Replaces, k)
		}
		return
	}
}

func (t *Timeline) addTrace(tr *traceInfo, now time.Time) {
	t.lastTrace = now
	if e := t.byTrace[tr.ID]; e != nil {
		e.trace = tr
		t.fill(e)
		t.refresh(e, false)
		t.refreshNeighbours(e)
		return
	}
	if tr.VarnishXID != "" {
		if e := t.byBereq[tr.VarnishXID]; e != nil && e.trace == nil {
			e.trace = tr
			t.byTrace[tr.ID] = e
			t.fill(e)
			t.refresh(e, true)
			return
		}
		if !t.lastVarnish.IsZero() && now.Sub(t.lastVarnish) < sideKeep {
			for i, p := range t.parked {
				if p.trace.ID == tr.ID {
					t.parked[i].trace = tr
					return
				}
			}
			t.parked = append(t.parked, parkedTrace{trace: tr, since: now})
			return
		}
	}
	e := t.newTraceEntry(tr)
	t.refresh(e, true)
	t.refreshNeighbours(e)
}

func (t *Timeline) newTraceEntry(tr *traceInfo) *entry {
	e := t.insert("t:" + tr.ID)
	e.trace = tr
	t.byTrace[tr.ID] = e
	t.fill(e)
	t.absorbAccess(e)
	return e
}

// An access line only stands in for a missing source, so the varnish request
// or trace for the same URL that turns up after it replaces its row.
func (t *Timeline) absorbAccess(e *entry) {
	sc := e.scale()
	from, to := sc.from.Add(-slack), sc.to.Add(accessLookback)
	for _, other := range slices.Clone(t.entries) {
		a := other.access
		if other == e || a == nil || other.varnish != nil || other.trace != nil {
			continue
		}
		if a.URL != e.req.URL || !sameMethod(a.Method, e.req.Method) || a.Time.Before(from) || a.Time.After(to) {
			continue
		}
		t.remove(other)
		t.aliases[other.req.Key] = e.req.Key
		e.req.Replaces = append(e.req.Replaces, other.req.Key)
	}
}

// Varnish fetches a missed HEAD from the backend as GET, so nginx logs GET.
func sameMethod(access, req string) bool {
	return strings.EqualFold(access, req) || (strings.EqualFold(access, "GET") && strings.EqualFold(req, "HEAD"))
}

func (t *Timeline) addAccess(a core.NginxAccess, now time.Time) {
	if t.active(t.lastVarnish, now) || t.active(t.lastTrace, now) {
		return
	}
	base := fmt.Sprintf("n:%d", a.Time.UnixNano())
	key := base
	for i := 2; t.byKey[key] != nil; i++ {
		key = fmt.Sprintf("%s-%d", base, i)
	}
	e := t.insert(key)
	e.access = &a
	t.fill(e)
	t.refresh(e, true)
	t.refreshNeighbours(e)
}

func (t *Timeline) active(last, now time.Time) bool {
	return !last.IsZero() && now.Sub(last) < sideKeep
}

var tagsPatternRe = regexp.MustCompile(`(?i)\bReqHeader\s+(?:c\s+)?X-Magento-Tags-Pattern:\s*(.+?)\s*$`)

func (t *Timeline) addBans(v core.VarnishReq) {
	for _, line := range v.Raw {
		m := tagsPatternRe.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		re, err := regexp.Compile(m[1])
		if err != nil {
			continue
		}
		at := v.Start
		if at.IsZero() {
			at = t.now()
		}
		b := ban{at: at, pattern: m[1], re: re}
		i := sort.Search(len(t.bans), func(i int) bool { return t.bans[i].at.After(at) })
		t.bans = slices.Insert(t.bans, i, b)
	}
}

func (t *Timeline) insert(key string) *entry {
	t.seq++
	e := &entry{seq: t.seq, req: Request{Key: key}}
	t.entries = append(t.entries, e)
	t.byKey[key] = e
	for len(t.entries) > maxRequests {
		t.remove(t.entries[0])
	}
	return e
}

func (t *Timeline) remove(e *entry) {
	if i := slices.Index(t.entries, e); i >= 0 {
		t.entries = slices.Delete(t.entries, i, i+1)
	}
	delete(t.byKey, e.req.Key)
	if e.varnish != nil && t.byBereq[e.varnish.BereqVXID] == e {
		delete(t.byBereq, e.varnish.BereqVXID)
	}
	if e.trace != nil && t.byTrace[e.trace.ID] == e {
		delete(t.byTrace, e.trace.ID)
	}
	for alias, target := range t.aliases {
		if target == e.req.Key {
			delete(t.aliases, alias)
		}
	}
}

func (t *Timeline) fill(e *entry) {
	r := &e.req
	switch {
	case e.varnish != nil:
		v := e.varnish
		r.Method, r.URL, r.Host, r.Status, r.Handling = v.Method, v.URL, v.Host, v.Status, v.Handling
		r.Start, r.Duration, r.VXID, r.Source = v.Start, v.Duration, v.VXID, "varnish"
	case e.trace != nil:
		tr := e.trace
		r.Method, r.Status, r.Start, r.Duration, r.Source = tr.Method, tr.Status, tr.Start, tr.Duration, "trace"
		r.URL, r.Host = splitURL(tr.URL)
	case e.access != nil:
		a := e.access
		r.Method, r.URL, r.Status, r.Start, r.Source = a.Method, a.URL, a.Status, a.Time, "nginx"
	}
	r.TraceID = ""
	if e.trace != nil {
		r.TraceID = e.trace.ID
	}
}

func splitURL(raw string) (path, host string) {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return raw, ""
	}
	return u.RequestURI(), u.Host
}

func (t *Timeline) refresh(e *entry, force bool) {
	before := e.req
	t.build(e)
	if force || !reflect.DeepEqual(before, e.req) {
		t.publish(e.req)
	}
}

func (t *Timeline) refreshNeighbours(e *entry) {
	w, ok := e.window()
	if !ok {
		return
	}
	for _, other := range t.entries {
		if other == e {
			continue
		}
		if ow, ok := other.window(); ok && ow.overlaps(w) {
			t.refresh(other, false)
		}
	}
}

func (t *Timeline) touch(at time.Time, delay time.Duration) {
	for _, e := range t.entries {
		w, ok := e.window()
		if ok && !at.Before(w.from) && !at.After(w.to.Add(delay)) {
			t.refresh(e, false)
		}
	}
}

func (t *Timeline) publish(r Request) {
	for _, ch := range t.subs {
		select {
		case ch <- r:
			continue
		default:
		}
		select {
		case <-ch:
		default:
		}
		select {
		case ch <- r:
		default:
		}
	}
}

func (t *Timeline) Recent(n int) []Request {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.expire(t.now())
	list := slices.Clone(t.entries)
	sort.SliceStable(list, func(i, j int) bool {
		a, b := list[i].req.Start, list[j].req.Start
		if !a.Equal(b) {
			return a.After(b)
		}
		return list[i].seq > list[j].seq
	})
	if n > 0 && len(list) > n {
		list = list[:n]
	}
	out := make([]Request, len(list))
	for i, e := range list {
		out[i] = e.req
	}
	return out
}

func (t *Timeline) Get(key string) (Request, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if target, ok := t.aliases[key]; ok {
		key = target
	}
	e, ok := t.byKey[key]
	if !ok {
		return Request{}, false
	}
	return e.req, true
}

func (t *Timeline) Subscribe() (<-chan Request, func()) {
	t.mu.Lock()
	defer t.mu.Unlock()
	id := t.nextSub
	t.nextSub++
	ch := make(chan Request, subBuffer)
	t.subs[id] = ch
	var once sync.Once
	return ch, func() {
		once.Do(func() {
			t.mu.Lock()
			defer t.mu.Unlock()
			delete(t.subs, id)
			close(ch)
		})
	}
}

type ring[T any] struct {
	items []T
	at    func(T) time.Time
}

func (r *ring[T]) add(v T) {
	at := r.at(v)
	i := len(r.items)
	for i > 0 && r.at(r.items[i-1]).After(at) {
		i--
	}
	r.items = slices.Insert(r.items, i, v)
	if len(r.items) > maxSide {
		r.items = r.items[len(r.items)-maxSide:]
	}
}

func (r *ring[T]) prune(before time.Time) {
	i := sort.Search(len(r.items), func(i int) bool { return !r.at(r.items[i]).Before(before) })
	if i > 0 {
		r.items = r.items[i:]
	}
}

func (r *ring[T]) between(from, to time.Time) []T {
	lo := sort.Search(len(r.items), func(i int) bool { return !r.at(r.items[i]).Before(from) })
	hi := sort.Search(len(r.items), func(i int) bool { return r.at(r.items[i]).After(to) })
	if lo >= hi {
		return nil
	}
	return r.items[lo:hi]
}
