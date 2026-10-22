package magento

import (
	"bytes"
	"context"
	"errors"
	"io"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dockergiant/images/inspector/internal/docker"
)

type php struct {
	t         *testing.T
	mu        sync.Mutex
	mtime     string
	installed string
	env       []byte
	stdout    map[string]string
	exit      map[string]int
	scripts   []string
	args      [][]string
}

func newPHP(t *testing.T) (*php, *docker.Fake) {
	p := &php{t: t, mtime: "1792466946", installed: "1792466000", stdout: map[string]string{}, exit: map[string]int{}}
	f := &docker.Fake{
		EnvName: "shop",
		ContainerList: []docker.Container{
			{ID: "fpm1", Service: "php-fpm", Running: true},
			{ID: "dbg1", Service: "php-debug", Running: true},
		},
		ExecFunc: p.exec,
	}
	return p, f
}

func (p *php) exec(ctx context.Context, spec docker.ExecSpec) (docker.ExecResult, error) {
	if spec.Container != "fpm1" || spec.User != PHPUser || spec.Privileged {
		p.t.Errorf("exec %v in %q as %q privileged=%v", spec.Cmd, spec.Container, spec.User, spec.Privileged)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	switch {
	case reflect.DeepEqual(spec.Cmd, []string{"stat", "-c", "%n %Y", "app/etc/env.php", "vendor/composer/installed.json"}):
		var out, errs string
		code := 0
		for _, f := range []struct{ name, mtime string }{{"app/etc/env.php", p.mtime}, {"vendor/composer/installed.json", p.installed}} {
			if f.mtime == "" {
				errs += "stat: cannot statx '" + f.name + "': No such file or directory\n"
				code = 1
				continue
			}
			out += f.name + " " + f.mtime + "\n"
		}
		return docker.ExecResult{Stdout: []byte(out), Stderr: []byte(errs), ExitCode: code}, nil
	case spec.Cmd[0] == "php" && !spec.KillOnCancel:
		p.t.Errorf("php exec %v outlives its timeout", spec.Cmd)
	case spec.Cmd[0] == "php" && spec.Stdin != nil:
		script, _ := io.ReadAll(spec.Stdin)
		name := scriptName(script)
		p.scripts = append(p.scripts, name)
		p.args = append(p.args, spec.Cmd[1:])
		if code := p.exit[name]; code != 0 {
			return docker.ExecResult{Stderr: []byte("PHP Fatal error: boom"), ExitCode: code}, nil
		}
		if name == "envphp" {
			return docker.ExecResult{Stdout: scriptOutput(p.env)}, nil
		}
		return docker.ExecResult{Stdout: []byte(jsonMarker + p.stdout[name] + "\n")}, nil
	case len(spec.Cmd) >= 3 && spec.Cmd[0] == "php" && spec.Cmd[1] == "bin/magento":
		name := spec.Cmd[2]
		p.scripts = append(p.scripts, name)
		p.args = append(p.args, spec.Cmd[3:])
		return docker.ExecResult{Stdout: []byte(p.stdout[name]), ExitCode: p.exit[name]}, nil
	}
	p.t.Errorf("unexpected exec %v", spec.Cmd)
	return docker.ExecResult{ExitCode: 127}, nil
}

func scriptName(script []byte) string {
	switch {
	case bytes.Equal(script, envScript):
		return "envphp"
	case bytes.Equal(script, cacheTypesScript):
		return "cachetypes"
	case bytes.Equal(script, fpcKeyScript):
		return "fpckey"
	case bytes.Equal(script, consumersScript):
		return "consumers"
	}
	return "unknown"
}

func (p *php) ran() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.scripts...)
}

type clock struct{ t time.Time }

func (c *clock) now() time.Time { return c.t }

func newTestReader(f docker.API) (*reader, *clock) {
	c := &clock{t: time.Unix(1_800_000_000, 0)}
	r := NewReader(f).(*reader)
	r.now = c.now
	return r, c
}

func TestLoadParsesAndCaches(t *testing.T) {
	p, f := newPHP(t)
	p.env = readFixture(t, "envphp-mageos.json")
	r, c := newTestReader(f)
	ctx := context.Background()

	env, err := r.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if env.Framework != "mage-os" || env.Cache["page_cache"].Database != 1 {
		t.Fatalf("got %+v", env)
	}
	if len(f.Execs()) != 2 {
		t.Fatalf("first load: %d execs", len(f.Execs()))
	}

	c.t = c.t.Add(4 * time.Second)
	if again, _ := r.Load(ctx); again != env || len(f.Execs()) != 2 {
		t.Fatalf("within 5 s Load must not exec, got %d execs", len(f.Execs()))
	}

	c.t = c.t.Add(2 * time.Second)
	if again, _ := r.Load(ctx); again != env || len(f.Execs()) != 3 {
		t.Fatalf("same mtime: want only a stat, got %d execs", len(f.Execs()))
	}

	p.mu.Lock()
	p.mtime = "1792466999"
	p.mu.Unlock()
	c.t = c.t.Add(6 * time.Second)
	if _, err := r.Load(ctx); err != nil || len(f.Execs()) != 5 {
		t.Fatalf("changed mtime: want stat and script, got %d execs, %v", len(f.Execs()), err)
	}
	if got := p.ran(); !reflect.DeepEqual(got, []string{"envphp", "envphp"}) {
		t.Fatalf("scripts %v", got)
	}
}

func TestLoadRereadsOnNewContainer(t *testing.T) {
	p, f := newPHP(t)
	p.env = readFixture(t, "envphp-magento.json")
	r, c := newTestReader(f)
	if _, err := r.Load(context.Background()); err != nil {
		t.Fatal(err)
	}
	r.mu.Lock()
	r.envContainer = "old"
	r.mu.Unlock()
	c.t = c.t.Add(10 * time.Second)
	if _, err := r.Load(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(p.ran()) != 2 {
		t.Fatalf("a recreated php-fpm container must be reread, scripts %v", p.ran())
	}
}

func TestLoadGenericWhenEnvMissing(t *testing.T) {
	p, f := newPHP(t)
	p.mtime = ""
	p.env = []byte(`{"generic":true}`)
	r, _ := newTestReader(f)
	env, err := r.Load(context.Background())
	if err != nil || !env.Generic {
		t.Fatalf("got %+v, %v", env, err)
	}
}

func TestLoadRereadsWhenInstalledJSONChanges(t *testing.T) {
	p, f := newPHP(t)
	p.installed = ""
	p.env = []byte(`{"generic":true}`)
	r, c := newTestReader(f)
	env, err := r.Load(context.Background())
	if err != nil || !env.Generic {
		t.Fatalf("before composer install: %+v, %v", env, err)
	}

	p.mu.Lock()
	p.installed = "1792467000"
	p.env = readFixture(t, "envphp-mageos.json")
	p.mu.Unlock()
	c.t = c.t.Add(statEvery)
	env, err = r.Load(context.Background())
	if err != nil || env.Generic {
		t.Fatalf("after composer install with env.php untouched: %+v, %v", env, err)
	}
	if got := p.ran(); len(got) != 2 {
		t.Fatalf("scripts %v, want the env script to run again", got)
	}

	c.t = c.t.Add(statEvery)
	if _, err := r.Load(context.Background()); err != nil || len(p.ran()) != 2 {
		t.Fatalf("unchanged files must not rerun the script: %v, %v", p.ran(), err)
	}
}

func TestLoadWithoutPHP(t *testing.T) {
	f := &docker.Fake{}
	r, _ := newTestReader(f)
	if _, err := r.Load(context.Background()); !errors.Is(err, ErrNoPHP) {
		t.Fatalf("got %v", err)
	}
	if _, err := r.CacheTypeTags(context.Background()); !errors.Is(err, ErrNoPHP) {
		t.Fatalf("got %v", err)
	}
}

func TestLoadScriptFailure(t *testing.T) {
	p, f := newPHP(t)
	p.exit["envphp"] = 255
	r, _ := newTestReader(f)
	_, err := r.Load(context.Background())
	if err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("got %v", err)
	}
}

func TestCacheTypeTags(t *testing.T) {
	p, f := newPHP(t)
	p.stdout["cachetypes"] = `{"config":"CONFIG","full_page":"FPC","compiled_plugins":""}`
	r, c := newTestReader(f)
	tags, err := r.CacheTypeTags(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"config": "CONFIG", "full_page": "FPC", "compiled_plugins": ""}
	if !reflect.DeepEqual(tags, want) {
		t.Fatalf("got %v", tags)
	}
	tags["config"] = "changed"
	c.t = c.t.Add(9 * time.Minute)
	again, _ := r.CacheTypeTags(context.Background())
	if again["config"] != "CONFIG" || len(p.ran()) != 1 {
		t.Fatalf("cached copy: %v, scripts %v", again, p.ran())
	}
	c.t = c.t.Add(2 * time.Minute)
	r.CacheTypeTags(context.Background())
	if len(p.ran()) != 2 {
		t.Fatalf("after 10 min: scripts %v", p.ran())
	}
}

func TestConsumers(t *testing.T) {
	p, f := newPHP(t)
	p.stdout["consumers"] = `{"async.operations.all":"async.operations.all","exportProcessor":"export"}`
	r, c := newTestReader(f)
	got, err := r.Consumers(context.Background())
	if err != nil || got["exportProcessor"] != "export" || len(got) != 2 {
		t.Fatalf("got %v, %v", got, err)
	}
	c.t = c.t.Add(9 * time.Minute)
	r.Consumers(context.Background())
	if len(p.ran()) != 1 {
		t.Fatalf("consumers must be cached for 10 min: %v", p.ran())
	}
	c.t = c.t.Add(2 * time.Minute)
	r.Consumers(context.Background())
	if len(p.ran()) != 2 {
		t.Fatalf("after 10 min: %v", p.ran())
	}
}

func TestFPCKey(t *testing.T) {
	p, f := newPHP(t)
	p.stdout["fpckey"] = `{"key":"f4ef1b765b4b9f56343b28127d22a2549e84a85e"}`
	r, _ := newTestReader(f)
	url := `https://app.shop.test/a b?q="x";$(id)`
	key, err := r.FPCKey(context.Background(), url)
	if err != nil || key != "f4ef1b765b4b9f56343b28127d22a2549e84a85e" {
		t.Fatalf("got %q, %v", key, err)
	}
	if got := p.args[0]; !reflect.DeepEqual(got, []string{"--", url}) {
		t.Fatalf("url must be its own argv element, got %q", got)
	}
}

func TestFPCKeyRejectsBadURLs(t *testing.T) {
	_, f := newPHP(t)
	r, _ := newTestReader(f)
	for _, u := range []string{"", "/relative", "ftp://x.test/", "https:///nohost", "https://x.test/\nfoo", "https://x.test/\x00", "-r phpinfo()"} {
		if _, err := r.FPCKey(context.Background(), u); err == nil {
			t.Errorf("%q accepted", u)
		}
	}
	if len(f.Execs()) != 0 {
		t.Fatalf("rejected URLs must not exec: %v", f.Execs())
	}
}

func TestFPCKeyFailure(t *testing.T) {
	p, f := newPHP(t)
	p.exit["fpckey"] = 1
	r, _ := newTestReader(f)
	if _, err := r.FPCKey(context.Background(), "https://x.test/"); err == nil {
		t.Fatal("want error")
	}
	p.exit["fpckey"] = 0
	p.stdout["fpckey"] = `{"key":""}`
	if _, err := r.FPCKey(context.Background(), "https://x.test/"); err == nil {
		t.Fatal("empty key must be an error")
	}
}

const configDump = `catalog/search/engine - opensearch
system/full_page_cache/varnish/access_list - localhost
system/full_page_cache/varnish/backend_host - localhost
system/full_page_cache/caching_application - 2
system/full_page_cache/ttl - 604800
web/unsecure/base_url - https://app.shop.test/
web/secure/base_url - https://app.shop.test/
design/head/includes - <link rel="stylesheet" type="text/css" media="all" href="{{MEDIA_URL}}styles.css" />
<script>var a = 1;</script>
web/cookie/cookie_domain -
carriers/ups/password - ******
payment/stripe/api_key - sk_test_123
dev/debug/value - a - b
`

func TestConfigShow(t *testing.T) {
	p, f := newPHP(t)
	p.stdout["config:show"] = configDump
	r, c := newTestReader(f)
	got, err := r.ConfigShow(context.Background(), []string{"system/full_page_cache", "web/secure/base_url", "web/cookie/cookie_domain", "design/head/includes", "payment/stripe/api_key", "dev/debug/value", "missing/path/x"})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"system/full_page_cache/varnish/access_list":  "localhost",
		"system/full_page_cache/varnish/backend_host": "localhost",
		"system/full_page_cache/caching_application":  "2",
		"system/full_page_cache/ttl":                  "604800",
		"web/secure/base_url":                         "https://app.shop.test/",
		"web/cookie/cookie_domain":                    "",
		"design/head/includes":                        "<link rel=\"stylesheet\" type=\"text/css\" media=\"all\" href=\"{{MEDIA_URL}}styles.css\" />\n<script>var a = 1;</script>",
		"payment/stripe/api_key":                      "REDACTED",
		"dev/debug/value":                             "a - b",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %#v", got)
	}
	if args := p.args[0]; !reflect.DeepEqual(args, []string{"--no-ansi", "--no-interaction"}) {
		t.Fatalf("config:show args %v", args)
	}

	c.t = c.t.Add(time.Minute)
	if _, err := r.ConfigShow(context.Background(), []string{"web/unsecure/base_url"}); err != nil || len(p.ran()) != 1 {
		t.Fatalf("config dump must be cached, ran %v, %v", p.ran(), err)
	}
	c.t = c.t.Add(10 * time.Minute)
	r.ConfigShow(context.Background(), []string{"web/unsecure/base_url"})
	if len(p.ran()) != 2 {
		t.Fatalf("expired dump must be reread, ran %v", p.ran())
	}
}

func TestConfigShowRejectsBadPaths(t *testing.T) {
	_, f := newPHP(t)
	r, _ := newTestReader(f)
	for _, path := range []string{"", "-v", "web/../x", "web/secure base_url", "web/secure/\n"} {
		if _, err := r.ConfigShow(context.Background(), []string{path}); err == nil {
			t.Errorf("%q accepted", path)
		}
	}
	if len(f.Execs()) != 0 {
		t.Fatal("rejected paths must not exec")
	}
}

func TestConfigShowFailure(t *testing.T) {
	p, f := newPHP(t)
	p.exit["config:show"] = 1
	r, _ := newTestReader(f)
	if _, err := r.ConfigShow(context.Background(), []string{"web/secure/base_url"}); err == nil {
		t.Fatal("want error")
	}
}

func TestCacheClean(t *testing.T) {
	p, f := newPHP(t)
	r, _ := newTestReader(f)
	if err := r.CacheClean(context.Background(), []string{"full_page", "config"}); err != nil {
		t.Fatal(err)
	}
	if got := p.args[0]; !reflect.DeepEqual(got, []string{"--no-ansi", "--no-interaction", "full_page", "config"}) {
		t.Fatalf("args %v", got)
	}
	for _, bad := range [][]string{{"--all"}, {"full page"}, {""}, {"x;rm"}} {
		if err := r.CacheClean(context.Background(), bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
	if len(p.ran()) != 1 {
		t.Fatalf("rejected types must not exec: %v", p.ran())
	}
	p.exit["cache:clean"] = 1
	if err := r.CacheClean(context.Background(), []string{"config"}); err == nil {
		t.Fatal("want error on failure")
	}
}
