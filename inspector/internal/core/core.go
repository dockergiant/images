package core

import (
	"context"
	"errors"
	"net/http"
	"time"
)

type Kind string

const (
	Varnish  Kind = "varnish"
	Redis    Kind = "redis"
	Search   Kind = "search"
	RabbitMQ Kind = "rabbitmq"
	PHP      Kind = "php"
)

type Tone string

const (
	OK       Tone = "ok"
	Warn     Tone = "warn"
	Error    Tone = "error"
	ToneInfo Tone = "info"
)

type Info struct {
	Kind      Kind            `json:"kind"`
	Present   bool            `json:"present"`
	Engine    string          `json:"engine"`
	Version   string          `json:"version"`
	Tested    bool            `json:"tested"`
	Features  map[string]bool `json:"features"`
	Container string          `json:"container"`
	Error     string          `json:"error,omitempty"`
}

type Card struct {
	Label string `json:"label"`
	Value string `json:"value"`
	Sub   string `json:"sub"`
	Tone  Tone   `json:"tone"`
}

type Warning struct {
	ID   string `json:"id"`
	Text string `json:"text"`
	Fix  string `json:"fix,omitempty"`
	Tone Tone   `json:"tone"`
}

type Health struct {
	Status   Tone      `json:"status"`
	Summary  string    `json:"summary"`
	Cards    []Card    `json:"cards"`
	Warnings []Warning `json:"warnings"`
	Raw      any       `json:"raw,omitempty"`
}

type Route struct {
	Method  string
	Path    string
	Handler http.HandlerFunc
	// Params lists the query parameters the handler reads, for the v1
	// OpenAPI document.
	Params []Param
}

type Param struct {
	Name        string
	Type        string
	Required    bool
	Description string
}

// WindowParam is the ?window= every /metrics route reads.
var WindowParam = Param{Name: "window", Type: "string", Description: "5m, 15m or 60m; default 15m."}

type Event struct {
	Type string    `json:"type"`
	Time time.Time `json:"time"`
	Kind Kind      `json:"kind"`
	Data any       `json:"data"`
}

type Emit func(Event)

type Filter map[string]string

type StreamSpec struct {
	Name  string
	Title string
	OptIn bool
	// Filters are the only filter keys Start reads. Any other key is
	// refused, because each distinct filter starts its own producer.
	Filters []string
	Start   func(ctx context.Context, f Filter, emit Emit) error
}

type ConfirmLevel string

const (
	ConfirmNone  ConfirmLevel = "none"
	ConfirmPlain ConfirmLevel = "plain"
	ConfirmTyped ConfirmLevel = "typed"
)

type ActionRequest struct {
	Target string            `json:"target"`
	Params map[string]string `json:"params"`
}

type ActionResult struct {
	Message string `json:"message"`
	Data    any    `json:"data,omitempty"`
}

type ActionSpec struct {
	Name        string
	Title       string
	Destructive bool
	Confirm     ConfirmLevel
	Run         func(ctx context.Context, req ActionRequest) (ActionResult, error)
}

type Adapter interface {
	Kind() Kind
	Detect(ctx context.Context) (Info, error)
	Health(ctx context.Context) (Health, error)
	Browse() []Route
	Streams() []StreamSpec
	Actions() []ActionSpec
}

// ErrAbsent means the service is not part of this env; a sampler skips it
// quietly instead of logging an error every step.
var ErrAbsent = errors.New("service is not part of this env")

// MetricsSource is optional on an adapter. Metrics reads the service once,
// records the reading in the adapter's history and returns it.
type MetricsSource interface {
	Metrics(ctx context.Context) (map[string]float64, error)
}
