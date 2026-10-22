package search

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

type fakeIndex struct {
	settings map[string]any
	created  int64
	docs     int64
}

type recorded struct {
	Method string
	Path   string
	Query  string
	Body   map[string]any
}

type fakeOS struct {
	t   *testing.T
	srv *httptest.Server

	mu           sync.Mutex
	root         []byte
	rootHeader   http.Header
	plugins      []byte
	health       []byte
	nodes        []byte
	stats        []byte
	insights     []byte
	insightsErr  int
	indices      map[string]*fakeIndex
	aliases      map[string]string
	markerIndex  bool
	markers      map[string]map[string]any
	log          []recorded
	noOpaque     []string
	dropAfterPut map[string]func()
}

func newFakeOS(t *testing.T) *fakeOS {
	f := &fakeOS{
		t:          t,
		root:       fixture(t, "root-3.5.json"),
		rootHeader: fixtureHeaders(t, "root-3.5.headers"),
		plugins:    fixture(t, "plugins-3.5.json"),
		health:     fixture(t, "cluster-health-3.5.json"),
		nodes:      fixture(t, "cat-nodes-3.5.json"),
		stats:      fixture(t, "nodes-stats-3.5.json"),
		insights:   []byte(`{"top_queries":[]}`),
		indices:    map[string]*fakeIndex{},
		aliases:    map[string]string{},
		markers:    map[string]map[string]any{},
	}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeOS) addIndex(name string, created int64, settings map[string]any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if settings == nil {
		settings = map[string]any{}
	}
	f.indices[name] = &fakeIndex{settings: settings, created: created}
}

func (f *fakeOS) setAlias(alias, index string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.aliases[alias] = index
}

func (f *fakeOS) settingsOf(name string) map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := map[string]any{}
	if ix, ok := f.indices[name]; ok {
		for k, v := range ix.settings {
			out[k] = v
		}
	}
	return out
}

func (f *fakeOS) markerDocs() map[string]map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := map[string]map[string]any{}
	for k, v := range f.markers {
		out[k] = v
	}
	return out
}

func (f *fakeOS) requests(method, pathPrefix string) []recorded {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []recorded
	for _, r := range f.log {
		if r.Method == method && strings.HasPrefix(r.Path, pathPrefix) {
			out = append(out, r)
		}
	}
	return out
}

func fakeWrite(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if b, ok := v.([]byte); ok {
		w.Write(b)
		return
	}
	json.NewEncoder(w).Encode(v)
}

func fakeNotFound(w http.ResponseWriter, index string) {
	fakeWrite(w, 404, map[string]any{"error": map[string]any{"type": "index_not_found_exception", "reason": "no such index [" + index + "]"}, "status": 404})
}

func (f *fakeOS) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r.Header.Get("X-Opaque-Id") != opaqueID {
		f.noOpaque = append(f.noOpaque, r.URL.Path)
	}
	rec := recorded{Method: r.Method, Path: r.URL.Path, Query: r.URL.RawQuery}
	if data, _ := io.ReadAll(r.Body); len(data) > 0 {
		if err := json.Unmarshal(data, &rec.Body); err != nil {
			f.t.Errorf("non-JSON body to %s: %s", r.URL.Path, data)
		}
	}
	f.log = append(f.log, rec)

	p := r.URL.Path
	switch {
	case p == "/":
		for k, v := range f.rootHeader {
			w.Header()[k] = v
		}
		fakeWrite(w, 200, f.root)
	case p == "/_cat/plugins":
		fakeWrite(w, 200, f.plugins)
	case p == "/_cluster/health":
		fakeWrite(w, 200, f.health)
	case p == "/_cat/nodes":
		fakeWrite(w, 200, f.nodes)
	case strings.HasPrefix(p, "/_nodes/stats"):
		fakeWrite(w, 200, f.stats)
	case p == "/_insights/top_queries":
		if f.insightsErr != 0 {
			fakeWrite(w, f.insightsErr, []byte(`{"error":"no handler found for uri [/_insights/top_queries] and method [GET]"}`))
			return
		}
		fakeWrite(w, 200, f.insights)
	case p == "/_cat/indices":
		fakeWrite(w, 200, f.catIndices())
	case p == "/_cat/aliases":
		fakeWrite(w, 200, f.catAliases())
	case p == "/"+markerIndex:
		f.serveMarkerIndex(w, r)
	case strings.HasPrefix(p, "/"+markerIndex+"/"):
		f.serveMarker(w, r, rec, strings.TrimPrefix(p, "/"+markerIndex+"/"))
	default:
		f.serveIndex(w, r, rec)
	}
}

func (f *fakeOS) catIndices() []map[string]any {
	names := make([]string, 0, len(f.indices))
	for n := range f.indices {
		names = append(names, n)
	}
	sort.Strings(names)
	out := []map[string]any{}
	for _, n := range names {
		ix := f.indices[n]
		out = append(out, map[string]any{"health": "green", "status": "open", "index": n, "uuid": "u-" + n, "pri": "1", "rep": "0",
			"docs.count": fmt.Sprint(ix.docs), "docs.deleted": "0", "store.size": "208", "creation.date": fmt.Sprint(ix.created)})
	}
	return out
}

func (f *fakeOS) catAliases() []map[string]any {
	out := []map[string]any{}
	for a, ix := range f.aliases {
		out = append(out, map[string]any{"alias": a, "index": ix, "filter": "-", "routing.index": "-", "routing.search": "-", "is_write_index": "-"})
	}
	return out
}

func (f *fakeOS) serveMarkerIndex(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPut {
		fakeWrite(w, 405, map[string]any{})
		return
	}
	if f.markerIndex {
		fakeWrite(w, 400, map[string]any{"error": map[string]any{"type": "resource_already_exists_exception", "reason": "index [.roll-inspector] already exists"}, "status": 400})
		return
	}
	f.markerIndex = true
	fakeWrite(w, 200, map[string]any{"acknowledged": true})
}

func (f *fakeOS) serveMarker(w http.ResponseWriter, r *http.Request, rec recorded, rest string) {
	if !f.markerIndex {
		fakeNotFound(w, markerIndex)
		return
	}
	if rest == "_search" {
		hits := []map[string]any{}
		for id, src := range f.markers {
			hits = append(hits, map[string]any{"_id": id, "_source": src})
		}
		fakeWrite(w, 200, map[string]any{"hits": map[string]any{"hits": hits}})
		return
	}
	id, ok := strings.CutPrefix(rest, "_doc/")
	if !ok {
		fakeWrite(w, 400, map[string]any{})
		return
	}
	switch r.Method {
	case http.MethodGet:
		src, found := f.markers[id]
		if !found {
			fakeWrite(w, 404, map[string]any{"_id": id, "found": false})
			return
		}
		fakeWrite(w, 200, map[string]any{"_id": id, "found": true, "_source": src})
	case http.MethodPut:
		f.markers[id] = rec.Body
		fakeWrite(w, 201, map[string]any{"result": "created"})
	case http.MethodDelete:
		if _, found := f.markers[id]; !found {
			fakeWrite(w, 404, map[string]any{"result": "not_found"})
			return
		}
		delete(f.markers, id)
		fakeWrite(w, 200, map[string]any{"result": "deleted"})
	}
}

func (f *fakeOS) resolve(name string) (string, bool) {
	if _, ok := f.indices[name]; ok {
		return name, true
	}
	if ix, ok := f.aliases[name]; ok {
		return ix, true
	}
	return "", false
}

func (f *fakeOS) serveIndex(w http.ResponseWriter, r *http.Request, rec recorded) {
	parts := strings.SplitN(strings.TrimPrefix(r.URL.Path, "/"), "/", 3)
	name, ok := f.resolve(parts[0])
	if !ok {
		fakeNotFound(w, parts[0])
		return
	}
	ix := f.indices[name]
	op := ""
	if len(parts) > 1 {
		op = parts[1]
	}
	switch {
	case op == "" && r.Method == http.MethodDelete:
		delete(f.indices, name)
		for a, target := range f.aliases {
			if target == name {
				delete(f.aliases, a)
			}
		}
		fakeWrite(w, 200, map[string]any{"acknowledged": true})
	case op == "_settings" && r.Method == http.MethodGet:
		filter := ""
		if len(parts) == 3 {
			filter = strings.TrimSuffix(parts[2], "*")
		}
		out := map[string]any{}
		for k, v := range ix.settings {
			if strings.HasPrefix(k, filter) {
				out[k] = v
			}
		}
		if len(out) == 0 {
			fakeWrite(w, 200, map[string]any{})
			return
		}
		fakeWrite(w, 200, map[string]any{name: map[string]any{"settings": out}})
	case op == "_settings" && r.Method == http.MethodPut:
		for k, v := range rec.Body {
			if v == nil {
				delete(ix.settings, k)
			} else {
				ix.settings[k] = v
			}
		}
		if hook := f.dropAfterPut[name]; hook != nil {
			delete(f.dropAfterPut, name)
			hook()
			select {
			case <-r.Context().Done():
			case <-time.After(2 * time.Second):
			}
			return
		}
		fakeWrite(w, 200, map[string]any{"acknowledged": true})
	case op == "_mapping":
		fakeWrite(w, 200, map[string]any{name: map[string]any{"mappings": map[string]any{"properties": map[string]any{
			"name":      map[string]any{"type": "text", "analyzer": "standard", "copy_to": []any{"_search"}},
			"sku":       map[string]any{"type": "keyword"},
			"_search":   map[string]any{"type": "text"},
			"price_0_1": map[string]any{"type": "double"},
			"category":  map[string]any{"properties": map[string]any{"name": map[string]any{"type": "text"}}},
		}}}})
	case op == "_search" || op == "_analyze":
		fakeWrite(w, 200, map[string]any{"took": 1, "hits": map[string]any{"hits": []any{}}})
	default:
		fakeWrite(w, 400, map[string]any{"error": "unsupported " + r.Method + " " + r.URL.Path})
	}
}
