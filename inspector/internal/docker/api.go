package docker

import (
	"context"
	"io"
	"time"
)

const ExecMarker = "ROLL_INSPECTOR_EXEC=1"

type Container struct {
	ID      string            `json:"id"`
	Name    string            `json:"name"`
	Service string            `json:"service"`
	Project string            `json:"project"`
	Image   string            `json:"image"`
	IP      string            `json:"ip"`
	Labels  map[string]string `json:"-"`
	Running bool              `json:"running"`
	Health  string            `json:"health"`
}

type ExecSpec struct {
	Container  string
	Cmd        []string
	User       string
	Privileged bool
	Env        []string
	Stdin      io.Reader
	// KillOnCancel stops the process in the container when ctx ends first;
	// without it a timed-out exec keeps running there with nobody waiting.
	KillOnCancel bool
}

type ExecResult struct {
	Stdout   []byte
	Stderr   []byte
	ExitCode int
}

type LogLine struct {
	Time   time.Time
	Stream int
	Line   string
}

type TraefikInfo struct {
	Found    bool
	IP       string
	LAN      bool
	Bindings []string
}

type API interface {
	Env() string
	Service(ctx context.Context, service string) (*Container, error)
	Containers(ctx context.Context) ([]Container, error)
	ContainerEnv(ctx context.Context, id string) ([]string, error)
	Exec(ctx context.Context, spec ExecSpec) (ExecResult, error)
	Stream(ctx context.Context, spec ExecSpec, onLine func(string)) error
	Logs(ctx context.Context, id string, since time.Time, follow bool, onLine func(LogLine)) error
	Traefik(ctx context.Context, refresh bool) (TraefikInfo, error)
	Watch(ctx context.Context, onChange func(service string))
	Sweep(ctx context.Context) error
	SelfStats(ctx context.Context) (Stats, error)
	ExecsSince(t time.Time) int
}
