package docker

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// The sweep exec carries the marker itself, so it skips its own PID. Its
// children start after the glob is expanded and never appear in the list.
const sweepScript = `self=$$
parent=$PPID
for d in /proc/[0-9]*; do
  p=${d#/proc/}
  [ "$p" = "$self" ] && continue
  [ "$p" = "$parent" ] && continue
  st=$(sed -n 's/^State:[[:space:]]*\(.\).*/\1/p' "$d/status" 2>/dev/null)
  case "$st" in Z|X|'') continue ;; esac
  if tr '\0' '\n' 2>/dev/null < "$d/environ" | grep -qx '` + ExecMarker + `'; then
    kill -TERM "$p" 2>/dev/null && echo "$p"
  fi
done
exit 0`

// Privileged because plain root cannot read /proc/<pid>/environ of processes
// that run as another uid (varnish, www-data), which is every stream.
func sweepSpec(containerID string) ExecSpec {
	return ExecSpec{
		Container:  containerID,
		Cmd:        []string{"sh", "-c", sweepScript},
		User:       "root",
		Privileged: true,
	}
}

func (c *Client) Sweep(ctx context.Context) error {
	list, err := c.Containers(ctx)
	if err != nil {
		return err
	}
	var errs []error
	for _, ct := range list {
		if !ct.Running || ct.Service == "" {
			continue
		}
		res, err := c.Exec(ctx, sweepSpec(ct.ID))
		if err != nil {
			errs = append(errs, fmt.Errorf("sweep %s: %w", ct.Service, err))
			continue
		}
		if res.ExitCode != 0 {
			errs = append(errs, fmt.Errorf("sweep %s: exit %d: %s", ct.Service, res.ExitCode, strings.TrimSpace(string(res.Stderr))))
		}
	}
	return errors.Join(errs...)
}
