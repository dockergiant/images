package apm

import (
	"fmt"
	"testing"
	"time"
)

var routeSeq int

// webTrace assembles a request trace the way the Datadog intake does: full
// detail carries magento.pathinfo on the root span, lean only the resource.
func webTrace(t *testing.T, url, resource, pathinfo string) Trace {
	t.Helper()
	routeSeq++
	attrs := map[string]string{"http.method": "GET", "http.url": url, "http.status_code": "200"}
	if pathinfo != "" {
		attrs["magento.pathinfo"] = pathinfo
	}
	id := fmt.Sprintf("%032x", 0xabc000+routeSeq)
	tr, ok := assemble([]Span{{TraceID: id, SpanID: "1", Name: "magento.request", Resource: resource, Start: time.Unix(1792600000, 0).Add(time.Duration(routeSeq) * time.Second), Duration: 100 * time.Millisecond, Attrs: attrs}})
	if !ok {
		t.Fatal("assemble refused the trace")
	}
	return tr
}

func routeCounts(s *Store) map[string]int {
	out := map[string]int{}
	for _, r := range s.Hotspots(0).Routes {
		out[r.Route] = r.Count
	}
	return out
}

func TestLeanTraceTakesTheRouteFullTracesTaught(t *testing.T) {
	s := NewStore(0, 0)
	s.Add(webTrace(t, "http://app.shop.test/category-1.html", "catalog/category/view", "catalog/category/view"))
	lean := webTrace(t, "http://app.shop.test/category-1.html?p=2", "GET /category-1.html", "")
	if lean.Route != "GET /category-1.html" {
		t.Fatalf("assembled lean route %q", lean.Route)
	}
	s.Add(lean)
	if got, _ := s.Get(lean.ID); got.Route != "catalog/category/view" {
		t.Fatalf("lean route %q, want the controller full traces gave this URL", got.Route)
	}
	if got := routeCounts(s); got["catalog/category/view"] != 2 || len(got) != 1 {
		t.Fatalf("hotspot routes %v, want one route with both traces", got)
	}
}

func TestEarlierLeanTracesMoveOnceAFullTraceArrives(t *testing.T) {
	s := NewStore(0, 0)
	lean := webTrace(t, "http://app.shop.test/women.html", "GET /women.html", "")
	s.Add(lean)
	if got := s.List(ListFilter{Route: "GET /women.html"}); len(got) != 1 {
		t.Fatalf("routes before %v", got)
	}
	ch, stop := s.Subscribe()
	defer stop()
	s.Add(webTrace(t, "http://app.shop.test/women.html", "catalog/category/view", "catalog/category/view"))
	<-ch
	if got, _ := s.Get(lean.ID); got.Route != "catalog/category/view" {
		t.Fatalf("lean route %q after the full trace", got.Route)
	}
	if got := routeCounts(s); got["catalog/category/view"] != 2 || len(got) != 1 {
		t.Fatalf("routes after %v", got)
	}
	if list := s.List(ListFilter{Route: "catalog/category/view"}); len(list) != 2 {
		t.Fatalf("route filter finds %d traces", len(list))
	}
}

func TestLeanTraceWithoutAFullOneKeepsItsURL(t *testing.T) {
	s := NewStore(0, 0)
	s.Add(webTrace(t, "http://app.shop.test/category-1.html", "catalog/category/view", "catalog/category/view"))
	for _, url := range []string{"http://app.shop.test/other.html", "http://b.shop.test/category-1.html"} {
		lean := webTrace(t, url, "GET /x", "")
		s.Add(lean)
		if got, _ := s.Get(lean.ID); got.Route != "GET /x" {
			t.Fatalf("%s took route %q from another page", url, got.Route)
		}
	}
}
