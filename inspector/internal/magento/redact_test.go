package magento

import (
	"encoding/json"
	"os"
	"reflect"
	"strings"
	"testing"
)

func TestRedactKeys(t *testing.T) {
	cases := []struct {
		key    string
		redact bool
	}{
		{"valkey", false},
		{"api_key", true},
		{"client_secret", true},
		{"apikey_live", true},
		{"access_token", true},
		{"ftp_password", true},
		{"userid", true},
		{"username", true},
		{"user", true},
		{"id_salt", true},
		{"password", true},
		{"PASSWORD", true},
		{"pwd", true},
		{"db_passwd", true},
		{"api_signature", true},
		{"ssl_cert", true},
		{"key", true},
		{"apiKey", true},
		{"id_prefix", false},
		{"server", false},
		{"monkey", false},
		{"turkey_count", false},
		{"compression_lib", false},
		{"persistent_identifier", false},
		{"opensearch_username", false},
		{"base_url", false},
	}
	for _, c := range cases {
		got := Redact(map[string]any{c.key: "value"}).(map[string]any)[c.key]
		if (got == "REDACTED") != c.redact {
			t.Errorf("%s: got %v, want redacted=%v", c.key, got, c.redact)
		}
	}
}

func TestRedactNested(t *testing.T) {
	in := map[string]any{
		"queue": map[string]any{"amqp": map[string]any{"host": "rabbitmq", "user": "guest", "password": "guest"}},
		"list":  []any{map[string]any{"token": "x", "host": "a"}, "plain"},
		"secret": map[string]any{
			"nested": "gone",
		},
		"n": 3,
	}
	want := map[string]any{
		"queue":  map[string]any{"amqp": map[string]any{"host": "rabbitmq", "user": "REDACTED", "password": "REDACTED"}},
		"list":   []any{map[string]any{"token": "REDACTED", "host": "a"}, "plain"},
		"secret": "REDACTED",
		"n":      3,
	}
	if got := Redact(in); !reflect.DeepEqual(got, want) {
		t.Fatalf("got %#v", got)
	}
	if in["queue"].(map[string]any)["amqp"].(map[string]any)["password"] != "guest" {
		t.Fatal("Redact modified its input")
	}
}

func TestRedactStringMap(t *testing.T) {
	got := Redact(map[string]string{"carriers/ups/password": "x", "password": "y", "host": "z"})
	want := map[string]string{"carriers/ups/password": "x", "password": "REDACTED", "host": "z"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %#v", got)
	}
}

func TestRedactStruct(t *testing.T) {
	type creds struct {
		Host     string `json:"host"`
		Password string `json:"password"`
	}
	got := Redact(struct {
		Creds creds `json:"creds"`
	}{creds{"db", "hunter2"}})
	b, _ := json.Marshal(got)
	if strings.Contains(string(b), "hunter2") || !strings.Contains(string(b), `"host":"db"`) {
		t.Fatalf("got %s", b)
	}
	if Redact(nil) != nil {
		t.Fatal("nil should stay nil")
	}
}

func TestFixturesAreRedacted(t *testing.T) {
	for _, name := range []string{"envphp-mageos.json", "envphp-magento.json"} {
		b, err := os.ReadFile(fixture(name))
		if err != nil {
			t.Fatal(err)
		}
		var v any
		if err := json.Unmarshal(b, &v); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(Redact(v), v) {
			t.Errorf("%s holds values Redact would replace", name)
		}
	}
}
