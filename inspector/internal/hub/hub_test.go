package hub

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dockergiant/images/inspector/internal/core"
)

func recv(t *testing.T, ch <-chan core.Event) core.Event {
	t.Helper()
	select {
	case ev, ok := <-ch:
		if !ok {
			t.Fatal("channel closed")
		}
		return ev
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for event")
	}
	return core.Event{}
}

func TestSubscribersShareOneProducer(t *testing.T) {
	h := New(8)
	var starts int32
	ready := make(chan core.Emit, 1)
	start := func(ctx context.Context, emit core.Emit) error {
		atomic.AddInt32(&starts, 1)
		ready <- emit
		<-ctx.Done()
		return nil
	}
	a, cancelA := h.Subscribe("k", start)
	b, cancelB := h.Subscribe("k", start)
	defer cancelA()
	defer cancelB()

	emit := <-ready
	emit(core.Event{Type: "x"})
	if recv(t, a).Type != "x" || recv(t, b).Type != "x" {
		t.Fatal("both subscribers should get the event")
	}
	if n := atomic.LoadInt32(&starts); n != 1 {
		t.Fatalf("starts = %d, want 1", n)
	}
	if got := h.Active(); len(got) != 1 || got[0] != "k" {
		t.Fatalf("Active = %v", got)
	}
}

func noLinger(t *testing.T) {
	old := lingerDelay
	lingerDelay = 0
	t.Cleanup(func() { lingerDelay = old })
}

func TestLastCancelStopsProducer(t *testing.T) {
	noLinger(t)
	h := New(8)
	running := make(chan struct{})
	stopped := make(chan struct{})
	start := func(ctx context.Context, emit core.Emit) error {
		close(running)
		<-ctx.Done()
		close(stopped)
		return nil
	}
	_, c1 := h.Subscribe("k", start)
	_, c2 := h.Subscribe("k", start)
	<-running

	c1()
	select {
	case <-stopped:
		t.Fatal("producer stopped with a subscriber left")
	case <-time.After(50 * time.Millisecond):
	}
	c2()
	select {
	case <-stopped:
	case <-time.After(100 * time.Millisecond):
		t.Fatal("producer not stopped within 100ms")
	}
	if len(h.Active()) != 0 {
		t.Fatalf("Active = %v", h.Active())
	}
}

func TestSlowSubscriberDoesNotBlockFast(t *testing.T) {
	h := New(2)
	ready := make(chan core.Emit, 1)
	start := func(ctx context.Context, emit core.Emit) error {
		ready <- emit
		<-ctx.Done()
		return nil
	}
	fast, cf := h.Subscribe("k", start)
	slow, cs := h.Subscribe("k", start)
	defer cf()
	defer cs()
	emit := <-ready

	done := make(chan struct{})
	go func() {
		for i := 0; i < 100; i++ {
			emit(core.Event{Type: "n", Data: i})
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("producer blocked by slow subscriber")
	}

	if len(slow) != 2 {
		t.Fatalf("slow buffered %d, want 2", len(slow))
	}
	first := recv(t, slow)
	second := recv(t, slow)
	if first.Data != 98 || second.Data != 99 {
		t.Fatalf("drop-oldest kept %v,%v want 98,99", first.Data, second.Data)
	}
	if recv(t, fast).Data != 98 {
		t.Fatal("fast subscriber should hold the newest events too")
	}
}

func TestRestartAfterError(t *testing.T) {
	old := restartDelay
	restartDelay = 10 * time.Millisecond
	defer func() { restartDelay = old }()

	h := New(8)
	var starts int32
	start := func(ctx context.Context, emit core.Emit) error {
		if atomic.AddInt32(&starts, 1) == 1 {
			return errors.New("boom")
		}
		emit(core.Event{Type: "again"})
		<-ctx.Done()
		return nil
	}
	ch, cancel := h.Subscribe("k", start)
	defer cancel()

	ev := recv(t, ch)
	if ev.Type != "stream.error" || ev.Data != "boom" {
		t.Fatalf("got %+v", ev)
	}
	if recv(t, ch).Type != "again" {
		t.Fatal("producer not restarted")
	}
}

func TestNoRestartAfterLastCancel(t *testing.T) {
	old := restartDelay
	restartDelay = 20 * time.Millisecond
	defer func() { restartDelay = old }()
	noLinger(t)

	h := New(8)
	var starts int32
	failed := make(chan struct{}, 1)
	start := func(ctx context.Context, emit core.Emit) error {
		atomic.AddInt32(&starts, 1)
		failed <- struct{}{}
		return errors.New("boom")
	}
	_, cancel := h.Subscribe("k", start)
	<-failed
	cancel()
	time.Sleep(80 * time.Millisecond)
	if n := atomic.LoadInt32(&starts); n != 1 {
		t.Fatalf("starts = %d, want 1", n)
	}
}

func TestTapSeesEveryProducer(t *testing.T) {
	h := New(8)
	tap, cancelTap := h.Tap()
	defer cancelTap()

	mk := func(name string) func(ctx context.Context, emit core.Emit) error {
		return func(ctx context.Context, emit core.Emit) error {
			emit(core.Event{Type: name})
			<-ctx.Done()
			return nil
		}
	}
	_, c1 := h.Subscribe("a", mk("a"))
	_, c2 := h.Subscribe("b", mk("b"))
	defer c1()
	defer c2()

	seen := map[string]bool{}
	seen[recv(t, tap).Type] = true
	seen[recv(t, tap).Type] = true
	if !seen["a"] || !seen["b"] {
		t.Fatalf("tap saw %v", seen)
	}
}

func TestResubscribeWithinLingerReusesProducer(t *testing.T) {
	old := lingerDelay
	lingerDelay = 200 * time.Millisecond
	defer func() { lingerDelay = old }()

	h := New(8)
	var starts int32
	stopped := make(chan struct{})
	ready := make(chan core.Emit, 2)
	start := func(ctx context.Context, emit core.Emit) error {
		atomic.AddInt32(&starts, 1)
		ready <- emit
		<-ctx.Done()
		close(stopped)
		return nil
	}
	_, c1 := h.Subscribe("k", start)
	emit := <-ready
	c1()
	ch, c2 := h.Subscribe("k", start)
	emit(core.Event{Type: "still"})
	if recv(t, ch).Type != "still" {
		t.Fatal("resubscriber did not join the running producer")
	}
	time.Sleep(300 * time.Millisecond)
	if n := atomic.LoadInt32(&starts); n != 1 {
		t.Fatalf("starts = %d, want 1", n)
	}
	c2()
	select {
	case <-stopped:
		t.Fatal("producer stopped before the linger ran out")
	case <-time.After(50 * time.Millisecond):
	}
	select {
	case <-stopped:
	case <-time.After(time.Second):
		t.Fatal("producer kept running after the linger")
	}
	if len(h.Active()) != 0 {
		t.Fatalf("Active = %v", h.Active())
	}
}

func TestCleanEndRestarts(t *testing.T) {
	old := restartDelay
	restartDelay = 10 * time.Millisecond
	defer func() { restartDelay = old }()

	h := New(8)
	var starts int32
	start := func(ctx context.Context, emit core.Emit) error {
		if atomic.AddInt32(&starts, 1) == 1 {
			return nil
		}
		emit(core.Event{Type: "again"})
		<-ctx.Done()
		return nil
	}
	ch, cancel := h.Subscribe("k", start)
	defer cancel()

	if ev := recv(t, ch); ev.Type != "stream.error" || ev.Data != "stream ended" {
		t.Fatalf("got %+v", ev)
	}
	if recv(t, ch).Type != "again" {
		t.Fatal("producer not restarted after a clean end")
	}
}

func idleStart(ctx context.Context, emit core.Emit) error {
	<-ctx.Done()
	return nil
}

func TestSubscribeLimitedBoundsTheVariantsOfAStream(t *testing.T) {
	h := New(8)
	var cancels []func()
	defer func() {
		for _, c := range cancels {
			c()
		}
	}()
	for _, k := range []string{"redis:monitor", "redis:monitor;match=a", "redis:monitor;match=b"} {
		_, c, ok := h.SubscribeLimited(k, "redis:monitor", 3, idleStart)
		if !ok {
			t.Fatalf("%s refused below the limit", k)
		}
		cancels = append(cancels, c)
	}
	if _, _, ok := h.SubscribeLimited("redis:monitor;match=c", "redis:monitor", 3, idleStart); ok {
		t.Fatal("a fourth variant started")
	}
	// Joining a running key starts nothing, so the limit does not apply.
	_, c, ok := h.SubscribeLimited("redis:monitor;match=a", "redis:monitor", 3, idleStart)
	if !ok {
		t.Fatal("joining a running key was refused")
	}
	cancels = append(cancels, c)
	// Another stream of the same service has its own count.
	_, c, ok = h.SubscribeLimited("redis:monitorx", "redis:monitorx", 1, idleStart)
	if !ok {
		t.Fatal("a different stream shared the count")
	}
	cancels = append(cancels, c)
}

func TestStopEndsTheProducerAndClosesItsSubscribers(t *testing.T) {
	h := New(8)
	stopped := make(chan struct{})
	a, cancelA := h.Subscribe("k", func(ctx context.Context, emit core.Emit) error {
		<-ctx.Done()
		close(stopped)
		return nil
	})
	b, cancelB := h.Subscribe("k", idleStart)
	defer cancelA()
	defer cancelB()
	if !h.Stop("k") {
		t.Fatal("stop found nothing")
	}
	for _, ch := range []<-chan core.Event{a, b} {
		select {
		case _, ok := <-ch:
			if ok {
				t.Fatal("event instead of a close")
			}
		case <-time.After(2 * time.Second):
			t.Fatal("subscriber channel left open")
		}
	}
	select {
	case <-stopped:
	case <-time.After(2 * time.Second):
		t.Fatal("producer kept running")
	}
	if len(h.Active()) != 0 || h.Stop("k") {
		t.Fatalf("still active: %v", h.Active())
	}
	// The old subscribers' cancels must not touch a new stream on the key.
	c, cancelC := h.Subscribe("k", idleStart)
	defer cancelC()
	cancelA()
	cancelB()
	time.Sleep(lingerDelay + 50*time.Millisecond)
	if got := h.Active(); len(got) != 1 {
		t.Fatalf("a stale cancel dropped the new stream: %v", got)
	}
	select {
	case _, ok := <-c:
		if !ok {
			t.Fatal("new subscriber closed")
		}
	default:
	}
}
