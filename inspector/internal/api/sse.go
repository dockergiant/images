package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/dockergiant/images/inspector/internal/apm"
	"github.com/dockergiant/images/inspector/internal/core"
)

const (
	ringSize   = 2000
	batchEvery = 75 * time.Millisecond
	pingEvery  = 15 * time.Second
	maxSubs    = 16
	// maxVariants bounds the producers of one stream: its unfiltered key and
	// its filtered variants each run their own MONITOR or varnishlog. v1 may
	// start fewer, so a foreign page cannot take the Inspector page's share.
	maxVariants   = 4
	maxV1Variants = 2
	// stopHold keeps a source stopped from the Performance page from coming
	// straight back through a foreign page's EventSource reconnect.
	stopHold = 10 * time.Minute
	// The UI's drawer matches this text to deselect the source.
	stoppedMsg = "stopped from Inspector's Performance page"
	// Longer than the "retry: 2000" reconnect delay, so a Last-Event-ID
	// reconnect still finds the events it missed in the ring.
	wantLinger = 5 * time.Second

	timelineKey = "timeline"
	tracesKey   = "php:traces"
)

type ringEntry struct {
	id   uint64
	key  string
	typ  string
	data []byte
}

type ring struct {
	mu    sync.Mutex
	items []ringEntry
	next  int
	full  bool
	last  uint64
	lost  map[string]uint64
}

func newRing(size int) *ring {
	return &ring{items: make([]ringEntry, size), lost: map[string]uint64{}}
}

type wireEvent struct {
	Sub string `json:"sub"`
	core.Event
}

func (r *ring) add(key string, ev core.Event) {
	data, err := json.Marshal(wireEvent{Sub: key, Event: ev})
	if err != nil {
		data, _ = json.Marshal(wireEvent{Sub: key, Event: core.Event{Type: ev.Type, Time: ev.Time, Kind: ev.Kind, Data: "unencodable event: " + err.Error()}})
	}
	r.mu.Lock()
	r.last++
	if old := r.items[r.next]; old.id != 0 {
		r.lost[old.key] = old.id
	}
	r.items[r.next] = ringEntry{id: r.last, key: key, typ: ev.Type, data: data}
	r.next = (r.next + 1) % len(r.items)
	if r.next == 0 {
		r.full = true
	}
	r.mu.Unlock()
}

func (r *ring) head() uint64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.last
}

func (r *ring) since(after uint64, keys map[string]bool) ([]ringEntry, uint64, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if after >= r.last {
		return nil, r.last, false
	}
	n := r.next
	start := 0
	if r.full {
		n = len(r.items)
		start = r.next
	}
	gap := false
	for k := range keys {
		if r.lost[k] > after {
			gap = true
			break
		}
	}
	var out []ringEntry
	for i := 0; i < n; i++ {
		e := r.items[(start+i)%len(r.items)]
		if e.id > after && keys[e.key] {
			out = append(out, e)
		}
	}
	return out, r.last, gap
}

type subscription struct {
	key    string
	kind   core.Kind
	spec   core.StreamSpec
	filter core.Filter
}

// The pairs in a sub value are separated by ";", which url.ParseQuery refuses,
// so the raw query is split by hand.
func parseSubs(rawQuery string) ([]string, error) {
	var subs []string
	for _, part := range strings.Split(rawQuery, "&") {
		if part == "" {
			continue
		}
		k, v, _ := strings.Cut(part, "=")
		key, err := url.QueryUnescape(k)
		if err != nil {
			return nil, fmt.Errorf("bad query: %w", err)
		}
		if key != "sub" {
			continue
		}
		val, err := url.QueryUnescape(v)
		if err != nil {
			return nil, fmt.Errorf("bad sub value: %w", err)
		}
		subs = append(subs, val)
	}
	return subs, nil
}

// subError carries the hint a v1 client gets next to the error.
type subError struct {
	msg  string
	hint string
}

func (e subError) Error() string { return e.msg }

const subHint = "a sub is timeline, php:traces or <kind>:<stream>[;key=value…]; " + v1Prefix + "/openapi.json lists the streams"

func (s *Server) resolveSub(raw string) (subscription, error) {
	parts := strings.Split(raw, ";")
	if strings.TrimSpace(parts[0]) == timelineKey && len(parts) == 1 {
		return s.timelineSub(), nil
	}
	if strings.TrimSpace(parts[0]) == benchKey && len(parts) == 1 {
		return subscription{key: benchKey, kind: core.Kind("perf")}, nil
	}
	kindName, streamName, ok := strings.Cut(strings.TrimSpace(parts[0]), ":")
	if !ok || kindName == "" || streamName == "" {
		return subscription{}, subError{fmt.Sprintf("sub %q: want <kind>:<stream>", raw), subHint}
	}
	filter := core.Filter{}
	for _, p := range parts[1:] {
		if p == "" {
			continue
		}
		k, v, ok := strings.Cut(p, "=")
		if !ok || k == "" {
			return subscription{}, subError{fmt.Sprintf("sub %q: filter %q is not key=value", raw, p), subHint}
		}
		filter[k] = v
	}
	kind := core.Kind(kindName)
	if kind == core.PHP && streamName == "traces" {
		return subscription{key: tracesKey, kind: kind}, nil
	}
	var specs []core.StreamSpec
	if a, ok := s.adapters[kind]; ok {
		specs = a.Streams()
	}
	for _, f := range s.cfg.Feeds {
		if f.Kind == kind {
			specs = append(specs, f.Spec)
		}
	}
	if len(specs) == 0 {
		return subscription{}, subError{fmt.Sprintf("sub %q: unknown service %s", raw, kindName), subHint}
	}
	for _, spec := range specs {
		if spec.Name != streamName {
			continue
		}
		for k := range filter {
			if !slices.Contains(spec.Filters, k) {
				hint := kindName + ":" + streamName + " takes no filters"
				if len(spec.Filters) > 0 {
					hint = kindName + ":" + streamName + " filters on " + strings.Join(spec.Filters, " and ")
				}
				return subscription{}, subError{fmt.Sprintf("sub %q: unknown filter %q", raw, k), hint}
			}
		}
		return subscription{key: streamKey(kind, streamName, filter), kind: kind, spec: spec, filter: filter}, nil
	}
	return subscription{}, subError{fmt.Sprintf("sub %q: %s has no stream %s", raw, kindName, streamName), subHint}
}

func writeSubError(w http.ResponseWriter, status int, err error) {
	var se subError
	if errors.As(err, &se) {
		writeHint(w, status, se.msg, se.hint)
		return
	}
	writeHint(w, status, err.Error(), subHint)
}

// streamGroup is the stream a key belongs to, without its filters.
func streamGroup(key string) string {
	return strings.SplitN(key, ";", 2)[0]
}

func streamKey(kind core.Kind, stream string, f core.Filter) string {
	keys := make([]string, 0, len(f))
	for k := range f {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	b.WriteString(string(kind))
	b.WriteByte(':')
	b.WriteString(stream)
	for _, k := range keys {
		b.WriteByte(';')
		b.WriteString(k)
		b.WriteByte('=')
		b.WriteString(f[k])
	}
	return b.String()
}

func (s *Server) startRelays() {
	s.relayOnce.Do(func() {
		if s.cfg.Store != nil {
			ch, _ := s.cfg.Store.Subscribe()
			go func() {
				for t := range ch {
					s.record(tracesKey, core.Event{Type: "trace", Time: t.Start, Kind: core.PHP, Data: apm.Summarize(t)})
				}
			}()
		}
	})
}

type streamError struct {
	sub string
	msg string
}

// serveStream serves /api/stream and, with v1 set, /api/v1/stream, which any
// origin may open and so never starts an opt-in capture.
func (s *Server) serveStream(w http.ResponseWriter, r *http.Request, v1 bool) {
	// HEAD on /api/stream stays refused: Go would discard the body of a
	// stream it had started. v1 answers HEAD after the same checks, without
	// starting anything.
	head := v1 && r.Method == http.MethodHead
	if r.Method != http.MethodGet && !head {
		allow := http.MethodGet
		if v1 {
			allow = "GET, HEAD, OPTIONS"
		}
		w.Header().Set("Allow", allow)
		writeHint(w, http.StatusMethodNotAllowed, "streams are opened with GET", "open it with new EventSource(url)")
		return
	}
	raw, err := parseSubs(r.URL.RawQuery)
	if err != nil {
		writeHint(w, http.StatusBadRequest, err.Error(), subHint)
		return
	}
	if len(raw) > maxSubs {
		writeHint(w, http.StatusBadRequest, fmt.Sprintf("at most %d subscriptions per stream", maxSubs), "open a second stream for the rest")
		return
	}
	keys := map[string]bool{}
	var subs []subscription
	optIn := false
	for _, v := range raw {
		sub, err := s.resolveSub(v)
		if err != nil {
			writeSubError(w, http.StatusBadRequest, err)
			return
		}
		if keys[sub.key] {
			continue
		}
		keys[sub.key] = true
		if sub.spec.Start != nil {
			subs = append(subs, sub)
			optIn = optIn || sub.spec.OptIn
		}
	}
	if optIn && v1 {
		writeHint(w, http.StatusForbidden, "opt-in captures are not on "+v1Prefix+"/stream", "they change service settings, so they start only from the Inspector page's live drawer")
		return
	}
	if optIn {
		// checkHeaders lets reads through without Fetch Metadata or with
		// "none"; a capture changes the service, so it needs the page itself.
		if r.Header.Get("Sec-Fetch-Site") != "same-origin" {
			writeError(w, http.StatusForbidden, "capture streams must be opened from the Inspector page")
			return
		}
		if lan, why := s.lan(r.Context()); lan {
			writeError(w, http.StatusLocked, "capture streams change the service: "+why)
			return
		}
	}
	if v1 {
		active := s.cfg.Hub.Active()
		for _, sub := range subs {
			if !slices.Contains(active, sub.key) && s.stoppedRecently(sub.key) {
				writeHint(w, http.StatusConflict, sub.key+" was stopped from Inspector's Performance page", "it stays off for "+v1Prefix+" for "+stopHold.String()+", unless the Inspector page starts it again")
				return
			}
		}
	}

	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-store")
	h.Set("X-Accel-Buffering", "no")
	if head {
		w.WriteHeader(http.StatusOK)
		return
	}

	s.watch(keys, v1)
	watching := true
	defer func() {
		if watching {
			time.AfterFunc(wantLinger, func() { s.unwatch(keys, v1) })
		}
	}()
	s.startRelays()
	cursor := s.ring.head()
	if id, err := strconv.ParseUint(r.Header.Get("Last-Event-ID"), 10, 64); err == nil && id <= cursor {
		cursor = id
	}

	ctx, cancel := context.WithCancel(r.Context())
	errs := make(chan streamError, maxSubs)
	var wg sync.WaitGroup
	var unsubs []func()
	defer func() {
		cancel()
		for _, u := range unsubs {
			u()
		}
		wg.Wait()
	}()
	limit := maxVariants
	if v1 {
		limit = maxV1Variants
	}
	// Subscribed before the 200, so a refused producer still gets its 429.
	chans := make([]<-chan core.Event, len(subs))
	for i, sub := range subs {
		ch, unsubscribe, ok := s.cfg.Hub.SubscribeLimited(sub.key, streamGroup(sub.key), limit, s.producer(sub))
		if !ok {
			watching = false
			s.unwatch(keys, v1)
			h.Del("Content-Type")
			h.Del("X-Accel-Buffering")
			writeHint(w, http.StatusTooManyRequests, fmt.Sprintf("%s already runs %d filtered variants", streamGroup(sub.key), limit),
				"each variant runs its own producer in the service; reuse a running filter, or stop one on Inspector's Performance page")
			return
		}
		unsubs = append(unsubs, unsubscribe)
		chans[i] = ch
	}

	rc := http.NewResponseController(w)
	w.WriteHeader(http.StatusOK)
	if _, err := fmt.Fprint(w, "retry: 2000\n\n"); err != nil {
		return
	}
	if err := rc.Flush(); err != nil {
		return
	}

	// A channel closes when the Performance page stops its source. The
	// stream ends once every source is gone, so a client is not left
	// holding a connection that can never deliver anything.
	allClosed := make(chan struct{})
	var closedMu sync.Mutex
	closed := 0
	for i, sub := range subs {
		wg.Add(1)
		go func(key string, ch <-chan core.Event) {
			defer wg.Done()
			for {
				select {
				case <-ctx.Done():
					return
				case ev, ok := <-ch:
					if !ok {
						select {
						case errs <- streamError{sub: key, msg: stoppedMsg}:
						case <-ctx.Done():
							return
						}
						closedMu.Lock()
						closed++
						if closed == len(subs) {
							close(allClosed)
						}
						closedMu.Unlock()
						return
					}
					if ev.Type != "stream.error" {
						continue
					}
					select {
					case errs <- streamError{sub: key, msg: fmt.Sprint(ev.Data)}:
					default:
					}
				}
			}
		}(sub.key, chans[i])
	}

	batch := time.NewTicker(batchEvery)
	defer batch.Stop()
	ping := time.NewTicker(pingEvery)
	defer ping.Stop()
	var buf bytes.Buffer
	writeErr := func(e streamError) {
		data, _ := json.Marshal(map[string]string{"sub": e.sub, "error": e.msg})
		buf.WriteString("event: stream.error\ndata: ")
		buf.Write(data)
		buf.WriteString("\n\n")
	}
	for {
		done := false
		select {
		case <-ctx.Done():
			return
		case <-allClosed:
			for len(errs) > 0 {
				writeErr(<-errs)
			}
			done = true
		case e := <-errs:
			writeErr(e)
		case <-ping.C:
			buf.WriteString(": ping\n\n")
		case <-batch.C:
			entries, last, gap := s.ring.since(cursor, keys)
			if gap {
				buf.WriteString("event: gap\ndata: {}\n\n")
			}
			for _, e := range entries {
				fmt.Fprintf(&buf, "id: %d\nevent: %s\ndata: ", e.id, e.typ)
				buf.Write(e.data)
				buf.WriteString("\n\n")
			}
			if len(entries) == 0 && last > cursor && !gap {
				fmt.Fprintf(&buf, "id: %d\n\n", last)
			}
			cursor = last
		}
		if buf.Len() > 0 {
			if _, err := w.Write(buf.Bytes()); err != nil {
				return
			}
			buf.Reset()
			if err := rc.Flush(); err != nil {
				return
			}
		}
		if done {
			return
		}
	}
}

func (s *Server) producer(sub subscription) func(ctx context.Context, emit core.Emit) error {
	return func(ctx context.Context, emit core.Emit) error {
		return sub.spec.Start(ctx, sub.filter, func(ev core.Event) {
			if ctx.Err() != nil {
				return
			}
			if ev.Kind == "" {
				ev.Kind = sub.kind
			}
			if ev.Time.IsZero() {
				ev.Time = s.now()
			}
			s.record(sub.key, ev)
			// The ring copy reaches the client and replays; emitting it too
			// would make the relay send it a second time.
			if ev.Type == "stream.error" {
				return
			}
			emit(ev)
		})
	}
}

// watch counts who streams each key; apiWant counts the v1 share of want,
// so the Performance page can tell a foreign page from the drawer.
func (s *Server) watch(keys map[string]bool, v1 bool) {
	s.wantMu.Lock()
	defer s.wantMu.Unlock()
	for k := range keys {
		s.want[k]++
		if v1 {
			s.apiWant[k]++
		}
	}
}

func (s *Server) unwatch(keys map[string]bool, v1 bool) {
	s.wantMu.Lock()
	defer s.wantMu.Unlock()
	for k := range keys {
		if s.want[k]--; s.want[k] <= 0 {
			delete(s.want, k)
		}
		if v1 {
			if s.apiWant[k]--; s.apiWant[k] <= 0 {
				delete(s.apiWant, k)
			}
		}
	}
}

func (s *Server) stoppedRecently(key string) bool {
	s.wantMu.Lock()
	defer s.wantMu.Unlock()
	at, ok := s.stopped[key]
	if ok && s.now().Sub(at) >= stopHold {
		delete(s.stopped, key)
		return false
	}
	return ok
}

// Sources the timeline follows run without a client on their own key, so
// their events are kept only while someone streams that key.
func (s *Server) record(key string, ev core.Event) {
	s.wantMu.Lock()
	wanted := s.want[key] > 0
	s.wantMu.Unlock()
	if wanted {
		s.ring.add(key, ev)
	}
}
