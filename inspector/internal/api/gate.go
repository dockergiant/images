package api

import (
	"crypto/subtle"
	"net"
	"net/http"
	"net/netip"
	"sync"
	"time"

	"github.com/dockergiant/images/inspector/internal/docker"
)

const (
	gateHeader   = "X-Inspector-Gate"
	refreshEvery = time.Second
)

type gate struct {
	docker docker.API
	env    string
	secret []byte

	mu          sync.Mutex
	lastRefresh time.Time
	now         func() time.Time
}

func newGate(d docker.API, env, secret string) *gate {
	return &gate{docker: d, env: env, secret: []byte(secret), now: time.Now}
}

func (g *gate) wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if len(g.secret) == 0 {
			http.Error(w, "inspector has no gate secret configured", http.StatusInternalServerError)
			return
		}
		peer, ok := peerAddr(r.RemoteAddr)
		if !ok {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		status, msg := g.checkPeer(r, peer)
		if status != 0 {
			http.Error(w, msg, status)
			return
		}
		got := []byte(r.Header.Get(gateHeader))
		if subtle.ConstantTimeCompare(got, g.secret) != 1 {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		r.Header.Del(gateHeader)
		next.ServeHTTP(w, r)
	})
}

func (g *gate) checkPeer(r *http.Request, peer netip.Addr) (int, string) {
	absent := "Traefik is not connected to " + g.env + "_default, run roll svc up"
	if g.docker == nil {
		return http.StatusServiceUnavailable, absent
	}
	ip, ok := g.traefikIP(r, false)
	if ok && ip == peer {
		return 0, ""
	}
	if g.mayRefresh() {
		ip, ok = g.traefikIP(r, true)
		if ok && ip == peer {
			return 0, ""
		}
	}
	if !ok {
		return http.StatusServiceUnavailable, absent
	}
	return http.StatusForbidden, "forbidden"
}

func (g *gate) traefikIP(r *http.Request, refresh bool) (netip.Addr, bool) {
	t, err := g.docker.Traefik(r.Context(), refresh)
	if err != nil || !t.Found || t.IP == "" {
		return netip.Addr{}, false
	}
	ip, err := netip.ParseAddr(t.IP)
	if err != nil {
		return netip.Addr{}, false
	}
	return ip.Unmap(), true
}

// A peer that is not Traefik forces a fresh inspect, so the refresh is
// throttled to keep stray connections from turning into Docker API calls.
func (g *gate) mayRefresh() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	now := g.now()
	if now.Sub(g.lastRefresh) < refreshEvery {
		return false
	}
	g.lastRefresh = now
	return true
}

func peerAddr(remote string) (netip.Addr, bool) {
	host, _, err := net.SplitHostPort(remote)
	if err != nil {
		host = remote
	}
	ip, err := netip.ParseAddr(host)
	if err != nil {
		return netip.Addr{}, false
	}
	return ip.Unmap(), true
}
