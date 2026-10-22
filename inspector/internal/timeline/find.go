package timeline

import (
	"net"
	"net/url"
	"slices"
	"sort"
	"strings"
	"time"
)

// Query selects timeline rows. Every field that is set must match.
type Query struct {
	// URL is an absolute URL, or a path with its query string. An absolute
	// URL also matches the host when the row knows it.
	URL string
	// XIDs are Varnish transaction ids, as the X-Varnish response header
	// lists them. A row matches on its client request, its backend request or
	// the varnish.xid its trace carries.
	XIDs  []string
	Since time.Time
	Limit int
}

// Find returns the matching rows, newest first.
func (t *Timeline) Find(q Query) []Request {
	host, uri := splitQueryURL(q.URL)
	xids := map[string]bool{}
	for _, x := range q.XIDs {
		if x = strings.TrimSpace(x); x != "" {
			xids[x] = true
		}
	}

	t.mu.Lock()
	defer t.mu.Unlock()
	t.expire(t.now())
	var list []*entry
	for _, e := range t.entries {
		if uri != "" && !e.atURL(host, uri) {
			continue
		}
		if len(xids) > 0 && !e.hasXID(xids) {
			continue
		}
		if !q.Since.IsZero() && e.req.Start.Before(q.Since) {
			continue
		}
		list = append(list, e)
	}
	sort.SliceStable(list, func(i, j int) bool {
		a, b := list[i].req.Start, list[j].req.Start
		if !a.Equal(b) {
			return a.After(b)
		}
		return list[i].seq > list[j].seq
	})
	if q.Limit > 0 && len(list) > q.Limit {
		list = list[:q.Limit]
	}
	out := make([]Request, len(list))
	for i, e := range list {
		out[i] = e.req
	}
	return out
}

func (e *entry) atURL(host, uri string) bool {
	if e.req.URL != uri {
		return false
	}
	return host == "" || e.req.Host == "" || strings.EqualFold(stripPort(e.req.Host), host)
}

func (e *entry) hasXID(xids map[string]bool) bool {
	ids := []string{e.req.VXID}
	if e.varnish != nil {
		ids = append(ids, e.varnish.BereqVXID)
	}
	if e.trace != nil {
		ids = append(ids, e.trace.VarnishXID)
	}
	return slices.ContainsFunc(ids, func(id string) bool { return id != "" && xids[id] })
}

// splitQueryURL turns what a browser shows as location.href into the host
// and the request URI the timeline keeps; the fragment never reaches a server.
func splitQueryURL(raw string) (host, uri string) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", ""
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", raw
	}
	if u.Host == "" {
		return "", u.RequestURI()
	}
	return strings.ToLower(stripPort(u.Host)), u.RequestURI()
}

func stripPort(hostport string) string {
	if h, _, err := net.SplitHostPort(hostport); err == nil {
		return h
	}
	return hostport
}
