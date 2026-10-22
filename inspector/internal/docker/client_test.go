package docker

import (
	"context"
	"strings"
	"testing"
)

func TestContainersFromLabels(t *testing.T) {
	srv := newFakeDaemon(t)
	net := map[string]string{"shop2_default": "192.168.147.5", "roll": "192.168.97.9"}
	srv.containers = []map[string]any{
		summary("v1", "shop2-varnish-1", "shop2", "varnish", "running", net),
		summary("i1", "shop2-inspector-1", "shop2", "inspector", "running", net),
		summary("r1", "shop2-valkey-1", "shop2", "redis", "exited", nil),
		summary("o1", "shop3-redis-1", "shop3", "redis", "running", nil),
	}
	srv.containers[0]["Health"] = map[string]any{"Status": "healthy"}
	c := srv.client(t, "shop2")

	list, err := c.Containers(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 {
		t.Fatalf("got %+v", list)
	}
	r, v := list[0], list[1]
	if r.Service != "redis" || r.Running || r.IP != "" || r.Name != "shop2-valkey-1" {
		t.Fatalf("redis %+v", r)
	}
	if v.ID != "v1" || !v.Running || v.IP != "192.168.147.5" || v.Health != "healthy" || v.Project != "shop2" {
		t.Fatalf("varnish %+v", v)
	}
	if f := srv.lastFilter("/containers/json"); !strings.Contains(f, "com.docker.compose.project=shop2") {
		t.Fatalf("filter %s", f)
	}

	svc, err := c.Service(context.Background(), "redis")
	if err != nil || svc != nil {
		t.Fatalf("stopped redis should be absent: %+v, %v", svc, err)
	}
	svc, err = c.Service(context.Background(), "varnish")
	if err != nil || svc == nil || svc.ID != "v1" {
		t.Fatalf("varnish: %+v, %v", svc, err)
	}
	if svc, _ := c.Service(context.Background(), "inspector"); svc != nil {
		t.Fatal("inspector must be skipped")
	}
}

func TestContainerEnv(t *testing.T) {
	srv := newFakeDaemon(t)
	srv.inspect["p1"] = map[string]any{"Id": "p1", "Config": map[string]any{
		"Env":    []string{"A=1", "B=2"},
		"Labels": map[string]string{labelProject: "shop3", labelService: "php-fpm"},
	}}
	env, err := srv.client(t, "shop3").ContainerEnv(context.Background(), "p1")
	if err != nil || len(env) != 2 || env[1] != "B=2" {
		t.Fatalf("got %v, %v", env, err)
	}
}

func TestExecRefusesForeignContainers(t *testing.T) {
	srv := newFakeDaemon(t)
	srv.inspect["x1"] = map[string]any{"Id": "x1", "Config": map[string]any{"Labels": map[string]string{labelProject: "shop3", labelService: "php-fpm"}}}
	srv.inspect["t1"] = map[string]any{"Id": "t1", "Config": map[string]any{"Labels": map[string]string{labelProject: "roll", labelService: "traefik"}}}
	srv.inspect["s1"] = map[string]any{"Id": "s1", "Config": map[string]any{"Labels": map[string]string{labelProject: "shop", labelService: "inspector"}}}
	c := srv.client(t, "shop")
	for _, id := range []string{"x1", "t1", "s1", "missing"} {
		if _, err := c.Exec(context.Background(), ExecSpec{Container: id, Cmd: []string{"true"}}); err == nil {
			t.Errorf("exec in %s allowed", id)
		}
		if err := c.Stream(context.Background(), ExecSpec{Container: id, Cmd: []string{"true"}}, func(string) {}); err == nil {
			t.Errorf("stream in %s allowed", id)
		}
	}
	if n := srv.count("/containers/x1/exec"); n != 0 {
		t.Fatalf("exec created %d times", n)
	}
}

func TestWithMarker(t *testing.T) {
	in := []string{"A=1"}
	out := withMarker(in)
	if len(out) != 2 || out[1] != ExecMarker || len(in) != 1 {
		t.Fatalf("got %v from %v", out, in)
	}
	if out := withMarker(nil); len(out) != 1 || out[0] != ExecMarker {
		t.Fatalf("got %v", out)
	}
}
