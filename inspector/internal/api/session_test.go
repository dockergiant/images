package api

import (
	"net/http"
	"regexp"
	"testing"
	"testing/fstest"
)

func TestSessionCarriesVersionAndUIHash(t *testing.T) {
	read := func(files fstest.MapFS) session {
		t.Helper()
		cfg := Config{Env: "shop", Domain: testDomain, Secret: testSecret, Version: "1.2.3", Docker: fakeDocker(traefikAddr), Log: quietLog()}
		if files != nil {
			cfg.UI = files
		}
		s := New(cfg)
		w := serve(s, browserReq(http.MethodGet, "/api/session", nil))
		if w.Code != http.StatusOK {
			t.Fatalf("%d %s", w.Code, w.Body)
		}
		return decode[session](t, w)
	}
	a := read(fstest.MapFS{"index.html": {Data: []byte("<p>a")}, "app.js": {Data: []byte("1")}})
	if a.Version != "1.2.3" || !regexp.MustCompile(`^[0-9a-f]{16}$`).MatchString(a.UIHash) {
		t.Fatalf("session version %q uiHash %q", a.Version, a.UIHash)
	}
	if b := read(fstest.MapFS{"index.html": {Data: []byte("<p>a")}, "app.js": {Data: []byte("1")}}); b.UIHash != a.UIHash {
		t.Fatalf("the same files hash to %q and %q", a.UIHash, b.UIHash)
	}
	if c := read(fstest.MapFS{"index.html": {Data: []byte("<p>a")}, "app.js": {Data: []byte("2")}}); c.UIHash == a.UIHash {
		t.Fatal("a changed file kept the hash")
	}
	if d := read(nil); d.UIHash != "" {
		t.Fatalf("no UI, hash %q", d.UIHash)
	}
}
