package docker

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
)

func TestReadStreamPIDThenLines(t *testing.T) {
	var pid int
	var lines []string
	err := readStream(strings.NewReader("4711\n*   << Request  >> 32770\n-   Begin req 32769 rxreq\n"),
		func(p int) { pid = p },
		func(l string) { lines = append(lines, l) })
	if err != nil {
		t.Fatal(err)
	}
	if pid != 4711 {
		t.Fatalf("pid %d", pid)
	}
	if len(lines) != 2 || lines[1] != "-   Begin req 32769 rxreq" {
		t.Fatalf("lines %q", lines)
	}
}

func TestReadStreamBadPID(t *testing.T) {
	for _, in := range []string{"varnishlog: not found\n", "-3\n", "\n", " 12 x\n"} {
		called := false
		err := readStream(strings.NewReader(in), func(int) { called = true }, func(string) { called = true })
		if err == nil || called {
			t.Errorf("%q: err %v, called %v", in, err, called)
		}
	}
}

func TestReadStreamEmpty(t *testing.T) {
	err := readStream(strings.NewReader(""), func(int) {}, func(string) {})
	if !errors.Is(err, errNoPID) {
		t.Fatalf("got %v", err)
	}
}

func TestReadStreamLongLine(t *testing.T) {
	long := strings.Repeat("x", 900<<10)
	var got string
	err := readStream(strings.NewReader("1\n"+long+"\n"), func(int) {}, func(l string) { got = l })
	if err != nil || len(got) != len(long) {
		t.Fatalf("900 KB line: err %v, len %d", err, len(got))
	}
	err = readStream(strings.NewReader("1\n"+strings.Repeat("x", 2<<20)+"\n"), func(int) {}, func(string) {})
	if err == nil {
		t.Fatal("2 MB line accepted")
	}
}

func TestReadStreamCRLF(t *testing.T) {
	var pid int
	if err := readStream(strings.NewReader("99\r\n"), func(p int) { pid = p }, func(string) {}); err != nil || pid != 99 {
		t.Fatalf("pid %d err %v", pid, err)
	}
}

func TestStreamCmdWrapsArgv(t *testing.T) {
	got := streamCmd([]string{"varnishlog", "-q", `ReqURL ~ "^/a b"; rm -rf /`})
	want := []string{"sh", "-c", `echo $$; exec "$@"`, "sh", "varnishlog", "-q", `ReqURL ~ "^/a b"; rm -rf /`}
	if strings.Join(got, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("got %q", got)
	}
}

func TestKillCmd(t *testing.T) {
	got := killCmd(321)
	if len(got) != 5 || got[0] != "sh" || got[1] != "-c" || got[3] != "sh" || got[4] != "321" {
		t.Fatalf("got %q", got)
	}
	if !strings.Contains(got[2], "ROLL_INSPECTOR_EXEC=1") || !strings.Contains(got[2], `kill -TERM "$1"`) {
		t.Fatalf("script %q", got[2])
	}
}

func TestKillEscalatesOnlyForRoot(t *testing.T) {
	cases := []struct {
		name  string
		spec  ExecSpec
		execs []ExecSpec
	}{
		{"php user", ExecSpec{Container: "php", User: "www-data"}, []ExecSpec{{User: "www-data"}}},
		{"container default user", ExecSpec{Container: "varnish"}, []ExecSpec{{}}},
		{"root", ExecSpec{Container: "php", User: "root"}, []ExecSpec{{User: "root"}, {User: "root", Privileged: true}}},
		{"root by uid", ExecSpec{Container: "php", User: "0:0"}, []ExecSpec{{User: "0:0"}, {User: "root", Privileged: true}}},
		{"already privileged", ExecSpec{Container: "php", User: "root", Privileged: true}, []ExecSpec{{User: "root", Privileged: true}}},
	}
	for _, c := range cases {
		f := &Fake{ExecFunc: func(context.Context, ExecSpec) (ExecResult, error) {
			return ExecResult{ExitCode: 1, Stderr: []byte("kill: Operation not permitted")}, nil
		}}
		err := killExec(context.Background(), f.Exec, c.spec, 321)
		if err == nil || !strings.Contains(err.Error(), "Operation not permitted") {
			t.Errorf("%s: err %v, want the failed kill", c.name, err)
		}
		got := f.Execs()
		if len(got) != len(c.execs) {
			t.Errorf("%s: %d kill execs, want %d: %+v", c.name, len(got), len(c.execs), got)
			continue
		}
		for i, want := range c.execs {
			if got[i].Container != c.spec.Container || got[i].User != want.User || got[i].Privileged != want.Privileged || got[i].Cmd[4] != "321" {
				t.Errorf("%s: kill exec %d %+v, want user %q privileged %v", c.name, i, got[i], want.User, want.Privileged)
			}
		}
	}

	f := &Fake{}
	if err := killExec(context.Background(), f.Exec, ExecSpec{Container: "php", User: "root"}, 321); err != nil || len(f.Execs()) != 1 {
		t.Fatalf("a kill that works: err %v, %d execs", err, len(f.Execs()))
	}
}

func TestTailBuffer(t *testing.T) {
	b := &tailBuffer{max: 8}
	io.WriteString(b, "hello ")
	io.WriteString(b, "world, again")
	if got := b.String(); got != ", again" && got != "d, again" {
		t.Fatalf("got %q", got)
	}
	if len(b.String()) > 8 {
		t.Fatalf("over max: %q", b.String())
	}
}

func TestExitErrorText(t *testing.T) {
	e := &ExitError{Cmd: "varnishlog", Code: 1, Stderr: "Can't open log\n"}
	if e.Error() != "varnishlog exited with code 1: Can't open log" {
		t.Fatalf("got %q", e.Error())
	}
	if (&ExitError{Cmd: "x", Code: 0}).Error() != "x exited with code 0" {
		t.Fatal("no stderr")
	}
}
