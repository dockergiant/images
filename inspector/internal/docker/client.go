package docker

import (
	"context"
	"errors"
	"log/slog"
	"net/netip"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/network"
	"github.com/moby/moby/client"
)

const (
	labelProject = "com.docker.compose.project"
	labelService = "com.docker.compose.service"
	selfService  = "inspector"
)

type Client struct {
	env string
	cli *client.Client
	log *slog.Logger

	execs execLog

	allowMu sync.Mutex
	allowed map[string]bool

	traefikMu      sync.Mutex
	traefikInfo    TraefikInfo
	traefikExpires time.Time
	now            func() time.Time
}

var _ API = (*Client)(nil)

func New(env string) (*Client, error) {
	return newClient(env, client.FromEnv)
}

func newClient(env string, opts ...client.Opt) (*Client, error) {
	if env == "" {
		return nil, errors.New("docker: empty env name")
	}
	cli, err := client.New(opts...)
	if err != nil {
		return nil, err
	}
	return &Client{env: env, cli: cli, log: slog.Default(), allowed: map[string]bool{}, now: time.Now}, nil
}

func (c *Client) SetLogger(log *slog.Logger) {
	c.log = log
}

func (c *Client) Close() error {
	return c.cli.Close()
}

func (c *Client) Env() string {
	return c.env
}

func (c *Client) network() string {
	return c.env + "_default"
}

func (c *Client) Containers(ctx context.Context) ([]Container, error) {
	res, err := c.cli.ContainerList(ctx, client.ContainerListOptions{
		All:     true,
		Filters: client.Filters{}.Add("label", labelProject+"="+c.env),
	})
	if err != nil {
		return nil, err
	}
	out := make([]Container, 0, len(res.Items))
	for _, s := range res.Items {
		if s.Labels[labelProject] != c.env || s.Labels[labelService] == selfService {
			continue
		}
		out = append(out, c.fromSummary(s))
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Service != out[j].Service {
			return out[i].Service < out[j].Service
		}
		return out[i].Name < out[j].Name
	})
	return out, nil
}

func (c *Client) fromSummary(s container.Summary) Container {
	ct := Container{
		ID:      s.ID,
		Service: s.Labels[labelService],
		Project: s.Labels[labelProject],
		Image:   s.Image,
		Labels:  s.Labels,
		Running: s.State == container.StateRunning,
	}
	if len(s.Names) > 0 {
		ct.Name = strings.TrimPrefix(s.Names[0], "/")
	}
	if s.Health != nil && s.Health.Status != container.NoHealthcheck {
		ct.Health = string(s.Health.Status)
	}
	if s.NetworkSettings != nil {
		ct.IP = endpointIP(s.NetworkSettings.Networks[c.network()])
	}
	return ct
}

func endpointIP(ep *network.EndpointSettings) string {
	if ep == nil {
		return ""
	}
	return addrString(ep.IPAddress)
}

func addrString(a netip.Addr) string {
	if !a.IsValid() {
		return ""
	}
	return a.String()
}

func (c *Client) Service(ctx context.Context, service string) (*Container, error) {
	list, err := c.Containers(ctx)
	if err != nil {
		return nil, err
	}
	for i := range list {
		if list[i].Service == service && list[i].Running {
			return &list[i], nil
		}
	}
	return nil, nil
}

func (c *Client) ContainerEnv(ctx context.Context, id string) ([]string, error) {
	if err := c.allow(ctx, id); err != nil {
		return nil, err
	}
	res, err := c.cli.ContainerInspect(ctx, id, client.ContainerInspectOptions{})
	if err != nil {
		return nil, err
	}
	if res.Container.Config == nil {
		return nil, nil
	}
	return append([]string(nil), res.Container.Config.Env...), nil
}
