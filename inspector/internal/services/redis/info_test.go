package redis

import (
	"os"
	"path/filepath"
	"testing"
)

func readFixture(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(fixtures, name))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestParseInfoEngines(t *testing.T) {
	cases := []struct {
		file, engine, version string
		tested, noTouch       bool
	}{
		{"info-valkey90.txt", "valkey", "9.0.6", true, true},
		{"info-valkey81.txt", "valkey", "8.1.10", true, true},
		{"info-redis72.txt", "redis", "7.2.14", true, true},
	}
	for _, tc := range cases {
		t.Run(tc.file, func(t *testing.T) {
			info := ParseInfo(readFixture(t, tc.file))
			for _, s := range []string{"server", "clients", "memory", "stats", "keyspace", "commandstats"} {
				if len(info[s]) == 0 {
					t.Errorf("section %s missing", s)
				}
			}
			engine, version := DetectEngine(info)
			if engine != tc.engine || version != tc.version {
				t.Errorf("engine %s %s", engine, version)
			}
			if Tested(engine, version) != tc.tested {
				t.Errorf("tested flag")
			}
			f := Features(engine, version)
			if f["noTouch"] != tc.noTouch || !f["latencyHistogram"] || !f["objectEncoding"] || !f["scanType"] || !f["unlink"] {
				t.Errorf("features %v", f)
			}

			ks := ParseKeyspace(info)
			if len(ks) != 3 || ks[0].DB != 0 || ks[0].Keys != 3 || ks[1].DB != 1 || ks[2].DB != 3 || ks[2].Expires != 1 {
				t.Errorf("keyspace %+v", ks)
			}
			stats := ParseCommandStats(info)
			var set bool
			for _, s := range stats {
				if s.Cmd == "set" && s.Calls >= 3 {
					set = true
				}
			}
			if !set {
				t.Errorf("commandstats %+v", stats)
			}
			if info.Int("keyspace_misses") < 1 || info.Int("keyspace_hits") < 1 {
				t.Errorf("hits %d misses %d", info.Int("keyspace_hits"), info.Int("keyspace_misses"))
			}
			if info.Get("used_memory_human") == "" {
				t.Error("used_memory_human")
			}
			if len(ParseLatencyStats(info)) == 0 {
				t.Error("latencystats")
			}
		})
	}
}

func TestDetectEngineOthers(t *testing.T) {
	cases := []struct {
		text, engine, version string
		tested                bool
	}{
		{"# Server\r\nredis_version:7.4.0\r\ndragonfly_version:df-v1.24.0\r\n", "dragonfly", "1.24.0", false},
		{"# Server\r\nredis_version:5.0.14\r\n", "redis", "5.0.14", false},
		{"# Server\r\nredis_version:7.2.4\r\nserver_name:valkey\r\n", "valkey", "7.2.4", false},
		{"# Server\r\nredis_version:7.2.5\r\n", "redis", "7.2.5", true},
	}
	for _, tc := range cases {
		e, v := DetectEngine(ParseInfo(tc.text))
		if e != tc.engine || v != tc.version || Tested(e, v) != tc.tested {
			t.Errorf("%q: %s %s tested=%v", tc.text, e, v, Tested(e, v))
		}
	}
	df := Features("dragonfly", "1.24.0")
	if df["objectEncoding"] || df["latency"] || df["latencyHistogram"] || df["noTouch"] {
		t.Errorf("dragonfly features %v", df)
	}
	r5 := Features("redis", "5.0.14")
	if r5["latencyHistogram"] || r5["scanType"] || r5["noTouch"] || !r5["unlink"] || !r5["latency"] {
		t.Errorf("redis 5 features %v", r5)
	}
	r6 := Features("redis", "6.2.0")
	if !r6["scanType"] || r6["latencyHistogram"] {
		t.Errorf("redis 6.2 features %v", r6)
	}
}

func TestParseClientList(t *testing.T) {
	for _, file := range []string{"clients-valkey90.txt", "clients-valkey81.txt", "clients-redis72.txt"} {
		clients := ParseClientList(readFixture(t, file))
		if len(clients) != 3 {
			t.Fatalf("%s: %d clients", file, len(clients))
		}
		var named, self bool
		for _, c := range clients {
			if c["name"] == "php-worker" && c["db"] == "3" && c["cmd"] == "blpop" {
				named = true
			}
			if c["cmd"] == "client|list" {
				self = true
				if _, ok := c["lib-name"]; !ok {
					t.Errorf("%s: empty values dropped: %v", file, c)
				}
			}
			if c["addr"] == "" || c["id"] == "" {
				t.Errorf("%s: %v", file, c)
			}
		}
		if !named || !self {
			t.Errorf("%s: named=%v self=%v", file, named, self)
		}
	}
}
