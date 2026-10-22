package docker

import (
	"context"
	"testing"
	"time"
)

func TestLanFromBindings(t *testing.T) {
	cases := []struct {
		hosts []string
		want  bool
	}{
		{[]string{"127.0.0.1"}, false},
		{[]string{"0.0.0.0"}, true},
		{[]string{""}, true},
		{[]string{"::1"}, false},
		{[]string{"192.168.1.5"}, true},
		{[]string{"::"}, true},
		{[]string{"127.0.0.2"}, false},
		{[]string{"::ffff:127.0.0.1"}, false},
		{[]string{"localhost"}, true},
		{[]string{"127.0.0.1", "10.0.0.1"}, true},
		{[]string{"127.0.0.1", "::1"}, false},
		{nil, false},
	}
	for _, tc := range cases {
		if got := lanFromBindings(tc.hosts); got != tc.want {
			t.Errorf("lanFromBindings(%q) = %v, want %v", tc.hosts, got, tc.want)
		}
	}
}

func TestTraefikLookup(t *testing.T) {
	srv := newFakeDaemon(t)
	srv.containers = []map[string]any{
		summary("t1", "traefik", "roll", "traefik", "running", map[string]string{"shop_default": "192.168.156.2"}),
	}
	srv.inspect["t1"] = map[string]any{
		"Id": "t1",
		"HostConfig": map[string]any{"PortBindings": map[string]any{
			"80/tcp":  []map[string]string{{"HostIp": "127.0.0.1", "HostPort": "80"}},
			"443/tcp": []map[string]string{{"HostIp": "127.0.0.1", "HostPort": "443"}},
		}},
		"NetworkSettings": map[string]any{"Networks": map[string]any{
			"roll":         map[string]any{"IPAddress": "192.168.97.6"},
			"shop_default": map[string]any{"IPAddress": "192.168.156.2"},
		}},
	}
	c := srv.client(t, "shop")
	now := time.Unix(1000, 0)
	c.now = func() time.Time { return now }

	info, err := c.Traefik(context.Background(), false)
	if err != nil {
		t.Fatal(err)
	}
	if !info.Found || info.IP != "192.168.156.2" || info.LAN {
		t.Fatalf("got %+v", info)
	}
	if len(info.Bindings) != 2 || info.Bindings[0] != "127.0.0.1:80" || info.Bindings[1] != "127.0.0.1:443" {
		t.Fatalf("bindings %v", info.Bindings)
	}
	if f := srv.lastFilter("/containers/json"); !contains(f, "com.docker.compose.project=roll") || !contains(f, "com.docker.compose.service=traefik") {
		t.Fatalf("filter %s", f)
	}

	srv.inspect["t1"]["HostConfig"] = map[string]any{"PortBindings": map[string]any{
		"80/tcp": []map[string]string{{"HostIp": "", "HostPort": "80"}},
	}}
	calls := srv.count("/containers/t1/json")
	if _, err := c.Traefik(context.Background(), false); err != nil || srv.count("/containers/t1/json") != calls {
		t.Fatal("expected cached answer within 10s")
	}
	now = now.Add(11 * time.Second)
	info, err = c.Traefik(context.Background(), false)
	if err != nil || !info.LAN || info.Bindings[0] != "*:80" {
		t.Fatalf("after expiry got %+v, %v", info, err)
	}
	info, err = c.Traefik(context.Background(), true)
	if err != nil || srv.count("/containers/t1/json") != calls+2 {
		t.Fatalf("refresh did not re-inspect: %+v, %v", info, err)
	}
}

func TestTraefikMissing(t *testing.T) {
	srv := newFakeDaemon(t)
	c := srv.client(t, "shop3")
	info, err := c.Traefik(context.Background(), false)
	if err != nil {
		t.Fatal(err)
	}
	if info.Found || !info.LAN {
		t.Fatalf("got %+v", info)
	}
}

func TestTraefikNotOnEnvNetwork(t *testing.T) {
	srv := newFakeDaemon(t)
	srv.containers = []map[string]any{summary("t1", "traefik", "roll", "traefik", "running", nil)}
	srv.inspect["t1"] = map[string]any{
		"Id": "t1",
		"HostConfig": map[string]any{"PortBindings": map[string]any{
			"443/tcp": []map[string]string{{"HostIp": "127.0.0.1", "HostPort": "443"}},
		}},
		"NetworkSettings": map[string]any{"Networks": map[string]any{"roll": map[string]any{"IPAddress": "192.168.97.6"}}},
	}
	info, err := srv.client(t, "shop3").Traefik(context.Background(), false)
	if err != nil || !info.Found || info.IP != "" || info.LAN {
		t.Fatalf("got %+v, %v", info, err)
	}
}
