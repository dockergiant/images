package nginx

import (
	"context"
	"testing"
	"time"

	"github.com/dockergiant/images/inspector/internal/core"
	"github.com/dockergiant/images/inspector/internal/docker"
)

const mainLine = `192.168.165.13 - - [20/Oct/2026:21:24:27 +0000] "GET /category-1.html?p=2 HTTP/1.1" 200 18576 "https://app.inspector-e2e.test/" "Mozilla/5.0 (Macintosh)" "192.168.148.1, 192.168.165.2"`

func TestParseMainFormat(t *testing.T) {
	at := time.Date(2026, 10, 20, 21, 24, 27, 993443292, time.UTC)
	a, ok := ParseAccess(mainLine, at)
	if !ok {
		t.Fatal("main format line not parsed")
	}
	if a.Method != "GET" || a.URL != "/category-1.html?p=2" || a.Status != 200 || !a.Time.Equal(at) {
		t.Fatalf("parsed %+v", a)
	}
	if a, ok := ParseAccess(`10.0.0.1 - user [20/Oct/2026:21:24:27 +0000] "POST /checkout/cart/add HTTP/2.0" 302 0 "-" "-"`, at); !ok || a.Method != "POST" || a.Status != 302 {
		t.Fatalf("second line %+v %v", a, ok)
	}
	for _, bad := range []string{
		`10.0.0.1 - - [20/Oct/2026:21:24:27 +0000] "-" 400 0 "-" "-"`,
		`2026/10/20 21:24:27 [error] 31#31: *1 open() "/var/www/html/pub/x" failed`,
		``,
		`10.0.0.1 - - [20/Oct/2026:21:24:27 +0000] "\x16\x03\x01" 400 157 "-" "-"`,
	} {
		if a, ok := ParseAccess(bad, at); ok {
			t.Errorf("%q parsed as %+v", bad, a)
		}
	}
}

func TestStreamFollowsNginxLogs(t *testing.T) {
	start := time.Date(2026, 10, 20, 21, 24, 0, 0, time.UTC)
	d := &docker.Fake{
		EnvName:       "shop",
		ContainerList: []docker.Container{{ID: "ngx", Service: "nginx", Running: true}},
		LogLines: map[string][]docker.LogLine{"ngx": {
			{Time: start.Add(-time.Minute), Line: mainLine},
			{Time: start.Add(time.Second), Line: mainLine},
			{Time: start.Add(2 * time.Second), Stream: 2, Line: "2026/10/20 21:24:27 [warn] something"},
		}},
	}
	spec := newStream(d, func() time.Time { return start })
	if spec.Name != "access" || spec.OptIn {
		t.Fatalf("spec %+v", spec)
	}
	ctx, cancel := context.WithCancel(context.Background())
	got := make(chan core.Event, 8)
	done := make(chan error, 1)
	go func() { done <- spec.Start(ctx, core.Filter{}, func(ev core.Event) { got <- ev }) }()

	select {
	case ev := <-got:
		a, ok := ev.Data.(core.NginxAccess)
		if ev.Type != "nginx.access" || ev.Kind != Kind || !ok || !ev.Time.Equal(start.Add(time.Second)) || a.URL != "/category-1.html?p=2" {
			t.Fatalf("event %+v", ev)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no access event")
	}
	select {
	case ev := <-got:
		t.Fatalf("extra event %+v", ev)
	case <-time.After(50 * time.Millisecond):
	}
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("stream did not stop")
	}
}

func TestStreamEndsWithErrorWhenLogsClose(t *testing.T) {
	d := &docker.Fake{EnvName: "shop", ContainerList: []docker.Container{{ID: "ngx", Service: "nginx", Running: true}}}
	s := &source{docker: d, now: time.Now, follow: false}
	if err := s.start(context.Background(), core.Filter{}, func(core.Event) {}); err == nil {
		t.Fatal("a closed log must return an error so the hub restarts the stream")
	}
}

func TestStreamWaitsForNginx(t *testing.T) {
	d := &docker.Fake{EnvName: "shop"}
	s := &source{docker: d, now: time.Now, follow: true, wait: 10 * time.Millisecond}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Millisecond)
	defer cancel()
	if err := s.start(ctx, core.Filter{}, func(core.Event) {}); err != context.DeadlineExceeded {
		t.Fatalf("err %v", err)
	}
}
