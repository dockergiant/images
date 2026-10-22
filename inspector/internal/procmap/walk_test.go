package procmap

import (
	"bytes"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

// A process picks its own argv, so a newline in it must not reach the line
// protocol: the walk would read the rest as records of its own.
func TestWalkScriptFlattensNewlines(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("needs /proc")
	}
	if _, err := os.Stat("/proc/self/cmdline"); err != nil {
		t.Skip("needs /proc")
	}
	forged := "x\nF 4242 999\nP 4242 php-fpm: master\r\nP 4243 forged"
	child := exec.Command("sh", "-c", "sleep 30; :", "sh", forged)
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		child.Process.Kill()
		child.Wait()
	}()
	for i := 0; i < 100; i++ {
		b, _ := os.ReadFile("/proc/" + strconv.Itoa(child.Process.Pid) + "/cmdline")
		if bytes.Contains(b, []byte("4242")) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	walk := exec.Command("sh", "-s")
	walk.Stdin = strings.NewReader(walkScript)
	out, err := walk.Output()
	if err != nil {
		t.Fatal(err)
	}
	w, err := ParseWalk(bytes.NewReader(out))
	if err != nil {
		t.Fatal(err)
	}
	if cmd, ok := w.Procs[4242]; ok {
		t.Fatalf("argv forged a process record: %q", cmd)
	}
	if cmd, ok := w.Procs[4243]; ok {
		t.Fatalf("argv forged a process record after a CR: %q", cmd)
	}
	cmd := w.Procs[child.Process.Pid]
	if !strings.Contains(cmd, "F 4242 999 P 4242 php-fpm: master") {
		t.Fatalf("child cmd = %q, want its argv on one line", cmd)
	}
}

func TestParseWalkKeepsFlattenedArgvInCmd(t *testing.T) {
	in := strings.Join([]string{
		"T    0: 0A9CA8C0:9306 059CA8C0:18EB 01 00000000:00000000 00:00000000 00000000    33        0 999 1",
		"P 50 php -r sleep(1e6); x F 4242 999 P 4242 php-fpm: master",
		"F 50 999",
	}, "\n")
	w, err := ParseWalk(strings.NewReader(in))
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := w.Procs[4242]; ok {
		t.Fatalf("procs = %v", w.Procs)
	}
	if s, ok := findSocket(w, 999); !ok || len(s.PIDs) != 1 || s.PIDs[0] != 50 {
		t.Fatalf("socket 999 = %+v, want only pid 50", s)
	}
}

func TestScrubHidesCommandLineSecrets(t *testing.T) {
	cases := map[string]string{
		"php bin/magento setup:install --db-password=s3cret --admin-password hunter2 --db-name=shop": "php bin/magento setup:install --db-password=*** --admin-password *** --db-name=shop",
		"mysql -ushop -pTopSecret shop":                           "mysql -ushop -p*** shop",
		"php bin/magento x --amqp_password=a --api-key=b --token": "php bin/magento x --amqp_password=*** --api-key=*** --token",
		"php-fpm: pool www":                                       "php-fpm: pool www",
		"/usr/local/bin/php bin/magento queue:consumers:start async.operations.all --single-thread": "/usr/local/bin/php bin/magento queue:consumers:start async.operations.all --single-thread",
	}
	for in, want := range cases {
		if got := scrub(in); got != want {
			t.Errorf("scrub(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestIndexScrubsCmdButKeepsRole(t *testing.T) {
	in := strings.Join([]string{
		"T    0: 0A9CA8C0:9306 059CA8C0:18EB 01 00000000:00000000 00:00000000 00000000    33        0 77 1",
		"P 60 php bin/magento queue:consumers:start product_action_attribute.update --db-password=s3cret",
		"F 60 77",
	}, "\n")
	w, err := ParseWalk(strings.NewReader(in))
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range index("php-fpm", w) {
		if strings.Contains(p.Cmd, "s3cret") || p.Role != "consumer" {
			t.Fatalf("proc %+v", p)
		}
		return
	}
	t.Fatal("socket not indexed")
}
