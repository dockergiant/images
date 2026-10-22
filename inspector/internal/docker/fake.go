package docker

import (
	"context"
	"sync"
	"time"
)

type Fake struct {
	EnvName       string
	ContainerList []Container
	ContainerEnvs map[string][]string
	ExecFunc      func(ctx context.Context, spec ExecSpec) (ExecResult, error)
	StreamFunc    func(ctx context.Context, spec ExecSpec, onLine func(string)) error
	LogsFunc      func(ctx context.Context, id string, since time.Time, follow bool, onLine func(LogLine)) error
	LogLines      map[string][]LogLine
	TraefikInfo   TraefikInfo
	TraefikErr    error
	SweepErr      error
	SelfStatsFunc func(ctx context.Context) (Stats, error)

	mu           sync.Mutex
	execs        []ExecSpec
	execTimes    []time.Time
	streams      []ExecSpec
	traefikCalls int
	sweeps       int
	watchers     map[int]func(string)
	nextWatcher  int
}

var _ API = (*Fake)(nil)

func (f *Fake) Env() string {
	return f.EnvName
}

func (f *Fake) Service(ctx context.Context, service string) (*Container, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.ContainerList {
		if c.Service == service && c.Running {
			return &c, nil
		}
	}
	return nil, nil
}

func (f *Fake) Containers(ctx context.Context) ([]Container, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]Container(nil), f.ContainerList...), nil
}

func (f *Fake) SetContainers(list []Container) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ContainerList = list
}

func (f *Fake) ContainerEnv(ctx context.Context, id string) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.ContainerEnvs[id]...), nil
}

func (f *Fake) Exec(ctx context.Context, spec ExecSpec) (ExecResult, error) {
	f.mu.Lock()
	f.execs = append(f.execs, spec)
	f.execTimes = append(f.execTimes, time.Now())
	fn := f.ExecFunc
	f.mu.Unlock()
	if fn == nil {
		return ExecResult{}, nil
	}
	return fn(ctx, spec)
}

func (f *Fake) Stream(ctx context.Context, spec ExecSpec, onLine func(string)) error {
	f.mu.Lock()
	f.streams = append(f.streams, spec)
	fn := f.StreamFunc
	f.mu.Unlock()
	if fn == nil {
		<-ctx.Done()
		return ctx.Err()
	}
	return fn(ctx, spec, onLine)
}

func (f *Fake) Logs(ctx context.Context, id string, since time.Time, follow bool, onLine func(LogLine)) error {
	f.mu.Lock()
	lines := append([]LogLine(nil), f.LogLines[id]...)
	fn := f.LogsFunc
	f.mu.Unlock()
	if fn != nil {
		return fn(ctx, id, since, follow, onLine)
	}
	for _, l := range lines {
		if !since.IsZero() && l.Time.Before(since) {
			continue
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		onLine(l)
	}
	if !follow {
		return nil
	}
	<-ctx.Done()
	return ctx.Err()
}

func (f *Fake) Traefik(ctx context.Context, refresh bool) (TraefikInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.traefikCalls++
	return f.TraefikInfo, f.TraefikErr
}

func (f *Fake) SetTraefik(info TraefikInfo, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.TraefikInfo = info
	f.TraefikErr = err
}

func (f *Fake) Watch(ctx context.Context, onChange func(service string)) {
	f.mu.Lock()
	if f.watchers == nil {
		f.watchers = map[int]func(string){}
	}
	id := f.nextWatcher
	f.nextWatcher++
	f.watchers[id] = onChange
	f.mu.Unlock()
	<-ctx.Done()
	f.mu.Lock()
	delete(f.watchers, id)
	f.mu.Unlock()
}

func (f *Fake) Change(service string) {
	f.mu.Lock()
	fns := make([]func(string), 0, len(f.watchers))
	for _, fn := range f.watchers {
		fns = append(fns, fn)
	}
	f.mu.Unlock()
	for _, fn := range fns {
		fn(service)
	}
}

func (f *Fake) Watchers() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.watchers)
}

func (f *Fake) Sweep(ctx context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sweeps++
	return f.SweepErr
}

func (f *Fake) Execs() []ExecSpec {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]ExecSpec(nil), f.execs...)
}

func (f *Fake) Streams() []ExecSpec {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]ExecSpec(nil), f.streams...)
}

func (f *Fake) TraefikCalls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.traefikCalls
}

func (f *Fake) Sweeps() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.sweeps
}

func (f *Fake) SelfStats(ctx context.Context) (Stats, error) {
	f.mu.Lock()
	fn := f.SelfStatsFunc
	f.mu.Unlock()
	if fn == nil {
		return Stats{}, nil
	}
	return fn(ctx)
}

func (f *Fake) ExecsSince(t time.Time) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, at := range f.execTimes {
		if !at.Before(t) {
			n++
		}
	}
	return n
}
