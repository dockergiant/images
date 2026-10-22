package docker

import (
	"bytes"
	"context"
	"io"
	"strings"
	"time"

	"github.com/moby/moby/api/pkg/stdcopy"
	"github.com/moby/moby/client"
)

func (c *Client) Logs(ctx context.Context, id string, since time.Time, follow bool, onLine func(LogLine)) error {
	if err := c.allow(ctx, id); err != nil {
		return err
	}
	ins, err := c.cli.ContainerInspect(ctx, id, client.ContainerInspectOptions{})
	if err != nil {
		return err
	}
	tty := ins.Container.Config != nil && ins.Container.Config.Tty
	opts := client.ContainerLogsOptions{ShowStdout: true, ShowStderr: true, Timestamps: true, Follow: follow}
	if !since.IsZero() {
		opts.Since = since.Format(time.RFC3339Nano)
	}
	rc, err := c.cli.ContainerLogs(ctx, id, opts)
	if err != nil {
		return err
	}
	defer rc.Close()
	err = demuxLogs(rc, tty, func(l LogLine) {
		if ctx.Err() == nil {
			onLine(l)
		}
	})
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return err
}

func demuxLogs(r io.Reader, tty bool, onLine func(LogLine)) error {
	out := &lineWriter{stream: int(stdcopy.Stdout), emit: onLine}
	errw := &lineWriter{stream: int(stdcopy.Stderr), emit: onLine}
	var err error
	if tty {
		_, err = io.Copy(out, r)
	} else {
		_, err = stdcopy.StdCopy(out, errw, r)
	}
	out.flush()
	errw.flush()
	return err
}

type lineWriter struct {
	stream int
	buf    []byte
	emit   func(LogLine)
}

func (w *lineWriter) Write(p []byte) (int, error) {
	w.buf = append(w.buf, p...)
	for {
		i := bytes.IndexByte(w.buf, '\n')
		if i < 0 {
			break
		}
		w.emit(parseLogLine(w.stream, string(w.buf[:i])))
		w.buf = w.buf[i+1:]
	}
	if len(w.buf) > maxLine {
		w.flush()
	}
	return len(p), nil
}

func (w *lineWriter) flush() {
	if len(w.buf) > 0 {
		w.emit(parseLogLine(w.stream, string(w.buf)))
		w.buf = nil
	}
}

func parseLogLine(stream int, s string) LogLine {
	s = strings.TrimSuffix(s, "\r")
	ts, rest, _ := strings.Cut(s, " ")
	t, err := time.Parse(time.RFC3339Nano, ts)
	if err != nil {
		return LogLine{Stream: stream, Line: s}
	}
	return LogLine{Time: t, Stream: stream, Line: rest}
}
