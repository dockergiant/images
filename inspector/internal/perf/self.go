package perf

import (
	"context"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/dockergiant/images/inspector/internal/docker"
)

const (
	selfTTL     = 5 * time.Second
	selfTimeout = 5 * time.Second
	loadPath    = "/proc/loadavg"
)

// SelfStats is the inspector container's own CPU and memory. CPUPct is the
// share of one core between the last two readings, so it is null on the
// first one.
type SelfStats struct {
	CPUPct     *float64 `json:"cpuPct"`
	MemBytes   uint64   `json:"memBytes"`
	MemLimit   uint64   `json:"memLimit"`
	MemPct     float64  `json:"memPct"`
	OnlineCPUs uint32   `json:"onlineCpus"`
	PIDs       uint64   `json:"pids"`
	At         int64    `json:"at"`
	Error      string   `json:"error,omitempty"`
}

type self struct {
	docker docker.API
	now    func() time.Time

	mu   sync.Mutex
	last SelfStats
	at   time.Time
	prev *docker.Stats
}

func newSelf(d docker.API) *self {
	return &self{docker: d, now: time.Now}
}

// Read answers from a 5 s cache, so a page that polls costs one Docker stats
// call per 5 s however many tabs are open.
func (s *self) Read(ctx context.Context) SelfStats {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.at.IsZero() && s.now().Sub(s.at) < selfTTL {
		return s.last
	}
	ctx, cancel := context.WithTimeout(ctx, selfTimeout)
	defer cancel()
	st, err := s.docker.SelfStats(ctx)
	s.at = s.now()
	if err != nil {
		s.last = SelfStats{Error: err.Error(), At: s.at.Unix()}
		return s.last
	}
	out := SelfStats{MemBytes: st.MemBytes, MemLimit: st.MemLimit, OnlineCPUs: st.OnlineCPUs, PIDs: st.PIDs, At: s.at.Unix()}
	if st.MemLimit > 0 {
		out.MemPct = float64(st.MemBytes) / float64(st.MemLimit) * 100
	}
	if p := s.prev; p != nil && st.Read.After(p.Read) && st.CPUNanos >= p.CPUNanos {
		pct := float64(st.CPUNanos-p.CPUNanos) / float64(st.Read.Sub(p.Read)) * 100
		out.CPUPct = &pct
	}
	s.prev = &st
	s.last = out
	return out
}

// Load is the Docker VM's load average, which the inspector container sees
// in its own /proc.
type Load struct {
	One     float64 `json:"one"`
	Five    float64 `json:"five"`
	Fifteen float64 `json:"fifteen"`
	CPUs    int     `json:"cpus"`
	Error   string  `json:"error,omitempty"`
}

func readLoad(path string) Load {
	l := Load{CPUs: runtime.NumCPU()}
	data, err := os.ReadFile(path)
	if err != nil {
		l.Error = err.Error()
		return l
	}
	f := strings.Fields(string(data))
	if len(f) < 3 {
		l.Error = "unexpected " + path + ": " + strings.TrimSpace(string(data))
		return l
	}
	l.One, _ = strconv.ParseFloat(f[0], 64)
	l.Five, _ = strconv.ParseFloat(f[1], 64)
	l.Fifteen, _ = strconv.ParseFloat(f[2], 64)
	return l
}
