package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/dockergiant/images/inspector/internal/apm"
)

const batchTrace = "6ad7d78100000000d4c01c89ecced149"

func traceServer(t *testing.T) (*Server, *apm.Store) {
	t.Helper()
	store := apm.NewStore(0, 0)
	body, err := os.ReadFile(filepath.Join("..", "..", "testdata", "apm", "dd-batch3.msgpack"))
	if err != nil {
		t.Fatal(err)
	}
	intake := apm.NewDDIntake(store, func(string) bool { return true })
	req := httptest.NewRequest(http.MethodPut, "/v0.4/traces", bytes.NewReader(body))
	req.RemoteAddr = "127.0.0.1:5000"
	w := httptest.NewRecorder()
	intake.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("intake = %d %s", w.Code, w.Body)
	}
	s := New(Config{
		Env:     "shop",
		Domain:  testDomain,
		Secret:  testSecret,
		Version: "test",
		Docker:  fakeDocker(traefikAddr),
		Store:   store,
		UI:      fstest.MapFS{"index.html": {Data: []byte("x")}},
		Log:     quietLog(),
	})
	return s, store
}

func TestTraceAnalysisRoute(t *testing.T) {
	s, _ := traceServer(t)
	w := serve(s, browserReq(http.MethodGet, "/api/traces/"+batchTrace+"/analysis", nil))
	if w.Code != http.StatusOK || w.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("analysis: %d %s", w.Code, w.Body)
	}
	var a struct {
		ID         string            `json:"id"`
		Duration   float64           `json:"duration"`
		Unit       string            `json:"unit"`
		Categories []json.RawMessage `json:"categories"`
		Hot        []struct {
			Rank    int      `json:"rank"`
			SpanID  string   `json:"spanId"`
			SpanIDs []string `json:"spanIds"`
			SelfMs  float64  `json:"selfMs"`
			Kind    string   `json:"kind"`
			Path    []string `json:"path"`
		} `json:"hot"`
		Repeats []json.RawMessage `json:"repeats"`
		Slowest []json.RawMessage `json:"slowest"`
		Profile *bool             `json:"profile"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &a); err != nil {
		t.Fatal(err)
	}
	if a.ID != batchTrace || a.Unit != "ms" || a.Duration <= 0 || len(a.Categories) != 5 || len(a.Hot) == 0 || a.Profile == nil {
		t.Fatalf("analysis = %s", w.Body)
	}
	if h := a.Hot[0]; h.Rank != 1 || h.SpanID == "" || len(h.SpanIDs) == 0 || h.Kind == "" || h.Path == nil {
		t.Errorf("hot[0] = %+v", h)
	}
	if a.Repeats == nil || a.Slowest == nil {
		t.Errorf("lists missing: %s", w.Body)
	}

	if w := serve(s, browserReq(http.MethodGet, "/api/traces/"+strings.Repeat("0", 32)+"/analysis", nil)); w.Code != http.StatusNotFound || !strings.Contains(w.Body.String(), "trace not found") {
		t.Errorf("missing analysis: %d %s", w.Code, w.Body)
	}
}

func TestHotspotsRouteBeatsTraceID(t *testing.T) {
	s, _ := traceServer(t)
	w := serve(s, browserReq(http.MethodGet, "/api/traces/hotspots", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("hotspots: %d %s", w.Code, w.Body)
	}
	var h struct {
		Traces     int               `json:"traces"`
		Since      *string           `json:"since"`
		Routes     []json.RawMessage `json:"routes"`
		Operations []json.RawMessage `json:"operations"`
		Repeats    []json.RawMessage `json:"repeats"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &h); err != nil {
		t.Fatal(err)
	}
	if h.Traces != 3 || h.Since == nil || len(h.Routes) == 0 || len(h.Operations) < 2 || h.Repeats == nil {
		t.Fatalf("hotspots = %s", w.Body)
	}

	w = serve(s, browserReq(http.MethodGet, "/api/traces/hotspots?limit=1", nil))
	if err := json.Unmarshal(w.Body.Bytes(), &h); err != nil {
		t.Fatal(err)
	}
	if len(h.Routes) != 1 || len(h.Operations) != 1 {
		t.Errorf("limit=1: %s", w.Body)
	}
}

func TestTraceRoutesKeepTheirShape(t *testing.T) {
	s, _ := traceServer(t)
	w := serve(s, browserReq(http.MethodGet, "/api/traces/"+batchTrace, nil))
	var tr apm.Trace
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &tr) != nil || tr.ID != batchTrace || len(tr.Spans) == 0 {
		t.Fatalf("trace: %d", w.Code)
	}
	w = serve(s, browserReq(http.MethodGet, "/api/traces", nil))
	var list []map[string]json.RawMessage
	if err := json.Unmarshal(w.Body.Bytes(), &list); err != nil || len(list) != 3 {
		t.Fatalf("list: %v %s", err, w.Body)
	}
	for _, f := range []string{"id", "start", "duration", "method", "url", "route", "status", "counts", "nPlusOne", "hasProfile", "times"} {
		if _, ok := list[0][f]; !ok {
			t.Errorf("summary misses %s", f)
		}
	}
	var times map[string]float64
	if err := json.Unmarshal(list[0]["times"], &times); err != nil || len(times) != 5 {
		t.Errorf("times = %s", list[0]["times"])
	}
}

func TestAnalysisRoutesWithoutStore(t *testing.T) {
	s := newServer(t, fakeDocker(traefikAddr))
	w := serve(s, browserReq(http.MethodGet, "/api/traces/hotspots", nil))
	if w.Code != http.StatusOK || strings.TrimSpace(w.Body.String()) != `{"traces":0,"since":null,"routes":[],"operations":[],"repeats":[]}` {
		t.Errorf("hotspots without store: %d %s", w.Code, w.Body)
	}
	if w := serve(s, browserReq(http.MethodGet, "/api/traces/abc/analysis", nil)); w.Code != http.StatusNotFound {
		t.Errorf("analysis without store: %d", w.Code)
	}
}
