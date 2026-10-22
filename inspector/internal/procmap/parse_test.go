package procmap

import (
	"net/netip"
	"os"
	"strings"
	"testing"
)

func loadWalk(t *testing.T, name string) Walk {
	t.Helper()
	f, err := os.Open("../../testdata/procmap/" + name)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	w, err := ParseWalk(f)
	if err != nil {
		t.Fatal(err)
	}
	return w
}

func findSocket(w Walk, inode uint64) (Socket, bool) {
	for _, s := range w.Sockets {
		if s.Inode == inode {
			return s, true
		}
	}
	return Socket{}, false
}

func TestParseWalkConsumers(t *testing.T) {
	w := loadWalk(t, "walk-consumers.txt")

	if got := len(w.Procs); got != 11 {
		t.Fatalf("procs = %d, want 11", got)
	}
	if got := len(w.Sockets); got != 61 {
		t.Fatalf("sockets with an inode = %d, want 61", got)
	}
	if got := w.Procs[1]; got != "php-fpm: master process (/usr/local/etc/php-fpm.conf)" {
		t.Fatalf("pid 1 cmd = %q", got)
	}
	if got := w.Procs[8024]; !strings.Contains(got, "queue:consumers:start product_action_attribute.update") {
		t.Fatalf("pid 8024 cmd = %q", got)
	}

	redis, ok := findSocket(w, 104339724)
	if !ok {
		t.Fatal("socket 104339724 missing")
	}
	if redis.Local != netip.MustParseAddrPort("192.168.156.10:37638") {
		t.Fatalf("local = %s", redis.Local)
	}
	if redis.Remote != netip.MustParseAddrPort("192.168.156.5:6379") {
		t.Fatalf("remote = %s", redis.Remote)
	}
	if redis.State != stateEstablished {
		t.Fatalf("state = %x", redis.State)
	}
	if len(redis.PIDs) != 0 {
		t.Fatalf("worker socket resolved without privileged walk: %v", redis.PIDs)
	}

	listen, ok := findSocket(w, 98178649)
	if !ok {
		t.Fatal("listen socket missing")
	}
	if listen.State != stateListen || listen.Local.Port() != 9000 || !listen.Local.Addr().IsUnspecified() {
		t.Fatalf("listen socket = %+v", listen)
	}
	if len(listen.PIDs) != 1 || listen.PIDs[0] != 1 {
		t.Fatalf("listen pids = %v", listen.PIDs)
	}

	nginx, ok := findSocket(w, 104723481)
	if !ok {
		t.Fatal("v4-mapped socket missing")
	}
	if nginx.Local != netip.MustParseAddrPort("192.168.156.10:9000") {
		t.Fatalf("v4-mapped local = %s", nginx.Local)
	}
	if nginx.Remote != netip.MustParseAddrPort("192.168.156.11:47752") {
		t.Fatalf("v4-mapped remote = %s", nginx.Remote)
	}

	for _, s := range w.Sockets {
		if s.Inode == 0 {
			t.Fatal("TIME_WAIT row without inode kept")
		}
	}
}

func TestParseWalkIdle(t *testing.T) {
	w := loadWalk(t, "walk-idle.txt")

	if got := len(w.Procs); got != 6 {
		t.Fatalf("procs = %d, want 6", got)
	}
	if got := len(w.Sockets); got != 2 {
		t.Fatalf("sockets = %d, want 2", got)
	}
	dns, ok := findSocket(w, 101122840)
	if !ok || dns.Local != netip.MustParseAddrPort("127.0.0.11:33047") {
		t.Fatalf("dns socket = %+v", dns)
	}
	listen, ok := findSocket(w, 101123869)
	if !ok || len(listen.PIDs) != 1 || listen.PIDs[0] != 1 {
		t.Fatalf("listen socket = %+v", listen)
	}
	if Role(w.Procs[1081]) != "fpm-worker" || Role(w.Procs[1]) != "fpm-master" {
		t.Fatal("roles from idle fixture")
	}
}

func TestDecodeAddr(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"0100007F:1F90", "127.0.0.1:8080"},
		{"0A9CA8C0:18EB", "192.168.156.10:6379"},
		{"00000000:0000", "0.0.0.0:0"},
		{"00000000000000000000000000000000:2328", "[::]:9000"},
		{"00000000000000000000000001000000:0050", "[::1]:80"},
		{"000080FE00000000000000000100000A:1F90", "[fe80::a00:1]:8080"},
		{"B80D0120000000000000000001000000:01BB", "[2001:db8::1]:443"},
		{"0000000000000000FFFF00000A9CA8C0:2328", "192.168.156.10:9000"},
	}
	for _, c := range cases {
		got, err := decodeAddr(c.in)
		if err != nil {
			t.Fatalf("%s: %v", c.in, err)
		}
		if got.String() != c.want {
			t.Errorf("%s = %s, want %s", c.in, got, c.want)
		}
	}
	for _, bad := range []string{"", "0100007F", "0100007F:", "ZZ00007F:0050", "0100007:0050", "0100007F:GGGG"} {
		if _, err := decodeAddr(bad); err == nil {
			t.Errorf("%q decoded without error", bad)
		}
	}
}

func TestRole(t *testing.T) {
	cases := map[string]string{
		"php-fpm: pool www": "fpm-worker",
		"php-fpm: master process (/usr/local/etc/php-fpm.conf)":                                                   "fpm-master",
		"/usr/local/bin/php /var/www/html/bin/magento queue:consumers:start async.operations.all --single-thread": "consumer",
		"/usr/local/bin/php bin/magento cron:run --group=index":                                                   "cron",
		"php bin/magento cache:clean config":                                                                      "cli",
		"/usr/sbin/cron":                                                                                          "cli",
		"":                                                                                                        "cli",
	}
	for cmd, want := range cases {
		if got := Role(cmd); got != want {
			t.Errorf("Role(%q) = %s, want %s", cmd, got, want)
		}
	}
}

func TestParseWalkSkipsMalformed(t *testing.T) {
	in := strings.Join([]string{
		"T   sl  local_address rem_address   st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode",
		"T    0: nothex:0050 00000000:0000 01 00000000:00000000 00:00000000 00000000    33        0 55 1",
		"T    1: 0A9CA8C0:9306",
		"P abc not-a-pid",
		"F 12",
		"F x 55",
		"garbage",
		"",
		"P 12 php-fpm: pool www ",
		"F 12 56",
		"T    2: 0A9CA8C0:9306 059CA8C0:18EB 01 00000000:00000000 00:00000000 00000000    33        0 56 1",
	}, "\n")
	w, err := ParseWalk(strings.NewReader(in))
	if err != nil {
		t.Fatal(err)
	}
	if len(w.Sockets) != 1 || w.Sockets[0].Inode != 56 || len(w.Sockets[0].PIDs) != 1 || w.Sockets[0].PIDs[0] != 12 {
		t.Fatalf("sockets = %+v", w.Sockets)
	}
	if len(w.Procs) != 1 || w.Procs[12] != "php-fpm: pool www" {
		t.Fatalf("procs = %v", w.Procs)
	}
}
