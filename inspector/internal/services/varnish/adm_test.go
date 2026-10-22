package varnish

import (
	"strings"
	"testing"
)

func TestParseAdmEnvelope(t *testing.T) {
	env, err := ParseAdm(fixture(t, "adm-8.0-backend.list.json"))
	if err != nil {
		t.Fatal(err)
	}
	if env.Version != 3 || len(env.Argv) != 2 || env.Argv[0] != "backend.list" || env.Time.IsZero() || len(env.Payload) != 1 {
		t.Fatalf("envelope %+v", env)
	}
	if _, err := ParseAdm([]byte(`[2]`)); err == nil {
		t.Fatal("short array accepted")
	}
	if _, err := ParseAdm([]byte(`Command failed with error code 106`)); err == nil {
		t.Fatal("plain text accepted")
	}
}

func TestParseBans(t *testing.T) {
	bans, err := ParseBans(fixture(t, "adm-8.0-ban.list-purges.json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(bans) != 3 {
		t.Fatalf("got %d bans", len(bans))
	}
	if bans[0].Spec != `req.url ~ \.html$` || bans[0].Completed || bans[0].Refs != 0 {
		t.Fatalf("ban 0: %+v", bans[0])
	}
	if bans[1].Spec != "obj.http.X-Magento-Tags ~ ((^|,)cat_c_41(,|$))" || bans[1].Time.IsZero() {
		t.Fatalf("ban 1: %+v", bans[1])
	}
	if !bans[2].Completed || bans[2].Spec != "" {
		t.Fatalf("ban 2: %+v", bans[2])
	}
	for _, name := range []string{"adm-7.1-ban.list.json", "adm-8.0-ban.list.json", "adm-7.1-ban.list-purges.json"} {
		if _, err := ParseBans(fixture(t, name)); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
	}
}

func TestParseVCLs(t *testing.T) {
	for _, name := range []string{"adm-8.0-vcl.list.json", "adm-7.1-vcl.list.json"} {
		vcls, err := ParseVCLs(fixture(t, name))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if len(vcls) != 1 || vcls[0].Name != "boot" || vcls[0].Status != "active" || vcls[0].Temperature != "warm" {
			t.Fatalf("%s: %+v", name, vcls)
		}
	}
}

func TestParseBackends(t *testing.T) {
	for _, name := range []string{"adm-8.0-backend.list.json", "adm-7.1-backend.list.json"} {
		bs, err := ParseBackends(fixture(t, name))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if len(bs) != 1 {
			t.Fatalf("%s: %+v", name, bs)
		}
		b := bs[0]
		if b.Name != "boot.default" || b.Type != "backend" || b.AdminHealth != "healthy" || b.Probe != "healthy" || b.LastChange.IsZero() {
			t.Fatalf("%s: %+v", name, b)
		}
		if !b.Healthy() {
			t.Fatalf("%s: not healthy", name)
		}
	}
	sick := []byte(`[ 3, ["backend.list", "-j"], 1.0, {"boot.default": {"type": "backend", "admin_health": "probe", "probe_message": [2, 8, "sick"], "last_change": 1.0}} ]`)
	bs, err := ParseBackends(sick)
	if err != nil || len(bs) != 1 || bs[0].Healthy() || bs[0].Good != 2 || bs[0].Window != 8 {
		t.Fatalf("sick backend: %+v %v", bs, err)
	}
}

func TestParseStorages(t *testing.T) {
	ss, err := ParseStorages(fixture(t, "adm-8.0-storage.list.json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(ss) != 2 || ss[0].Name != "s0" || ss[0].Storage != "malloc" || ss[1].Name != "Transient" {
		t.Fatalf("storages %+v", ss)
	}
}

func TestParseParams(t *testing.T) {
	ps, err := ParseParams(fixture(t, "adm-8.0-param.show.json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(ps) != 2 || ps[0].Name != "default_grace" || ps[0].Value != "3600.000" || ps[0].Units != "seconds" || ps[0].Default != "10.000" {
		t.Fatalf("params %+v", ps)
	}
}

func TestSplitVCLShow(t *testing.T) {
	files := SplitVCLShow(string(fixture(t, "adm-8.0-vcl.show.txt")))
	if len(files) != 2 {
		t.Fatalf("got %d files", len(files))
	}
	if files[0].Path != "/etc/varnish/default.vcl" || !strings.HasPrefix(files[0].Source, "vcl 4.0;") {
		t.Fatalf("file 0: %q %q", files[0].Path, files[0].Source[:20])
	}
	if files[1].Path != "<builtin>" {
		t.Fatalf("file 1: %q", files[1].Path)
	}
	if len(files[0].Source) != 7111 {
		t.Fatalf("file 0 length %d, header says 7111", len(files[0].Source))
	}
}

func TestPanicState(t *testing.T) {
	panicked, text := panicState(1, string(fixture(t, "adm-panic-none-stdout.txt")), string(fixture(t, "adm-panic-none-stderr.txt")))
	if panicked || !strings.Contains(text, "has not panicked") {
		t.Fatalf("no panic: %v %q", panicked, text)
	}
	panicked, text = panicState(0, "Panic at: Tue, 20 Oct 2026\nAssert error in ...", "")
	if !panicked || !strings.HasPrefix(text, "Panic at") {
		t.Fatalf("panic: %v %q", panicked, text)
	}
}
