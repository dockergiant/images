package perf

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/dockergiant/images/inspector/internal/docker"
)

func TestSelfComputesCPUBetweenReadings(t *testing.T) {
	t0 := time.Unix(1_792_000_000, 0)
	calls := 0
	readings := []docker.Stats{
		{Read: t0, CPUNanos: 1e9, MemBytes: 100 << 20, MemLimit: 768 << 20, OnlineCPUs: 4},
		{Read: t0.Add(5 * time.Second), CPUNanos: 1e9 + 25e7, MemBytes: 110 << 20, MemLimit: 768 << 20, OnlineCPUs: 4},
	}
	d := &docker.Fake{SelfStatsFunc: func(context.Context) (docker.Stats, error) {
		r := readings[min(calls, len(readings)-1)]
		calls++
		return r, nil
	}}
	clock := t0
	s := newSelf(d)
	s.now = func() time.Time { return clock }

	first := s.Read(context.Background())
	if first.CPUPct != nil || first.MemBytes != 100<<20 || first.MemPct < 13 || first.MemPct > 13.1 {
		t.Fatalf("first %+v", first)
	}
	if again := s.Read(context.Background()); calls != 1 || again.MemBytes != first.MemBytes {
		t.Fatalf("cached read made %d calls", calls)
	}
	clock = clock.Add(5 * time.Second)
	second := s.Read(context.Background())
	if second.CPUPct == nil || *second.CPUPct != 5 {
		t.Fatalf("cpu %v", second.CPUPct)
	}
}

func TestSelfReportsAnError(t *testing.T) {
	d := &docker.Fake{SelfStatsFunc: func(context.Context) (docker.Stats, error) { return docker.Stats{}, errors.New("no socket") }}
	if r := newSelf(d).Read(context.Background()); r.Error != "no socket" {
		t.Fatalf("%+v", r)
	}
}

func TestReadLoad(t *testing.T) {
	p := filepath.Join(t.TempDir(), "loadavg")
	os.WriteFile(p, []byte("5.52 6.14 8.97 5/1841 853\n"), 0o644)
	l := readLoad(p)
	if l.One != 5.52 || l.Five != 6.14 || l.Fifteen != 8.97 || l.CPUs <= 0 || l.Error != "" {
		t.Fatalf("%+v", l)
	}
	if l := readLoad(filepath.Join(t.TempDir(), "none")); l.Error == "" {
		t.Fatal("missing file has no error")
	}
}
