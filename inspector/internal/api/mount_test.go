package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/dockergiant/images/inspector/internal/capture"
	"github.com/dockergiant/images/inspector/internal/core"
	"github.com/dockergiant/images/inspector/internal/magento"
	"github.com/dockergiant/images/inspector/internal/procmap"
	"github.com/dockergiant/images/inspector/internal/services"
	"github.com/dockergiant/images/inspector/internal/services/rabbitmq"
	"github.com/dockergiant/images/inspector/internal/services/redis"
	"github.com/dockergiant/images/inspector/internal/services/search"
	"github.com/dockergiant/images/inspector/internal/services/varnish"
)

func TestRealAdaptersMount(t *testing.T) {
	d := fakeDocker(traefikAddr)
	deps := services.Deps{Docker: d, Magento: magento.NewReader(d), Proc: procmap.New(d), Capture: capture.New(quietLog()), Log: quietLog()}
	adapters := []core.Adapter{varnish.New(deps), redis.New(deps), search.New(deps), rabbitmq.New(deps)}
	s := newServer(t, d, adapters...)
	h := s.Handler()
	for _, a := range adapters {
		w := httpGet(h, "/api/"+string(a.Kind())+"/info")
		if w.Code != http.StatusOK {
			t.Fatalf("%s info: %d %s", a.Kind(), w.Code, w.Body)
		}
		for _, rt := range a.Browse() {
			if rt.Method != http.MethodGet {
				continue
			}
			path := strings.ReplaceAll(rt.Path, "{name}", "x")
			if w := httpGet(h, "/api/"+string(a.Kind())+path); strings.Contains(w.Body.String(), "no such API route") {
				t.Fatalf("%s%s not mounted", a.Kind(), rt.Path)
			}
			if w := httpGet(h, "/api/v1/"+string(a.Kind())+path); strings.Contains(w.Body.String(), "no such v1 route") {
				t.Fatalf("%s%s not mirrored on v1", a.Kind(), rt.Path)
			}
		}
	}
	doc := httpGet(h, "/api/v1/openapi.json").Body.String()
	for _, a := range adapters {
		for _, rt := range a.Browse() {
			listed := strings.Contains(doc, `"/api/v1/`+string(a.Kind())+rt.Path+`"`)
			if want := rt.Method == http.MethodGet || postReads[a.Kind()][rt.Path]; listed != want {
				t.Errorf("%s %s%s: in openapi %v, want %v", rt.Method, a.Kind(), rt.Path, listed, want)
			}
		}
	}
	if strings.Contains(doc, "/api/v1/rabbitmq/peek") {
		t.Fatal("the RabbitMQ peek reached v1")
	}

	// A route that refuses an empty query must say in the OpenAPI document
	// what it wants, or a generated client can never call it.
	var spec struct {
		Paths map[string]struct {
			Get struct {
				Parameters []struct {
					Name     string `json:"name"`
					In       string `json:"in"`
					Required bool   `json:"required"`
				} `json:"parameters"`
			} `json:"get"`
		} `json:"paths"`
	}
	if err := json.Unmarshal([]byte(doc), &spec); err != nil {
		t.Fatal(err)
	}
	refused := 0
	for _, a := range adapters {
		for _, rt := range a.Browse() {
			if rt.Method != http.MethodGet {
				continue
			}
			path := "/api/v1/" + string(a.Kind()) + rt.Path
			w := httpGet(h, strings.ReplaceAll(path, "{name}", "x"))
			if w.Code >= 400 && !strings.Contains(w.Body.String(), `"hint"`) {
				t.Errorf("%s answers %d without a hint: %s", path, w.Code, w.Body)
			}
			if w.Code != http.StatusBadRequest {
				continue
			}
			refused++
			query := 0
			for _, p := range spec.Paths[path].Get.Parameters {
				if p.In == "query" {
					query++
				}
			}
			if query == 0 {
				t.Errorf("%s answers 400 to an empty query but declares no query parameter: %s", path, w.Body)
			}
		}
	}
	if refused < 4 {
		t.Fatalf("only %d routes refused an empty query; the check is not reaching them", refused)
	}
	for _, path := range []string{"/api/v1/redis/key", "/api/v1/redis/tag", "/api/v1/redis/find"} {
		required := false
		for _, p := range spec.Paths[path].Get.Parameters {
			required = required || (p.In == "query" && p.Required)
		}
		if !required {
			t.Errorf("%s declares no required query parameter", path)
		}
	}
}

func httpGet(h http.Handler, path string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	h.ServeHTTP(w, browserReq(http.MethodGet, path, nil))
	return w
}

func TestUIAssetsRevalidate(t *testing.T) {
	s := newServer(t, fakeDocker(traefikAddr))
	w := httpGet(s.Handler(), "/")
	if w.Code != http.StatusOK || w.Header().Get("Cache-Control") != "no-cache" {
		t.Fatalf("ui: %d cache-control %q", w.Code, w.Header().Get("Cache-Control"))
	}
	if w := httpGet(s.Handler(), "/api/session"); w.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("api cache-control %q", w.Header().Get("Cache-Control"))
	}
}
