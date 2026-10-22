package capture

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"
)

func newManager() *Manager {
	return New(slog.New(slog.NewTextHandler(io.Discard, nil)))
}

func counter() (*int32, func(context.Context) error) {
	var n int32
	return &n, func(context.Context) error {
		atomic.AddInt32(&n, 1)
		return nil
	}
}

func TestEndRestoresExactlyOnce(t *testing.T) {
	m := newManager()
	n, restore := counter()
	if err := m.Begin("rabbit", time.Hour, restore); err != nil {
		t.Fatal(err)
	}
	if !m.Active("rabbit") {
		t.Fatal("session should be active")
	}
	if err := m.End(context.Background(), "rabbit"); err != nil {
		t.Fatal(err)
	}
	if err := m.End(context.Background(), "rabbit"); err != nil {
		t.Fatal(err)
	}
	if got := atomic.LoadInt32(n); got != 1 {
		t.Fatalf("restore ran %d times", got)
	}
	if m.Active("rabbit") || len(m.Keys()) != 0 {
		t.Fatal("session should be removed")
	}
}

func TestTimeoutRestoresOnceAndEndIsNoop(t *testing.T) {
	m := newManager()
	n, restore := counter()
	if err := m.Begin("search", 20*time.Millisecond, restore); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for atomic.LoadInt32(n) == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	_ = m.End(context.Background(), "search")
	time.Sleep(50 * time.Millisecond)
	if got := atomic.LoadInt32(n); got != 1 {
		t.Fatalf("restore ran %d times", got)
	}
	if m.Active("search") {
		t.Fatal("session should be gone after timeout")
	}
}

func TestEndAll(t *testing.T) {
	m := newManager()
	a, ra := counter()
	b, rb := counter()
	_ = m.Begin("a", time.Hour, ra)
	_ = m.Begin("b", time.Hour, rb)
	if keys := m.Keys(); len(keys) != 2 || keys[0] != "a" || keys[1] != "b" {
		t.Fatalf("keys = %v", keys)
	}
	m.EndAll(context.Background())
	m.EndAll(context.Background())
	if atomic.LoadInt32(a) != 1 || atomic.LoadInt32(b) != 1 {
		t.Fatalf("restores a=%d b=%d", *a, *b)
	}
	if len(m.Keys()) != 0 {
		t.Fatal("sessions left")
	}
}

func TestBeginRejectsDuplicateKey(t *testing.T) {
	m := newManager()
	_, r := counter()
	_ = m.Begin("a", time.Hour, r)
	if err := m.Begin("a", time.Hour, r); err == nil {
		t.Fatal("expected error")
	}
	m.EndAll(context.Background())
}

func TestEndReturnsRestoreError(t *testing.T) {
	m := newManager()
	_ = m.Begin("a", time.Hour, func(context.Context) error { return errors.New("nope") })
	if err := m.End(context.Background(), "a"); err == nil || err.Error() != "nope" {
		t.Fatalf("err = %v", err)
	}
}

func TestBeginWhileRestoringIsRefused(t *testing.T) {
	m := newManager()
	release := make(chan struct{})
	entered := make(chan struct{})
	if err := m.Begin("search", time.Hour, func(context.Context) error {
		close(entered)
		<-release
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	ended := make(chan error, 1)
	go func() { ended <- m.End(context.Background(), "search") }()
	<-entered

	if err := m.Begin("search", time.Hour, func(context.Context) error { return nil }); !errors.Is(err, ErrRestoring) {
		t.Fatalf("Begin during restore = %v, want ErrRestoring", err)
	}
	if m.Active("search") {
		t.Fatal("a restoring session counts as active")
	}
	waited := make(chan struct{})
	go func() {
		m.EndAll(context.Background())
		close(waited)
	}()
	select {
	case <-waited:
		t.Fatal("EndAll returned before the restore finished")
	case <-time.After(50 * time.Millisecond):
	}

	close(release)
	if err := <-ended; err != nil {
		t.Fatal(err)
	}
	<-waited
	if err := m.Begin("search", time.Hour, func(context.Context) error { return nil }); err != nil {
		t.Fatalf("Begin after restore = %v", err)
	}
}
