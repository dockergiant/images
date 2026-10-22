package perf

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/dockergiant/images/inspector/internal/docker"
)

func readMarker(t *testing.T, path string) (benchMarker, bool) {
	t.Helper()
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return benchMarker{}, false
	}
	if err != nil {
		t.Fatal(err)
	}
	var m benchMarker
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatal(err)
	}
	return m, true
}

func TestBenchMarkerLivesWhileTheRunDoes(t *testing.T) {
	b, _, _, n := newTestBench(t)
	gate := make(chan struct{})
	n.mu.Lock()
	n.gate = gate
	n.mu.Unlock()
	if _, err := b.Start(context.Background(), "/women.html"); err != nil {
		t.Fatal(err)
	}
	m, ok := readMarker(t, b.cfg.MarkerPath)
	if !ok {
		t.Fatal("no marker before the first arm")
	}
	want := Want{Tracing: true, Detail: "full", Profiling: true}
	if m.Services["php-fpm"] != want || len(m.Services) != 1 {
		t.Fatalf("marker %+v, want php-fpm %+v", m, want)
	}
	n.mu.Lock()
	n.gate = nil
	n.mu.Unlock()
	close(gate)
	if st := waitDone(t, b); !st.Restored {
		t.Fatalf("not restored: %+v", st)
	}
	if _, ok := readMarker(t, b.cfg.MarkerPath); ok {
		t.Fatal("the marker outlived a verified restore")
	}
}

func TestBenchRefusesToStartWithoutAMarker(t *testing.T) {
	b, c, _, _ := newTestBench(t)
	b.cfg.MarkerPath = filepath.Join(t.TempDir(), "missing", "dir", "marker.json")
	if _, err := b.Start(context.Background(), "/women.html"); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("err = %v, want ErrUnavailable", err)
	}
	if c.Busy() {
		t.Fatal("a refused start kept the switches")
	}
}

// After a crash between arms the marker holds what php-fpm ran before; the
// next start puts it back and drops the marker.
func TestRecoverRestoresAfterACrash(t *testing.T) {
	b, _, php, _ := newTestBench(t)
	php.set("fpm", docker.IniState{Script: true, DetailSupport: true, Template: true})
	before := php.reloads["fpm"]
	writeTestMarker(t, b.cfg.MarkerPath, map[string]Want{"php-fpm": {Tracing: true, Detail: "lean"}})

	restored, err := b.Recover(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(restored) != 1 || restored[0] != "php-fpm" {
		t.Fatalf("restored %v", restored)
	}
	s := php.get("fpm")
	if !s.Tracing() || s.Detail() != "lean" || s.Profiling() || php.reloads["fpm"] != before+1 {
		t.Fatalf("php-fpm after recover: %+v, reloads %d", s, php.reloads["fpm"])
	}
	if _, ok := readMarker(t, b.cfg.MarkerPath); ok {
		t.Fatal("marker left after the restore")
	}
}

func TestRecoverSkipsAContainerAlreadyRight(t *testing.T) {
	b, _, php, _ := newTestBench(t)
	writeTestMarker(t, b.cfg.MarkerPath, map[string]Want{"php-fpm": {Tracing: true, Detail: "full", Profiling: true}})
	if _, err := b.Recover(context.Background()); err != nil {
		t.Fatal(err)
	}
	if php.reloads["fpm"] != 0 || php.switches != 0 {
		t.Fatalf("switched a container that already ran the old settings: %d switches", php.switches)
	}
	if _, ok := readMarker(t, b.cfg.MarkerPath); ok {
		t.Fatal("marker left")
	}
}

func TestRecoverKeepsTheMarkerWhenTheRestoreFails(t *testing.T) {
	b, _, php, _ := newTestBench(t)
	php.set("fpm", docker.IniState{Script: true, DetailSupport: true, Template: true})
	php.mu.Lock()
	php.fail["fpm"] = true
	php.mu.Unlock()
	writeTestMarker(t, b.cfg.MarkerPath, map[string]Want{"php-fpm": {Tracing: true, Detail: "full"}})
	if _, err := b.Recover(context.Background()); err == nil {
		t.Fatal("a failed restore reported success")
	}
	if _, ok := readMarker(t, b.cfg.MarkerPath); !ok {
		t.Fatal("the marker went though php-fpm was not restored")
	}
}

func TestRecoverWithoutMarkerDoesNothing(t *testing.T) {
	b, _, php, _ := newTestBench(t)
	restored, err := b.Recover(context.Background())
	if err != nil || len(restored) != 0 || php.switches != 0 {
		t.Fatalf("restored %v err %v switches %d", restored, err, php.switches)
	}
}

func writeTestMarker(t *testing.T, path string, services map[string]Want) {
	t.Helper()
	data, err := json.Marshal(benchMarker{Services: services})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
}
