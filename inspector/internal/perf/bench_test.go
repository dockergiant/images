package perf

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dockergiant/images/inspector/internal/docker"
)

type fakeNginx struct {
	srv      *httptest.Server
	php      *fakePHP
	requests atomic.Int32
	status   atomic.Int32
	// profileStatus, when set, is what the page answers with profiling on.
	profileStatus atomic.Int32
	gate          chan struct{}
	mu            sync.Mutex
	hosts         map[string]int
	protos        map[string]int
}

func newFakeNginx(t *testing.T, php *fakePHP) *fakeNginx {
	n := &fakeNginx{php: php, hosts: map[string]int{}, protos: map[string]int{}}
	n.status.Store(200)
	n.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n.requests.Add(1)
		n.mu.Lock()
		n.hosts[r.Host]++
		n.protos[r.Header.Get("X-Forwarded-Proto")]++
		gate := n.gate
		n.mu.Unlock()
		if gate != nil {
			<-gate
		}
		// Each arm costs a little more, so the result has an order to check.
		s := php.get("fpm")
		d := time.Millisecond
		if s.Tracing() {
			d = 3 * time.Millisecond
			if s.Detail() == "full" {
				d = 5 * time.Millisecond
			}
			if s.Profiling() {
				d = 6 * time.Millisecond
			}
		}
		time.Sleep(d)
		code := int(n.status.Load())
		if p := int(n.profileStatus.Load()); p != 0 && s.Profiling() {
			code = p
		}
		if code == 302 {
			w.Header().Set("Location", "https://app.shop.test/")
		}
		w.WriteHeader(code)
		w.Write([]byte("<html>ok</html>"))
	}))
	t.Cleanup(n.srv.Close)
	return n
}

func newTestBench(t *testing.T) (*Bench, *Controller, *fakePHP, *fakeNginx) {
	t.Helper()
	c, php, _ := newTestController(t)
	php.set("fpm", docker.IniState{Script: true, DetailSupport: true, Template: true, Loaded: true, Settings: true, Profile: true})
	n := newFakeNginx(t, php)
	b := NewBench(c, BenchConfig{Base: n.srv.URL, Warmups: 1, Rounds: 3, ReadyWait: time.Second, RequestTimeout: 5 * time.Second, MarkerPath: filepath.Join(t.TempDir(), "bench.json")}, "shop.test", func(context.Context) string { return "app.shop.test" })
	return b, c, php, n
}

func waitDone(t *testing.T, b *Bench) BenchState {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		st := b.State()
		if !st.Running && st.Phase != "" && !b.ctl.Busy() {
			return st
		}
		if time.Now().After(deadline) {
			t.Fatalf("benchmark still running: %+v", st)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestBenchRunsEveryArmAndRestores(t *testing.T) {
	b, c, php, n := newTestBench(t)
	var events []BenchState
	var mu sync.Mutex
	b.OnProgress(func(s BenchState) {
		mu.Lock()
		events = append(events, s)
		mu.Unlock()
	})
	st, err := b.Start(context.Background(), "https://app.shop.test/women.html?p=2")
	if err != nil {
		t.Fatal(err)
	}
	if !st.Running || st.Path != "/women.html?p=2" || st.Host != "app.shop.test" || st.Total != 12 || st.PerRound != 3 {
		t.Fatalf("start %+v", st)
	}
	done := waitDone(t, b)
	if done.Phase != "done" || done.Error != "" || !done.Restored || done.Done != 12 {
		t.Fatalf("done %+v", done)
	}
	if len(done.Arms) != 4 {
		t.Fatalf("arms %+v", done.Arms)
	}
	ids := []string{}
	for _, a := range done.Arms {
		ids = append(ids, a.ID)
		if len(a.Samples) != 3 || a.MinMs > a.MedianMs || a.MedianMs > a.MaxMs || a.NonOK != 0 {
			t.Fatalf("arm %+v", a)
		}
	}
	if strings.Join(ids, ",") != "off,lean,full,full+profile" {
		t.Fatalf("arm order %v", ids)
	}
	if done.Arms[0].DeltaPct != nil || done.Arms[3].DeltaPct == nil || *done.Arms[3].DeltaPct <= 0 {
		t.Fatalf("deltas %+v", done.Arms)
	}
	if done.Arms[3].MedianMs <= done.Arms[0].MedianMs {
		t.Fatalf("full+profile %v not slower than off %v", done.Arms[3].MedianMs, done.Arms[0].MedianMs)
	}
	// probe + per arm per round: ready check, 1 warm-up, 3 measured
	if got := int(n.requests.Load()); got < 1+12*(1+1+3) {
		t.Fatalf("requests %d", got)
	}
	if n.hosts["app.shop.test"] != int(n.requests.Load()) || n.protos["https"] != int(n.requests.Load()) {
		t.Fatalf("hosts %v protos %v", n.hosts, n.protos)
	}
	s := php.get("fpm")
	if !s.Tracing() || s.Detail() != "full" || !s.Profiling() {
		t.Fatalf("not restored: %+v", s)
	}
	if php.reloads["fpm"] != 13 || php.reloads["debug"] != 0 {
		t.Fatalf("reloads %v: one per switch plus the restore, php-debug untouched", php.reloads)
	}
	if c.Busy() {
		t.Fatal("the switches are still held")
	}
	mu.Lock()
	defer mu.Unlock()
	if len(events) < 12 || events[len(events)-1].Phase != "done" {
		t.Fatalf("%d events, last %+v", len(events), events[len(events)-1])
	}
}

func TestBenchRoundsAlternateTheArmOrder(t *testing.T) {
	order := armOrder(4, 3)
	want := [][]int{{0, 1, 2, 3}, {1, 2, 3, 0}, {2, 3, 0, 1}}
	for r := range want {
		for k := range want[r] {
			if order[r][k] != want[r][k] {
				t.Fatalf("order %v", order)
			}
		}
	}
}

func TestBenchCancelRestoresAndFreesTheSwitches(t *testing.T) {
	b, c, php, n := newTestBench(t)
	n.mu.Lock()
	n.gate = make(chan struct{})
	gate := n.gate
	n.mu.Unlock()
	if _, err := b.Start(context.Background(), "/"); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Start(context.Background(), "/"); !errors.Is(err, ErrBusy) {
		t.Fatalf("second start: %v", err)
	}
	on := false
	if _, err := c.Apply(context.Background(), "php-fpm", Change{Tracing: &on}); !errors.Is(err, ErrBusy) {
		t.Fatalf("manual change while running: %v", err)
	}
	// let the probe through, then hold the first measured arm
	gate <- struct{}{}
	deadline := time.Now().Add(5 * time.Second)
	for b.State().Phase == "probing" && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if !b.Cancel() {
		t.Fatal("cancel found nothing running")
	}
	close(gate)
	st := waitDone(t, b)
	if st.Phase != "cancelled" || !st.Restored {
		t.Fatalf("cancelled %+v", st)
	}
	s := php.get("fpm")
	if !s.Tracing() || !s.Profiling() {
		t.Fatalf("not restored after cancel: %+v", s)
	}
	if c.Busy() {
		t.Fatal("still held")
	}
	if b.Cancel() {
		t.Fatal("cancel with nothing running")
	}
}

func TestBenchRefusesAPageThatDoesNotAnswer200(t *testing.T) {
	b, _, php, n := newTestBench(t)
	n.status.Store(302)
	if _, err := b.Start(context.Background(), "/"); err != nil {
		t.Fatal(err)
	}
	st := waitDone(t, b)
	if st.Phase != "failed" || !strings.Contains(st.Error, "302") || !strings.Contains(st.Error, "https://app.shop.test/") {
		t.Fatalf("failed %+v", st)
	}
	if php.reloads["fpm"] != 1 {
		t.Fatalf("a failed probe switches nothing but the restore: %v", php.reloads)
	}
}

func TestBenchValidatesTheURL(t *testing.T) {
	b, _, _, _ := newTestBench(t)
	for _, u := range []string{
		"https://evil.example/", "https://inspector.shop.test/", "ftp://app.shop.test/", "women.html",
		"https://app.shop.test.evil/", "/a\r\nX-Evil: 1", "",
	} {
		if _, err := b.Start(context.Background(), u); err == nil || errors.Is(err, ErrBusy) {
			t.Errorf("%q: %v", u, err)
		}
	}
	for in, want := range map[string][2]string{
		"https://app.shop.test:443/x": {"app.shop.test", "/x"},
		"http://shop.test":            {"shop.test", "/"},
		"/checkout/cart/?a=1":         {"app.shop.test", "/checkout/cart/?a=1"},
		"https://APP.shop.test/y":     {"app.shop.test", "/y"},
	} {
		host, path, err := b.target(context.Background(), in)
		if err != nil || host != want[0] || path != want[1] {
			t.Errorf("%q: %s %s %v", in, host, path, err)
		}
	}
}

func TestBenchRefusesAnUnavailableSwitch(t *testing.T) {
	b, _, php, _ := newTestBench(t)
	php.set("fpm", docker.IniState{Script: true})
	b.ctl.forget()
	if _, err := b.Start(context.Background(), "/"); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("err %v", err)
	}
	if b.ctl.Busy() {
		t.Fatal("held after a refused start")
	}
}

func TestBenchFailsOnAnErrorPageInsideAnArm(t *testing.T) {
	b, _, php, n := newTestBench(t)
	// The probe runs at the restored setting, full with profiling, so the
	// error page appears only once profiling is switched on again.
	php.set("fpm", docker.IniState{Script: true, DetailSupport: true, Template: true, Loaded: true, Settings: true})
	b.ctl.forget()
	n.profileStatus.Store(500)
	if _, err := b.Start(context.Background(), "/"); err != nil {
		t.Fatal(err)
	}
	st := waitDone(t, b)
	if st.Phase != "failed" || !strings.Contains(st.Error, "Full + profiling") || !strings.Contains(st.Error, "500") {
		t.Fatalf("failed %+v", st)
	}
	for _, a := range st.Arms {
		if a.ID == "full+profile" && len(a.Samples) != 0 {
			t.Fatalf("error pages were timed: %+v", a)
		}
	}
	if !st.Restored {
		t.Fatalf("not restored: %+v", st)
	}
}

func TestBenchSkipsLeanOnAnImageWithoutIt(t *testing.T) {
	b, _, php, _ := newTestBench(t)
	php.set("fpm", docker.IniState{Script: true, Template: true, Loaded: true, Settings: true})
	b.ctl.forget()
	st, err := b.Start(context.Background(), "/")
	if err != nil {
		t.Fatal(err)
	}
	if st.Total != 9 {
		t.Fatalf("total %d, want 3 arms x 3 rounds", st.Total)
	}
	done := waitDone(t, b)
	if done.Phase != "done" || done.Done != 9 {
		t.Fatalf("done %+v", done)
	}
	for _, a := range done.Arms {
		if a.ID == "lean" {
			if len(a.Samples) != 0 || a.DeltaPct != nil || !strings.Contains(a.Skipped, "predates") {
				t.Fatalf("lean %+v", a)
			}
			continue
		}
		if len(a.Samples) != 3 || a.Skipped != "" {
			t.Fatalf("arm %+v", a)
		}
	}
	if php.reloads["fpm"] != 10 {
		t.Fatalf("reloads %v: 9 switches and the restore", php.reloads)
	}
}

func TestBenchReportsARestoreThatDidNotStick(t *testing.T) {
	b, _, php, _ := newTestBench(t)
	php.mu.Lock()
	php.ignoreAfter = 12
	php.mu.Unlock()
	if _, err := b.Start(context.Background(), "/"); err != nil {
		t.Fatal(err)
	}
	st := waitDone(t, b)
	if st.Restored || !strings.Contains(st.RestoreError, "php-fpm is now") {
		t.Fatalf("a restore that changed nothing was reported as done: %+v", st)
	}
}

func TestBenchStartWaitsForAnApplyInFlight(t *testing.T) {
	b, c, php, _ := newTestBench(t)
	entered := make(chan struct{})
	proceed := make(chan struct{})
	var first sync.Once
	php.mu.Lock()
	php.onSwitch = func(ctx context.Context) error {
		first.Do(func() {
			close(entered)
			<-proceed
		})
		return nil
	}
	php.mu.Unlock()
	off := false
	applied := make(chan error, 1)
	go func() {
		_, err := c.Apply(context.Background(), "php-fpm", Change{Tracing: &off})
		applied <- err
	}()
	<-entered
	started := make(chan error, 1)
	go func() {
		_, err := b.Start(context.Background(), "/")
		started <- err
	}()
	select {
	case err := <-started:
		t.Fatalf("Start ran while Apply was switching: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	close(proceed)
	if err := <-applied; err != nil {
		t.Fatal(err)
	}
	if err := <-started; err != nil {
		t.Fatal(err)
	}
	st := waitDone(t, b)
	if st.Previous == nil || st.Previous.Tracing || !st.Restored {
		t.Fatalf("the run did not start from the applied setting: %+v", st)
	}
	if s := php.get("fpm"); s.Tracing() {
		t.Fatalf("the user's off was undone: %+v", s)
	}
}

func TestBenchCloseRestoresAndRefusesNewRuns(t *testing.T) {
	idle, _, _, _ := newTestBench(t)
	if err := idle.Close(context.Background()); err != nil {
		t.Fatalf("close with nothing running: %v", err)
	}
	b, _, php, n := newTestBench(t)
	n.mu.Lock()
	n.gate = make(chan struct{})
	gate := n.gate
	n.mu.Unlock()
	if _, err := b.Start(context.Background(), "/"); err != nil {
		t.Fatal(err)
	}
	gate <- struct{}{}
	closed := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		closed <- b.Close(ctx)
	}()
	close(gate)
	if err := <-closed; err != nil {
		t.Fatal(err)
	}
	if st := b.State(); st.Running || !st.Restored {
		t.Fatalf("close returned before the restore: %+v", st)
	}
	if s := php.get("fpm"); !s.Tracing() || !s.Profiling() {
		t.Fatalf("not restored on close: %+v", s)
	}
	if _, err := b.Start(context.Background(), "/"); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("start after close: %v", err)
	}
}

func TestBenchCloseCutsAHangingRestoreAtItsDeadline(t *testing.T) {
	b, _, php, n := newTestBench(t)
	n.mu.Lock()
	n.gate = make(chan struct{})
	gate := n.gate
	n.mu.Unlock()
	defer close(gate)
	if _, err := b.Start(context.Background(), "/"); err != nil {
		t.Fatal(err)
	}
	gate <- struct{}{}
	// The first arm is switched and its ready check waits at the gate.
	deadline := time.Now().Add(5 * time.Second)
	for {
		php.mu.Lock()
		reloaded := php.reloads["fpm"] > 0
		php.mu.Unlock()
		if reloaded || time.Now().After(deadline) {
			break
		}
		time.Sleep(time.Millisecond)
	}
	php.mu.Lock()
	php.onSwitch = func(ctx context.Context) error {
		<-ctx.Done()
		return ctx.Err()
	}
	php.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	began := time.Now()
	err := b.Close(ctx)
	if took := time.Since(began); took > 2*time.Second {
		t.Fatalf("close took %s past its deadline", took)
	}
	if err == nil {
		t.Fatal("close reported a finished restore")
	}
	st := waitDone(t, b)
	if st.Restored || st.RestoreError == "" {
		t.Fatalf("a cut restore was reported as done: %+v", st)
	}
}

func TestStatsHelpers(t *testing.T) {
	if m := median([]float64{5, 1, 3}); m != 3 {
		t.Fatal(m)
	}
	if m := median([]float64{4, 1, 3, 2}); m != 2.5 {
		t.Fatal(m)
	}
	if m := median(nil); m != 0 {
		t.Fatal(m)
	}
}
