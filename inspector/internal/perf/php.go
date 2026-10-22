// Package perf shows what Inspector costs the shop and switches the costly
// parts off from inside Inspector: PHP tracing, its detail, profiling, and a
// benchmark that measures the difference.
package perf

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/dockergiant/images/inspector/internal/docker"
)

const (
	stateTTL = 5 * time.Second
	// summaryTTL serves the top-bar pill, which every tab polls every 10 s:
	// with stateTTL each poll would miss and exec into php-fpm as root. A
	// switch made in Inspector forgets the cache, so only an outside
	// `roll inspector trace` reaches the pill this late.
	summaryTTL  = time.Minute
	execTimeout = 20 * time.Second
)

var (
	ErrBusy        = errors.New("a benchmark is running; it owns the tracing switches until it ends")
	ErrUnavailable = errors.New("switch unavailable")
	ErrInvalid     = errors.New("invalid setting")
)

// Services are the containers tracing can run in, php-fpm first.
var Services = []string{"php-fpm", "php-debug"}

// EnvValues are the .env.roll values the container started with.
type EnvValues struct {
	Key       string `json:"key"`
	Tracing   bool   `json:"tracing"`
	Detail    string `json:"detail"`
	Profiling bool   `json:"profiling"`
}

type PHPState struct {
	Service      string    `json:"service"`
	Present      bool      `json:"present"`
	Available    bool      `json:"available"`
	Reason       string    `json:"reason,omitempty"`
	Tracing      bool      `json:"tracing"`
	Detail       string    `json:"detail"`
	Profiling    bool      `json:"profiling"`
	DetailReason string    `json:"detailReason,omitempty"`
	Env          EnvValues `json:"env"`
	Differs      bool      `json:"differs"`
	Permanent    string    `json:"permanent"`
	Error        string    `json:"error,omitempty"`
}

// Change names the switches a request moves; nil leaves one as it is.
type Change struct {
	Tracing   *bool   `json:"tracing,omitempty"`
	Detail    *string `json:"detail,omitempty"`
	Profiling *bool   `json:"profiling,omitempty"`
}

// Want is a complete setting for one container.
type Want struct {
	Tracing   bool   `json:"tracing"`
	Detail    string `json:"detail"`
	Profiling bool   `json:"profiling"`
}

func (s PHPState) want() Want {
	return Want{Tracing: s.Tracing, Detail: s.Detail, Profiling: s.Profiling}
}

type cached struct {
	state PHPState
	id    string
	at    time.Time
}

// Controller reads and switches the tracing ini files in the php containers.
type Controller struct {
	docker docker.API
	env    string
	now    func() time.Time

	applyMu sync.Mutex

	mu    sync.Mutex
	cache map[string]cached
	busy  bool
}

func NewController(d docker.API, env string) *Controller {
	return &Controller{docker: d, env: env, now: time.Now, cache: map[string]cached{}}
}

func known(service string) bool {
	for _, s := range Services {
		if s == service {
			return true
		}
	}
	return false
}

// hold gives the benchmark the switches; Apply refuses while it holds them.
func (c *Controller) hold() (func(), bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.busy {
		return nil, false
	}
	c.busy = true
	var once sync.Once
	return func() {
		once.Do(func() {
			c.mu.Lock()
			c.busy = false
			c.mu.Unlock()
		})
	}, true
}

func (c *Controller) Busy() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.busy
}

func (c *Controller) forget() {
	c.mu.Lock()
	c.cache = map[string]cached{}
	c.mu.Unlock()
}

func (c *Controller) States(ctx context.Context) []PHPState {
	out := make([]PHPState, len(Services))
	var wg sync.WaitGroup
	for i, s := range Services {
		wg.Go(func() { out[i] = c.State(ctx, s, false) })
	}
	wg.Wait()
	return out
}

func (c *Controller) State(ctx context.Context, service string, fresh bool) PHPState {
	st, _ := c.read(ctx, service, fresh)
	return st
}

func (c *Controller) read(ctx context.Context, service string, fresh bool) (PHPState, string) {
	maxAge := stateTTL
	if fresh {
		maxAge = 0
	}
	return c.readWithin(ctx, service, maxAge)
}

func (c *Controller) readWithin(ctx context.Context, service string, maxAge time.Duration) (PHPState, string) {
	c.mu.Lock()
	hit, ok := c.cache[service]
	c.mu.Unlock()
	if ok && c.now().Sub(hit.at) < maxAge {
		return hit.state, hit.id
	}
	ctx, cancel := context.WithTimeout(ctx, execTimeout)
	defer cancel()
	st, id := c.load(ctx, service)
	c.mu.Lock()
	c.cache[service] = cached{state: st, id: id, at: c.now()}
	c.mu.Unlock()
	return st, id
}

func (c *Controller) load(ctx context.Context, service string) (PHPState, string) {
	st := PHPState{Service: service, Detail: "full", Env: EnvValues{Key: envKey(service), Detail: "full"}}
	ct, err := c.docker.Service(ctx, service)
	if err != nil {
		st.Reason = "Docker cannot list the containers: " + err.Error()
		return st, ""
	}
	if ct == nil {
		st.Reason = service + " is not running in this env"
		st.Permanent = permanent(service, Want{Detail: "full"})
		return st, ""
	}
	st.Present = true
	env, err := c.docker.ContainerEnv(ctx, ct.ID)
	newrelic := false
	if err == nil {
		vars := envMap(env)
		st.Env.Tracing = vars["ROLL_INSPECTOR_TRACE"] == "1"
		st.Env.Profiling = vars["ROLL_INSPECTOR_PROFILE"] == "excimer"
		if vars["ROLL_INSPECTOR_TRACE_DETAIL"] == "lean" {
			st.Env.Detail = "lean"
		}
		newrelic = vars["ROLL_NEWRELIC"] == "1"
	}
	res, err := c.docker.Exec(ctx, docker.IniStateRead(ct.ID))
	if err != nil || res.ExitCode != 0 {
		if err == nil {
			err = fmt.Errorf("exit %d: %s", res.ExitCode, strings.TrimSpace(string(res.Stderr)))
		}
		st.Reason = "Inspector cannot read the tracing state of " + service + ": " + err.Error()
		st.Error = err.Error()
		return st, ct.ID
	}
	ini := docker.ParseIniState(res.Stdout)
	st.Tracing, st.Detail, st.Profiling = ini.Tracing(), ini.Detail(), ini.Profiling()
	switch {
	case !ini.Script || !ini.Template:
		st.Reason = "this " + service + " image predates tracing: run roll env pull && roll env up"
	case newrelic || ini.NewRelic:
		st.Reason = "New Relic is on (ROLL_NEWRELIC=1), and tracing cannot run next to it"
	default:
		st.Available = true
	}
	if !ini.DetailSupport {
		st.DetailReason = "this " + service + " image predates lean tracing: run roll env pull && roll env up"
	} else if !st.Tracing {
		st.DetailReason = "tracing is off"
	}
	st.Differs = st.want() != st.Env.want()
	st.Permanent = permanent(service, st.want())
	return st, ct.ID
}

func (e EnvValues) want() Want {
	return Want{Tracing: e.Tracing, Detail: e.Detail, Profiling: e.Tracing && e.Profiling}
}

func envKey(service string) string {
	if service == "php-debug" {
		return "ROLL_INSPECTOR_TRACE_DEBUG"
	}
	return "ROLL_INSPECTOR_TRACE"
}

// permanent is the .env.roll text that makes a setting survive a restart.
// Detail and profiling are shared by both containers there.
func permanent(service string, w Want) string {
	flag := "0"
	if w.Tracing {
		flag = "1"
	}
	profile := "off"
	if w.Profiling {
		profile = "excimer"
	}
	detail := w.Detail
	if detail == "" {
		detail = "full"
	}
	lines := []string{envKey(service) + "=" + flag}
	if w.Tracing {
		lines = append(lines, "ROLL_INSPECTOR_TRACE_DETAIL="+detail, "ROLL_INSPECTOR_PROFILE="+profile)
	}
	return strings.Join(lines, "\n")
}

func envMap(env []string) map[string]string {
	out := map[string]string{}
	for _, kv := range env {
		if k, v, ok := strings.Cut(kv, "="); ok {
			out[k] = v
		}
	}
	return out
}

// Apply moves the named switches of one container, unless a benchmark holds
// them, and returns the state read back afterwards.
func (c *Controller) Apply(ctx context.Context, service string, ch Change) (PHPState, error) {
	if !known(service) {
		return PHPState{}, fmt.Errorf("%w: unknown service %q, use php-fpm or php-debug", ErrInvalid, service)
	}
	if ch.Tracing == nil && ch.Detail == nil && ch.Profiling == nil {
		return PHPState{}, fmt.Errorf("%w: nothing to change, send tracing, detail or profiling", ErrInvalid)
	}
	if ch.Detail != nil && *ch.Detail != "full" && *ch.Detail != "lean" {
		return PHPState{}, fmt.Errorf("%w: detail %q is not full or lean", ErrInvalid, *ch.Detail)
	}
	// Lock before the Busy check: Bench.Start takes applyMu around hold, so
	// a benchmark either starts after this change or this one sees it.
	c.applyMu.Lock()
	defer c.applyMu.Unlock()
	if c.Busy() {
		return c.State(ctx, service, false), ErrBusy
	}
	st, _ := c.read(ctx, service, true)
	w := st.want()
	if ch.Tracing != nil {
		// With tracing off there are no ini files left to remember detail and
		// profiling, so switching it on starts from the .env.roll values.
		if *ch.Tracing && !st.Tracing {
			w.Detail, w.Profiling = st.Env.Detail, st.Env.Profiling
		}
		w.Tracing = *ch.Tracing
	}
	if !w.Tracing && (ch.Detail != nil || ch.Profiling != nil) && ch.Tracing == nil {
		return st, fmt.Errorf("%w: tracing is off in %s, so there is no detail or profiling to switch", ErrUnavailable, service)
	}
	if ch.Detail != nil {
		w.Detail = *ch.Detail
	}
	if ch.Profiling != nil {
		w.Profiling = *ch.Profiling
	}
	return c.set(ctx, service, st, w)
}

func (c *Controller) set(ctx context.Context, service string, st PHPState, w Want) (PHPState, error) {
	if !st.Available {
		return st, fmt.Errorf("%w: %s", ErrUnavailable, st.Reason)
	}
	if w.Tracing && w.Detail == "lean" && strings.Contains(st.DetailReason, "predates") {
		return st, fmt.Errorf("%w: %s", ErrUnavailable, st.DetailReason)
	}
	_, id := c.read(ctx, service, false)
	if err := c.switchTo(ctx, id, w); err != nil {
		c.forget()
		return c.State(ctx, service, true), fmt.Errorf("%s: %w", service, err)
	}
	c.forget()
	return c.State(ctx, service, true), nil
}

func (c *Controller) switchTo(ctx context.Context, id string, w Want) error {
	profile := "off"
	if w.Profiling {
		profile = "excimer"
	}
	spec, err := docker.IniSwitch(id, w.Tracing, docker.TraceSettings{Env: c.env, Profile: profile, Detail: w.Detail})
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, execTimeout)
	defer cancel()
	res, err := c.docker.Exec(ctx, spec)
	if err == nil && res.ExitCode != 0 {
		err = fmt.Errorf("roll-inspector-ini exited %d: %s", res.ExitCode, strings.TrimSpace(string(res.Stderr)))
	}
	if err != nil {
		return err
	}
	res, err = c.docker.Exec(ctx, docker.FPMReload(id))
	if err == nil && res.ExitCode != 0 {
		err = fmt.Errorf("the FPM reload exited %d: %s", res.ExitCode, strings.TrimSpace(string(res.Stderr)))
	}
	return err
}

// Summary feeds the top-bar pill: php-fpm's state in a few words.
type Summary struct {
	Available bool   `json:"available"`
	Tracing   bool   `json:"tracing"`
	Detail    string `json:"detail"`
	Profiling bool   `json:"profiling"`
	Label     string `json:"label"`
	Busy      bool   `json:"busy"`
}

func (c *Controller) Summary(ctx context.Context) Summary {
	st, _ := c.readWithin(ctx, Services[0], summaryTTL)
	s := Summary{Available: st.Available, Tracing: st.Tracing, Detail: st.Detail, Profiling: st.Profiling, Busy: c.Busy()}
	switch {
	case !st.Tracing:
		s.Label = "Tracing off"
	case st.Profiling:
		s.Label = "Tracing · " + st.Detail + " · profiling"
	default:
		s.Label = "Tracing · " + st.Detail
	}
	return s
}
