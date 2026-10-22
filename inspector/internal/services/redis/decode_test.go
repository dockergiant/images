package redis

import (
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type entryFixtures struct {
	Symfony map[string]string            `json:"symfony"`
	Cm      map[string]map[string]string `json:"cm"`
}

func loadEntries(t *testing.T) (map[string][]byte, map[string]map[string][]byte) {
	t.Helper()
	var f entryFixtures
	if err := json.Unmarshal([]byte(readFixture(t, "entries.json")), &f); err != nil {
		t.Fatal(err)
	}
	dec := func(s string) []byte {
		b, err := base64.StdEncoding.DecodeString(s)
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	sym := map[string][]byte{}
	for k, v := range f.Symfony {
		sym[k] = dec(v)
	}
	cm := map[string]map[string][]byte{}
	for k, h := range f.Cm {
		cm[k] = map[string][]byte{}
		for field, v := range h {
			cm[k][field] = dec(v)
		}
	}
	return sym, cm
}

func TestDecodeSymfonyRecorded(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join(fixtures, "symfony-interception.bin"))
	if err != nil {
		t.Fatal(err)
	}
	e, err := DecodeSymfony(raw)
	if err != nil {
		t.Fatal(err)
	}
	if e.Compression != "gzip" || e.DataForm != "json" {
		t.Errorf("form %s compression %s", e.DataForm, e.Compression)
	}
	if strings.Join(e.Tags, ",") != "CONFIG,MAGE" {
		t.Errorf("tags %v", e.Tags)
	}
	if e.MTime != 1792528681 || e.Expire != 1792535881 || e.Infinite {
		t.Errorf("times %d %d %v", e.MTime, e.Expire, e.Infinite)
	}
	if e.Size != len(raw) || e.DataSize <= e.Size {
		t.Errorf("size %d data %d", e.Size, e.DataSize)
	}
	if !strings.HasPrefix(e.Preview, `{"Magento\\`) {
		t.Errorf("preview %.40q", e.Preview)
	}
}

func TestDecodeSymfonyForms(t *testing.T) {
	sym, _ := loadEntries(t)
	cases := []struct {
		name, form, compression, tags string
		infinite                      bool
		preview                       string
	}{
		{"fast-text", "text", "", "", true, "plain cached value"},
		{"page-json", "page", "", "FPC,CMS_P_2", true, "<!doctype html>"},
		{"page-compressed", "page", "gzip", "FPC,CAT_C_41", false, "<!doctype html>"},
		{"config-php", "php", "", "CONFIG", false, "a:1:"},
		{"encrypted", "encrypted", "", "CONFIG", false, ""},
		{"binary", "binary", "", "", true, "00000000  00 01 02 ff fe"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e, err := DecodeSymfony(sym[tc.name])
			if err != nil {
				t.Fatal(err)
			}
			if e.DataForm != tc.form || e.Compression != tc.compression {
				t.Errorf("form %s compression %s", e.DataForm, e.Compression)
			}
			if strings.Join(e.Tags, ",") != tc.tags {
				t.Errorf("tags %v", e.Tags)
			}
			if e.Infinite != tc.infinite {
				t.Errorf("infinite %v", e.Infinite)
			}
			if !strings.HasPrefix(e.Preview, tc.preview) {
				t.Errorf("preview %.60q", e.Preview)
			}
			if tc.form == "page" {
				if e.Status != 200 || e.Context["customer_group"] != "0" {
					t.Errorf("page status %d context %v", e.Status, e.Context)
				}
			}
		})
	}
}

func TestDecodeCmForms(t *testing.T) {
	_, cm := loadEntries(t)
	cases := []struct {
		name, form, compression, tags string
		infinite                      bool
		preview                       string
	}{
		{"text", "text", "", "69d_CONFIG,69d_MAGE", false, "plain cached value"},
		{"page-gz", "page", "gzip", "69d_FPC,69d_CAT_C_41", false, "<!doctype html>"},
		{"config-php", "php", "", "69d_CONFIG", true, "a:1:"},
		{"encrypted", "encrypted", "", "", false, ""},
		{"link", "link", "", "69d_EAV", false, "l:"},
		{"snappy", "compressed", "snappy", "69d_LAYOUT_GENERAL_CACHE_TAG", false, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e, err := DecodeCm(cm[tc.name])
			if err != nil {
				t.Fatal(err)
			}
			if e.DataForm != tc.form || e.Compression != tc.compression {
				t.Errorf("form %s compression %s", e.DataForm, e.Compression)
			}
			if strings.Join(e.Tags, ",") != tc.tags {
				t.Errorf("tags %v", e.Tags)
			}
			if e.Infinite != tc.infinite || e.MTime != 1792528681 {
				t.Errorf("infinite %v mtime %d", e.Infinite, e.MTime)
			}
			if !strings.HasPrefix(e.Preview, tc.preview) {
				t.Errorf("preview %.60q", e.Preview)
			}
			if e.Size != len(cm[tc.name]["d"]) {
				t.Errorf("size %d", e.Size)
			}
			if tc.form == "link" && len(e.Link) != 32 {
				t.Errorf("link %q", e.Link)
			}
		})
	}
	if _, err := DecodeCm(map[string][]byte{"t": []byte("x")}); err == nil {
		t.Error("hash without d decoded")
	}
}

func TestDetectLayout(t *testing.T) {
	cases := []struct {
		keys   []string
		layout string
	}{
		{[]string{"69d_:SYSTEM_DEFAULT", "cache:tags:69d_CONFIG"}, "symfony"},
		{[]string{"cache:all_ids"}, "symfony"},
		{[]string{"cache:id_tags:69d_X"}, "symfony"},
		{[]string{"zc:k:69d_SYSTEM_DEFAULT", "zc:ti:69d_CONFIG"}, "cm"},
		{[]string{"zc:k:69d_X"}, "cm"},
		{[]string{"sess_abc", "foo"}, ""},
		{nil, ""},
	}
	for _, tc := range cases {
		if got := DetectLayout(tc.keys); got != tc.layout {
			t.Errorf("%v: %q", tc.keys, got)
		}
	}
}

func TestPHPUnserialize(t *testing.T) {
	v, err := phpUnserialize([]byte(`a:5:{s:1:"a";i:-3;i:7;b:1;s:2:"é";d:1.5;s:1:"n";N;s:3:"arr";a:1:{i:0;s:6:"x;y\"z";}}`))
	if err != nil {
		t.Fatal(err)
	}
	arr, ok := v.(phpArray)
	if !ok || len(arr) != 5 {
		t.Fatalf("%#v", v)
	}
	if x, _ := arr.Get("a"); x != int64(-3) {
		t.Errorf("a=%v", x)
	}
	if x, _ := arr.Get("7"); x != true {
		t.Errorf("7=%v", x)
	}
	if x, _ := arr.Get("é"); x != 1.5 {
		t.Errorf("é=%v", x)
	}
	if x, ok := arr.Get("n"); !ok || x != nil {
		t.Errorf("n=%v", x)
	}
	inner, _ := arr.Get("arr")
	if s, _ := inner.(phpArray).Get("0"); s != `x;y\"z` {
		t.Errorf("inner %q", s)
	}
	for _, bad := range []string{`s:5:"abc";`, `a:2:{i:0;}`, `i:1`, `O:8:"stdClass":0:{}`, `x`} {
		if _, err := phpUnserialize([]byte(bad)); err == nil {
			t.Errorf("%q parsed", bad)
		}
	}
}

func TestCleanID(t *testing.T) {
	if got := cleanID("store.cfg-1/x"); got != "STORE__CFG_1_X" {
		t.Errorf("%q", got)
	}
}
