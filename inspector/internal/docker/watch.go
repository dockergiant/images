package docker

import (
	"context"
	"strings"
	"sync"
	"time"

	"github.com/moby/moby/api/types/events"
	"github.com/moby/moby/client"
)

var containerActions = map[string]bool{
	"create": true, "start": true, "restart": true, "stop": true, "die": true, "kill": true,
	"destroy": true, "pause": true, "unpause": true, "rename": true, "update": true,
	"health_status": true, "oom": true,
}

// The daemon ANDs label filters, so the project and Traefik need separate
// subscriptions. Network events carry no labels; the third one catches
// Traefik joining or leaving <env>_default on roll svc up/down.
func (c *Client) Watch(ctx context.Context, onChange func(service string)) {
	subs := []client.EventsListOptions{
		{Filters: client.Filters{}.Add("type", "container").Add("label", labelProject+"="+c.env)},
		{Filters: client.Filters{}.Add("type", "container").Add("label", labelProject+"=roll", labelService+"=traefik")},
		{Filters: client.Filters{}.Add("type", "network").Add("network", c.network())},
	}
	ch := make(chan events.Message, 64)
	var wg sync.WaitGroup
	for _, o := range subs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c.follow(ctx, o, ch)
		}()
	}
	go func() {
		wg.Wait()
		close(ch)
	}()
	for m := range ch {
		if ctx.Err() != nil {
			continue
		}
		svc, ok, traefik := classifyEvent(c.env, m, c.labelsOf(ctx))
		if traefik {
			c.invalidateTraefik()
		}
		if ok {
			onChange(svc)
		}
	}
}

func (c *Client) labelsOf(ctx context.Context) func(string) map[string]string {
	return func(id string) map[string]string {
		res, err := c.cli.ContainerInspect(ctx, id, client.ContainerInspectOptions{})
		if err != nil || res.Container.Config == nil {
			return nil
		}
		return res.Container.Config.Labels
	}
}

func (c *Client) follow(ctx context.Context, opts client.EventsListOptions, out chan<- events.Message) {
	backoff := time.Second
	for ctx.Err() == nil {
		started := time.Now()
		res := c.cli.Events(ctx, opts)
	read:
		for {
			select {
			case <-ctx.Done():
				return
			case <-res.Err:
				break read
			case m := <-res.Messages:
				select {
				case out <- m:
				case <-ctx.Done():
					return
				}
			}
		}
		if time.Since(started) > time.Minute {
			backoff = time.Second
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, 30*time.Second)
	}
}

func classifyEvent(env string, m events.Message, labelsOf func(id string) map[string]string) (service string, ok bool, traefik bool) {
	var labels map[string]string
	switch m.Type {
	case events.ContainerEventType:
		action, _, _ := strings.Cut(string(m.Action), ":")
		if !containerActions[action] {
			return "", false, false
		}
		labels = m.Actor.Attributes
	case events.NetworkEventType:
		if m.Action != events.ActionConnect && m.Action != events.ActionDisconnect {
			return "", false, false
		}
		if m.Actor.Attributes["name"] != env+"_default" || m.Actor.Attributes["container"] == "" {
			return "", false, false
		}
		labels = labelsOf(m.Actor.Attributes["container"])
	default:
		return "", false, false
	}
	project, svc := labels[labelProject], labels[labelService]
	switch {
	case project == "roll" && svc == "traefik":
		return "traefik", true, true
	case project == env && svc != "" && svc != selfService:
		return svc, true, false
	}
	return "", false, false
}
