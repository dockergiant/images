package varnish

import (
	"testing"
	"time"
)

func TestParseStatVersioned(t *testing.T) {
	for _, name := range []string{"stat-8.0.json", "stat-7.1.json"} {
		c, err := ParseStat(fixture(t, name))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if len(c) < 100 {
			t.Fatalf("%s: only %d counters", name, len(c))
		}
		for _, k := range []string{"MAIN.cache_hit", "MAIN.cache_miss", "MAIN.n_object", "SMA.s0.g_bytes", "MAIN.n_lru_nuked", "MAIN.bans", "MAIN.backend_fail"} {
			if _, ok := c[k]; !ok {
				t.Errorf("%s: %s missing", name, k)
			}
		}
		for k := range c {
			if k == "VBE.boot.default.happy" {
				t.Errorf("%s: bitmap counter %s not skipped", name, k)
			}
		}
	}
}

func TestParseStatFlat60(t *testing.T) {
	c, err := ParseStat(fixture(t, "stat-6.0-flat.json"))
	if err != nil {
		t.Fatal(err)
	}
	if c["MAIN.cache_hit"] != 120 || c["MAIN.cache_miss"] != 40 || c["SMA.s0.g_bytes"] != 1048576 || c["MGT.uptime"] != 8692 {
		t.Fatalf("flat values: %v", c)
	}
	if _, ok := c["VBE.boot.default.happy"]; ok {
		t.Fatal("bitmap counter not skipped")
	}
	if _, ok := c["timestamp"]; ok {
		t.Fatal("timestamp parsed as a counter")
	}
}

func TestParseStatRejectsGarbage(t *testing.T) {
	if _, err := ParseStat([]byte("not json")); err == nil {
		t.Fatal("expected an error")
	}
}

func TestRates(t *testing.T) {
	prev := map[string]uint64{"MAIN.cache_hit": 100, "MAIN.cache_miss": 10, "MAIN.n_lru_nuked": 5}
	cur := map[string]uint64{"MAIN.cache_hit": 120, "MAIN.cache_miss": 14, "MAIN.n_lru_nuked": 2}
	r := Rates(prev, cur, 2*time.Second, "MAIN.cache_hit", "MAIN.cache_miss", "MAIN.n_lru_nuked", "MAIN.absent")
	if r["MAIN.cache_hit"] != 10 || r["MAIN.cache_miss"] != 2 {
		t.Fatalf("rates %v", r)
	}
	if r["MAIN.n_lru_nuked"] != 0 {
		t.Fatalf("counter reset should give 0, got %v", r["MAIN.n_lru_nuked"])
	}
	if _, ok := r["MAIN.absent"]; ok {
		t.Fatal("absent counter got a rate")
	}
	if len(Rates(prev, cur, 0, "MAIN.cache_hit")) != 0 {
		t.Fatal("zero interval must give no rates")
	}
}
