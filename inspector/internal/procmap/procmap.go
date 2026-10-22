package procmap

import (
	"bytes"
	"context"
	_ "embed"
	"errors"
	"fmt"
	"net/netip"
	"strings"
	"sync"
	"time"

	"github.com/dockergiant/images/inspector/internal/docker"
)

//go:embed walk.sh
var walkScript string

const (
	refreshEvery = 5 * time.Second
	missGap      = 500 * time.Millisecond
	walkTimeout  = 10 * time.Second
)

type Proc struct {
	PID       int    `json:"pid"`
	Container string `json:"container"`
	Role      string `json:"role"`
	Cmd       string `json:"cmd"`
}

type Map struct {
	docker docker.API
	now    func() time.Time

	// OnError, when set before Run, receives the errors of Run's refreshes.
	OnError func(error)

	mu      sync.RWMutex
	ips     map[netip.Addr]docker.Container
	walked  map[string]map[netip.AddrPort]Proc
	sockets map[netip.AddrPort]Proc
	base    context.Context

	missMu   sync.Mutex
	lastMiss map[string]time.Time
	inflight map[string]bool
	wg       sync.WaitGroup
}

func New(d docker.API) *Map {
	return &Map{
		docker:   d,
		now:      time.Now,
		ips:      map[netip.Addr]docker.Container{},
		walked:   map[string]map[netip.AddrPort]Proc{},
		sockets:  map[netip.AddrPort]Proc{},
		base:     context.Background(),
		lastMiss: map[string]time.Time{},
		inflight: map[string]bool{},
	}
}

func isPHP(c docker.Container) bool {
	return c.Running && (c.Service == "php-fpm" || c.Service == "php-debug")
}

func (m *Map) Refresh(ctx context.Context) error {
	list, err := m.docker.Containers(ctx)
	if err != nil {
		return err
	}
	ips := map[netip.Addr]docker.Container{}
	var targets []docker.Container
	for _, c := range list {
		if !c.Running {
			continue
		}
		if a, err := netip.ParseAddr(c.IP); err == nil {
			ips[a.Unmap()] = c
		}
		if isPHP(c) {
			targets = append(targets, c)
		}
	}

	type result struct {
		id   string
		idx  map[netip.AddrPort]Proc
		err  error
		busy bool
	}
	results := make([]result, len(targets))
	var wg sync.WaitGroup
	for i, c := range targets {
		// A walk that outlives its timeout keeps running in the container,
		// so the next tick must not start a second one beside it.
		if !m.claim(c.ID) {
			results[i] = result{id: c.ID, busy: true}
			continue
		}
		wg.Go(func() {
			defer m.release(c.ID)
			idx, err := m.walk(ctx, c)
			results[i] = result{id: c.ID, idx: idx, err: err}
		})
	}
	wg.Wait()

	m.mu.Lock()
	defer m.mu.Unlock()
	walked := map[string]map[netip.AddrPort]Proc{}
	var errs []error
	for _, r := range results {
		if r.busy || r.err != nil {
			if r.err != nil {
				errs = append(errs, r.err)
			}
			if prev, ok := m.walked[r.id]; ok {
				walked[r.id] = prev
			}
			continue
		}
		walked[r.id] = r.idx
	}
	m.ips = ips
	m.walked = walked
	m.merge()
	return errors.Join(errs...)
}

func (m *Map) claim(id string) bool {
	m.missMu.Lock()
	defer m.missMu.Unlock()
	if m.inflight[id] {
		return false
	}
	m.inflight[id] = true
	return true
}

func (m *Map) release(id string) {
	m.missMu.Lock()
	delete(m.inflight, id)
	m.missMu.Unlock()
}

func (m *Map) refreshOne(ctx context.Context, c docker.Container) error {
	idx, err := m.walk(ctx, c)
	if err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, known := range m.ips {
		if known.ID == c.ID {
			m.walked[c.ID] = idx
			m.merge()
			break
		}
	}
	return nil
}

func (m *Map) merge() {
	sockets := map[netip.AddrPort]Proc{}
	for _, idx := range m.walked {
		for k, p := range idx {
			sockets[k] = p
		}
	}
	m.sockets = sockets
}

func (m *Map) walk(ctx context.Context, c docker.Container) (map[netip.AddrPort]Proc, error) {
	res, err := m.docker.Exec(ctx, docker.ExecSpec{
		Container:  c.ID,
		Cmd:        []string{"sh", "-s"},
		User:       "root",
		Privileged: true,
		Stdin:      strings.NewReader(walkScript),
		// A stalled walk is root and privileged; it must not outlive its timeout.
		KillOnCancel: true,
	})
	if err != nil {
		return nil, fmt.Errorf("proc walk in %s: %w", c.Service, err)
	}
	if res.ExitCode != 0 {
		return nil, fmt.Errorf("proc walk in %s: exit %d: %s", c.Service, res.ExitCode, strings.TrimSpace(string(res.Stderr)))
	}
	w, err := ParseWalk(bytes.NewReader(res.Stdout))
	if err != nil {
		return nil, fmt.Errorf("proc walk in %s: %w", c.Service, err)
	}
	return index(c.Service, w), nil
}

// Accepted connections on the FPM listen port share one local address across
// workers, so such a key says nothing about a single process and is dropped.
func index(service string, w Walk) map[netip.AddrPort]Proc {
	idx := map[netip.AddrPort]Proc{}
	ambiguous := map[netip.AddrPort]bool{}
	for _, s := range w.Sockets {
		a := s.Local.Addr()
		if s.State == stateListen || len(s.PIDs) == 0 || a.IsLoopback() || a.IsUnspecified() || ambiguous[s.Local] {
			continue
		}
		pid := owner(s.PIDs, w.Procs)
		cmd := w.Procs[pid]
		p := Proc{PID: pid, Container: service, Role: Role(cmd), Cmd: scrub(cmd)}
		if prev, ok := idx[s.Local]; ok && prev.PID != p.PID {
			delete(idx, s.Local)
			ambiguous[s.Local] = true
			continue
		}
		idx[s.Local] = p
	}
	return idx
}

// A socket inherited across fork is held by the master and its workers; the
// worker is the process that uses it.
func owner(pids []int, procs map[int]string) int {
	for _, pid := range pids {
		if Role(procs[pid]) != "fpm-master" {
			return pid
		}
	}
	return pids[0]
}

func (m *Map) Run(ctx context.Context) {
	m.mu.Lock()
	m.base = ctx
	m.mu.Unlock()

	changed := make(chan struct{}, 1)
	go m.docker.Watch(ctx, func(string) {
		select {
		case changed <- struct{}{}:
		default:
		}
	})
	tick := time.NewTicker(refreshEvery)
	defer tick.Stop()
	for {
		rctx, cancel := context.WithTimeout(ctx, walkTimeout)
		if err := m.Refresh(rctx); err != nil && m.OnError != nil && ctx.Err() == nil {
			m.OnError(err)
		}
		cancel()
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		case <-changed:
		}
	}
}

func parseAddrPort(ip string, port int) (netip.AddrPort, bool) {
	a, err := netip.ParseAddr(ip)
	if err != nil || port < 0 || port > 65535 {
		return netip.AddrPort{}, false
	}
	return netip.AddrPortFrom(a.Unmap(), uint16(port)), true
}

func (m *Map) Lookup(ip string, port int) (Proc, bool) {
	k, ok := parseAddrPort(ip, port)
	if !ok {
		return Proc{}, false
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	p, ok := m.sockets[k]
	return p, ok
}

func (m *Map) Miss(ip string, port int) {
	k, ok := parseAddrPort(ip, port)
	if !ok {
		return
	}
	m.mu.RLock()
	c, known := m.ips[k.Addr()]
	_, mapped := m.sockets[k]
	base := m.base
	m.mu.RUnlock()
	if !known || mapped || !isPHP(c) {
		return
	}

	m.missMu.Lock()
	now := m.now()
	if m.inflight[c.ID] || now.Sub(m.lastMiss[c.ID]) < missGap {
		m.missMu.Unlock()
		return
	}
	m.lastMiss[c.ID] = now
	m.inflight[c.ID] = true
	m.missMu.Unlock()

	m.wg.Go(func() {
		ctx, cancel := context.WithTimeout(base, walkTimeout)
		defer cancel()
		defer m.release(c.ID)
		_ = m.refreshOne(ctx, c)
	})
}

func (m *Map) ContainerOf(ip string) (string, bool) {
	a, err := netip.ParseAddr(ip)
	if err != nil {
		return "", false
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	c, ok := m.ips[a.Unmap()]
	return c.Service, ok
}
