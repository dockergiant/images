package docker

import (
	"context"
	"testing"
	"time"
)

func TestFakeService(t *testing.T) {
	f := &Fake{ContainerList: []Container{
		{ID: "a", Service: "varnish", Running: false},
		{ID: "b", Service: "varnish", Running: true},
	}}
	c, err := f.Service(context.Background(), "varnish")
	if err != nil || c == nil || c.ID != "b" {
		t.Fatalf("got %+v, %v", c, err)
	}
	c, err = f.Service(context.Background(), "redis")
	if err != nil || c != nil {
		t.Fatalf("missing service: got %+v, %v", c, err)
	}
}

func TestFakeExecRecordsAndDelegates(t *testing.T) {
	f := &Fake{ExecFunc: func(ctx context.Context, spec ExecSpec) (ExecResult, error) {
		return ExecResult{Stdout: []byte(spec.Cmd[0])}, nil
	}}
	res, err := f.Exec(context.Background(), ExecSpec{Container: "x", Cmd: []string{"echo"}})
	if err != nil || string(res.Stdout) != "echo" {
		t.Fatalf("got %q, %v", res.Stdout, err)
	}
	if got := f.Execs(); len(got) != 1 || got[0].Container != "x" {
		t.Fatalf("recorded %+v", got)
	}
}

func TestFakeStreamDefaultBlocksUntilCancel(t *testing.T) {
	f := &Fake{}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := f.Stream(ctx, ExecSpec{}, func(string) {}); err != context.DeadlineExceeded {
		t.Fatalf("got %v", err)
	}
}

func TestFakeLogsSince(t *testing.T) {
	base := time.Unix(1000, 0)
	f := &Fake{LogLines: map[string][]LogLine{"c": {
		{Time: base, Line: "old"},
		{Time: base.Add(time.Second), Line: "new"},
	}}}
	var got []string
	if err := f.Logs(context.Background(), "c", base.Add(time.Millisecond), false, func(l LogLine) { got = append(got, l.Line) }); err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0] != "new" {
		t.Fatalf("got %v", got)
	}
}

func TestFakeWatchChange(t *testing.T) {
	f := &Fake{}
	ctx, cancel := context.WithCancel(context.Background())
	seen := make(chan string, 1)
	done := make(chan struct{})
	go func() {
		f.Watch(ctx, func(s string) { seen <- s })
		close(done)
	}()
	for f.Watchers() == 0 {
		time.Sleep(time.Millisecond)
	}
	f.Change("php-fpm")
	if s := <-seen; s != "php-fpm" {
		t.Fatalf("got %q", s)
	}
	cancel()
	<-done
	if f.Watchers() != 0 {
		t.Fatal("watcher not removed")
	}
}
