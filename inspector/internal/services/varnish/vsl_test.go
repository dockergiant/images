package varnish

import (
	"bytes"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/dockergiant/images/inspector/internal/core"
)

func fixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "..", "testdata", "varnish", name))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func parseFixture(t *testing.T, name string) map[string]core.VarnishReq {
	t.Helper()
	out := map[string]core.VarnishReq{}
	ParseGroups(bytes.NewReader(fixture(t, name)), func(r core.VarnishReq) {
		if _, dup := out[r.VXID]; dup {
			t.Fatalf("vxid %s emitted twice", r.VXID)
		}
		out[r.VXID] = r
	})
	return out
}

func epoch(sec, usec int64) time.Time {
	return time.Unix(sec, usec*1000).UTC()
}

func TestParseGroupsCases80(t *testing.T) {
	reqs := parseFixture(t, "vsl-8.0-cases.txt")
	if len(reqs) != 11 {
		t.Fatalf("got %d requests, want 11", len(reqs))
	}

	miss := reqs["2"]
	if miss.Method != "GET" || miss.URL != "/catalog/product.html" || miss.Host != "app.fixture.test" || miss.Status != 200 {
		t.Fatalf("miss basics: %+v", miss)
	}
	if miss.Handling != "MISS" || miss.BereqVXID != "3" || miss.Grace {
		t.Fatalf("miss handling: %+v", miss)
	}
	if miss.Reason != "not in cache yet" {
		t.Fatalf("miss reason %q", miss.Reason)
	}
	if !slices.Equal(miss.Tags, []string{"cat_p_17", "cat_c_41", "cms_b_footer", "store"}) {
		t.Fatalf("tags %v", miss.Tags)
	}
	if !strings.HasPrefix(miss.TTL, "RFC 2 3600 0 ") || !strings.HasSuffix(miss.TTL, "cacheable") {
		t.Fatalf("ttl %q", miss.TTL)
	}
	if !miss.Start.Equal(epoch(1792528077, 712176)) {
		t.Fatalf("start %v", miss.Start)
	}
	if miss.TTFB != 51085*time.Microsecond || miss.Duration != 55132*time.Microsecond {
		t.Fatalf("ttfb %v duration %v", miss.TTFB, miss.Duration)
	}
	if !miss.BereqStart.Equal(epoch(1792528077, 725000)) || !miss.BereqEnd.Equal(epoch(1792528077, 767583)) {
		t.Fatalf("bereq window %v %v", miss.BereqStart, miss.BereqEnd)
	}
	if len(miss.Raw) == 0 || !strings.Contains(miss.Raw[0], "<< Request  >>") || !strings.Contains(miss.Raw[len(miss.Raw)-1], "End") {
		t.Fatalf("raw lines not kept: first %q", miss.Raw[0])
	}

	hit := reqs["32770"]
	if hit.Handling != "HIT" || hit.BereqVXID != "" || hit.Grace || hit.Reason != "" {
		t.Fatalf("hit: %+v", hit)
	}

	grace := reqs["65538"]
	if grace.Handling != "HIT" || !grace.Grace || grace.BereqVXID != "65539" {
		t.Fatalf("grace: %+v", grace)
	}

	private := reqs["32772"]
	if private.Handling != "MISS" || private.Reason != "Cache-Control: private, no-cache from backend" {
		t.Fatalf("private: %q %q", private.Handling, private.Reason)
	}
	if !strings.HasSuffix(private.TTL, "uncacheable") {
		t.Fatalf("private ttl %q", private.TTL)
	}

	cookie := reqs["5"]
	if cookie.Handling != "MISS" || cookie.Reason != "not in cache yet" {
		t.Fatalf("cookie stripped by VCL is cacheable: %q", cookie.Reason)
	}

	post := reqs["98306"]
	if post.Method != "POST" || post.Handling != "PASS" || post.Reason != "POST is never cached (PASS in VCL)" {
		t.Fatalf("post: %+v", post.Reason)
	}

	purge := reqs["65541"]
	if purge.Method != "PURGE" || purge.Handling != "SYNTH" || purge.Status != 200 || purge.Reason != "" {
		t.Fatalf("purge: %+v", purge)
	}

	banned := reqs["32775"]
	if banned.Handling != "MISS" || banned.Reason != "banned by a purge" || len(banned.Tags) != 4 {
		t.Fatalf("banned: %q %v", banned.Reason, banned.Tags)
	}

	pipe := reqs["65543"]
	if pipe.Handling != "PIPE" || pipe.BereqVXID != "65544" || pipe.Duration != 20128*time.Microsecond {
		t.Fatalf("pipe: %+v", pipe)
	}

	hfm := reqs["32778"]
	if hfm.Handling != "MISS" || hfm.Reason != "hit-for-miss: Cache-Control: private, no-cache from backend" {
		t.Fatalf("hit-for-miss: %q", hfm.Reason)
	}
}

func TestParseGroupsCases71(t *testing.T) {
	reqs := parseFixture(t, "vsl-7.1-cases.txt")
	if len(reqs) != 11 {
		t.Fatalf("got %d requests, want 11", len(reqs))
	}
	want := map[string][2]string{
		"2":     {"MISS", "not in cache yet"},
		"32770": {"HIT", ""},
		"5":     {"HIT", ""},
		"32772": {"MISS", "Cache-Control: private, no-cache from backend"},
		"32775": {"PASS", "POST is never cached (PASS in VCL)"},
		"8":     {"SYNTH", ""},
		"98306": {"MISS", "banned by a purge"},
		"10":    {"PIPE", ""},
		"13":    {"MISS", "hit-for-miss: Cache-Control: private, no-cache from backend"},
	}
	for vxid, w := range want {
		r, ok := reqs[vxid]
		if !ok {
			t.Fatalf("vxid %s missing", vxid)
		}
		if r.Handling != w[0] || r.Reason != w[1] {
			t.Errorf("vxid %s: got %q %q, want %q %q", vxid, r.Handling, r.Reason, w[0], w[1])
		}
	}
	if g := reqs["5"]; !g.Grace || g.BereqVXID != "6" {
		t.Fatalf("7.1 grace: %+v", g)
	}
}

func TestParseGroupsRealEnvs(t *testing.T) {
	m := parseFixture(t, "vsl-8.0-admin.txt")
	admin := m["7"]
	if admin.Handling != "MISS" || admin.Host != "app.shop.test" || admin.BereqVXID != "8" {
		t.Fatalf("admin: %+v", admin)
	}
	if admin.Reason != "Cache-Control: max-age=0, must-revalidate, no-cache, no-store from backend" {
		t.Fatalf("admin reason %q", admin.Reason)
	}
	if admin.Duration != 29091115*time.Microsecond {
		t.Fatalf("duration %v", admin.Duration)
	}
	static := m["98312"]
	if static.Handling != "PASS" || static.Reason != "never cached (PASS in VCL)" {
		t.Fatalf("static: %q %q", static.Handling, static.Reason)
	}

	g := parseFixture(t, "vsl-7.1-media.txt")
	forbidden := g["2"]
	if forbidden.Status != 403 || forbidden.Handling != "MISS" || forbidden.Reason != "status 403 is not cacheable by default" {
		t.Fatalf("403: %d %q %q", forbidden.Status, forbidden.Handling, forbidden.Reason)
	}
	media := g["196610"]
	if media.Handling != "PASS" || media.Reason != "never cached (PASS in VCL)" || media.Host != "app.shop2.test" {
		t.Fatalf("media: %+v", media.Reason)
	}
}

func TestParseGroupsESIMissKeepsBackendLink(t *testing.T) {
	m := parseFixture(t, "vsl-8.0-esi-miss-inspector-e2e.txt")
	miss, ok := m["1441840"]
	if !ok {
		t.Fatalf("client request missing: %v", m)
	}
	if miss.Handling != "MISS" || miss.BereqVXID != "1441841" || miss.Grace {
		t.Fatalf("esi miss: handling %q bereq %q grace %v", miss.Handling, miss.BereqVXID, miss.Grace)
	}
	if miss.BereqStart.IsZero() || miss.BereqEnd.IsZero() {
		t.Fatalf("bereq window %v %v", miss.BereqStart, miss.BereqEnd)
	}
}

func TestParserLineByLine(t *testing.T) {
	var got []core.VarnishReq
	p := newGroupParser(func(r core.VarnishReq) { got = append(got, r) })
	for line := range strings.SplitSeq(string(fixture(t, "vsl-8.0-cases.txt")), "\n") {
		p.Line(line)
	}
	if len(got) != 11 {
		t.Fatalf("line feed gave %d requests, want 11", len(got))
	}
}

func TestParseGroupsEndWithoutBlankLine(t *testing.T) {
	text := strings.Join([]string{
		"*              << Request  >>   2         ",
		"-            2 Begin          c req 1 rxreq",
		"-            2 Timestamp      c Start: 1792528077.712176 0.000000 0.000000",
		"-            2 ReqMethod      c GET",
		"-            2 ReqURL         c /a",
		"-            2 VCL_call       c HASH",
		"-            2 Hit            c 3 1.983783 3600.000000 0.000000",
		"-            2 VCL_call       c HIT",
		"-            2 RespStatus     c 200",
		"-            2 Timestamp      c Resp: 1792528077.769262 0.000287 0.000139",
		"-            2 End            c ",
	}, "\n")
	var got []core.VarnishReq
	ParseGroups(strings.NewReader(text), func(r core.VarnishReq) { got = append(got, r) })
	if len(got) != 1 || got[0].Handling != "HIT" || got[0].URL != "/a" {
		t.Fatalf("got %+v", got)
	}
}

func TestParseGroupsSkipsBackendOnlyGroups(t *testing.T) {
	text := "**             << BeReq    >>   3         \n" +
		"--           3 Begin          b bereq 2 fetch\n" +
		"--           3 End            b \n\n"
	n := 0
	ParseGroups(strings.NewReader(text), func(core.VarnishReq) { n++ })
	if n != 0 {
		t.Fatalf("emitted %d requests for a backend-only group", n)
	}
}

func TestParseRecordTrailingSpace(t *testing.T) {
	rec, ok := parseRecord("-            2 End            c ")
	if !ok || rec.tag != "End" || rec.side != 'c' || rec.value != "" || rec.vxid != "2" || rec.depth != 1 {
		t.Fatalf("got %+v %v", rec, ok)
	}
	rec, ok = parseRecord("--           3 Filters        b  esi_gzip")
	if !ok || rec.depth != 2 || strings.TrimSpace(rec.value) != "esi_gzip" {
		t.Fatalf("got %+v %v", rec, ok)
	}
	if _, ok := parseRecord("*              << Request  >>   2         "); ok {
		t.Fatal("group header parsed as a record")
	}
}
