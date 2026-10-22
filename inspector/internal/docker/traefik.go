package docker

import (
	"context"
	"net"
	"net/netip"
	"time"

	"github.com/moby/moby/api/types/network"
	"github.com/moby/moby/client"
)

const traefikTTL = 10 * time.Second

var traefikPorts = []network.Port{network.MustParsePort("80/tcp"), network.MustParsePort("443/tcp")}

func (c *Client) Traefik(ctx context.Context, refresh bool) (TraefikInfo, error) {
	c.traefikMu.Lock()
	defer c.traefikMu.Unlock()
	if !refresh && c.now().Before(c.traefikExpires) {
		return cloneTraefik(c.traefikInfo), nil
	}
	info, err := c.lookupTraefik(ctx)
	if err != nil {
		return TraefikInfo{LAN: true}, err
	}
	c.traefikInfo = info
	c.traefikExpires = c.now().Add(traefikTTL)
	return cloneTraefik(info), nil
}

func (c *Client) invalidateTraefik() {
	c.traefikMu.Lock()
	c.traefikExpires = time.Time{}
	c.traefikMu.Unlock()
}

func cloneTraefik(t TraefikInfo) TraefikInfo {
	t.Bindings = append([]string(nil), t.Bindings...)
	return t
}

func (c *Client) lookupTraefik(ctx context.Context) (TraefikInfo, error) {
	res, err := c.cli.ContainerList(ctx, client.ContainerListOptions{
		Filters: client.Filters{}.Add("label", labelProject+"=roll", labelService+"=traefik"),
	})
	if err != nil {
		return TraefikInfo{}, err
	}
	if len(res.Items) == 0 {
		return TraefikInfo{LAN: true}, nil
	}
	ins, err := c.cli.ContainerInspect(ctx, res.Items[0].ID, client.ContainerInspectOptions{})
	if err != nil {
		return TraefikInfo{}, err
	}
	info := TraefikInfo{Found: true}
	if ns := ins.Container.NetworkSettings; ns != nil {
		info.IP = endpointIP(ns.Networks[c.network()])
	}
	var hosts []string
	if hc := ins.Container.HostConfig; hc != nil {
		for _, p := range traefikPorts {
			for _, b := range hc.PortBindings[p] {
				host := addrString(b.HostIP)
				hosts = append(hosts, host)
				shown := host
				if shown == "" {
					shown = "*"
				}
				info.Bindings = append(info.Bindings, net.JoinHostPort(shown, b.HostPort))
			}
		}
	}
	info.LAN = lanFromBindings(hosts)
	return info, nil
}

func lanFromBindings(hosts []string) bool {
	for _, h := range hosts {
		if hostIsLAN(h) {
			return true
		}
	}
	return false
}

func hostIsLAN(h string) bool {
	if h == "" {
		return true
	}
	a, err := netip.ParseAddr(h)
	if err != nil {
		return true
	}
	a = a.Unmap()
	return a.IsUnspecified() || !a.IsLoopback()
}
