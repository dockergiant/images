package magento

import (
	"context"
	"errors"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dockergiant/images/inspector/internal/core"
)

func fakeHosts(t *testing.T, missing ...string) *atomic.Int32 {
	t.Helper()
	calls := &atomic.Int32{}
	gone := map[string]bool{}
	for _, h := range missing {
		gone[h] = true
	}
	prev := hosts
	hosts = newResolver(func(ctx context.Context, host string) error {
		calls.Add(1)
		if gone[host] {
			return &net.DNSError{Err: "no such host", Name: host, IsNotFound: true}
		}
		return nil
	})
	t.Cleanup(func() { hosts = prev })
	return calls
}

func healthyEnv() *Env {
	return &Env{
		Framework:      "mage-os",
		Cache:          map[string]Frontend{"default": {Server: "redis"}, "page_cache": {Server: "redis"}},
		SessionSave:    "redis",
		SessionHost:    "redis",
		AMQP:           &AMQP{Host: "rabbitmq"},
		HTTPCacheHosts: []string{"varnish:80"},
		CacheTypes:     map[string]bool{"full_page": true, "config": true},
		DBHost:         "db",
	}
}

var allPresent = map[core.Kind]bool{core.Varnish: true, core.Redis: true, core.Search: true, core.RabbitMQ: true}

func ids(ws []core.Warning) map[string]core.Warning {
	out := map[string]core.Warning{}
	for _, w := range ws {
		out[w.ID] = w
	}
	return out
}

func TestWarningsNone(t *testing.T) {
	fakeHosts(t)
	if ws := Warnings(healthyEnv(), allPresent, true); len(ws) != 0 {
		t.Fatalf("got %+v", ws)
	}
}

func TestWarningsFPCOff(t *testing.T) {
	fakeHosts(t)
	env := healthyEnv()
	env.CacheTypes["full_page"] = false
	w, ok := ids(Warnings(env, allPresent, true))["fpc-disabled"]
	if !ok || w.Fix != "roll magento cache:enable full_page" || w.Tone != core.Warn {
		t.Fatalf("got %+v", w)
	}
	delete(env.CacheTypes, "full_page")
	if _, ok := ids(Warnings(env, allPresent, true))["fpc-disabled"]; !ok {
		t.Fatal("a missing full_page entry means disabled")
	}
}

func TestWarningsSessionValkey(t *testing.T) {
	fakeHosts(t)
	env := healthyEnv()
	env.SessionSave = "valkey"
	w, ok := ids(Warnings(env, allPresent, true))["session-valkey"]
	want := "roll magento setup:config:set --session-save=redis --session-save-redis-host=redis --session-save-redis-db=2"
	if !ok || w.Fix != want {
		t.Fatalf("got %+v", w)
	}
}

func TestWarningsHTTPCacheHostsWithoutVarnish(t *testing.T) {
	fakeHosts(t)
	present := map[core.Kind]bool{core.Redis: true}
	got := ids(Warnings(healthyEnv(), present, true))
	if _, ok := got["http-cache-hosts-without-varnish"]; !ok {
		t.Fatalf("got %+v", got)
	}
	if _, ok := got["unresolved-hosts"]; ok {
		t.Fatal("resolver says every host resolves")
	}
	env := healthyEnv()
	env.HTTPCacheHosts = nil
	if _, ok := ids(Warnings(env, present, true))["http-cache-hosts-without-varnish"]; ok {
		t.Fatal("no http_cache_hosts, no warning")
	}
}

func TestWarningsNoRequestSource(t *testing.T) {
	fakeHosts(t)
	w, ok := ids(Warnings(healthyEnv(), allPresent, false))["no-request-source"]
	if !ok || w.Text != "No request source: Varnish and PHP tracing are off" || w.Fix != "roll inspector trace on" {
		t.Fatalf("got %+v", w)
	}
	w, ok = ids(Warnings(nil, nil, false))["no-request-source"]
	if !ok {
		t.Fatal("no request source applies without an env too")
	}
}

func TestWarningsGeneric(t *testing.T) {
	calls := fakeHosts(t, "redis")
	env := healthyEnv()
	env.Generic = true
	env.SessionSave = "valkey"
	env.CacheTypes = nil
	got := Warnings(env, map[core.Kind]bool{}, false)
	m := ids(got)
	if len(got) != 2 || m["generic"].Text != "Not a Magento 2 project, Magento panels hidden" || m["generic"].Tone != core.ToneInfo {
		t.Fatalf("got %+v", got)
	}
	if _, ok := m["no-request-source"]; !ok {
		t.Fatalf("got %+v", got)
	}
	if calls.Load() != 0 {
		t.Fatal("generic mode must not resolve hosts")
	}
}

func TestWarningsUnresolvedHosts(t *testing.T) {
	calls := fakeHosts(t, "rabbitmq", "db")
	env := healthyEnv()
	env.DBHost = "db:3306"
	env.SessionHost = "127.0.0.1"
	env.Cache["default"] = Frontend{Server: "/var/run/redis.sock"}
	w, ok := ids(Warnings(env, allPresent, true))["unresolved-hosts"]
	if !ok || w.Text != "env.php hosts that do not resolve: db, rabbitmq" || w.Tone != core.Error {
		t.Fatalf("got %+v", w)
	}
	first := calls.Load()
	if first != 4 {
		t.Fatalf("looked up %d hosts, want db, rabbitmq, redis, varnish", first)
	}
	Warnings(env, allPresent, true)
	if calls.Load() != first {
		t.Fatal("lookups within the cache window should not repeat")
	}
}

func TestResolverCacheExpiresAndIgnoresTimeouts(t *testing.T) {
	now := time.Unix(1000, 0)
	var calls atomic.Int32
	r := newResolver(func(ctx context.Context, host string) error {
		calls.Add(1)
		if host == "slow" {
			return errors.New("i/o timeout")
		}
		return &net.DNSError{IsNotFound: true}
	})
	r.now = func() time.Time { return now }
	if got := r.unresolved([]string{"slow", "gone"}); len(got) != 1 || got[0] != "gone" {
		t.Fatalf("got %v", got)
	}
	r.unresolved([]string{"slow", "gone"})
	if calls.Load() != 2 {
		t.Fatalf("calls %d", calls.Load())
	}
	now = now.Add(31 * time.Second)
	r.unresolved([]string{"gone"})
	if calls.Load() != 3 {
		t.Fatalf("calls %d after expiry", calls.Load())
	}
}
