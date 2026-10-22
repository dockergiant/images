package perf

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dockergiant/images/inspector/internal/docker"
)

// fakePHP plays the php containers: it keeps the ini state per container and
// answers the state read, the switch and the reload the way the image does.
type fakePHP struct {
	mu      sync.Mutex
	state   map[string]docker.IniState
	reloads map[string]int
	fail    map[string]bool
	// switches counts roll-inspector-ini calls; those after ignoreAfter
	// succeed without changing anything, like a CLI toggle racing them.
	switches    int
	ignoreAfter int
	// onSwitch runs before a switch takes the lock, so a test can hold it.
	onSwitch func(ctx context.Context) error
}

func newFakePHP() *fakePHP {
	base := docker.IniState{Script: true, DetailSupport: true, Template: true}
	return &fakePHP{
		state:   map[string]docker.IniState{"fpm": base, "debug": base},
		reloads: map[string]int{},
		fail:    map[string]bool{},
	}
}

func (f *fakePHP) set(id string, s docker.IniState) {
	f.mu.Lock()
	f.state[id] = s
	f.mu.Unlock()
}

func (f *fakePHP) get(id string) docker.IniState {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.state[id]
}

func envOf(spec docker.ExecSpec, key string) string {
	for _, kv := range spec.Env {
		if v, ok := strings.CutPrefix(kv, key+"="); ok {
			return v
		}
	}
	return ""
}

func boolOut(b bool) string {
	if b {
		return "1"
	}
	return "0"
}

func (f *fakePHP) exec(ctx context.Context, spec docker.ExecSpec) (docker.ExecResult, error) {
	if spec.Cmd[0] == "roll-inspector-ini" {
		f.mu.Lock()
		hook := f.onSwitch
		f.mu.Unlock()
		if hook != nil {
			if err := hook(ctx); err != nil {
				return docker.ExecResult{}, err
			}
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	id := spec.Container
	switch spec.Cmd[0] {
	case "sh":
		s := f.state[id]
		out := "script=" + boolOut(s.Script) + "\ndetail=" + boolOut(s.DetailSupport) + "\ntemplate=" + boolOut(s.Template) +
			"\nddtrace=" + boolOut(s.Loaded) + "\nsettings=" + boolOut(s.Settings) + "\nprofile=" + boolOut(s.Profile) +
			"\nnewrelic=" + boolOut(s.NewRelic) + "\nlean=" + boolOut(s.Lean) + "\n"
		return docker.ExecResult{Stdout: []byte(out)}, nil
	case "roll-inspector-ini":
		if f.fail[id] {
			return docker.ExecResult{ExitCode: 1, Stderr: []byte("roll-inspector-ini: this image has no ddtrace\n")}, nil
		}
		f.switches++
		if f.ignoreAfter > 0 && f.switches > f.ignoreAfter {
			return docker.ExecResult{}, nil
		}
		s := f.state[id]
		if spec.Cmd[1] == "on" {
			s.Loaded, s.Settings = true, true
			s.Profile = envOf(spec, "ROLL_INSPECTOR_PROFILE") == "excimer"
			s.Lean = envOf(spec, "ROLL_INSPECTOR_TRACE_DETAIL") == "lean"
		} else {
			s.Loaded, s.Settings, s.Profile, s.Lean = false, false, false, false
		}
		f.state[id] = s
		return docker.ExecResult{}, nil
	case "kill":
		f.reloads[id]++
		return docker.ExecResult{}, nil
	}
	return docker.ExecResult{ExitCode: 127}, nil
}

func newTestController(t *testing.T) (*Controller, *fakePHP, *docker.Fake) {
	t.Helper()
	php := newFakePHP()
	d := &docker.Fake{
		EnvName: "shop",
		ContainerList: []docker.Container{
			{ID: "fpm", Service: "php-fpm", Running: true},
			{ID: "debug", Service: "php-debug", Running: true},
		},
		ContainerEnvs: map[string][]string{
			"fpm":   {"ROLL_INSPECTOR_TRACE=1", "ROLL_INSPECTOR_PROFILE=excimer", "ROLL_INSPECTOR_TRACE_DETAIL=full", "ROLL_NEWRELIC=0"},
			"debug": {"ROLL_INSPECTOR_TRACE=0", "ROLL_INSPECTOR_PROFILE=excimer"},
		},
		ExecFunc: php.exec,
	}
	return NewController(d, "shop"), php, d
}

func TestStateReadsIniFilesAndEnvValues(t *testing.T) {
	c, php, _ := newTestController(t)
	php.set("fpm", docker.IniState{Script: true, DetailSupport: true, Template: true, Loaded: true, Settings: true, Profile: true})
	states := c.States(context.Background())
	if len(states) != 2 || states[0].Service != "php-fpm" || states[1].Service != "php-debug" {
		t.Fatalf("states %+v", states)
	}
	fpm := states[0]
	if !fpm.Available || !fpm.Tracing || fpm.Detail != "full" || !fpm.Profiling || fpm.Differs {
		t.Fatalf("fpm %+v", fpm)
	}
	if fpm.Env.Key != "ROLL_INSPECTOR_TRACE" || !fpm.Env.Tracing || !fpm.Env.Profiling || fpm.Env.Detail != "full" {
		t.Fatalf("fpm env %+v", fpm.Env)
	}
	dbg := states[1]
	if dbg.Tracing || dbg.Env.Key != "ROLL_INSPECTOR_TRACE_DEBUG" || dbg.Env.Tracing || dbg.Env.Detail != "full" {
		t.Fatalf("debug %+v", dbg)
	}
	if dbg.Differs {
		t.Fatal("debug matches its env values")
	}
}

func TestApplySwitchesAndReloads(t *testing.T) {
	c, php, d := newTestController(t)
	ctx := context.Background()
	on, lean := true, "lean"
	st, err := c.Apply(ctx, "php-fpm", Change{Tracing: &on, Detail: &lean})
	if err != nil {
		t.Fatal(err)
	}
	if !st.Tracing || st.Detail != "lean" || !st.Profiling {
		t.Fatalf("state %+v: detail from the request, profiling from .env.roll", st)
	}
	if !php.get("fpm").Lean || php.reloads["fpm"] != 1 || php.reloads["debug"] != 0 {
		t.Fatalf("fake %+v reloads %v", php.get("fpm"), php.reloads)
	}
	if !st.Differs || !strings.Contains(st.Permanent, "ROLL_INSPECTOR_TRACE_DETAIL=lean") || !strings.Contains(st.Permanent, "ROLL_INSPECTOR_PROFILE=excimer") {
		t.Fatalf("permanent %q differs %v", st.Permanent, st.Differs)
	}
	var sw docker.ExecSpec
	for _, e := range d.Execs() {
		if e.Cmd[0] == "roll-inspector-ini" {
			sw = e
		}
	}
	if sw.User != "root" || envOf(sw, "ROLL_ENV_NAME") != "shop" {
		t.Fatalf("switch exec %+v", sw)
	}

	prof := false
	st, err = c.Apply(ctx, "php-fpm", Change{Profiling: &prof})
	if err != nil || st.Profiling || st.Detail != "lean" {
		t.Fatalf("profiling keeps detail: %+v %v", st, err)
	}
	off := false
	st, err = c.Apply(ctx, "php-fpm", Change{Tracing: &off})
	if err != nil || st.Tracing || php.get("fpm").Loaded || php.reloads["fpm"] != 3 {
		t.Fatalf("off: %+v %v", st, err)
	}
	if !strings.Contains(st.Permanent, "ROLL_INSPECTOR_TRACE=0") {
		t.Fatalf("permanent %q", st.Permanent)
	}
}

func TestApplyRefusesWhatCannotWork(t *testing.T) {
	c, php, d := newTestController(t)
	ctx := context.Background()
	on, prof, lean, bogus := true, true, "lean", "medium"

	if _, err := c.Apply(ctx, "php-debug", Change{Profiling: &prof}); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("profiling while tracing is off: %v", err)
	}
	if _, err := c.Apply(ctx, "php-fpm", Change{Tracing: &on, Detail: &bogus}); err == nil || errors.Is(err, ErrUnavailable) {
		t.Fatalf("bad detail: %v", err)
	}
	if _, err := c.Apply(ctx, "varnish", Change{Tracing: &on}); err == nil {
		t.Fatal("unknown service accepted")
	}
	if _, err := c.Apply(ctx, "php-fpm", Change{}); err == nil {
		t.Fatal("empty change accepted")
	}

	php.set("fpm", docker.IniState{Script: true})
	st, err := c.Apply(ctx, "php-fpm", Change{Tracing: &on})
	if !errors.Is(err, ErrUnavailable) || st.Available || !strings.Contains(st.Reason, "predates tracing") {
		t.Fatalf("old image: %+v %v", st, err)
	}

	php.set("fpm", docker.IniState{Script: true, Template: true})
	st, err = c.Apply(ctx, "php-fpm", Change{Tracing: &on, Detail: &lean})
	if !errors.Is(err, ErrUnavailable) || !st.Available || st.DetailReason == "" {
		t.Fatalf("no lean support: %+v %v", st, err)
	}

	php.set("fpm", docker.IniState{Script: true, Template: true, DetailSupport: true, NewRelic: true})
	st, err = c.Apply(ctx, "php-fpm", Change{Tracing: &on})
	if !errors.Is(err, ErrUnavailable) || !strings.Contains(st.Reason, "New Relic") {
		t.Fatalf("new relic: %+v %v", st, err)
	}

	d.SetContainers([]docker.Container{{ID: "fpm", Service: "php-fpm", Running: true}})
	st = c.State(ctx, "php-debug", true)
	if st.Present || st.Available || !strings.Contains(st.Reason, "not running") {
		t.Fatalf("missing debug %+v", st)
	}
}

func TestApplyReportsAFailedSwitch(t *testing.T) {
	c, php, _ := newTestController(t)
	php.fail["fpm"] = true
	on := true
	if _, err := c.Apply(context.Background(), "php-fpm", Change{Tracing: &on}); err == nil || !strings.Contains(err.Error(), "no ddtrace") {
		t.Fatalf("err %v", err)
	}
	if php.reloads["fpm"] != 0 {
		t.Fatal("reloaded after a failed switch")
	}
}

func TestBusyRefusesManualChanges(t *testing.T) {
	c, _, _ := newTestController(t)
	release, ok := c.hold()
	if !ok {
		t.Fatal("hold")
	}
	if _, ok := c.hold(); ok {
		t.Fatal("second hold")
	}
	on := true
	if _, err := c.Apply(context.Background(), "php-fpm", Change{Tracing: &on}); !errors.Is(err, ErrBusy) {
		t.Fatalf("err %v", err)
	}
	release()
	if _, err := c.Apply(context.Background(), "php-fpm", Change{Tracing: &on}); err != nil {
		t.Fatal(err)
	}
}

func TestStateIsCachedUntilAChange(t *testing.T) {
	c, _, d := newTestController(t)
	ctx := context.Background()
	c.States(ctx)
	n := len(d.Execs())
	c.States(ctx)
	if len(d.Execs()) != n {
		t.Fatal("second read within the cache time ran execs")
	}
	on := true
	c.Apply(ctx, "php-debug", Change{Tracing: &on})
	st := c.States(ctx)
	if !st[1].Tracing {
		t.Fatal("cache not refreshed after a change")
	}
	if !slices.ContainsFunc(d.Execs(), func(e docker.ExecSpec) bool { return e.Container == "debug" && e.Cmd[0] == "kill" }) {
		t.Fatal("debug not reloaded")
	}
}

func TestSummary(t *testing.T) {
	c, php, _ := newTestController(t)
	php.set("fpm", docker.IniState{Script: true, DetailSupport: true, Template: true, Loaded: true, Settings: true, Lean: true})
	s := c.Summary(context.Background())
	if !s.Tracing || s.Detail != "lean" || s.Profiling || s.Label != "Tracing · lean" {
		t.Fatalf("summary %+v", s)
	}
	php.set("fpm", docker.IniState{Script: true, Template: true})
	c.forget()
	if s := c.Summary(context.Background()); s.Tracing || s.Label != "Tracing off" {
		t.Fatalf("off summary %+v", s)
	}
}

func TestTracingOnTakesDetailAndProfilingFromEnv(t *testing.T) {
	c, php, d := newTestController(t)
	d.ContainerEnvs["fpm"] = []string{"ROLL_INSPECTOR_TRACE=1", "ROLL_INSPECTOR_PROFILE=excimer", "ROLL_INSPECTOR_TRACE_DETAIL=lean"}
	ctx := context.Background()
	on := true
	st, err := c.Apply(ctx, "php-fpm", Change{Tracing: &on})
	if err != nil || !st.Tracing || st.Detail != "lean" || !st.Profiling || st.Differs {
		t.Fatalf("on from off: %+v %v", st, err)
	}
	off, full := false, "full"
	c.Apply(ctx, "php-fpm", Change{Tracing: &off})
	st, err = c.Apply(ctx, "php-fpm", Change{Tracing: &on, Detail: &full, Profiling: &off})
	if err != nil || st.Detail != "full" || st.Profiling || !php.get("fpm").Loaded {
		t.Fatalf("explicit values win: %+v %v", st, err)
	}
}

func TestSummaryKeepsItsReadForAMinute(t *testing.T) {
	c, _, d := newTestController(t)
	now := time.Unix(1000, 0)
	c.now = func() time.Time { return now }
	ctx := context.Background()
	reads := func() int {
		n := 0
		for _, e := range d.Execs() {
			if e.Cmd[0] == "sh" {
				n++
			}
		}
		return n
	}
	c.Summary(ctx)
	now = now.Add(30 * time.Second)
	c.Summary(ctx)
	if n := reads(); n != 1 {
		t.Fatalf("the pill read php-fpm %d times in 30 s", n)
	}
	// The Performance page still reads within stateTTL.
	c.States(ctx)
	if n := reads(); n != 3 {
		t.Fatalf("States read %d times in all, want a fresh read of both", n)
	}
	now = now.Add(summaryTTL)
	c.Summary(ctx)
	if n := reads(); n != 4 {
		t.Fatalf("an expired pill read did not refresh: %d", n)
	}
}
