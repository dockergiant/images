package timeline

import (
	"testing"
	"time"

	"github.com/dockergiant/images/inspector/internal/core"
)

func keys(rs []Request) []string {
	out := make([]string, len(rs))
	for i, r := range rs {
		out[i] = r.Key
	}
	return out
}

func sameKeys(t *testing.T, got []Request, want ...string) {
	t.Helper()
	g := keys(got)
	if len(g) != len(want) {
		t.Fatalf("got %v, want %v", g, want)
	}
	for i := range want {
		if g[i] != want[i] {
			t.Fatalf("got %v, want %v", g, want)
		}
	}
}

func findTimeline(t *testing.T) (*Timeline, time.Time) {
	t.Helper()
	base := ts(1792526556)
	tl := newAt(nil, base.Add(time.Minute))
	tl.Ingest(event("varnish.req", core.Varnish, base, shortMiss("7", "8", base, 300*time.Millisecond)))
	hit := shortMiss("9", "", base.Add(10*time.Second), 2*time.Millisecond)
	hit.Handling = "HIT"
	tl.Ingest(event("varnish.req", core.Varnish, hit.Start, hit))
	other := shortMiss("11", "12", base.Add(20*time.Second), 200*time.Millisecond)
	other.URL = "/men.html?p=2"
	tl.Ingest(event("varnish.req", core.Varnish, other.Start, other))
	return tl, base
}

func TestFindByAbsoluteURL(t *testing.T) {
	tl, _ := findTimeline(t)
	sameKeys(t, tl.Find(Query{URL: "https://app.shop.test/women.html"}), "v:9", "v:7")
	sameKeys(t, tl.Find(Query{URL: "https://APP.shop.test:443/women.html#reviews"}), "v:9", "v:7")
	sameKeys(t, tl.Find(Query{URL: "https://app.shop.test/men.html?p=2"}), "v:11")
	sameKeys(t, tl.Find(Query{URL: "https://app.shop.test/men.html"}))
	sameKeys(t, tl.Find(Query{URL: "https://other.shop.test/women.html"}))
	sameKeys(t, tl.Find(Query{URL: "/women.html", Limit: 1}), "v:9")
}

func TestFindByXID(t *testing.T) {
	tl, _ := findTimeline(t)
	sameKeys(t, tl.Find(Query{XIDs: []string{"7"}}), "v:7")
	// The backend request's xid is what a trace carries as varnish.xid.
	sameKeys(t, tl.Find(Query{XIDs: []string{"8"}}), "v:7")
	// X-Varnish on a HIT names the client request and the fetch that stored
	// the object; either token finds a row.
	sameKeys(t, tl.Find(Query{XIDs: []string{"9", "8"}}), "v:9", "v:7")
	sameKeys(t, tl.Find(Query{XIDs: []string{"404"}}))
	sameKeys(t, tl.Find(Query{XIDs: []string{"7"}, URL: "https://app.shop.test/men.html?p=2"}))
}

func TestFindSince(t *testing.T) {
	tl, base := findTimeline(t)
	sameKeys(t, tl.Find(Query{URL: "/women.html", Since: base.Add(5 * time.Second)}), "v:9")
	sameKeys(t, tl.Find(Query{Since: base.Add(time.Hour)}))
}

func TestFindTraceOnlyRows(t *testing.T) {
	base := ts(1792526556)
	tl := newAt(nil, base.Add(time.Minute))
	tr := rootTrace("0000000000000000000000000000abcd", "8", base, 900*time.Millisecond, nil)
	tr.URL = "https://app.shop.test/women.html?color=red"
	tl.Ingest(event("trace", core.PHP, base, tr))
	sameKeys(t, tl.Find(Query{URL: "https://app.shop.test/women.html?color=red"}), "t:0000000000000000000000000000abcd")
	sameKeys(t, tl.Find(Query{XIDs: []string{"8"}}), "t:0000000000000000000000000000abcd")
	sameKeys(t, tl.Find(Query{URL: "https://app.shop.test/women.html"}))
}
