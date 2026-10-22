package api

import (
	"context"
	"slices"
	"sync"
	"time"

	"github.com/dockergiant/images/inspector/internal/core"
	"github.com/dockergiant/images/inspector/internal/timeline"
)

const (
	// leaseFor keeps varnishlog feeding the timeline after a v1 page lookup,
	// so the userscript finds the HITs that never reach PHP.
	leaseFor = 3 * time.Minute

	holderTimeline = "timeline"
	holderLease    = "api lease"

	timelineSourceTrace   = timeline.SourceTrace
	timelineSourceMonitor = timeline.SourceMonitor
)

// feeds are the hub subscriptions that feed the timeline. The Overview and
// the v1 lease each want some of them; a source runs once for both.
type feeds struct {
	// syncMu orders the syncs, so two never subscribe the same key twice.
	syncMu sync.Mutex

	mu       sync.Mutex
	overview int
	lease    time.Time
	timer    *time.Timer
	joined   map[string]*feed
}

type feed struct {
	unsub   func()
	holders []string
}

type wanted struct {
	sub     subscription
	holders []string
}

// syncFeeds subscribes the sources the holders want now and drops the rest.
func (s *Server) syncFeeds(ctx context.Context) {
	if s.cfg.Timeline == nil {
		return
	}
	s.feeds.syncMu.Lock()
	defer s.feeds.syncMu.Unlock()

	s.feeds.mu.Lock()
	overview := s.feeds.overview > 0
	lease := s.now().Before(s.feeds.lease)
	s.feeds.mu.Unlock()

	want := map[string]*wanted{}
	add := func(sub subscription, holder string) {
		w := want[sub.key]
		if w == nil {
			w = &wanted{sub: sub}
			want[sub.key] = w
		}
		w.holders = append(w.holders, holder)
	}
	if overview {
		subs, cache := s.timelineInputs(ctx)
		for _, sub := range subs {
			add(sub, holderTimeline)
		}
		s.cfg.Timeline.SetCacheSource(cache)
	}
	if lease {
		if sub, ok := s.leaseInput(ctx); ok {
			add(sub, holderLease)
		}
	}

	s.feeds.mu.Lock()
	var drop []func()
	for key, f := range s.feeds.joined {
		if w, ok := want[key]; ok {
			f.holders = w.holders
			continue
		}
		delete(s.feeds.joined, key)
		drop = append(drop, f.unsub)
	}
	var start []*wanted
	for key, w := range want {
		if _, ok := s.feeds.joined[key]; !ok && !s.stoppedRecently(key) {
			start = append(start, w)
		}
	}
	s.feeds.mu.Unlock()

	for _, u := range drop {
		u()
	}
	for _, w := range start {
		ch, unsubscribe := s.cfg.Hub.Subscribe(w.sub.key, s.producer(w.sub))
		f := &feed{unsub: unsubscribe, holders: w.holders}
		s.feeds.mu.Lock()
		s.feeds.joined[w.sub.key] = f
		s.feeds.mu.Unlock()
		go s.ingest(w.sub.key, f, ch)
	}
}

// ingest feeds one source into the timeline until it is unsubscribed or
// stopped from the Performance page. A stopped source leaves the joined set,
// and stoppedRecently keeps the next sync from starting it again at once.
func (s *Server) ingest(key string, f *feed, ch <-chan core.Event) {
	for ev := range ch {
		if ev.Type != "stream.error" {
			s.cfg.Timeline.Ingest(ev)
		}
	}
	s.feeds.mu.Lock()
	if s.feeds.joined[key] == f {
		delete(s.feeds.joined, key)
	}
	s.feeds.mu.Unlock()
}

func (s *Server) leaseInput(ctx context.Context) (subscription, bool) {
	a, ok := s.adapters[core.Varnish]
	if !ok || !s.info(ctx, a, false).Present {
		return subscription{}, false
	}
	for _, spec := range a.Streams() {
		if spec.Name == "log" && spec.Start != nil {
			return subscription{key: streamKey(core.Varnish, "log", nil), kind: core.Varnish, spec: spec, filter: core.Filter{}}, true
		}
	}
	return subscription{}, false
}

// touchLease moves the end of the lease to leaseFor from now. Only a lease
// that was not running syncs at once; the timer ends it.
func (s *Server) touchLease() {
	if s.cfg.Timeline == nil {
		return
	}
	s.feeds.mu.Lock()
	fresh := !s.now().Before(s.feeds.lease)
	s.feeds.lease = s.now().Add(s.leaseFor)
	if s.feeds.timer == nil {
		s.feeds.timer = time.AfterFunc(s.leaseFor, s.leaseCheck)
	} else {
		s.feeds.timer.Reset(s.leaseFor)
	}
	s.feeds.mu.Unlock()
	if fresh {
		go s.syncFeeds(context.Background())
	}
}

func (s *Server) leaseCheck() {
	s.feeds.mu.Lock()
	left := s.feeds.lease.Sub(s.now())
	if left > 0 {
		s.feeds.timer.Reset(left)
	}
	s.feeds.mu.Unlock()
	if left <= 0 {
		s.syncFeeds(context.Background())
	}
}

// feedHolders says who keeps the timeline feed on key running.
func (s *Server) feedHolders(key string) []string {
	s.feeds.mu.Lock()
	defer s.feeds.mu.Unlock()
	if f, ok := s.feeds.joined[key]; ok {
		return slices.Clone(f.holders)
	}
	return nil
}
