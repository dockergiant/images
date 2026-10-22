//go:build integration

package search

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"reflect"
	"testing"
	"time"

	"github.com/dockergiant/images/inspector/internal/core"
	"github.com/dockergiant/images/inspector/internal/docker"
)

// Runs against a scratch engine only: it creates and deletes indices.
func TestAgainstEngine(t *testing.T) {
	base := os.Getenv("SEARCH_SCRATCH_URL")
	if base == "" {
		t.Skip("SEARCH_SCRATCH_URL not set")
	}
	ctx := context.Background()
	c := &client{base: base, http: newHTTPClient()}
	must := func(method, path string, body any) {
		t.Helper()
		if _, _, err := c.do(ctx, method, path, body); err != nil {
			t.Fatalf("%s %s: %v", method, path, err)
		}
	}
	for _, ix := range []string{"itest_product_1_v1", "itest_product_1_v2", "itest_product_5_v1", markerIndex} {
		c.do(ctx, http.MethodDelete, indexPath(ix), nil)
	}
	t.Cleanup(func() {
		for _, ix := range []string{"itest_product_1_v1", "itest_product_1_v2", "itest_product_5_v1", markerIndex} {
			c.do(context.Background(), http.MethodDelete, indexPath(ix), nil)
		}
	})
	mapping := map[string]any{"settings": map[string]any{"number_of_replicas": 0}, "mappings": map[string]any{"properties": map[string]any{
		"name": map[string]any{"type": "text", "copy_to": "_search"}, "_search": map[string]any{"type": "text"}, "sku": map[string]any{"type": "keyword"},
	}}}
	must(http.MethodPut, "/itest_product_1_v1", mapping)
	must(http.MethodPut, "/itest_product_5_v1", mapping)
	must(http.MethodPut, "/itest_product_1_v1/_settings", map[string]any{queryInfo: "5s"})
	must(http.MethodPost, "/_aliases", map[string]any{"actions": []any{map[string]any{"add": map[string]any{"index": "itest_product_1_v1", "alias": "itest_product_1"}}}})
	must(http.MethodPut, "/itest_product_1_v1/_doc/1?refresh=true", map[string]any{"name": "Blue jacket", "sku": "J1"})

	d := &docker.Fake{ContainerList: []docker.Container{{ID: "x", Name: "scratch", Service: "opensearch", Running: true}}}
	a, _ := testAdapter(t, &fakeOS{}, storeSet{Magento: true, Stores: map[int]string{1: "default"}})
	a.deps.Docker = d
	a.baseURL = func(*docker.Container) string { return base }
	a.now = time.Now

	info, err := a.Detect(ctx)
	if err != nil || !info.Present || info.Version == "" {
		t.Fatalf("detect %+v %v", info, err)
	}
	t.Logf("engine %s %s insights=%v", info.Engine, info.Version, info.Features["insights"])
	if _, err := a.Health(ctx); err != nil {
		t.Fatal(err)
	}

	mux := browseMux(a)
	if code, out := call(t, mux, "GET", "/index/itest_product_1/mapping?field=name", nil); code != 200 || len(out["fields"].([]any)) != 1 {
		t.Errorf("mapping %d %v", code, out)
	}
	if code, out := call(t, mux, "GET", "/index/itest_product_1/analyzers", nil); code != 200 {
		t.Errorf("analyzers %d %v", code, out)
	}
	if code, out := call(t, mux, "POST", "/analyze", map[string]any{"index": "itest_product_1", "field": "name", "text": "Blue jackets"}); code != 200 {
		t.Errorf("analyze %d %v", code, out)
	}
	code, out := call(t, mux, "POST", "/search", map[string]any{"index": "itest_product_1", "term": "jacket", "profile": true, "explain": true})
	if code != 200 {
		t.Fatalf("search %d %v", code, out)
	}
	resp, _ := json.Marshal(out["response"])
	var sr struct {
		Hits struct {
			Hits []struct {
				Explanation any `json:"_explanation"`
			} `json:"hits"`
		} `json:"hits"`
		Profile any `json:"profile"`
	}
	json.Unmarshal(resp, &sr)
	if len(sr.Hits.Hits) != 1 || sr.Hits.Hits[0].Explanation == nil || sr.Profile == nil {
		t.Errorf("search response %s", resp)
	}

	thresholds := func(ix string) map[string]any {
		m, err := c.thresholds(ctx, ix)
		if err != nil {
			t.Fatal(err)
		}
		return m
	}
	before := thresholds("itest_product_1_v1")
	s := newCaptureSession(c, time.Now)
	if err := s.begin(ctx); err != nil {
		t.Fatal(err)
	}
	if got := thresholds("itest_product_1_v1"); got[queryWarn] != "0ms" || got[fetchWarn] != "0ms" {
		t.Errorf("captured %v", got)
	}
	must(http.MethodPut, "/itest_product_1_v2", mapping)
	must(http.MethodPost, "/_aliases", map[string]any{"actions": []any{
		map[string]any{"remove": map[string]any{"index": "itest_product_1_v1", "alias": "itest_product_1"}},
		map[string]any{"add": map[string]any{"index": "itest_product_1_v2", "alias": "itest_product_1"}},
	}})
	if err := s.sync(ctx); err != nil {
		t.Fatal(err)
	}
	if got := thresholds("itest_product_1_v2"); got[queryWarn] != "0ms" {
		t.Errorf("follow %v", got)
	}
	if n, err := reconcile(ctx, c); err != nil || n != 2 {
		t.Fatalf("reconcile as after a crash: %d %v", n, err)
	}
	if got := thresholds("itest_product_1_v1"); !reflect.DeepEqual(got, before) {
		t.Errorf("v1 after reconcile %v want %v", got, before)
	}
	if got := thresholds("itest_product_1_v2"); got[queryWarn] != nil || got[fetchWarn] != nil {
		t.Errorf("v2 after reconcile %v", got)
	}
	if err := s.restore(ctx); err != nil {
		t.Fatalf("restore after reconcile: %v", err)
	}

	time.Sleep(time.Second)
	a.now = func() time.Time { return time.Now().Add(time.Hour) }
	act := a.Actions()[0]
	if _, err := act.Run(ctx, core.ActionRequest{Target: "itest_product_1_v1"}); err == nil {
		t.Error("deleted the unaliased index of an existing store")
	}
	if _, err := act.Run(ctx, core.ActionRequest{Target: "itest_product_5_v1"}); err != nil {
		t.Errorf("orphan delete: %v", err)
	}
}
