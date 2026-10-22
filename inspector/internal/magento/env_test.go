package magento

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func fixture(name string) string {
	return filepath.Join("..", "..", "testdata", "magento", name)
}

func readFixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(fixture(name))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func scriptOutput(b []byte) []byte {
	var line bytes.Buffer
	if err := json.Compact(&line, b); err != nil {
		panic(err)
	}
	return []byte("PHP Deprecated: noise\n" + jsonMarker + line.String() + "\n")
}

func TestParseEnvMageOS(t *testing.T) {
	env, err := parseEnv(scriptOutput(readFixture(t, "envphp-mageos.json")))
	if err != nil {
		t.Fatal(err)
	}
	if env.Generic || env.Framework != "mage-os" || env.FrameworkVersion != "3.5.0" || env.Mode != "developer" {
		t.Fatalf("framework fields: %+v", env)
	}
	want := map[string]Frontend{
		"default":    {Backend: "valkey", Server: "redis", Port: 6379, Database: 0, IDPrefix: "69d_", CompressData: true, Serializer: "igbinary"},
		"page_cache": {Backend: "valkey", Server: "redis", Port: 6379, Database: 1, IDPrefix: "69d_", CompressData: false, Serializer: "igbinary"},
	}
	if !reflect.DeepEqual(env.Cache, want) {
		t.Fatalf("cache: %+v", env.Cache)
	}
	if env.SessionSave != "valkey" || env.SessionHost != "127.0.0.1" || env.SessionDB != 2 {
		t.Fatalf("session: %q %q %d", env.SessionSave, env.SessionHost, env.SessionDB)
	}
	if env.AMQP == nil || env.AMQP.Host != "rabbitmq" || env.AMQP.Port != 5672 || env.AMQP.VHost != "/" || env.AMQP.User != "REDACTED" {
		t.Fatalf("amqp: %+v", env.AMQP)
	}
	if !reflect.DeepEqual(env.HTTPCacheHosts, []string{"varnish:80"}) {
		t.Fatalf("http cache hosts: %v", env.HTTPCacheHosts)
	}
	if env.CacheTypes["full_page"] || !env.CacheTypes["config"] || len(env.CacheTypes) != 18 {
		t.Fatalf("cache types: %v", env.CacheTypes)
	}
	if env.DBHost != "db" || env.DBName != "magento" || env.TablePrefix != "" {
		t.Fatalf("db: %q %q %q", env.DBHost, env.DBName, env.TablePrefix)
	}
	if env.BaseURLs["default/web/secure/base_url"] != "https://app.shop.test/" {
		t.Fatalf("base urls: %v", env.BaseURLs)
	}
	if env.FPCApplication != "varnish" || env.Igbinary || env.MTime == 0 {
		t.Fatalf("fpc %q igbinary %v mtime %d", env.FPCApplication, env.Igbinary, env.MTime)
	}
}

func TestParseEnvMagento(t *testing.T) {
	env, err := parseEnv(scriptOutput(readFixture(t, "envphp-magento.json")))
	if err != nil {
		t.Fatal(err)
	}
	if env.Framework != "magento" || env.FrameworkVersion != "2.4.7-p10" {
		t.Fatalf("framework %q %q", env.Framework, env.FrameworkVersion)
	}
	if env.AMQP != nil {
		t.Fatalf("amqp should be absent: %+v", env.AMQP)
	}
	if env.SessionSave != "redis" || env.SessionHost == "" {
		t.Fatalf("session %q %q", env.SessionSave, env.SessionHost)
	}
	if env.FPCApplication != "builtin" {
		t.Fatalf("fpc %q", env.FPCApplication)
	}
	if env.Cache["default"].Backend == "" || env.Cache["page_cache"].Server == "" {
		t.Fatalf("cache %+v", env.Cache)
	}
}

func TestParseEnvSynthetic(t *testing.T) {
	env, err := parseEnv(scriptOutput(readFixture(t, "envphp-synthetic.json")))
	if err != nil {
		t.Fatal(err)
	}
	if env.Framework != "magento" || env.FrameworkVersion != "2.4.7-p3" || env.TablePrefix != "m2_" {
		t.Fatalf("got %+v", env)
	}
	if env.Cache["default"].CompressionLib != "gzip" || !env.Cache["default"].CompressData {
		t.Fatalf("default frontend %+v", env.Cache["default"])
	}
	if env.CacheTypes["full_page"] || !env.CacheTypes["layout"] {
		t.Fatalf("cache types %v", env.CacheTypes)
	}
	wantURLs := map[string]string{
		"default/web/unsecure/base_url":     "https://app.example.test/",
		"default/web/secure/base_url":       "https://app.example.test/",
		"websites/nl/web/unsecure/base_url": "https://nl.example.test/",
		"websites/nl/web/secure/base_url":   "https://nl.example.test/",
		"stores/be_fr/web/secure/base_url":  "https://fr.example.test/",
	}
	if !reflect.DeepEqual(env.BaseURLs, wantURLs) {
		t.Fatalf("base urls %v", env.BaseURLs)
	}
	if env.SearchEngine != "opensearch" || env.FPCApplication != "varnish" {
		t.Fatalf("search %q fpc %q", env.SearchEngine, env.FPCApplication)
	}
	if env.AMQP == nil || env.AMQP.User != "planted-amqp-user" || env.AMQP.Password != "planted-amqp-password" {
		t.Fatalf("amqp credentials must stay in memory: %+v", env.AMQP)
	}
	b, err := json.Marshal(env)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "planted") {
		t.Fatalf("Env JSON leaks AMQP credentials: %s", b)
	}
}

var planted = []string{
	"PLANTED", "sk_live", "live_PLANTED",
}

func TestEnvScriptOutputHoldsNoCredentials(t *testing.T) {
	raw := readFixture(t, "envphp-synthetic.json")
	for _, p := range planted {
		if bytes.Contains(raw, []byte(p)) {
			t.Fatalf("script output holds %q", p)
		}
	}
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(Redact(v))
	if bytes.Contains(b, []byte("planted-amqp")) {
		t.Fatalf("Redact left AMQP credentials: %s", b)
	}
}

func TestEnvScriptOnFixture(t *testing.T) {
	php, err := exec.LookPath("php")
	if err != nil {
		t.Skip("php not installed; envphp-synthetic.json was recorded from testdata/magento/root in the php-fpm image")
	}
	cmd := exec.Command(php)
	cmd.Dir = fixture("root")
	cmd.Stdin = bytes.NewReader(envScript)
	out, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range planted {
		if bytes.Contains(out, []byte(p)) {
			t.Fatalf("script output holds %q", p)
		}
	}
	got, err := parseEnv(out)
	if err != nil {
		t.Fatal(err)
	}
	want, _ := parseEnv(scriptOutput(readFixture(t, "envphp-synthetic.json")))
	got.MTime, want.MTime = 0, 0
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v\nwant %+v", got, want)
	}
}

func TestParseEnvGeneric(t *testing.T) {
	env, err := parseEnv([]byte(jsonMarker + `{"generic":true}` + "\n"))
	if err != nil || !env.Generic || env.MTime != 0 {
		t.Fatalf("got %+v, %v", env, err)
	}
	env, err = parseEnv([]byte(jsonMarker + `{"generic":true,"mtime":5,"igbinary":false,"framework":null,"frameworkVersion":null,"env":{"MAGE_MODE":"default"}}` + "\n"))
	if err != nil || !env.Generic || env.Mode != "default" || env.Framework != "" {
		t.Fatalf("got %+v, %v", env, err)
	}
}

func TestParseEnvErrors(t *testing.T) {
	if _, err := parseEnv([]byte("Fatal error: something\n")); err == nil {
		t.Fatal("missing marker should fail")
	}
	if _, err := parseEnv([]byte(jsonMarker + "{broken\n")); err == nil {
		t.Fatal("broken JSON should fail")
	}
}
