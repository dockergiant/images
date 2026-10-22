package redis

import (
	"bufio"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dockergiant/images/inspector/internal/core"
)

const fixtures = "../../../testdata/redis"

func readLines(t *testing.T, name string) []string {
	t.Helper()
	f, err := os.Open(filepath.Join(fixtures, name))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var out []string
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		out = append(out, sc.Text())
	}
	return out
}

func TestParseMonitorFixtures(t *testing.T) {
	for _, name := range []string{"monitor-valkey90.txt", "monitor-valkey81.txt", "monitor-redis72.txt"} {
		t.Run(name, func(t *testing.T) {
			lines := readLines(t, name)
			if lines[0] != "OK" {
				t.Fatalf("first line %q", lines[0])
			}
			var cmds []core.RedisCmd
			for _, l := range lines[1:] {
				c, err := ParseMonitor(l)
				if err != nil {
					t.Fatalf("%q: %v", l, err)
				}
				cmds = append(cmds, c)
			}
			find := func(cmd, key string) core.RedisCmd {
				for _, c := range cmds {
					if c.Cmd == cmd && len(c.Args) > 0 && c.Args[0] == key {
						return c
					}
				}
				t.Fatalf("no %s %s", cmd, key)
				return core.RedisCmd{}
			}

			set := find("SET", "roll-inspector:test")
			if set.DB != 3 || set.IP != "127.0.0.1" || set.Port == 0 {
				t.Errorf("set addr: %+v", set)
			}
			if set.Args[1] != "say \"hi\"\nline2\ttab\\back" {
				t.Errorf("escapes: %q", set.Args[1])
			}
			if set.Time.Year() != 2026 {
				t.Errorf("time %v", set.Time)
			}

			bin := find("SET", "bin")
			if bin.Args[1] != `\x00\x01\xff\xfe` {
				t.Errorf("binary arg should stay escaped for display: %q", bin.Args[1])
			}

			unix := find("GET", "zc:k:69d_SYSTEM_DEFAULT")
			if unix.Addr != "unix:/tmp/r.sock" || unix.IP != "" || unix.Port != 0 || unix.DB != 1 {
				t.Errorf("unix: %+v", unix)
			}

			v6 := find("HGET", "zc:k:69d_DICONFIG")
			if v6.IP != "::1" || v6.Port == 0 || !strings.HasPrefix(v6.Addr, "[::1]:") {
				t.Errorf("ipv6: %+v", v6)
			}

			lua := find("GET", "some key")
			if lua.Addr != "lua" || lua.IP != "" {
				t.Errorf("lua: %+v", lua)
			}

			big := find("SET", "big")
			if len(big.Args[1]) != 400 {
				t.Errorf("big arg length %d", len(big.Args[1]))
			}
		})
	}
}

func TestParseMonitorForms(t *testing.T) {
	cases := []struct {
		line string
		db   int
		addr string
		ip   string
		port int
		cmd  string
		args []string
	}{
		{`1792527779.558532 [0 192.168.156.10:55646] "ECHO" "phpredis:6ad7cda38850a:00000058"`, 0, "192.168.156.10:55646", "192.168.156.10", 55646, "ECHO", []string{"phpredis:6ad7cda38850a:00000058"}},
		{`1792527778.432473 [0 127.0.0.1:51044] "ping"`, 0, "127.0.0.1:51044", "127.0.0.1", 51044, "PING", nil},
		{`1792527779.000001 [12 [fd00::5]:40000] "GET" "a] b"`, 12, "[fd00::5]:40000", "fd00::5", 40000, "GET", []string{"a] b"}},
		{`1792527779.000001 [0 lua] "SET" "k" "v"`, 0, "lua", "", 0, "SET", []string{"k", "v"}},
		{`1792527779.000001 [2 unix:/var/run/redis.sock] "GET" "\"q\""`, 2, "unix:/var/run/redis.sock", "", 0, "GET", []string{`"q"`}},
		{`1792527779.000001 [0 10.0.0.2:5000] "SET" "k" "a\r\b\a"`, 0, "10.0.0.2:5000", "10.0.0.2", 5000, "SET", []string{"k", `a\r\b\a`}},
	}
	for _, tc := range cases {
		c, err := ParseMonitor(tc.line)
		if err != nil {
			t.Fatalf("%s: %v", tc.line, err)
		}
		if c.DB != tc.db || c.Addr != tc.addr || c.IP != tc.ip || c.Port != tc.port || c.Cmd != tc.cmd {
			t.Errorf("%s: got %+v", tc.line, c)
		}
		if strings.Join(c.Args, "|") != strings.Join(tc.args, "|") {
			t.Errorf("%s: args %q", tc.line, c.Args)
		}
	}
	want := time.Unix(1792527779, 1000).UTC()
	c, _ := ParseMonitor(cases[3].line)
	if !c.Time.Equal(want) {
		t.Errorf("time %v want %v", c.Time, want)
	}
}

func TestParseMonitorRejects(t *testing.T) {
	for _, l := range []string{"", "OK", "1792527779.1 0 x", "abc [0 lua] \"GET\"", "1792527779.1 [0 lua]"} {
		if _, err := ParseMonitor(l); err == nil {
			t.Errorf("%q parsed", l)
		}
	}
}

func TestParseMonitorTruncatedLine(t *testing.T) {
	c, err := ParseMonitor(`1792527779.000001 [0 10.0.0.2:5000] "SET" "key" "unterminated valu`)
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Args) != 2 || c.Args[1] != "unterminated valu" {
		t.Errorf("%q", c.Args)
	}
}

func TestSkip(t *testing.T) {
	own := "172.20.0.9"
	cases := []struct {
		line string
		skip bool
	}{
		{`1.000001 [0 172.20.0.9:4000] "INFO" "all"`, true},
		{`1.000001 [0 127.0.0.1:51044] "ping"`, true},
		{`1.000001 [0 172.20.0.3:51044] "PING"`, false},
		{`1.000001 [0 172.20.0.3:55646] "ECHO" "phpredis:6ad7cda38850a:00000058"`, true},
		{`1.000001 [0 172.20.0.3:55646] "echo" "phpredis:x"`, true},
		{`1.000001 [0 172.20.0.3:55646] "ECHO" "hello"`, false},
		{`1.000001 [0 172.20.0.3:55646] "MGET" "69d_:SYSTEM_DEFAULT"`, false},
	}
	for _, tc := range cases {
		c, err := ParseMonitor(tc.line)
		if err != nil {
			t.Fatal(err)
		}
		if got := skip(c, own); got != tc.skip {
			t.Errorf("%s: skip=%v", tc.line, got)
		}
	}
}

func TestTruncateKeepsKey(t *testing.T) {
	key := strings.Repeat("k", 400)
	val := strings.Repeat("v", 1000)
	c := truncateArgs(core.RedisCmd{Cmd: "SET", Args: []string{key, val, "EX", "60"}})
	if c.Args[0] != key {
		t.Error("key was cut")
	}
	if !strings.HasPrefix(c.Args[1], strings.Repeat("v", maxArg)) || len(c.Args[1]) > maxArg+32 {
		t.Errorf("value not cut: %d", len(c.Args[1]))
	}
	if !strings.Contains(c.Args[1], "1000 bytes") {
		t.Errorf("no size note: %q", c.Args[1][maxArg:])
	}
	if c.Args[2] != "EX" || c.Args[3] != "60" {
		t.Errorf("short args changed: %q", c.Args[2:])
	}
}

func TestFoldBurst(t *testing.T) {
	lines := readLines(t, "monitor-redis72.txt")
	f := newFolder()
	var out []core.RedisCmd
	for _, l := range lines[1:] {
		c, err := ParseMonitor(l)
		if err != nil {
			t.Fatal(err)
		}
		if c.Cmd == "SELECT" {
			continue
		}
		out = append(out, f.Add(c)...)
	}
	out = append(out, f.FlushAll()...)

	var burst *core.RedisCmd
	for i := range out {
		if out[i].Cmd == "SET" && out[i].Args[0] == "69d_:BURST_ID" {
			burst = &out[i]
		}
		if out[i].Cmd == "SADD" && out[i].Args[0] == "cache:all_ids" {
			t.Error("SADD of the burst was not folded")
		}
	}
	if burst == nil {
		t.Fatal("burst row missing")
	}
	if burst.Folded != 4 {
		t.Errorf("folded %d, want 4", burst.Folded)
	}
	var get bool
	for _, c := range out {
		if c.Cmd == "GET" && c.Args[0] == "other" {
			get = true
			if c.Folded != 0 {
				t.Error("GET folded")
			}
		}
	}
	if !get {
		t.Error("GET after the burst was lost")
	}
}

func TestFoldRules(t *testing.T) {
	base := time.Unix(1792527779, 0).UTC()
	at := func(ms float64, addr, cmd string, args ...string) core.RedisCmd {
		return core.RedisCmd{Time: base.Add(time.Duration(ms * float64(time.Millisecond))), Addr: addr, Cmd: cmd, Args: args}
	}
	f := newFolder()
	var out []core.RedisCmd
	out = append(out, f.Add(at(0, "a:1", "SET", "k1", "v"))...)
	out = append(out, f.Add(at(1, "b:1", "SET", "x", "v"))...)
	out = append(out, f.Add(at(2, "a:1", "SADD", "t", "k1"))...)
	out = append(out, f.Add(at(6.5, "a:1", "EXPIRE", "k1", "60"))...)
	out = append(out, f.Add(at(20, "a:1", "DEL", "k2"))...)
	out = append(out, f.Add(at(21, "a:1", "UNLINK", "k3"))...)
	out = append(out, f.Add(at(21.5, "a:1", "SETEX", "k4", "60", "v"))...)
	if len(out) != 2 {
		t.Fatalf("emitted before flush: %+v", out)
	}
	if out[0].Args[0] != "x" || out[0].Folded != 0 {
		t.Errorf("single command from another client: %+v", out[0])
	}
	if out[1].Args[0] != "k1" || out[1].Folded != 3 {
		t.Errorf("burst within 5 ms between commands: %+v", out[1])
	}
	rest := f.Flush(base.Add(100 * time.Millisecond))
	if len(rest) != 1 || rest[0].Cmd != "DEL" || rest[0].Folded != 3 {
		t.Errorf("second burst: %+v", rest)
	}
	if len(f.FlushAll()) != 0 {
		t.Error("folder not empty")
	}

	f = newFolder()
	out = f.Add(at(0, "a:1", "SET", "k", "v"))
	out = append(out, f.Add(at(1, "a:1", "GET", "k"))...)
	if len(out) != 2 || out[0].Cmd != "SET" || out[0].Folded != 0 || out[1].Cmd != "GET" {
		t.Errorf("non-foldable command ends a burst: %+v", out)
	}
}

func TestGlob(t *testing.T) {
	cases := []struct {
		pat, s string
		ok     bool
	}{
		{"*", "anything/with/slashes", true},
		{"69d_:*", "69d_:SYSTEM_DEFAULT", true},
		{"zc:k:*CONFIG*", "zc:k:69d_DICONFIG", true},
		{"zc:k:*CONFIG", "zc:k:69d_DICONFIG_X", false},
		{"h?llo", "hello", true},
		{"h[ae]llo", "hallo", true},
		{"h[^e]llo", "hello", false},
		{"h[a-c]llo", "hbllo", true},
		{`a\*b`, "a*b", true},
		{`a\*b`, "axb", false},
		{"", "", true},
		{"", "x", false},
	}
	for _, tc := range cases {
		if got := glob(tc.pat, tc.s); got != tc.ok {
			t.Errorf("glob(%q,%q)=%v", tc.pat, tc.s, got)
		}
	}
}
