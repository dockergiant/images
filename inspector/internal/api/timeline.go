package api

import (
	"context"
	"time"

	"github.com/dockergiant/images/inspector/internal/core"
)

const timelineRecheck = 30 * time.Second

var timelineSources = []struct {
	kind    core.Kind
	stream  string
	feature string
}{
	{core.Varnish, "log", ""},
	{core.Redis, "monitor", ""},
	{core.Search, "insights", "insights"},
	{core.RabbitMQ, "rates", ""},
}

func (s *Server) timelineSub() subscription {
	sub := subscription{key: timelineKey, filter: core.Filter{}}
	if s.cfg.Timeline != nil {
		sub.spec = core.StreamSpec{Name: timelineKey, Title: "Request timeline", Start: s.runTimeline}
	}
	return sub
}

// runTimeline holds the Overview's share of the timeline feeds while a viewer
// streams it. The recheck picks up services that appear later and moves the
// Valkey lane between trace spans and MONITOR when tracing is switched.
func (s *Server) runTimeline(ctx context.Context, _ core.Filter, emit core.Emit) error {
	updates, stop := s.cfg.Timeline.Subscribe()
	defer stop()

	s.feeds.mu.Lock()
	s.feeds.overview++
	s.feeds.mu.Unlock()
	defer func() {
		s.feeds.mu.Lock()
		s.feeds.overview--
		s.feeds.mu.Unlock()
		s.syncFeeds(context.Background())
	}()
	s.syncFeeds(ctx)

	recheck := time.NewTicker(s.recheck)
	defer recheck.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-recheck.C:
			s.syncFeeds(ctx)
		case req, ok := <-updates:
			if !ok {
				return nil
			}
			emit(core.Event{Type: "timeline", Time: s.now(), Data: req})
		}
	}
}

// timelineInputs are the sources the Overview follows. MONITOR is left out
// while tracing covers the Valkey lane, since it slows every command on the
// shop's Valkey; the second result says which source the lane uses.
func (s *Server) timelineInputs(ctx context.Context) ([]subscription, string) {
	var out []subscription
	cache := ""
	for _, src := range timelineSources {
		a, ok := s.adapters[src.kind]
		if !ok {
			continue
		}
		info := s.info(ctx, a, false)
		if !info.Present || (src.feature != "" && !info.Features[src.feature]) {
			continue
		}
		if src.kind == core.Redis {
			if s.tracingCovers(ctx) {
				cache = timelineSourceTrace
				continue
			}
		}
		for _, spec := range a.Streams() {
			if spec.Name == src.stream && spec.Start != nil {
				out = append(out, subscription{key: streamKey(src.kind, src.stream, nil), kind: src.kind, spec: spec, filter: core.Filter{}})
				if src.kind == core.Redis {
					cache = timelineSourceMonitor
				}
			}
		}
	}
	for _, f := range s.cfg.Feeds {
		if f.Spec.Start != nil {
			out = append(out, subscription{key: streamKey(f.Kind, f.Spec.Name, nil), kind: f.Kind, spec: f.Spec, filter: core.Filter{}})
		}
	}
	return out, cache
}

// tracingCovers reports whether php-fpm traces carry the cache spans the
// Valkey lane needs. A running benchmark counts too: MONITOR would skew the
// arms it measures, and the benchmark owns the switches until it ends.
func (s *Server) tracingCovers(ctx context.Context) bool {
	if s.cfg.Perf == nil {
		return false
	}
	sum := s.cfg.Perf.Controller().Summary(ctx)
	return sum.Busy || (sum.Available && sum.Tracing)
}
