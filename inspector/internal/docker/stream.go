package docker

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/moby/moby/api/pkg/stdcopy"
)

const (
	maxLine     = 1 << 20
	stderrTail  = 8 << 10
	pidWait     = 2 * time.Second
	killTimeout = 5 * time.Second
)

var errNoPID = errors.New("docker: stream ended before it reported a pid")

type ExitError struct {
	Cmd    string
	Code   int
	Stderr string
}

func (e *ExitError) Error() string {
	msg := fmt.Sprintf("%s exited with code %d", e.Cmd, e.Code)
	if s := strings.TrimSpace(e.Stderr); s != "" {
		msg += ": " + s
	}
	return msg
}

func streamCmd(cmd []string) []string {
	return append([]string{"sh", "-c", `echo $$; exec "$@"`, "sh"}, cmd...)
}

// The marker check guards against PID reuse: by the time the kill runs, the
// stream may have ended and its PID may belong to someone else's process.
const killScript = `tr '\0' '\n' 2>/dev/null < "/proc/$1/environ" | grep -qx '` + ExecMarker + `' || exit 0
kill -TERM "$1"`

func killCmd(pid int) []string {
	return []string{"sh", "-c", killScript, "sh", strconv.Itoa(pid)}
}

func readStream(r io.Reader, onPID func(int), onLine func(string)) error {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64<<10), maxLine)
	if !sc.Scan() {
		if err := sc.Err(); err != nil {
			return err
		}
		return errNoPID
	}
	first := strings.TrimSuffix(sc.Text(), "\r")
	pid, err := strconv.Atoi(first)
	if err != nil || pid <= 0 {
		return fmt.Errorf("docker: stream did not start with a pid: %q", first)
	}
	onPID(pid)
	for sc.Scan() {
		onLine(sc.Text())
	}
	return sc.Err()
}

func (c *Client) Stream(ctx context.Context, spec ExecSpec, onLine func(string)) error {
	if len(spec.Cmd) == 0 {
		return errors.New("docker: empty command")
	}
	if err := c.allow(ctx, spec.Container); err != nil {
		return err
	}
	id, att, err := c.attach(ctx, spec, streamCmd(spec.Cmd))
	if err != nil {
		return err
	}
	defer att.Close()

	pr, pw := io.Pipe()
	defer pr.Close()
	stderr := &tailBuffer{max: stderrTail}
	go func() {
		_, err := stdcopy.StdCopy(pw, stderr, att.Reader)
		pw.CloseWithError(err)
	}()

	pidCh := make(chan int, 1)
	readDone := make(chan struct{})
	stopped := make(chan struct{})
	stop := context.AfterFunc(ctx, func() {
		defer close(stopped)
		select {
		case pid := <-pidCh:
			c.kill(spec, pid)
		case <-readDone:
		case <-time.After(pidWait):
		}
		att.Close()
	})

	var pid int
	err = readStream(pr, func(p int) {
		pid = p
		pidCh <- p
	}, func(l string) {
		if ctx.Err() == nil {
			onLine(l)
		}
	})
	close(readDone)

	if ctx.Err() != nil {
		if !stop() {
			<-stopped
		}
		return ctx.Err()
	}
	stop()
	if err != nil && !errors.Is(err, errNoPID) {
		if pid > 0 {
			c.kill(spec, pid)
		}
		return err
	}
	code, ierr := c.exitCode(ctx, id)
	if ierr != nil {
		return ierr
	}
	return &ExitError{Cmd: spec.Cmd[0], Code: code, Stderr: stderr.String()}
}

// The kill runs on its own context because the stream's context is already
// done. A kill that fails is only logged: the startup sweep finds what is
// left by its marker.
func (c *Client) kill(spec ExecSpec, pid int) {
	ctx, cancel := context.WithTimeout(context.Background(), killTimeout)
	defer cancel()
	if err := killExec(ctx, c.run, spec, pid); err != nil {
		c.log.Warn("killing an exec failed", "container", spec.Container, "user", spec.User, "pid", pid, "err", err)
	}
}

// Root with Privileged is reserved for the /proc walk and the orphan sweep,
// so only an exec that already ran as root may retry that way: plain root
// cannot read the environ of a process that dropped to another uid.
func killExec(ctx context.Context, run func(context.Context, ExecSpec) (ExecResult, error), spec ExecSpec, pid int) error {
	err := runKill(ctx, run, ExecSpec{Container: spec.Container, Cmd: killCmd(pid), User: spec.User, Privileged: spec.Privileged})
	if err == nil || spec.Privileged || !isRoot(spec.User) {
		return err
	}
	return runKill(ctx, run, ExecSpec{Container: spec.Container, Cmd: killCmd(pid), User: "root", Privileged: true})
}

func runKill(ctx context.Context, run func(context.Context, ExecSpec) (ExecResult, error), spec ExecSpec) error {
	res, err := run(ctx, spec)
	if err != nil {
		return err
	}
	if res.ExitCode != 0 {
		return &ExitError{Cmd: "kill", Code: res.ExitCode, Stderr: string(res.Stderr)}
	}
	return nil
}

func isRoot(user string) bool {
	name, _, _ := strings.Cut(user, ":")
	return name == "root" || name == "0"
}

type tailBuffer struct {
	mu  sync.Mutex
	max int
	buf []byte
}

func (b *tailBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.buf = append(b.buf, p...)
	if over := len(b.buf) - b.max; over > 0 {
		b.buf = append(b.buf[:0], b.buf[over:]...)
	}
	return len(p), nil
}

func (b *tailBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return string(b.buf)
}
