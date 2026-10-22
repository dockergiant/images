package docker

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/moby/moby/api/pkg/stdcopy"
	"github.com/moby/moby/client"
)

func withMarker(env []string) []string {
	out := make([]string, 0, len(env)+1)
	out = append(out, env...)
	return append(out, ExecMarker)
}

// Only containers carrying this env's project label may be exec'd into; the
// answer is cached because compose labels never change for a container ID.
func (c *Client) allow(ctx context.Context, id string) error {
	if id == "" {
		return errors.New("docker: no container given")
	}
	c.allowMu.Lock()
	ok := c.allowed[id]
	c.allowMu.Unlock()
	if ok {
		return nil
	}
	res, err := c.cli.ContainerInspect(ctx, id, client.ContainerInspectOptions{})
	if err != nil {
		return err
	}
	var labels map[string]string
	if res.Container.Config != nil {
		labels = res.Container.Config.Labels
	}
	if labels[labelProject] != c.env || labels[labelService] == selfService {
		return fmt.Errorf("docker: container %s is not a service of %s", id, c.env)
	}
	c.allowMu.Lock()
	c.allowed[id] = true
	if res.Container.ID != "" {
		c.allowed[res.Container.ID] = true
	}
	c.allowMu.Unlock()
	return nil
}

func (c *Client) attach(ctx context.Context, spec ExecSpec, cmd []string) (string, client.ExecAttachResult, error) {
	c.execs.add(time.Now())
	cr, err := c.cli.ExecCreate(ctx, spec.Container, client.ExecCreateOptions{
		User:         spec.User,
		Privileged:   spec.Privileged,
		AttachStdin:  spec.Stdin != nil,
		AttachStdout: true,
		AttachStderr: true,
		Env:          withMarker(spec.Env),
		Cmd:          cmd,
	})
	if err != nil {
		return "", client.ExecAttachResult{}, err
	}
	att, err := c.cli.ExecAttach(ctx, cr.ID, client.ExecAttachOptions{})
	if err != nil {
		return "", client.ExecAttachResult{}, err
	}
	if spec.Stdin != nil {
		go func() {
			io.Copy(att.Conn, spec.Stdin)
			att.CloseWrite()
		}()
	}
	return cr.ID, att, nil
}

func (c *Client) Exec(ctx context.Context, spec ExecSpec) (ExecResult, error) {
	if len(spec.Cmd) == 0 {
		return ExecResult{}, errors.New("docker: empty command")
	}
	if err := c.allow(ctx, spec.Container); err != nil {
		return ExecResult{}, err
	}
	if spec.KillOnCancel {
		return c.runKillable(ctx, spec)
	}
	return c.run(ctx, spec)
}

func execCmd(spec ExecSpec) []string {
	if spec.KillOnCancel {
		return streamCmd(spec.Cmd)
	}
	return spec.Cmd
}

// runKillable stays out of run because kill itself goes through run.
func (c *Client) runKillable(ctx context.Context, spec ExecSpec) (ExecResult, error) {
	id, att, err := c.attach(ctx, spec, execCmd(spec))
	if err != nil {
		return ExecResult{}, err
	}
	defer att.Close()
	var stdout, stderr bytes.Buffer
	err = readKillable(ctx, att.Reader, &stdout, &stderr, func(pid int) { c.kill(spec, pid) }, att.Close)
	res := ExecResult{Stdout: stdout.Bytes(), Stderr: stderr.Bytes()}
	if ctx.Err() != nil {
		return res, ctx.Err()
	}
	if err != nil {
		return res, err
	}
	code, err := c.exitCode(ctx, id)
	res.ExitCode = code
	return res, err
}

// readKillable demultiplexes an exec started through streamCmd: the first
// stdout line is the PID, which is kept back from stdout and killed when ctx
// ends before the stream does.
func readKillable(ctx context.Context, r io.Reader, stdout, stderr io.Writer, kill func(int), closeConn func()) error {
	pr, pw := io.Pipe()
	copied := make(chan struct{})
	go func() {
		defer close(copied)
		_, err := stdcopy.StdCopy(pw, stderr, r)
		pw.CloseWithError(err)
	}()
	defer func() {
		pr.Close()
		closeConn()
		<-copied
	}()

	pidCh := make(chan int, 1)
	readDone := make(chan struct{})
	stopped := make(chan struct{})
	stop := context.AfterFunc(ctx, func() {
		defer close(stopped)
		select {
		case pid := <-pidCh:
			kill(pid)
		case <-readDone:
		case <-time.After(pidWait):
		}
		closeConn()
	})

	err := copyAfterPID(pr, stdout, func(pid int) { pidCh <- pid })
	if err == nil {
		select {
		case <-pidCh:
		default:
		}
	}
	close(readDone)
	if ctx.Err() != nil {
		if !stop() {
			<-stopped
		}
		return ctx.Err()
	}
	stop()
	return err
}

func copyAfterPID(r io.Reader, w io.Writer, onPID func(int)) error {
	br := bufio.NewReader(r)
	first, err := br.ReadString('\n')
	if err != nil {
		if first == "" && errors.Is(err, io.EOF) {
			return errNoPID
		}
		if !errors.Is(err, io.EOF) {
			return err
		}
	}
	line := strings.TrimSuffix(strings.TrimSuffix(first, "\n"), "\r")
	pid, perr := strconv.Atoi(line)
	if perr != nil || pid <= 0 {
		return fmt.Errorf("docker: exec did not start with a pid: %q", line)
	}
	onPID(pid)
	_, err = io.Copy(w, br)
	return err
}

func (c *Client) run(ctx context.Context, spec ExecSpec) (ExecResult, error) {
	id, att, err := c.attach(ctx, spec, spec.Cmd)
	if err != nil {
		return ExecResult{}, err
	}
	defer att.Close()
	// The hijacked connection outlives ctx unless closed explicitly.
	stop := context.AfterFunc(ctx, att.Close)
	defer stop()

	var stdout, stderr bytes.Buffer
	_, cpErr := stdcopy.StdCopy(&stdout, &stderr, att.Reader)
	res := ExecResult{Stdout: stdout.Bytes(), Stderr: stderr.Bytes()}
	if ctx.Err() != nil {
		return res, ctx.Err()
	}
	if cpErr != nil {
		return res, cpErr
	}
	code, err := c.exitCode(ctx, id)
	res.ExitCode = code
	return res, err
}

// The attach stream can hit EOF a moment before the daemon records the exit
// code, so poll briefly while the exec still reports Running.
func (c *Client) exitCode(ctx context.Context, id string) (int, error) {
	deadline := time.Now().Add(2 * time.Second)
	for {
		ins, err := c.cli.ExecInspect(ctx, id, client.ExecInspectOptions{})
		if err != nil {
			return -1, err
		}
		if !ins.Running || time.Now().After(deadline) {
			return ins.ExitCode, nil
		}
		select {
		case <-ctx.Done():
			return -1, ctx.Err()
		case <-time.After(20 * time.Millisecond):
		}
	}
}
