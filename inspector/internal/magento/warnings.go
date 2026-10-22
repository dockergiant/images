package magento

import (
	"context"
	"errors"
	"net"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/dockergiant/images/inspector/internal/core"
)

const (
	fixFPC           = "roll magento cache:enable full_page"
	fixSession       = "roll magento setup:config:set --session-save=redis --session-save-redis-host=redis --session-save-redis-db=2"
	fixRequestSource = "roll inspector trace on"
)

func Warnings(env *Env, present map[core.Kind]bool, hasRequestSource bool) []core.Warning {
	var out []core.Warning
	if env != nil && env.Generic {
		out = append(out, core.Warning{
			ID:   "generic",
			Text: "Not a Magento 2 project, Magento panels hidden",
			Tone: core.ToneInfo,
		})
	}
	if env != nil && !env.Generic {
		out = append(out, magentoWarnings(env, present)...)
	}
	if !hasRequestSource {
		out = append(out, core.Warning{
			ID:   "no-request-source",
			Text: "No request source: Varnish and PHP tracing are off",
			Fix:  fixRequestSource,
			Tone: core.Warn,
		})
	}
	return out
}

func magentoWarnings(env *Env, present map[core.Kind]bool) []core.Warning {
	var out []core.Warning
	if !env.CacheTypes["full_page"] {
		out = append(out, core.Warning{
			ID:   "fpc-disabled",
			Text: "Full page cache is disabled, so every page renders in PHP",
			Fix:  fixFPC,
			Tone: core.Warn,
		})
	}
	if len(env.HTTPCacheHosts) > 0 && !present[core.Varnish] {
		out = append(out, core.Warning{
			ID:   "http-cache-hosts-without-varnish",
			Text: "http_cache_hosts is set in env.php but this env has no Varnish container, so purges go nowhere",
			Tone: core.Warn,
		})
	}
	if env.SessionSave == "valkey" {
		out = append(out, core.Warning{
			ID:   "session-valkey",
			Text: "Session handler valkey is not registered, so Magento stores sessions in files",
			Fix:  fixSession,
			Tone: core.Warn,
		})
	}
	if missing := hosts.unresolved(envHosts(env)); len(missing) > 0 {
		out = append(out, core.Warning{
			ID:   "unresolved-hosts",
			Text: "env.php hosts that do not resolve: " + strings.Join(missing, ", "),
			Tone: core.Error,
		})
	}
	return out
}

func envHosts(env *Env) []string {
	seen := map[string]bool{}
	add := func(h string) {
		h = strings.TrimPrefix(strings.TrimPrefix(h, "tcp://"), "tls://")
		if h == "" || strings.HasPrefix(h, "/") || strings.HasPrefix(h, "unix:") {
			return
		}
		if host, _, err := net.SplitHostPort(h); err == nil {
			h = host
		} else if i := strings.IndexByte(h, ':'); i > 0 && strings.Count(h, ":") == 1 {
			h = h[:i]
		}
		if h == "localhost" || net.ParseIP(h) != nil {
			return
		}
		seen[h] = true
	}
	for _, fe := range env.Cache {
		add(fe.Server)
	}
	if env.SessionSave == "redis" || env.SessionSave == "valkey" {
		add(env.SessionHost)
	}
	if env.AMQP != nil {
		add(env.AMQP.Host)
	}
	add(env.DBHost)
	for _, h := range env.HTTPCacheHosts {
		add(h)
	}
	out := make([]string, 0, len(seen))
	for h := range seen {
		out = append(out, h)
	}
	sort.Strings(out)
	return out
}

const (
	resolveTTL     = 30 * time.Second
	resolveTimeout = time.Second
)

type lookup struct {
	missing bool
	at      time.Time
}

type resolver struct {
	lookup func(ctx context.Context, host string) error
	now    func() time.Time

	mu   sync.Mutex
	seen map[string]lookup
}

func newResolver(fn func(ctx context.Context, host string) error) *resolver {
	return &resolver{lookup: fn, now: time.Now, seen: map[string]lookup{}}
}

var hosts = newResolver(func(ctx context.Context, host string) error {
	_, err := net.DefaultResolver.LookupHost(ctx, host)
	return err
})

func (r *resolver) unresolved(names []string) []string {
	now := r.now()
	r.mu.Lock()
	var stale []string
	for _, h := range names {
		if l, ok := r.seen[h]; !ok || now.Sub(l.at) >= resolveTTL {
			stale = append(stale, h)
		}
	}
	r.mu.Unlock()

	if len(stale) > 0 {
		ctx, cancel := context.WithTimeout(context.Background(), resolveTimeout)
		results := make([]bool, len(stale))
		var wg sync.WaitGroup
		for i, h := range stale {
			wg.Add(1)
			go func() {
				defer wg.Done()
				var dnsErr *net.DNSError
				err := r.lookup(ctx, h)
				results[i] = errors.As(err, &dnsErr) && dnsErr.IsNotFound
			}()
		}
		wg.Wait()
		cancel()
		r.mu.Lock()
		for i, h := range stale {
			r.seen[h] = lookup{missing: results[i], at: now}
		}
		r.mu.Unlock()
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	var out []string
	for _, h := range names {
		if r.seen[h].missing {
			out = append(out, h)
		}
	}
	return out
}
