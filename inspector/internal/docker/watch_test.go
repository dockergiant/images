package docker

import (
	"testing"

	"github.com/moby/moby/api/types/events"
)

func TestClassifyEvent(t *testing.T) {
	labels := map[string]map[string]string{
		"traefik-id": {labelProject: "roll", labelService: "traefik"},
		"php-id":     {labelProject: "shop3", labelService: "php-fpm"},
		"other-id":   {labelProject: "shop", labelService: "php-fpm"},
	}
	lookup := func(id string) map[string]string { return labels[id] }
	ctr := func(action, project, service string) events.Message {
		return events.Message{Type: events.ContainerEventType, Action: events.Action(action), Actor: events.Actor{
			ID: "x", Attributes: map[string]string{labelProject: project, labelService: service},
		}}
	}
	netEv := func(action, network, container string) events.Message {
		return events.Message{Type: events.NetworkEventType, Action: events.Action(action), Actor: events.Actor{
			ID: "n", Attributes: map[string]string{"name": network, "container": container},
		}}
	}
	cases := []struct {
		name    string
		msg     events.Message
		svc     string
		ok      bool
		traefik bool
	}{
		{"start", ctr("start", "shop3", "redis"), "redis", true, false},
		{"die", ctr("die", "shop3", "opensearch"), "opensearch", true, false},
		{"health", ctr("health_status: healthy", "shop3", "rabbitmq"), "rabbitmq", true, false},
		{"exec is noise", ctr("exec_start: sh -c echo", "shop3", "varnish"), "", false, false},
		{"exec die is noise", ctr("exec_die", "shop3", "varnish"), "", false, false},
		{"self", ctr("start", "shop3", "inspector"), "", false, false},
		{"other env", ctr("start", "shop2", "redis"), "", false, false},
		{"traefik", ctr("restart", "roll", "traefik"), "traefik", true, true},
		{"mailhog", ctr("start", "roll", "mailhog"), "", false, false},
		{"no service label", ctr("start", "shop3", ""), "", false, false},
		{"traefik joins", netEv("connect", "shop3_default", "traefik-id"), "traefik", true, true},
		{"php joins", netEv("disconnect", "shop3_default", "php-id"), "php-fpm", true, false},
		{"other network", netEv("connect", "shop_default", "traefik-id"), "", false, false},
		{"unknown container", netEv("connect", "shop3_default", "gone"), "", false, false},
		{"network create", netEv("create", "shop3_default", ""), "", false, false},
	}
	for _, tc := range cases {
		svc, ok, tr := classifyEvent("shop3", tc.msg, lookup)
		if svc != tc.svc || ok != tc.ok || tr != tc.traefik {
			t.Errorf("%s: got (%q, %v, %v)", tc.name, svc, ok, tr)
		}
	}
}
