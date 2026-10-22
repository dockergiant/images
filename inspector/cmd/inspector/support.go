package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/dockergiant/images/inspector/internal/docker"
	"github.com/dockergiant/images/inspector/internal/magento"
	"github.com/dockergiant/images/inspector/internal/metrics"
)

const (
	minSecret     = 32
	allowTTL      = 5 * time.Second
	allowRetry    = time.Second
	allowLoadTime = 3 * time.Second
)

type config struct {
	env       string
	domain    string
	subdomain string
	secret    string
	sample    time.Duration
	// warning is logged once the logger exists; a sampler typo should not
	// keep Inspector from starting.
	warning string
}

func loadConfig(getenv func(string) string) (config, error) {
	cfg := config{
		env:       strings.TrimSpace(getenv("ROLL_ENV_NAME")),
		domain:    strings.TrimSpace(getenv("TRAEFIK_DOMAIN")),
		secret:    getenv("ROLL_INSPECTOR_SECRET"),
		subdomain: strings.TrimSpace(getenv("TRAEFIK_SUBDOMAIN")),
		sample:    metrics.Step,
	}
	if d, err := metrics.ParseInterval(strings.TrimSpace(getenv("ROLL_INSPECTOR_SAMPLE"))); err != nil {
		cfg.warning = "ROLL_INSPECTOR_SAMPLE: " + err.Error() + "; sampling every 10 s"
	} else {
		cfg.sample = d
	}
	var missing []string
	for _, v := range []struct{ name, value string }{
		{"ROLL_ENV_NAME", cfg.env},
		{"TRAEFIK_DOMAIN", cfg.domain},
		{"ROLL_INSPECTOR_SECRET", cfg.secret},
	} {
		if v.value == "" {
			missing = append(missing, v.name)
		}
	}
	if len(missing) > 0 {
		return cfg, fmt.Errorf("%s not set; Inspector runs as the inspector service of a roll env (ROLL_INSPECTOR=1, then roll env up), which passes ROLL_ENV_NAME, TRAEFIK_DOMAIN and ROLL_INSPECTOR_SECRET", strings.Join(missing, ", "))
	}
	if len(cfg.secret) < minSecret {
		return cfg, fmt.Errorf("ROLL_INSPECTOR_SECRET has %d characters, it needs at least %d; delete ~/.roll/inspector/%s.secret and run roll env up to create a new one", len(cfg.secret), minSecret, cfg.env)
	}
	return cfg, nil
}

type allowList struct {
	docker docker.API
	now    func() time.Time

	mu       sync.Mutex
	ips      map[netip.Addr]bool
	loadedAt time.Time
}

func newAllowList(d docker.API) *allowList {
	return &allowList{docker: d, now: time.Now, ips: map[netip.Addr]bool{}}
}

func (a *allowList) allow(ip string) bool {
	addr, err := netip.ParseAddr(ip)
	if err != nil {
		return false
	}
	addr = addr.Unmap()
	if addr.IsLoopback() {
		return true
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	now := a.now()
	if now.Sub(a.loadedAt) >= allowTTL {
		a.load(now)
	}
	if a.ips[addr] {
		return true
	}
	// A recreated php-fpm gets a new address; one early reload keeps its
	// first traces from bouncing for the rest of the TTL.
	if now.Sub(a.loadedAt) >= allowRetry {
		a.load(now)
	}
	return a.ips[addr]
}

func (a *allowList) load(now time.Time) {
	a.loadedAt = now
	ctx, cancel := context.WithTimeout(context.Background(), allowLoadTime)
	defer cancel()
	list, err := a.docker.Containers(ctx)
	if err != nil {
		return
	}
	ips := map[netip.Addr]bool{}
	for _, c := range list {
		if addr, err := netip.ParseAddr(c.IP); err == nil {
			ips[addr.Unmap()] = true
		}
	}
	a.ips = ips
}

func redisFrontend(fe magento.Frontend) bool {
	b := strings.ToLower(fe.Backend)
	return fe.Server != "" || strings.Contains(b, "redis") || strings.Contains(b, "valkey")
}

func frontends(env *magento.Env) map[int]string {
	out := map[int]string{}
	if env == nil {
		return out
	}
	rank := map[string]int{"default": 0, "page_cache": 1}
	names := make([]string, 0, len(env.Cache))
	for name, fe := range env.Cache {
		if redisFrontend(fe) {
			names = append(names, name)
		}
	}
	sort.Slice(names, func(i, j int) bool {
		ri, iok := rank[names[i]]
		rj, jok := rank[names[j]]
		if iok != jok {
			return iok
		}
		if iok && ri != rj {
			return ri < rj
		}
		return names[i] < names[j]
	})
	add := func(db int, name string) {
		if prev, ok := out[db]; ok {
			out[db] = prev + " + " + name
			return
		}
		out[db] = name
	}
	for _, name := range names {
		add(env.Cache[name].Database, name)
	}
	if s := strings.ToLower(env.SessionSave); s == "redis" || s == "valkey" {
		add(env.SessionDB, "session")
	}
	return out
}

type rateEntry struct {
	text       string
	at         time.Time
	suppressed int
}

type rateLog struct {
	log   *slog.Logger
	every time.Duration
	now   func() time.Time

	mu   sync.Mutex
	last map[string]*rateEntry
}

func newRateLog(log *slog.Logger, every time.Duration) *rateLog {
	return &rateLog{log: log, every: every, now: time.Now, last: map[string]*rateEntry{}}
}

func (r *rateLog) Error(msg string, err error) {
	r.mu.Lock()
	now := r.now()
	e := r.last[msg]
	if e != nil && e.text == err.Error() && now.Sub(e.at) < r.every {
		e.suppressed++
		r.mu.Unlock()
		return
	}
	suppressed := 0
	if e != nil {
		suppressed = e.suppressed
	}
	r.last[msg] = &rateEntry{text: err.Error(), at: now}
	r.mu.Unlock()
	r.log.Error(msg, "err", err, "suppressed", suppressed)
}

func healthcheck(url string, timeout time.Duration) int {
	c := http.Client{Timeout: timeout}
	res, err := c.Get(url)
	if err != nil {
		fmt.Fprintln(os.Stderr, "inspector healthcheck:", err)
		return 1
	}
	defer res.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(res.Body, 4096))
	if res.StatusCode != http.StatusOK {
		fmt.Fprintln(os.Stderr, "inspector healthcheck: status", res.StatusCode)
		return 1
	}
	return 0
}

func serveErr(err error) error {
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

// shopHost is the Host header the benchmark sends for a bare path: the base
// URL from env.php when it is on this env's domain, else what roll routes.
func shopHost(r magento.Reader, cfg config) func(context.Context) string {
	fallback := cfg.domain
	if cfg.subdomain != "" {
		fallback = cfg.subdomain + "." + cfg.domain
	}
	return func(ctx context.Context) string {
		ctx, cancel := context.WithTimeout(ctx, envTimeout)
		defer cancel()
		env, err := r.Load(ctx)
		if err != nil || env == nil {
			return fallback
		}
		for _, key := range []string{"default/web/secure/base_url", "default/web/unsecure/base_url"} {
			u, err := url.Parse(env.BaseURLs[key])
			if err != nil {
				continue
			}
			h := strings.ToLower(u.Hostname())
			if h == cfg.domain || strings.HasSuffix(h, "."+cfg.domain) {
				return h
			}
		}
		return fallback
	}
}
