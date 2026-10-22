package docker

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestIniSwitchOnIsFixedArgvAsRoot(t *testing.T) {
	spec, err := IniSwitch("c1", true, TraceSettings{Env: "shop", Profile: "excimer", Detail: "lean"})
	if err != nil {
		t.Fatal(err)
	}
	if spec.Container != "c1" || spec.User != "root" || spec.Privileged {
		t.Fatalf("spec %+v", spec)
	}
	if !slices.Equal(spec.Cmd, []string{"roll-inspector-ini", "on"}) {
		t.Fatalf("cmd %q", spec.Cmd)
	}
	want := []string{"ROLL_ENV_NAME=shop", "ROLL_INSPECTOR_PROFILE=excimer", "ROLL_INSPECTOR_TRACE_DETAIL=lean"}
	if !slices.Equal(spec.Env, want) {
		t.Fatalf("env %q", spec.Env)
	}
}

func TestIniSwitchOffAndDefaults(t *testing.T) {
	spec, err := IniSwitch("c1", false, TraceSettings{Env: "shop"})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(spec.Cmd, []string{"roll-inspector-ini", "off"}) {
		t.Fatalf("cmd %q", spec.Cmd)
	}
	if !slices.Contains(spec.Env, "ROLL_INSPECTOR_PROFILE=off") || !slices.Contains(spec.Env, "ROLL_INSPECTOR_TRACE_DETAIL=full") {
		t.Fatalf("env %q", spec.Env)
	}
}

func TestIniSwitchRejectsUnknownValues(t *testing.T) {
	for _, s := range []TraceSettings{
		{Env: "shop", Profile: "xhprof"},
		{Env: "shop", Detail: "everything"},
		{Env: "shop; rm -rf /"},
		{Env: ""},
		{Env: "shop", Detail: "lean\nX=1"},
	} {
		if _, err := IniSwitch("c1", true, s); err == nil {
			t.Errorf("%+v: want an error", s)
		}
	}
	if _, err := IniSwitch("", true, TraceSettings{Env: "shop"}); err == nil {
		t.Error("no container: want an error")
	}
}

func TestFPMReload(t *testing.T) {
	spec := FPMReload("c2")
	if spec.Container != "c2" || spec.User != "root" || !slices.Equal(spec.Cmd, []string{"kill", "-USR2", "1"}) {
		t.Fatalf("spec %+v", spec)
	}
}

func TestIniStateSpecHasNoInput(t *testing.T) {
	spec := IniStateRead("c3")
	if spec.Container != "c3" || spec.User != "root" || len(spec.Cmd) != 3 || spec.Cmd[0] != "sh" || spec.Cmd[1] != "-c" {
		t.Fatalf("spec %+v", spec)
	}
	if spec.Stdin != nil || len(spec.Env) != 0 {
		t.Fatal("the state read takes no input")
	}
}

func TestParseIniState(t *testing.T) {
	out := "script=1\ndetail=1\ntemplate=1\nddtrace=1\nsettings=1\nprofile=0\nnewrelic=0\nlean=1\n"
	s := ParseIniState([]byte(out))
	want := IniState{Script: true, DetailSupport: true, Template: true, Loaded: true, Settings: true, Lean: true}
	if s != want {
		t.Fatalf("got %+v", s)
	}
	if !s.Tracing() || s.Detail() != "lean" || s.Profiling() {
		t.Fatalf("tracing %v detail %s profiling %v", s.Tracing(), s.Detail(), s.Profiling())
	}
	off := ParseIniState([]byte("script=1\ntemplate=1\nddtrace=0\nsettings=0\nprofile=0\nlean=0\n"))
	if off.Tracing() || off.Detail() != "full" {
		t.Fatalf("off %+v", off)
	}
	if ParseIniState([]byte("garbage\n=1\nddtrace=yes\n")) != (IniState{}) {
		t.Fatal("unknown lines are ignored")
	}
}

func TestIniStateScriptOnFakeConfD(t *testing.T) {
	// The script reads only fixed paths; a smoke check that it names each key
	// ParseIniState knows, so the two never drift apart.
	for _, k := range []string{"script=", "detail=", "template=", "ddtrace=", "settings=", "profile=", "newrelic=", "lean="} {
		if !strings.Contains(iniStateScript, strings.TrimSuffix(k, "=")) {
			t.Errorf("script lacks %s", k)
		}
	}
}

func TestParseStats(t *testing.T) {
	body := `{"read":"2026-10-21T10:00:00Z","cpu_stats":{"cpu_usage":{"total_usage":5000000000},"online_cpus":4},
"memory_stats":{"usage":209715200,"limit":805306368,"stats":{"inactive_file":10485760}},"pids_stats":{"current":12}}`
	s, err := parseStats(strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if s.CPUNanos != 5e9 || s.OnlineCPUs != 4 || s.MemBytes != 209715200-10485760 || s.MemLimit != 805306368 || s.PIDs != 12 {
		t.Fatalf("stats %+v", s)
	}
	if !s.Read.Equal(time.Date(2026, 10, 21, 10, 0, 0, 0, time.UTC)) {
		t.Fatalf("read %v", s.Read)
	}
}

func TestExecLogCountsSince(t *testing.T) {
	var l execLog
	t0 := time.Unix(1000, 0)
	for i := range 5 {
		l.add(t0.Add(time.Duration(i) * time.Minute))
	}
	if n := l.since(t0.Add(2 * time.Minute)); n != 3 {
		t.Fatalf("since: %d", n)
	}
	for i := range execLogSize + 10 {
		l.add(t0.Add(time.Hour + time.Duration(i)*time.Millisecond))
	}
	if n := l.since(t0); n != execLogSize {
		t.Fatalf("capped: %d", n)
	}
}

func TestFakeSelfStatsAndExecsSince(t *testing.T) {
	f := &Fake{SelfStatsFunc: func(context.Context) (Stats, error) { return Stats{MemBytes: 7}, nil }}
	s, err := f.SelfStats(context.Background())
	if err != nil || s.MemBytes != 7 {
		t.Fatalf("%+v %v", s, err)
	}
	before := time.Now().Add(-time.Second)
	f.Exec(context.Background(), ExecSpec{Container: "x", Cmd: []string{"true"}})
	if f.ExecsSince(before) != 1 || f.ExecsSince(time.Now().Add(time.Hour)) != 0 {
		t.Fatal("fake exec log")
	}
}
