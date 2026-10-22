package procmap

import (
	"context"
	"errors"
	"io"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dockergiant/images/inspector/internal/docker"
)

const tcpHeader = "T   sl  local_address rem_address   st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode"

func tcpRow(local, remote, state, inode string) string {
	return "T    0: " + local + " " + remote + " " + state + " 00000000:00000000 00:00000000 00000000    33        0 " + inode + " 1 0000000000000000 20 4 30 10 -1"
}

var fpmWalk = strings.Join([]string{
	tcpHeader,
	tcpRow("0A9CA8C0:9306", "059CA8C0:18EB", "01", "201"),
	tcpRow("0A9CA8C0:9308", "059CA8C0:18EB", "01", "202"),
	tcpRow("0A9CA8C0:C350", "079CA8C0:1628", "01", "301"),
	tcpRow("0100007F:A028", "0100007F:2328", "01", "501"),
	tcpRow("0A9CA8C0:95DA", "059CA8C0:18EB", "06", "0"),
	tcpHeader,
	tcpRow("00000000000000000000000000000000:2328", "00000000000000000000000000000000:0000", "0A", "100"),
	tcpRow("0000000000000000FFFF00000A9CA8C0:2328", "0000000000000000FFFF00000B9CA8C0:BA88", "01", "401"),
	tcpRow("0000000000000000FFFF00000A9CA8C0:2328", "0000000000000000FFFF00000B9CA8C0:BA90", "01", "402"),
	"P 1 php-fpm: master process (/usr/local/etc/php-fpm.conf) ",
	"F 1 100",
	"P 20 php-fpm: pool www ",
	"F 20 100",
	"F 20 201",
	"F 20 401",
	"P 21 php-fpm: pool www ",
	"F 21 100",
	"F 21 202",
	"F 21 402",
	"P 30 /usr/local/bin/php /var/www/html/bin/magento queue:consumers:start async.operations.all --single-thread ",
	"F 30 301",
	"F 30 501",
}, "\n") + "\n"

var debugWalk = strings.Join([]string{
	tcpHeader,
	tcpRow("0C9CA8C0:9C40", "059CA8C0:18EB", "01", "601"),
	"P 40 php-fpm: pool www ",
	"F 40 601",
}, "\n") + "\n"

type walker struct {
	mu      sync.Mutex
	outputs map[string]string
	errs    map[string]error
	stdins  []string
}

func (w *walker) set(id, out string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.outputs[id] = out
}

func (w *walker) fail(id string, err error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.errs[id] = err
}

func (w *walker) exec(ctx context.Context, spec docker.ExecSpec) (docker.ExecResult, error) {
	var in []byte
	if spec.Stdin != nil {
		in, _ = io.ReadAll(spec.Stdin)
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	w.stdins = append(w.stdins, string(in))
	if err := w.errs[spec.Container]; err != nil {
		return docker.ExecResult{}, err
	}
	return docker.ExecResult{Stdout: []byte(w.outputs[spec.Container])}, nil
}

func setup() (*docker.Fake, *walker, *Map) {
	w := &walker{
		outputs: map[string]string{"fpm1": fpmWalk, "dbg1": debugWalk},
		errs:    map[string]error{},
	}
	f := &docker.Fake{
		EnvName: "shop",
		ContainerList: []docker.Container{
			{ID: "fpm1", Name: "shop-php-fpm-1", Service: "php-fpm", IP: "192.168.156.10", Running: true},
			{ID: "dbg1", Name: "shop-php-debug-1", Service: "php-debug", IP: "192.168.156.12", Running: true},
			{ID: "red1", Name: "shop-redis-1", Service: "redis", IP: "192.168.156.5", Running: true},
			{ID: "old1", Name: "shop-php-fpm-2", Service: "php-fpm", IP: "192.168.156.20", Running: false},
		},
		ExecFunc: w.exec,
	}
	return f, w, New(f)
}

func TestRefreshLookup(t *testing.T) {
	_, _, m := setup()
	if err := m.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}

	p, ok := m.Lookup("192.168.156.10", 37638)
	if !ok {
		t.Fatal("worker socket not mapped")
	}
	want := Proc{PID: 20, Container: "php-fpm", Role: "fpm-worker", Cmd: "php-fpm: pool www"}
	if p != want {
		t.Fatalf("Lookup = %+v, want %+v", p, want)
	}
	if p, ok := m.Lookup("::ffff:192.168.156.10", 37640); !ok || p.PID != 21 {
		t.Fatalf("v4-mapped lookup = %+v %v", p, ok)
	}
	if p, ok := m.Lookup("192.168.156.10", 50000); !ok || p.Role != "consumer" || p.PID != 30 {
		t.Fatalf("consumer lookup = %+v %v", p, ok)
	}
	if p, ok := m.Lookup("192.168.156.12", 40000); !ok || p.Container != "php-debug" || p.PID != 40 {
		t.Fatalf("php-debug lookup = %+v %v", p, ok)
	}

	misses := []struct {
		ip   string
		port int
	}{
		{"192.168.156.10", 9000},
		{"127.0.0.1", 41000},
		{"0.0.0.0", 9000},
		{"192.168.156.10", 38362},
		{"not-an-ip", 37638},
		{"192.168.156.10", 70000},
	}
	for _, c := range misses {
		if p, ok := m.Lookup(c.ip, c.port); ok {
			t.Errorf("Lookup(%s, %d) = %+v, want no mapping", c.ip, c.port, p)
		}
	}

	if svc, ok := m.ContainerOf("192.168.156.5"); !ok || svc != "redis" {
		t.Fatalf("ContainerOf redis = %q %v", svc, ok)
	}
	if svc, ok := m.ContainerOf("::ffff:192.168.156.12"); !ok || svc != "php-debug" {
		t.Fatalf("ContainerOf php-debug = %q %v", svc, ok)
	}
	if _, ok := m.ContainerOf("192.168.156.20"); ok {
		t.Fatal("stopped container resolved")
	}
	if _, ok := m.ContainerOf("10.0.0.1"); ok {
		t.Fatal("unknown ip resolved")
	}
}

func TestExecSpecIsFixed(t *testing.T) {
	f, w, m := setup()
	if err := m.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}

	execs := f.Execs()
	var targets []string
	for _, e := range execs {
		targets = append(targets, e.Container)
		if e.User != "root" || !e.Privileged {
			t.Errorf("exec %s runs as %q privileged=%v", e.Container, e.User, e.Privileged)
		}
		if !slices.Equal(e.Cmd, []string{"sh", "-s"}) {
			t.Errorf("exec cmd = %q", e.Cmd)
		}
		if len(e.Env) != 0 {
			t.Errorf("exec env = %q", e.Env)
		}
		if !e.KillOnCancel {
			t.Errorf("exec %s survives its timeout", e.Container)
		}
	}
	slices.Sort(targets)
	if !slices.Equal(targets, []string{"dbg1", "fpm1"}) {
		t.Fatalf("walked %v, want php-fpm and php-debug only", targets)
	}
	for _, in := range w.stdins {
		if in != walkScript {
			t.Fatal("stdin is not the embedded walk script")
		}
	}
	if !strings.Contains(walkScript, "/proc/net/tcp6") {
		t.Fatal("unexpected walk script")
	}
}

func TestRefreshKeepsDataOnPartialFailure(t *testing.T) {
	f, w, m := setup()
	if err := m.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}

	w.fail("dbg1", errors.New("exec refused"))
	w.set("fpm1", debugWalk)
	if err := m.Refresh(context.Background()); err == nil || !strings.Contains(err.Error(), "exec refused") {
		t.Fatalf("Refresh error = %v", err)
	}
	if _, ok := m.Lookup("192.168.156.12", 40000); !ok {
		t.Fatal("php-debug mapping dropped after a failed walk")
	}
	if _, ok := m.Lookup("192.168.156.10", 37638); ok {
		t.Fatal("php-fpm mapping not replaced by the new walk")
	}

	w.set("fpm1", fpmWalk)
	f.SetContainers(f.ContainerList[:1])
	if err := m.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, ok := m.Lookup("192.168.156.12", 40000); ok {
		t.Fatal("mapping of a removed container kept")
	}
	if _, ok := m.ContainerOf("192.168.156.12"); ok {
		t.Fatal("ip of a removed container kept")
	}
}

func TestRefreshExitCode(t *testing.T) {
	f, _, m := setup()
	f.ExecFunc = func(ctx context.Context, spec docker.ExecSpec) (docker.ExecResult, error) {
		return docker.ExecResult{ExitCode: 126, Stderr: []byte("permission denied\n")}, nil
	}
	err := m.Refresh(context.Background())
	if err == nil || !strings.Contains(err.Error(), "permission denied") || !strings.Contains(err.Error(), "126") {
		t.Fatalf("Refresh error = %v", err)
	}
}

func TestMissRateLimit(t *testing.T) {
	f, w, m := setup()
	clock := time.Unix(1_800_000_000, 0)
	m.now = func() time.Time { return clock }
	if err := m.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	base := len(f.Execs())

	for range 50 {
		m.Miss("192.168.156.10", 41000)
		m.wg.Wait()
		clock = clock.Add(20 * time.Millisecond)
	}
	if got := len(f.Execs()) - base; got != 2 {
		t.Fatalf("50 misses in 1 s ran %d walks, want 2", got)
	}

	base = len(f.Execs())
	m.Miss("192.168.156.10", 37638)
	m.Miss("192.168.156.5", 50000)
	m.Miss("10.0.0.1", 50000)
	m.wg.Wait()
	if got := len(f.Execs()) - base; got != 0 {
		t.Fatalf("misses on mapped, non-PHP or unknown addresses ran %d walks", got)
	}

	w.set("dbg1", strings.Replace(debugWalk, "0C9CA8C0:9C40", "0C9CA8C0:9C41", 1))
	clock = clock.Add(time.Second)
	m.Miss("192.168.156.12", 40001)
	m.Miss("192.168.156.10", 41001)
	m.wg.Wait()
	execs := f.Execs()[base:]
	if len(execs) != 2 {
		t.Fatalf("misses on two containers ran %d walks, want 2", len(execs))
	}
	if p, ok := m.Lookup("192.168.156.12", 40001); !ok || p.PID != 40 {
		t.Fatalf("miss refresh did not map the new socket: %+v %v", p, ok)
	}
	if _, ok := m.Lookup("192.168.156.10", 37638); !ok {
		t.Fatal("miss refresh of php-debug dropped php-fpm")
	}
}

func TestRunRefreshesOnChange(t *testing.T) {
	f, _, m := setup()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		m.Run(ctx)
		close(done)
	}()

	waitFor(t, func() bool {
		_, ok := m.Lookup("192.168.156.10", 37638)
		return ok && f.Watchers() == 1
	})
	f.Change("php-fpm")
	waitFor(t, func() bool { return len(f.Execs()) >= 4 })

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after cancel")
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition not met within 2 s")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestRunReportsRefreshErrors(t *testing.T) {
	f, _, m := setup()
	f.ExecFunc = func(ctx context.Context, spec docker.ExecSpec) (docker.ExecResult, error) {
		return docker.ExecResult{ExitCode: 126, Stderr: []byte("permission denied\n")}, nil
	}
	errs := make(chan error, 8)
	m.OnError = func(err error) {
		select {
		case errs <- err:
		default:
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go m.Run(ctx)
	select {
	case err := <-errs:
		if !strings.Contains(err.Error(), "permission denied") {
			t.Fatalf("error %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run swallowed the refresh error")
	}
}

func TestStalledWalkIsNotRepeated(t *testing.T) {
	f, w, m := setup()
	if err := m.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	stall := make(chan struct{})
	stalled := make(chan struct{}, 1)
	f.ExecFunc = func(ctx context.Context, spec docker.ExecSpec) (docker.ExecResult, error) {
		if spec.Container == "fpm1" {
			select {
			case stalled <- struct{}{}:
				<-stall
			default:
			}
		}
		return w.exec(ctx, spec)
	}
	first := make(chan error, 1)
	go func() { first <- m.Refresh(context.Background()) }()
	<-stalled
	base := len(f.Execs())

	if err := m.Refresh(context.Background()); err != nil {
		t.Fatalf("Refresh beside a stalled walk = %v", err)
	}
	m.Miss("192.168.156.10", 41000)
	m.wg.Wait()
	for _, e := range f.Execs()[base:] {
		if e.Container == "fpm1" {
			t.Fatal("a second walk started in php-fpm while the first was still running")
		}
	}
	if _, ok := m.Lookup("192.168.156.10", 37638); !ok {
		t.Fatal("skipped container lost its mapping")
	}

	close(stall)
	if err := <-first; err != nil {
		t.Fatal(err)
	}
	if err := m.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, e := range f.Execs()[base:] {
		if e.Container == "fpm1" {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("php-fpm walks after the stall cleared = %d, want 1", n)
	}
}
