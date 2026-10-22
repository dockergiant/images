package nginx

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"time"

	"github.com/dockergiant/images/inspector/internal/core"
	"github.com/dockergiant/images/inspector/internal/docker"
)

const (
	Kind      core.Kind = "nginx"
	service             = "nginx"
	waitEvery           = 10 * time.Second
)

var mainFormat = regexp.MustCompile(`^\S+ - \S+ \[[^\]]*\] "([A-Z]+) (\S+) HTTP/[0-9.]+" (\d{3}) `)

func ParseAccess(line string, at time.Time) (core.NginxAccess, bool) {
	m := mainFormat.FindStringSubmatch(line)
	if m == nil {
		return core.NginxAccess{}, false
	}
	status, _ := strconv.Atoi(m[3])
	return core.NginxAccess{Time: at, Method: m[1], URL: m[2], Status: status}, true
}

type source struct {
	docker docker.API
	now    func() time.Time
	follow bool
	wait   time.Duration
}

func Stream(d docker.API) core.StreamSpec {
	return newStream(d, time.Now)
}

func newStream(d docker.API, now func() time.Time) core.StreamSpec {
	s := &source{docker: d, now: now, follow: true, wait: waitEvery}
	return core.StreamSpec{Name: "access", Title: "Nginx access log", Start: s.start}
}

func (s *source) start(ctx context.Context, _ core.Filter, emit core.Emit) error {
	var c *docker.Container
	for {
		var err error
		c, err = s.docker.Service(ctx, service)
		if err != nil {
			return fmt.Errorf("find nginx: %w", err)
		}
		if c != nil {
			break
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(s.wait):
		}
	}
	err := s.docker.Logs(ctx, c.ID, s.now(), s.follow, func(l docker.LogLine) {
		at := l.Time
		if at.IsZero() {
			at = s.now()
		}
		if a, ok := ParseAccess(l.Line, at); ok {
			emit(core.Event{Type: "nginx.access", Time: at, Kind: Kind, Data: a})
		}
	})
	if ctx.Err() != nil {
		return ctx.Err()
	}
	// The hub only restarts a stream that fails, and a log that ends means
	// nginx restarted, so a clean end is reported as an error.
	if err == nil {
		err = errors.New("log stream ended")
	}
	return fmt.Errorf("nginx logs: %w", err)
}
