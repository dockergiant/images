package perf

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// benchMarker is what a benchmark switched, written before its first arm.
type benchMarker struct {
	Started  time.Time       `json:"started"`
	Services map[string]Want `json:"services"`
}

func (b *Bench) writeMarker(services map[string]Want) error {
	data, err := json.Marshal(benchMarker{Started: time.Now(), Services: services})
	if err != nil {
		return err
	}
	tmp := b.cfg.MarkerPath + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, b.cfg.MarkerPath)
}

func (b *Bench) removeMarker() {
	_ = os.Remove(b.cfg.MarkerPath)
}

// Recover puts back what a benchmark switched when Inspector died before it
// could restore, and removes the marker once every container is back. It
// returns the services it switched. The containers are looked up by service,
// since php-fpm may have been recreated since.
func (b *Bench) Recover(ctx context.Context) ([]string, error) {
	data, err := os.ReadFile(b.cfg.MarkerPath)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", filepath.Base(b.cfg.MarkerPath), err)
	}
	var m benchMarker
	if err := json.Unmarshal(data, &m); err != nil {
		b.removeMarker()
		return nil, fmt.Errorf("the benchmark marker is unreadable, removed it: %w", err)
	}
	b.ctl.applyMu.Lock()
	defer b.ctl.applyMu.Unlock()
	var restored []string
	var errs []error
	for _, service := range Services {
		want, ok := m.Services[service]
		if !ok {
			continue
		}
		st, id := b.ctl.read(ctx, service, true)
		switch {
		case !st.Present:
			// Not running: it starts again from .env.roll, not from an arm.
			continue
		case st.Error != "" || id == "":
			errs = append(errs, fmt.Errorf("%s: %s", service, st.Reason))
			continue
		case st.want() == want:
			continue
		}
		if err := b.ctl.switchTo(ctx, id, want); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", service, err))
			continue
		}
		b.ctl.forget()
		if got, _ := b.ctl.read(ctx, service, true); got.want() != want {
			errs = append(errs, fmt.Errorf("%s is %s, not %s", service, describe(got.want()), describe(want)))
			continue
		}
		restored = append(restored, service)
	}
	b.ctl.forget()
	if err := errors.Join(errs...); err != nil {
		return restored, err
	}
	b.removeMarker()
	return restored, nil
}
