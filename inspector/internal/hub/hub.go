package hub

import (
	"context"
	"errors"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/dockergiant/images/inspector/internal/core"
)

var restartDelay = 2 * time.Second

// The UI closes its EventSource and opens a new one whenever its set of
// subscriptions changes, so a stream waits this long for the next viewer
// before its producer stops. A restart in between would race the old
// producer's teardown, which for a capture is still restoring settings.
var lingerDelay = time.Second

type sub struct {
	ch     chan core.Event
	closed bool
}

type fanout struct {
	mu   sync.Mutex
	subs map[*sub]struct{}
}

func (f *fanout) add(s *sub) {
	f.mu.Lock()
	if f.subs == nil {
		f.subs = map[*sub]struct{}{}
	}
	f.subs[s] = struct{}{}
	f.mu.Unlock()
}

func (f *fanout) remove(s *sub) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.subs[s]; ok {
		delete(f.subs, s)
		if !s.closed {
			s.closed = true
			close(s.ch)
		}
	}
	return len(f.subs)
}

func (f *fanout) closeAll() {
	f.mu.Lock()
	defer f.mu.Unlock()
	for s := range f.subs {
		delete(f.subs, s)
		if !s.closed {
			s.closed = true
			close(s.ch)
		}
	}
}

func (f *fanout) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.subs)
}

func (f *fanout) send(ev core.Event) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for s := range f.subs {
		push(s.ch, ev)
	}
}

func push(ch chan core.Event, ev core.Event) {
	for {
		select {
		case ch <- ev:
			return
		default:
		}
		select {
		case <-ch:
		default:
		}
	}
}

type stream struct {
	fanout
	cancel context.CancelFunc
	idle   *time.Timer
}

type Hub struct {
	buffer  int
	restart time.Duration
	linger  time.Duration
	mu      sync.Mutex
	streams map[string]*stream
	tap     fanout
}

func New(buffer int) *Hub {
	if buffer < 1 {
		buffer = 1
	}
	return &Hub{buffer: buffer, restart: restartDelay, linger: lingerDelay, streams: map[string]*stream{}}
}

func (h *Hub) Subscribe(key string, start func(ctx context.Context, emit core.Emit) error) (<-chan core.Event, func()) {
	ch, cancel, _ := h.SubscribeLimited(key, "", 0, start)
	return ch, cancel
}

// SubscribeLimited is Subscribe, except that it refuses to start a new
// producer once limit keys run under group: the group itself and every
// group;filter variant. Joining a running key always works. A limit of 0
// means no limit.
func (h *Hub) SubscribeLimited(key, group string, limit int, start func(ctx context.Context, emit core.Emit) error) (<-chan core.Event, func(), bool) {
	s := &sub{ch: make(chan core.Event, h.buffer)}

	h.mu.Lock()
	st, ok := h.streams[key]
	if !ok && limit > 0 && h.variants(group) >= limit {
		h.mu.Unlock()
		return nil, func() {}, false
	}
	if !ok {
		ctx, cancel := context.WithCancel(context.Background())
		st = &stream{cancel: cancel}
		h.streams[key] = st
		st.add(s)
		go h.run(ctx, st, start)
	} else {
		if st.idle != nil {
			st.idle.Stop()
			st.idle = nil
		}
		st.add(s)
	}
	h.mu.Unlock()

	var once sync.Once
	return s.ch, func() {
		once.Do(func() {
			h.mu.Lock()
			defer h.mu.Unlock()
			if st.remove(s) != 0 || h.streams[key] != st {
				return
			}
			if h.linger <= 0 {
				h.drop(key, st)
				return
			}
			st.idle = time.AfterFunc(h.linger, func() {
				h.mu.Lock()
				defer h.mu.Unlock()
				if st.count() == 0 {
					h.drop(key, st)
				}
			})
		})
	}, true
}

func (h *Hub) variants(group string) int {
	n := 0
	for k := range h.streams {
		if k == group || strings.HasPrefix(k, group+";") {
			n++
		}
	}
	return n
}

// Stop ends the producer of key at once and closes its subscribers'
// channels, whoever holds them.
func (h *Hub) Stop(key string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	st, ok := h.streams[key]
	if !ok {
		return false
	}
	if st.idle != nil {
		st.idle.Stop()
		st.idle = nil
	}
	h.drop(key, st)
	st.closeAll()
	return true
}

func (h *Hub) drop(key string, st *stream) {
	st.cancel()
	if h.streams[key] == st {
		delete(h.streams, key)
	}
}

func (h *Hub) run(ctx context.Context, st *stream, start func(ctx context.Context, emit core.Emit) error) {
	emit := func(ev core.Event) {
		if ctx.Err() != nil {
			return
		}
		st.send(ev)
		h.tap.send(ev)
	}
	for {
		err := start(ctx, emit)
		if ctx.Err() != nil {
			return
		}
		// A producer that ends on its own leaves its viewers with nothing,
		// so a clean end is retried like a failure.
		if err == nil {
			err = errors.New("stream ended")
		}
		st.send(core.Event{Type: "stream.error", Time: time.Now(), Data: err.Error()})
		select {
		case <-ctx.Done():
			return
		case <-time.After(h.restart):
		}
	}
}

func (h *Hub) Tap() (<-chan core.Event, func()) {
	s := &sub{ch: make(chan core.Event, h.buffer)}
	h.tap.add(s)
	var once sync.Once
	return s.ch, func() { once.Do(func() { h.tap.remove(s) }) }
}

func (h *Hub) Active() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	keys := make([]string, 0, len(h.streams))
	for k := range h.streams {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
