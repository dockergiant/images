package main

import (
	"bytes"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/dockergiant/images/inspector/internal/docker"
	"github.com/dockergiant/images/inspector/internal/magento"
)

const goodSecret = "0123456789abcdef0123456789abcdef"

func envOf(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func TestLoadConfig(t *testing.T) {
	cfg, err := loadConfig(envOf(map[string]string{"ROLL_ENV_NAME": "shop", "TRAEFIK_DOMAIN": "shop.test", "ROLL_INSPECTOR_SECRET": goodSecret}))
	if err != nil || cfg.env != "shop" || cfg.domain != "shop.test" || cfg.secret != goodSecret {
		t.Fatalf("cfg %+v err %v", cfg, err)
	}

	if cfg.sample != 10*time.Second || cfg.warning != "" {
		t.Fatalf("default sample %v warning %q", cfg.sample, cfg.warning)
	}
	base := map[string]string{"ROLL_ENV_NAME": "shop", "TRAEFIK_DOMAIN": "shop.test", "ROLL_INSPECTOR_SECRET": goodSecret, "TRAEFIK_SUBDOMAIN": "app"}
	for in, want := range map[string]time.Duration{"30": 30 * time.Second, "60": time.Minute, "off": 0} {
		base["ROLL_INSPECTOR_SAMPLE"] = in
		cfg, err = loadConfig(envOf(base))
		if err != nil || cfg.sample != want || cfg.subdomain != "app" {
			t.Fatalf("sample %q: %+v %v", in, cfg, err)
		}
	}
	base["ROLL_INSPECTOR_SAMPLE"] = "7"
	cfg, err = loadConfig(envOf(base))
	if err != nil || cfg.sample != 10*time.Second || !strings.Contains(cfg.warning, "ROLL_INSPECTOR_SAMPLE") {
		t.Fatalf("bad sample falls back: %+v %v", cfg, err)
	}

	_, err = loadConfig(envOf(nil))
	if err == nil {
		t.Fatal("started without env vars")
	}
	for _, name := range []string{"ROLL_ENV_NAME", "TRAEFIK_DOMAIN", "ROLL_INSPECTOR_SECRET"} {
		if !strings.Contains(err.Error(), name) {
			t.Errorf("message %q does not name %s", err, name)
		}
	}

	_, err = loadConfig(envOf(map[string]string{"ROLL_ENV_NAME": "shop", "TRAEFIK_DOMAIN": "shop.test", "ROLL_INSPECTOR_SECRET": "short"}))
	if err == nil || !strings.Contains(err.Error(), "32") {
		t.Fatalf("short secret: %v", err)
	}
}

func TestAllowList(t *testing.T) {
	d := &docker.Fake{EnvName: "shop", ContainerList: []docker.Container{
		{ID: "fpm", Service: "php-fpm", IP: "172.20.0.5", Running: true},
	}}
	clock := time.Unix(1_800_000_000, 0)
	a := newAllowList(d)
	a.now = func() time.Time { return clock }

	for _, ip := range []string{"127.0.0.1", "::1", "::ffff:127.0.0.1", "172.20.0.5"} {
		if !a.allow(ip) {
			t.Errorf("%s refused", ip)
		}
	}
	for _, ip := range []string{"172.20.0.9", "10.0.0.1", "", "nonsense"} {
		if a.allow(ip) {
			t.Errorf("%s allowed", ip)
		}
	}

	d.SetContainers(append(d.ContainerList, docker.Container{ID: "dbg", Service: "php-debug", IP: "172.20.0.9", Running: true}))
	if a.allow("172.20.0.9") {
		t.Fatal("cache refreshed on every miss")
	}
	clock = clock.Add(1500 * time.Millisecond)
	if !a.allow("172.20.0.9") {
		t.Fatal("a new container stayed refused after the retry gap")
	}

	d.SetContainers(nil)
	clock = clock.Add(2 * time.Second)
	if !a.allow("172.20.0.5") {
		t.Fatal("cache expired before 5 s")
	}
	clock = clock.Add(4 * time.Second)
	if a.allow("172.20.0.5") {
		t.Fatal("a removed container stayed allowed after 5 s")
	}
}

func TestFrontends(t *testing.T) {
	env := &magento.Env{
		Cache: map[string]magento.Frontend{
			"default":    {Backend: "Magento\\Framework\\Cache\\Backend\\Redis", Server: "redis", Database: 0},
			"page_cache": {Backend: "Magento\\Framework\\Cache\\Backend\\Redis", Server: "redis", Database: 1},
			"file_only":  {Backend: "Cm_Cache_Backend_File"},
			"extra":      {Backend: "Cm_Cache_Backend_Redis", Server: "redis", Database: 1},
		},
		SessionSave: "redis",
		SessionDB:   2,
	}
	got := frontends(env)
	want := map[int]string{0: "default", 1: "page_cache + extra", 2: "session"}
	if len(got) != len(want) {
		t.Fatalf("got %v", got)
	}
	for db, name := range want {
		if got[db] != name {
			t.Errorf("db %d: %q, want %q", db, got[db], name)
		}
	}
	if got := frontends(&magento.Env{SessionSave: "files"}); len(got) != 0 {
		t.Fatalf("file sessions gave %v", got)
	}
	if got := frontends(nil); len(got) != 0 {
		t.Fatalf("nil env gave %v", got)
	}
}

func TestRateLog(t *testing.T) {
	var buf bytes.Buffer
	clock := time.Unix(1_800_000_000, 0)
	r := newRateLog(slog.New(slog.NewTextHandler(&buf, nil)), time.Minute)
	r.now = func() time.Time { return clock }
	for range 5 {
		r.Error("pid map refresh failed", errors.New("walk php-fpm: exit 126"))
		clock = clock.Add(time.Second)
	}
	if n := strings.Count(buf.String(), "pid map refresh failed"); n != 1 {
		t.Fatalf("identical errors logged %d times:\n%s", n, buf.String())
	}
	r.Error("pid map refresh failed", errors.New("walk php-fpm: context deadline exceeded"))
	if n := strings.Count(buf.String(), "pid map refresh failed"); n != 2 {
		t.Fatalf("a changed error was suppressed:\n%s", buf.String())
	}
	clock = clock.Add(time.Minute)
	buf.Reset()
	r.Error("pid map refresh failed", errors.New("walk php-fpm: context deadline exceeded"))
	if !strings.Contains(buf.String(), "suppressed=0") {
		t.Fatalf("after the interval: %s", buf.String())
	}
	r.Error("pid map refresh failed", errors.New("walk php-fpm: context deadline exceeded"))
	clock = clock.Add(time.Minute)
	buf.Reset()
	r.Error("pid map refresh failed", errors.New("walk php-fpm: context deadline exceeded"))
	if !strings.Contains(buf.String(), "suppressed=1") {
		t.Fatalf("suppressed count missing: %s", buf.String())
	}
}

func TestHealthcheck(t *testing.T) {
	ok := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/info" {
			http.NotFound(w, r)
			return
		}
		w.Write([]byte(`{}`))
	}))
	defer ok.Close()
	if code := healthcheck(ok.URL+"/info", time.Second); code != 0 {
		t.Fatalf("healthy server: exit %d", code)
	}
	if code := healthcheck(ok.URL+"/nope", time.Second); code != 1 {
		t.Fatalf("404: exit %d", code)
	}
	ok.Close()
	if code := healthcheck(ok.URL+"/info", time.Second); code != 1 {
		t.Fatalf("nothing listening: exit %d", code)
	}
}

func TestRunRefusesWithoutEnv(t *testing.T) {
	t.Setenv("ROLL_ENV_NAME", "")
	t.Setenv("TRAEFIK_DOMAIN", "")
	t.Setenv("ROLL_INSPECTOR_SECRET", "")
	var stderr bytes.Buffer
	if code := run(nil, &stderr); code != 2 {
		t.Fatalf("exit %d", code)
	}
	if !strings.Contains(stderr.String(), "ROLL_ENV_NAME") {
		t.Fatalf("message %q", stderr.String())
	}
}
