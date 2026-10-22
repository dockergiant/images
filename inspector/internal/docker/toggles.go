package docker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/moby/moby/client"
)

// TraceSettings are the values roll-inspector-ini reads from its environment.
type TraceSettings struct {
	Env     string
	Profile string
	Detail  string
}

var envName = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]*$`)

// IniSwitch switches tracing in a php-fpm or php-debug container. The argv is
// fixed and the values travel as environment variables, each checked against
// the few the script knows.
func IniSwitch(container string, on bool, s TraceSettings) (ExecSpec, error) {
	if container == "" {
		return ExecSpec{}, errors.New("docker: no container given")
	}
	if !envName.MatchString(s.Env) {
		return ExecSpec{}, fmt.Errorf("docker: env name %q is not a compose project name", s.Env)
	}
	if s.Profile == "" {
		s.Profile = "off"
	}
	if s.Detail == "" {
		s.Detail = "full"
	}
	if s.Profile != "off" && s.Profile != "excimer" {
		return ExecSpec{}, fmt.Errorf("docker: profile %q is not off or excimer", s.Profile)
	}
	if s.Detail != "full" && s.Detail != "lean" {
		return ExecSpec{}, fmt.Errorf("docker: trace detail %q is not full or lean", s.Detail)
	}
	mode := "off"
	if on {
		mode = "on"
	}
	return ExecSpec{
		Container: container,
		Cmd:       []string{"roll-inspector-ini", mode},
		User:      "root",
		Env: []string{
			"ROLL_ENV_NAME=" + s.Env,
			"ROLL_INSPECTOR_PROFILE=" + s.Profile,
			"ROLL_INSPECTOR_TRACE_DETAIL=" + s.Detail,
		},
	}, nil
}

// FPMReload asks the FPM master for a graceful reload, which is how
// roll inspector trace makes the new ini files count.
func FPMReload(container string) ExecSpec {
	return ExecSpec{Container: container, Cmd: []string{"kill", "-USR2", "1"}, User: "root"}
}

const iniStateScript = `d=/usr/local/etc/php/conf.d
b=/usr/local/bin/roll-inspector-ini
has() { if [ -e "$1" ]; then echo "$2=1"; else echo "$2=0"; fi; }
if [ -x "$b" ]; then echo script=1; else echo script=0; fi
if grep -q TRACE_DETAIL "$b" 2>/dev/null; then echo detail=1; else echo detail=0; fi
has "$d/98-ddtrace.ini.template" template
has "$d/98-ddtrace.ini" ddtrace
has "$d/99-inspector-ddtrace.ini" settings
has "$d/99-inspector-profile.ini" profile
has "$d/newrelic.ini" newrelic
if grep -qx 'datadog.trace.magento_enabled=0' "$d/99-inspector-ddtrace.ini" 2>/dev/null; then echo lean=1; else echo lean=0; fi
exit 0`

// IniStateRead lists which of the tracing ini files exist, with a fixed script
// that takes no input.
func IniStateRead(container string) ExecSpec {
	return ExecSpec{Container: container, Cmd: []string{"sh", "-c", iniStateScript}, User: "root"}
}

type IniState struct {
	Script        bool `json:"script"`
	DetailSupport bool `json:"detailSupport"`
	Template      bool `json:"template"`
	Loaded        bool `json:"loaded"`
	Settings      bool `json:"settings"`
	Profile       bool `json:"profile"`
	NewRelic      bool `json:"newrelic"`
	Lean          bool `json:"lean"`
}

func (s IniState) Tracing() bool   { return s.Loaded && s.Settings }
func (s IniState) Profiling() bool { return s.Tracing() && s.Profile }

func (s IniState) Detail() string {
	if s.Tracing() && s.Lean {
		return "lean"
	}
	return "full"
}

func ParseIniState(out []byte) IniState {
	var s IniState
	fields := map[string]*bool{
		"script": &s.Script, "detail": &s.DetailSupport, "template": &s.Template, "ddtrace": &s.Loaded,
		"settings": &s.Settings, "profile": &s.Profile, "newrelic": &s.NewRelic, "lean": &s.Lean,
	}
	for _, line := range strings.Split(string(out), "\n") {
		k, v, ok := strings.Cut(strings.TrimSpace(line), "=")
		if p := fields[k]; ok && p != nil && v == "1" {
			*p = true
		}
	}
	return s
}

// Stats is one Docker stats reading of the inspector container. CPUNanos is
// cumulative, so a CPU share needs two readings.
type Stats struct {
	Read       time.Time `json:"read"`
	CPUNanos   uint64    `json:"cpuNanos"`
	OnlineCPUs uint32    `json:"onlineCpus"`
	MemBytes   uint64    `json:"memBytes"`
	MemLimit   uint64    `json:"memLimit"`
	PIDs       uint64    `json:"pids"`
}

type statsBody struct {
	Read     time.Time `json:"read"`
	CPUStats struct {
		CPUUsage struct {
			TotalUsage uint64 `json:"total_usage"`
		} `json:"cpu_usage"`
		OnlineCPUs uint32 `json:"online_cpus"`
	} `json:"cpu_stats"`
	MemoryStats struct {
		Usage uint64            `json:"usage"`
		Limit uint64            `json:"limit"`
		Stats map[string]uint64 `json:"stats"`
	} `json:"memory_stats"`
	PidsStats struct {
		Current uint64 `json:"current"`
	} `json:"pids_stats"`
}

// parseStats subtracts the page cache the way docker stats does, so the
// number matches what the CLI shows.
func parseStats(r io.Reader) (Stats, error) {
	var b statsBody
	if err := json.NewDecoder(io.LimitReader(r, 1<<20)).Decode(&b); err != nil {
		return Stats{}, err
	}
	mem := b.MemoryStats.Usage
	cache := b.MemoryStats.Stats["inactive_file"]
	if cache == 0 {
		cache = b.MemoryStats.Stats["total_inactive_file"]
	}
	if cache < mem {
		mem -= cache
	}
	return Stats{
		Read:       b.Read,
		CPUNanos:   b.CPUStats.CPUUsage.TotalUsage,
		OnlineCPUs: b.CPUStats.OnlineCPUs,
		MemBytes:   mem,
		MemLimit:   b.MemoryStats.Limit,
		PIDs:       b.PidsStats.Current,
	}, nil
}

// SelfStats reads the inspector container's own stats once. Containers and
// allow leave the inspector out on purpose, so it is looked up separately.
func (c *Client) SelfStats(ctx context.Context) (Stats, error) {
	res, err := c.cli.ContainerList(ctx, client.ContainerListOptions{
		Filters: client.Filters{}.Add("label", labelProject+"="+c.env).Add("label", labelService+"="+selfService),
	})
	if err != nil {
		return Stats{}, err
	}
	for _, s := range res.Items {
		if s.Labels[labelProject] != c.env || s.Labels[labelService] != selfService {
			continue
		}
		st, err := c.cli.ContainerStats(ctx, s.ID, client.ContainerStatsOptions{})
		if err != nil {
			return Stats{}, err
		}
		defer st.Body.Close()
		return parseStats(st.Body)
	}
	return Stats{}, errors.New("docker: the inspector container of " + c.env + " is not running")
}

const execLogSize = 4096

// execLog keeps the start times of the most recent execs, for the exec rate
// the Performance page shows.
type execLog struct {
	mu    sync.Mutex
	times [execLogSize]time.Time
	next  int
	n     int
}

func (l *execLog) add(t time.Time) {
	l.mu.Lock()
	l.times[l.next] = t
	l.next = (l.next + 1) % execLogSize
	if l.n < execLogSize {
		l.n++
	}
	l.mu.Unlock()
}

func (l *execLog) since(t time.Time) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	count := 0
	for i := range l.n {
		if !l.times[(l.next-1-i+execLogSize)%execLogSize].Before(t) {
			count++
		}
	}
	return count
}

func (c *Client) ExecsSince(t time.Time) int {
	return c.execs.since(t)
}
