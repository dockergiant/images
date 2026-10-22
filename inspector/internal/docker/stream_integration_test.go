//go:build integration

package docker

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"
)

const markedPIDsScript = `self=$$
for d in /proc/[0-9]*; do
  p=${d#/proc/}
  [ "$p" = "$self" ] && continue
  if tr '\0' '\n' 2>/dev/null < "$d/environ" | grep -qx '` + ExecMarker + `'; then
    echo "$p $(tr '\0' ' ' 2>/dev/null < "$d/cmdline")"
  fi
done`

func integrationClient(t *testing.T) *Client {
	env := os.Getenv("INSPECTOR_IT_ENV")
	if env == "" {
		t.Skip("INSPECTOR_IT_ENV is not set; scripts/integration sets it to the throwaway stack")
	}
	c, err := New(env)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}

func markedPIDs(t *testing.T, c *Client, container string) string {
	res, err := c.Exec(context.Background(), ExecSpec{Container: container, Cmd: []string{"sh", "-c", markedPIDsScript}, User: "root", Privileged: true})
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(res.Stdout))
}

func TestIntegrationStreamVarnishlog(t *testing.T) {
	c := integrationClient(t)
	ctx := context.Background()
	varnish, err := c.Service(ctx, "varnish")
	if err != nil || varnish == nil {
		t.Fatalf("varnish: %+v, %v", varnish, err)
	}
	php, err := c.Service(ctx, "php-fpm")
	if err != nil || php == nil {
		t.Fatalf("php-fpm: %+v, %v", php, err)
	}

	sctx, cancel := context.WithCancel(ctx)
	defer cancel()
	lines := make(chan string, 1024)
	done := make(chan error, 1)
	go func() {
		done <- c.Stream(sctx, ExecSpec{Container: varnish.ID, Cmd: []string{"varnishlog", "-g", "request"}}, func(l string) {
			select {
			case lines <- l:
			default:
			}
		})
	}()

	deadline := time.After(20 * time.Second)
	got := false
	for !got {
		if _, err := c.Exec(ctx, ExecSpec{Container: php.ID, User: "www-data", Cmd: []string{"curl", "-s", "-o", "/dev/null", "--max-time", "10", "http://varnish/"}}); err != nil {
			t.Fatal(err)
		}
		select {
		case <-lines:
			got = true
		case err := <-done:
			t.Fatalf("stream ended early: %v", err)
		case <-time.After(time.Second):
		case <-deadline:
			t.Fatal("no varnishlog line after a storefront request")
		}
	}

	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("stream returned %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("stream did not stop after cancel")
	}

	var left string
	for i := 0; i < 30; i++ {
		if left = markedPIDs(t, c, varnish.ID); left == "" {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("marked processes left in varnish: %s", left)
}
