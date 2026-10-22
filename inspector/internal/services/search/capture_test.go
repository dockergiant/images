package search

import (
	"context"
	"reflect"
	"testing"
	"time"
)

const (
	queryWarn = "index.search.slowlog.threshold.query.warn"
	fetchWarn = "index.search.slowlog.threshold.fetch.warn"
	queryInfo = "index.search.slowlog.threshold.query.info"
)

var fixedNow = time.Date(2026, 10, 20, 20, 0, 0, 0, time.UTC)

func testSession(f *fakeOS) *captureSession {
	return newCaptureSession(&client{base: f.srv.URL, http: newHTTPClient()}, func() time.Time { return fixedNow })
}

func TestCaptureRoundTrip(t *testing.T) {
	f := newFakeOS(t)
	f.addIndex("magento2_product_1_v1", 1, map[string]any{queryInfo: "5s", "index.number_of_replicas": "0"})
	f.addIndex("magento2_product_3_v1", 1, map[string]any{queryWarn: "10s"})
	f.addIndex(".plugins-ml-config", 1, nil)
	f.setAlias("magento2_product_1", "magento2_product_1_v1")
	f.setAlias("magento2_product_3", "magento2_product_3_v1")
	before1, before3 := f.settingsOf("magento2_product_1_v1"), f.settingsOf("magento2_product_3_v1")
	ctx := context.Background()

	s := testSession(f)
	if err := s.begin(ctx); err != nil {
		t.Fatal(err)
	}
	if got := s.indices(); !reflect.DeepEqual(got, []string{"magento2_product_1_v1", "magento2_product_3_v1"}) {
		t.Errorf("captured %v", got)
	}
	for _, ix := range []string{"magento2_product_1_v1", "magento2_product_3_v1"} {
		st := f.settingsOf(ix)
		if st[queryWarn] != "0ms" || st[fetchWarn] != "0ms" {
			t.Errorf("%s not at 0ms: %v", ix, st)
		}
	}
	if len(f.requests("PUT", "/.plugins-ml-config")) != 0 {
		t.Error("system index touched")
	}

	m := f.markerDocs()
	want1 := map[string]any{queryInfo: "5s", queryWarn: nil, fetchWarn: nil}
	if !reflect.DeepEqual(m["magento2_product_1_v1"]["prior"], want1) {
		t.Errorf("marker prior 1: %v", m["magento2_product_1_v1"])
	}
	want3 := map[string]any{queryWarn: "10s", fetchWarn: nil}
	if !reflect.DeepEqual(m["magento2_product_3_v1"]["prior"], want3) {
		t.Errorf("marker prior 3: %v", m["magento2_product_3_v1"])
	}
	if m["magento2_product_1_v1"]["started"] != "2026-10-20T20:00:00Z" || m["magento2_product_1_v1"]["index"] != "magento2_product_1_v1" {
		t.Errorf("marker fields: %v", m["magento2_product_1_v1"])
	}
	created := f.requests("PUT", "/"+markerIndex)
	if len(created) == 0 || created[0].Path != "/"+markerIndex {
		t.Fatalf("marker index not created: %+v", created)
	}
	st, _ := created[0].Body["settings"].(map[string]any)
	if st["index.hidden"] != true || st["index.number_of_replicas"] != float64(0) {
		t.Errorf("marker index settings: %v", created[0].Body)
	}
	for _, r := range f.requests("PUT", "/"+markerIndex+"/_doc/") {
		if r.Query != "refresh=true" {
			t.Errorf("marker write without refresh: %+v", r)
		}
	}

	if err := s.restore(ctx); err != nil {
		t.Fatal(err)
	}
	if got := f.settingsOf("magento2_product_1_v1"); !reflect.DeepEqual(got, before1) {
		t.Errorf("restore 1: %v want %v", got, before1)
	}
	if got := f.settingsOf("magento2_product_3_v1"); !reflect.DeepEqual(got, before3) {
		t.Errorf("restore 3: %v want %v", got, before3)
	}
	var restoreBody map[string]any
	for _, r := range f.requests("PUT", "/magento2_product_1_v1/_settings") {
		restoreBody = r.Body
	}
	if !reflect.DeepEqual(restoreBody, want1) {
		t.Errorf("restore PUT body: %v", restoreBody)
	}
	if len(f.markerDocs()) != 0 {
		t.Errorf("markers left: %v", f.markerDocs())
	}

	puts := len(f.log)
	if err := s.restore(ctx); err != nil || len(f.log) != puts {
		t.Errorf("second restore must be a no-op: %v", err)
	}
	if len(f.noOpaque) != 0 {
		t.Errorf("requests without X-Opaque-Id: %v", f.noOpaque)
	}
}

func TestCaptureFollowsAliasMove(t *testing.T) {
	f := newFakeOS(t)
	f.addIndex("magento2_product_1_v1", 1, nil)
	f.setAlias("magento2_product_1", "magento2_product_1_v1")
	ctx := context.Background()
	s := testSession(f)
	if err := s.begin(ctx); err != nil {
		t.Fatal(err)
	}

	f.addIndex("magento2_product_1_v2", 2, map[string]any{fetchWarn: "1s"})
	f.setAlias("magento2_product_1", "magento2_product_1_v2")
	if err := s.sync(ctx); err != nil {
		t.Fatal(err)
	}
	if st := f.settingsOf("magento2_product_1_v2"); st[queryWarn] != "0ms" || st[fetchWarn] != "0ms" {
		t.Errorf("new index not captured: %v", st)
	}
	if _, ok := f.markerDocs()["magento2_product_1_v2"]; !ok {
		t.Error("no marker for new index")
	}

	f.mu.Lock()
	delete(f.indices, "magento2_product_1_v1")
	f.mu.Unlock()
	if err := s.restore(ctx); err != nil {
		t.Fatalf("deleted index must not fail the restore: %v", err)
	}
	if st := f.settingsOf("magento2_product_1_v2"); !reflect.DeepEqual(st, map[string]any{fetchWarn: "1s"}) {
		t.Errorf("v2 restore: %v", st)
	}
	if len(f.markerDocs()) != 0 {
		t.Errorf("markers left: %v", f.markerDocs())
	}
}

func TestCaptureReusesCrashMarker(t *testing.T) {
	f := newFakeOS(t)
	f.addIndex("magento2_product_1_v1", 1, map[string]any{queryWarn: "0ms", fetchWarn: "0ms"})
	f.setAlias("magento2_product_1", "magento2_product_1_v1")
	f.markerIndex = true
	f.markers["magento2_product_1_v1"] = map[string]any{"index": "magento2_product_1_v1", "prior": map[string]any{queryWarn: "10s", fetchWarn: nil}, "started": "2026-10-20T19:00:00Z"}
	ctx := context.Background()
	s := testSession(f)
	if err := s.begin(ctx); err != nil {
		t.Fatal(err)
	}
	if got := f.markerDocs()["magento2_product_1_v1"]["started"]; got != "2026-10-20T19:00:00Z" {
		t.Errorf("crash marker overwritten: %v", got)
	}
	if err := s.restore(ctx); err != nil {
		t.Fatal(err)
	}
	if st := f.settingsOf("magento2_product_1_v1"); !reflect.DeepEqual(st, map[string]any{queryWarn: "10s"}) {
		t.Errorf("restored %v", st)
	}
}

func TestCaptureNothingToCapture(t *testing.T) {
	f := newFakeOS(t)
	f.addIndex(".plugins-ml-config", 1, nil)
	if err := testSession(f).begin(context.Background()); err == nil {
		t.Error("expected an error without indices")
	}
}

func TestReconcile(t *testing.T) {
	f := newFakeOS(t)
	ctx := context.Background()
	c := &client{base: f.srv.URL, http: newHTTPClient()}
	if n, err := reconcile(ctx, c); n != 0 || err != nil {
		t.Fatalf("no marker index: %d %v", n, err)
	}

	f.addIndex("magento2_product_1_v1", 1, map[string]any{queryWarn: "0ms", fetchWarn: "0ms", queryInfo: "5s"})
	f.markerIndex = true
	f.markers["magento2_product_1_v1"] = map[string]any{"index": "magento2_product_1_v1", "prior": map[string]any{queryInfo: "5s", queryWarn: nil, fetchWarn: nil}, "started": "2026-10-20T19:00:00Z"}
	f.markers["magento2_product_1_v0"] = map[string]any{"prior": map[string]any{queryWarn: "1s"}}
	n, err := reconcile(ctx, c)
	if err != nil || n != 2 {
		t.Fatalf("reconcile: %d %v", n, err)
	}
	if st := f.settingsOf("magento2_product_1_v1"); !reflect.DeepEqual(st, map[string]any{queryInfo: "5s"}) {
		t.Errorf("restored %v", st)
	}
	if len(f.markerDocs()) != 0 {
		t.Errorf("markers left: %v", f.markerDocs())
	}
	if n, err := reconcile(ctx, c); n != 0 || err != nil {
		t.Errorf("second reconcile: %d %v", n, err)
	}
}

func TestCaptureCancelDuringApplyRestores(t *testing.T) {
	f := newFakeOS(t)
	f.addIndex("magento2_product_1_v1", 1, nil)
	f.setAlias("magento2_product_1", "magento2_product_1_v1")
	s := testSession(f)
	if err := s.begin(context.Background()); err != nil {
		t.Fatal(err)
	}

	f.addIndex("magento2_product_1_v2", 2, map[string]any{queryInfo: "5s"})
	f.setAlias("magento2_product_1", "magento2_product_1_v2")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f.dropAfterPut = map[string]func(){"magento2_product_1_v2": cancel}
	if err := s.sync(ctx); err == nil {
		t.Fatal("sync must report the cancelled apply")
	}
	if err := s.restore(context.Background()); err != nil {
		t.Fatal(err)
	}
	if st := f.settingsOf("magento2_product_1_v2"); !reflect.DeepEqual(st, map[string]any{queryInfo: "5s"}) {
		t.Errorf("v2 not restored: %v", st)
	}
	if len(f.markerDocs()) != 0 {
		t.Errorf("markers left: %v", f.markerDocs())
	}
}

func TestCaptureRestoreAfterReconcileKeepsCustomThreshold(t *testing.T) {
	f := newFakeOS(t)
	f.addIndex("magento2_product_1_v1", 1, map[string]any{queryWarn: "10s"})
	f.setAlias("magento2_product_1", "magento2_product_1_v1")
	ctx := context.Background()
	s := testSession(f)
	if err := s.begin(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := reconcile(ctx, &client{base: f.srv.URL, http: newHTTPClient()}); err != nil {
		t.Fatal(err)
	}
	if err := s.restore(ctx); err != nil {
		t.Fatal(err)
	}
	if st := f.settingsOf("magento2_product_1_v1"); !reflect.DeepEqual(st, map[string]any{queryWarn: "10s"}) {
		t.Errorf("custom threshold lost: %v", st)
	}
}
