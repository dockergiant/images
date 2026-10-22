package search

import (
	"net/http"
	"testing"
)

func TestDetectFlavor(t *testing.T) {
	cases := []struct {
		body, headers string
		engine        string
		version       string
		tested        bool
	}{
		{"root-2.0.json", "root-2.0.headers", EngineOpenSearch, "2.0.1", true},
		{"root-2.6.json", "root-2.6.headers", EngineOpenSearch, "2.6.0", true},
		{"root-3.5.json", "root-3.5.headers", EngineOpenSearch, "3.5.0", true},
		{"root-es-7.8.json", "", EngineElasticsearch, "7.8.1", false},
	}
	for _, c := range cases {
		h := http.Header{}
		if c.headers != "" {
			h = fixtureHeaders(t, c.headers)
		}
		f, err := DetectFlavor(fixture(t, c.body), h)
		if err != nil {
			t.Fatalf("%s: %v", c.body, err)
		}
		if f.Engine != c.engine || f.Version != c.version || f.Tested() != c.tested {
			t.Errorf("%s: got %+v tested=%v", c.body, f, f.Tested())
		}
	}
}

func TestDetectFlavorElasticHeader(t *testing.T) {
	body := []byte(`{"name":"es","version":{"number":"8.17.0","build_flavor":"default"},"tagline":"You Know, for Search"}`)
	h := http.Header{}
	h.Set("X-Elastic-Product", "Elasticsearch")
	f, err := DetectFlavor(body, h)
	if err != nil || f.Engine != EngineElasticsearch || f.Version != "8.17.0" {
		t.Fatalf("got %+v %v", f, err)
	}
	body = []byte(`{"version":{"number":"8.17.0"}}`)
	if f, err := DetectFlavor(body, h); err != nil || f.Engine != EngineElasticsearch {
		t.Fatalf("header only: got %+v %v", f, err)
	}
}

func TestDetectFlavorUnknown(t *testing.T) {
	for _, body := range []string{`{"version":{"number":"1.0"}}`, `{}`, `not json`} {
		if f, err := DetectFlavor([]byte(body), http.Header{}); err == nil {
			t.Errorf("%s: expected error, got %+v", body, f)
		}
	}
}

func TestTestedVersions(t *testing.T) {
	cases := map[string]bool{
		"2.0.1": true, "2.6.0": true, "3.5.0": true, "3.5.12": true,
		"2.19.0": false, "1.3.20": false, "3.50.0": false, "2.12.0": false,
	}
	for v, want := range cases {
		if got := (Flavor{Engine: EngineOpenSearch, Version: v}).Tested(); got != want {
			t.Errorf("%s: got %v", v, got)
		}
	}
	if (Flavor{Engine: EngineElasticsearch, Version: "2.0.1"}).Tested() {
		t.Error("elasticsearch is best-effort")
	}
}

func TestHasInsights(t *testing.T) {
	cases := map[string]bool{"plugins-2.0.json": false, "plugins-2.6.json": false, "plugins-3.5.json": true}
	for name, want := range cases {
		got, err := HasInsights(fixture(t, name))
		if err != nil || got != want {
			t.Errorf("%s: got %v %v", name, got, err)
		}
	}
}
