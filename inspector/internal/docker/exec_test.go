package docker

import (
	"bytes"
	"context"
	"encoding/binary"
	"io"
	"slices"
	"testing"
	"time"
)

type frameWriter struct {
	w      io.Writer
	stream byte
}

func (f frameWriter) Write(p []byte) (int, error) {
	hdr := make([]byte, 8, 8+len(p))
	hdr[0] = f.stream
	binary.BigEndian.PutUint32(hdr[4:], uint32(len(p)))
	if _, err := f.w.Write(append(hdr, p...)); err != nil {
		return 0, err
	}
	return len(p), nil
}

func muxed(t *testing.T) (io.Reader, io.Writer, io.Writer, func()) {
	t.Helper()
	pr, pw := io.Pipe()
	return pr, frameWriter{pw, 1}, frameWriter{pw, 2}, func() { pw.Close() }
}

func TestReadKillableStripsPID(t *testing.T) {
	r, out, errw, closeConn := muxed(t)
	go func() {
		out.Write([]byte("4711\nP 1 php-fpm"))
		errw.Write([]byte("warn\n"))
		out.Write([]byte(": master\n"))
		closeConn()
	}()
	var stdout, stderr bytes.Buffer
	killed := false
	err := readKillable(context.Background(), r, &stdout, &stderr, func(int) { killed = true }, closeConn)
	if err != nil {
		t.Fatal(err)
	}
	if stdout.String() != "P 1 php-fpm: master\n" || stderr.String() != "warn\n" || killed {
		t.Fatalf("stdout %q stderr %q killed %v", stdout.String(), stderr.String(), killed)
	}
}

func TestReadKillableBadPID(t *testing.T) {
	r, out, _, closeConn := muxed(t)
	go func() {
		out.Write([]byte("sh: not found\n"))
		closeConn()
	}()
	var stdout, stderr bytes.Buffer
	if err := readKillable(context.Background(), r, &stdout, &stderr, func(int) {}, closeConn); err == nil {
		t.Fatal("want an error when the first line is no pid")
	}
}

func TestReadKillableKillsOnCancel(t *testing.T) {
	r, out, _, closeConn := muxed(t)
	go out.Write([]byte("321\npartial"))
	ctx, cancel := context.WithCancel(context.Background())
	var kills []int
	done := make(chan error, 1)
	var stdout, stderr bytes.Buffer
	go func() {
		done <- readKillable(ctx, r, &stdout, &stderr, func(pid int) { kills = append(kills, pid) }, closeConn)
	}()
	time.Sleep(50 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("want ctx error")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("read did not end after cancel")
	}
	if !slices.Equal(kills, []int{321}) {
		t.Fatalf("kills %v, want [321]", kills)
	}
}

func TestExecWrapsOnlyKillable(t *testing.T) {
	spec := ExecSpec{Cmd: []string{"sh", "-s"}}
	if got := execCmd(spec); !slices.Equal(got, spec.Cmd) {
		t.Fatalf("plain exec cmd %q", got)
	}
	spec.KillOnCancel = true
	if got := execCmd(spec); !slices.Equal(got, []string{"sh", "-c", `echo $$; exec "$@"`, "sh", "sh", "-s"}) {
		t.Fatalf("killable exec cmd %q", got)
	}
}
