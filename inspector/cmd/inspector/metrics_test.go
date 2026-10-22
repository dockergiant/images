package main

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dockergiant/images/inspector/internal/core"
	"github.com/dockergiant/images/inspector/internal/metrics"
)

type plainAdapter struct{ kind core.Kind }

func (p plainAdapter) Kind() core.Kind                             { return p.kind }
func (p plainAdapter) Detect(context.Context) (core.Info, error)   { return core.Info{}, nil }
func (p plainAdapter) Health(context.Context) (core.Health, error) { return core.Health{}, nil }
func (p plainAdapter) Browse() []core.Route                        { return nil }
func (p plainAdapter) Streams() []core.StreamSpec                  { return nil }
func (p plainAdapter) Actions() []core.ActionSpec                  { return nil }

type sampledAdapter struct {
	plainAdapter
	calls    atomic.Int32
	inFlight atomic.Int32
	overlap  atomic.Bool
	err      error
}

func (s *sampledAdapter) Metrics(ctx context.Context) (map[string]float64, error) {
	if s.inFlight.Add(1) > 1 {
		s.overlap.Store(true)
	}
	defer s.inFlight.Add(-1)
	s.calls.Add(1)
	select {
	case <-ctx.Done():
	case <-time.After(time.Millisecond):
	}
	return nil, s.err
}

func TestSampleMetricsRunsUntilShutdown(t *testing.T) {
	var buf bytes.Buffer
	var mu sync.Mutex
	errs := newRateLog(slog.New(slog.NewTextHandler(&lockedWriter{w: &buf, mu: &mu}, nil)), time.Minute)
	ok := &sampledAdapter{plainAdapter: plainAdapter{core.Varnish}}
	absent := &sampledAdapter{plainAdapter: plainAdapter{core.RabbitMQ}, err: core.ErrAbsent}
	broken := &sampledAdapter{plainAdapter: plainAdapter{core.Search}, err: errors.New("node stats: 503")}
	adapters := []core.Adapter{ok, plainAdapter{core.Redis}, absent, broken}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		sampleMetrics(ctx, adapters, metrics.NewSampler(2*time.Millisecond), errs)
		close(done)
	}()
	deadline := time.Now().Add(2 * time.Second)
	for (ok.calls.Load() < 3 || absent.calls.Load() < 3 || broken.calls.Load() < 1) && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("samplers did not stop on shutdown")
	}
	for _, a := range []*sampledAdapter{ok, absent, broken} {
		if a.overlap.Load() {
			t.Errorf("%s ran two samples at once", a.kind)
		}
	}
	if ok.calls.Load() < 3 || absent.calls.Load() < 3 {
		t.Fatalf("samples: ok %d absent %d", ok.calls.Load(), absent.calls.Load())
	}
	mu.Lock()
	out := buf.String()
	mu.Unlock()
	if !strings.Contains(out, "metrics sample failed") || !strings.Contains(out, "search") || strings.Contains(out, "rabbitmq") {
		t.Fatalf("log = %s", out)
	}
}

type lockedWriter struct {
	w  *bytes.Buffer
	mu *sync.Mutex
}

func (l *lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(p)
}
