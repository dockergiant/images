package docker

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync"
	"testing"

	"github.com/moby/moby/client"
)

type fakeDaemon struct {
	t          *testing.T
	srv        *httptest.Server
	mu         sync.Mutex
	containers []map[string]any
	inspect    map[string]map[string]any
	requests   []*http.Request
}

var versionPrefix = regexp.MustCompile(`^/v[0-9.]+`)

func newFakeDaemon(t *testing.T) *fakeDaemon {
	d := &fakeDaemon{t: t, inspect: map[string]map[string]any{}}
	d.srv = httptest.NewServer(http.HandlerFunc(d.serve))
	t.Cleanup(d.srv.Close)
	return d
}

func (d *fakeDaemon) client(t *testing.T, env string) *Client {
	c, err := newClient(env,
		client.WithHost("tcp://"+strings.TrimPrefix(d.srv.URL, "http://")),
		client.WithAPIVersion("1.47"),
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}

func (d *fakeDaemon) serve(w http.ResponseWriter, r *http.Request) {
	d.mu.Lock()
	defer d.mu.Unlock()
	r.URL.Path = versionPrefix.ReplaceAllString(r.URL.Path, "")
	d.requests = append(d.requests, r)
	w.Header().Set("Content-Type", "application/json")
	switch {
	case r.URL.Path == "/containers/json":
		json.NewEncoder(w).Encode(d.list(r))
	case strings.HasPrefix(r.URL.Path, "/containers/") && strings.HasSuffix(r.URL.Path, "/json"):
		id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/containers/"), "/json")
		body, ok := d.inspect[id]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			json.NewEncoder(w).Encode(map[string]string{"message": "No such container: " + id})
			return
		}
		json.NewEncoder(w).Encode(body)
	default:
		w.WriteHeader(http.StatusNotImplemented)
		json.NewEncoder(w).Encode(map[string]string{"message": "not faked: " + r.URL.Path})
	}
}

func (d *fakeDaemon) list(r *http.Request) []map[string]any {
	var filters map[string]map[string]bool
	if f := r.URL.Query().Get("filters"); f != "" {
		if err := json.Unmarshal([]byte(f), &filters); err != nil {
			d.t.Errorf("bad filters %q: %v", f, err)
		}
	}
	all := r.URL.Query().Get("all") == "1"
	out := []map[string]any{}
	for _, c := range d.containers {
		if !all && c["State"] != "running" {
			continue
		}
		labels, _ := c["Labels"].(map[string]string)
		match := true
		for kv := range filters["label"] {
			k, v, _ := strings.Cut(kv, "=")
			if labels[k] != v {
				match = false
			}
		}
		if match {
			out = append(out, c)
		}
	}
	return out
}

func (d *fakeDaemon) count(path string) int {
	d.mu.Lock()
	defer d.mu.Unlock()
	n := 0
	for _, r := range d.requests {
		if r.URL.Path == path {
			n++
		}
	}
	return n
}

func (d *fakeDaemon) lastFilter(path string) string {
	d.mu.Lock()
	defer d.mu.Unlock()
	for i := len(d.requests) - 1; i >= 0; i-- {
		if d.requests[i].URL.Path == path {
			return d.requests[i].URL.Query().Get("filters")
		}
	}
	return ""
}

func summary(id, name, project, service, state string, networks map[string]string) map[string]any {
	nets := map[string]any{}
	for k, ip := range networks {
		nets[k] = map[string]any{"IPAddress": ip}
	}
	labels := map[string]string{labelProject: project}
	if service != "" {
		labels[labelService] = service
	}
	return map[string]any{
		"Id":              id,
		"Names":           []string{"/" + name},
		"Image":           "img/" + service,
		"State":           state,
		"Labels":          labels,
		"NetworkSettings": map[string]any{"Networks": nets},
	}
}

func contains(s, sub string) bool {
	return strings.Contains(s, sub)
}
