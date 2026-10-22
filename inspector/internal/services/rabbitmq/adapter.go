package rabbitmq

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/dockergiant/images/inspector/internal/core"
	"github.com/dockergiant/images/inspector/internal/docker"
	"github.com/dockergiant/images/inspector/internal/magento"
	"github.com/dockergiant/images/inspector/internal/metrics"
	"github.com/dockergiant/images/inspector/internal/services"
)

var errAbsent = errors.New("no rabbitmq service in this environment")

var testedVersions = []string{"4.1", "4.3"}

type target struct {
	host     string
	base     string
	amqpAddr string
	amqpPort int
	user     string
	pass     string
	vhost    string
}

func (t target) key() string {
	return t.base + "|" + t.user + "|" + t.pass + "|" + t.vhost
}

type adapter struct {
	deps services.Deps

	override *target

	histOnce sync.Once
	history  *metrics.History

	mu     sync.Mutex
	cl     *Client
	clKey  string
	last   sample
	lastAt time.Time
}

func New(d services.Deps) core.Adapter {
	return &adapter{deps: d}
}

func (a *adapter) Kind() core.Kind {
	return core.RabbitMQ
}

func (a *adapter) target(ctx context.Context) (target, error) {
	if a.override != nil {
		return *a.override, nil
	}
	c, err := a.deps.Docker.Service(ctx, "rabbitmq")
	if err != nil {
		return target{}, err
	}
	if c == nil || c.IP == "" {
		return target{}, errAbsent
	}
	t := target{
		host:     c.IP,
		base:     "http://" + net.JoinHostPort(c.IP, "15672"),
		amqpPort: 5672,
		user:     "guest",
		pass:     "guest",
		vhost:    "/",
	}
	if a.deps.Magento != nil {
		if env, err := a.deps.Magento.Load(ctx); err == nil && env != nil && env.AMQP != nil && env.AMQP.User != "" {
			t.user, t.pass = env.AMQP.User, env.AMQP.Password
			if env.AMQP.VHost != "" {
				t.vhost = env.AMQP.VHost
			}
			if env.AMQP.Port != 0 {
				t.amqpPort = env.AMQP.Port
			}
		}
	}
	return t, nil
}

func (a *adapter) clientFor(t target) *Client {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.cl == nil || a.clKey != t.key() {
		a.cl = NewClient(t.base, t.user, t.pass, t.vhost)
		a.clKey = t.key()
	}
	return a.cl
}

func (a *adapter) client(ctx context.Context) (*Client, target, error) {
	t, err := a.target(ctx)
	if err != nil {
		return nil, t, err
	}
	return a.clientFor(t), t, nil
}

func isTested(version string) bool {
	for _, v := range testedVersions {
		if version == v || strings.HasPrefix(version, v+".") {
			return true
		}
	}
	return false
}

func authMessage(err error) string {
	if errors.Is(err, ErrAuth) {
		return "RabbitMQ rejected the credentials from env.php (queue.amqp); no retry with guest"
	}
	return err.Error()
}

func (a *adapter) Detect(ctx context.Context) (core.Info, error) {
	info := core.Info{Kind: core.RabbitMQ, Engine: "RabbitMQ", Features: map[string]bool{}}
	cl, _, err := a.client(ctx)
	if errors.Is(err, errAbsent) {
		return info, nil
	}
	if err != nil {
		info.Error = err.Error()
		return info, nil
	}
	if c, _ := a.deps.Docker.Service(ctx, "rabbitmq"); c != nil {
		info.Container = c.Name
	}
	info.Present = true
	o, err := cl.Overview(ctx)
	if err != nil {
		info.Error = authMessage(err)
		return info, nil
	}
	info.Version = o.Version()
	info.Tested = isTested(info.Version)
	info.Features["management"] = true
	if v, err := cl.VHost(ctx); err == nil {
		info.Features["tracing"] = v.Tracing
		if v.Tracing {
			bound, _ := hasTraceBinding(ctx, cl)
			info.Features["tracingOrphan"] = tracingOrphan(v, bound, a.deps.Capture != nil && a.deps.Capture.Active(firehoseKey))
		}
	}
	return info, nil
}

func hasTraceBinding(ctx context.Context, cl *Client) (bool, error) {
	bs, err := cl.Bindings(ctx)
	if err != nil {
		return false, err
	}
	for _, b := range bs {
		if b.Source == traceExchange {
			return true, nil
		}
	}
	return false, nil
}

func (a *adapter) cronState(ctx context.Context) cronState {
	c, err := a.deps.Docker.Service(ctx, "php-fpm")
	if err != nil || c == nil {
		return cronUnchecked
	}
	res, err := a.deps.Docker.Exec(ctx, docker.ExecSpec{
		Container: c.ID,
		Cmd:       []string{"crontab", "-l"},
		User:      magento.PHPUser,
	})
	if err != nil {
		return cronUnchecked
	}
	if res.ExitCode != 0 {
		if !strings.Contains(string(res.Stderr), "no crontab for") {
			return cronUnchecked
		}
		return cronAbsent
	}
	return cronInstalled
}

func (a *adapter) Health(ctx context.Context) (core.Health, error) {
	cl, _, err := a.client(ctx)
	if err != nil {
		return core.Health{Status: core.ToneInfo, Summary: "No RabbitMQ in this environment"}, nil
	}
	o, err := cl.Overview(ctx)
	if err != nil {
		return core.Health{Status: core.Error, Summary: authMessage(err)}, nil
	}
	qs, err := cl.Queues(ctx)
	if err != nil {
		return core.Health{Status: core.Error, Summary: authMessage(err)}, nil
	}
	now := time.Now()
	in := healthInput{overview: o, queues: qs, now: now}

	cur := sampleOf(o, now)
	a.mu.Lock()
	prev := a.last
	a.last = cur
	a.mu.Unlock()
	in.rates, in.haveRates = computeRates(prev, cur)

	if len(stuckQueues(qs)) > 0 {
		in.cron = a.cronState(ctx)
	}
	if v, err := cl.VHost(ctx); err == nil {
		in.vhost = v
		if v.Tracing {
			in.traceBind, _ = hasTraceBinding(ctx, cl)
		}
	}
	in.capturing = a.deps.Capture != nil && a.deps.Capture.Active(firehoseKey)
	return evaluate(in), nil
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, err error) {
	code := http.StatusBadGateway
	var se *StatusError
	switch {
	case errors.Is(err, errAbsent):
		code = http.StatusNotFound
	case errors.Is(err, ErrAuth):
		code = http.StatusUnauthorized
	case errors.As(err, &se) && se.Code == http.StatusNotFound:
		code = http.StatusNotFound
	}
	writeJSON(w, code, services.ErrorBody(code, authMessage(err)))
}

func (a *adapter) Browse() []core.Route {
	return []core.Route{
		{Method: http.MethodGet, Path: "/metrics", Handler: a.hist().ServeHTTP, Params: []core.Param{core.WindowParam}},
		{Method: http.MethodGet, Path: "/queues", Handler: a.handleQueues},
		{Method: http.MethodGet, Path: "/queue/{name}", Handler: a.handleQueue},
		{Method: http.MethodGet, Path: "/consumers", Handler: a.handleConsumers},
		{Method: http.MethodGet, Path: "/topology", Handler: a.handleTopology},
		{Method: http.MethodGet, Path: "/dbqueues", Handler: a.handleDBQueues},
		{Method: http.MethodPost, Path: "/peek", Handler: a.handlePeek},
	}
}

func (a *adapter) handleQueues(w http.ResponseWriter, r *http.Request) {
	cl, _, err := a.client(r.Context())
	if err != nil {
		writeErr(w, err)
		return
	}
	qs, err := cl.Queues(r.Context())
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"queues": queueRows(qs, time.Now())})
}

func (a *adapter) handleQueue(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	cl, _, err := a.client(r.Context())
	if err != nil {
		writeErr(w, err)
		return
	}
	q, err := cl.Queue(r.Context(), name)
	if err != nil {
		writeErr(w, err)
		return
	}
	out := map[string]any{"queue": q, "consumers": []Consumer{}, "bindings": []Binding{}}
	if cs, err := cl.Consumers(r.Context()); err == nil {
		var mine []Consumer
		for _, c := range cs {
			if c.Queue.Name == name {
				mine = append(mine, c)
			}
		}
		out["consumers"] = mine
	}
	if bs, err := cl.Bindings(r.Context()); err == nil {
		var mine []Binding
		for _, b := range bs {
			if b.Destination == name && b.Source != "" {
				mine = append(mine, b)
			}
		}
		out["bindings"] = mine
	}
	writeJSON(w, http.StatusOK, out)
}

type consumerRow struct {
	Queue      string `json:"queue"`
	Consumer   string `json:"consumer,omitempty"`
	Tag        string `json:"tag"`
	Prefetch   int64  `json:"prefetch"`
	Active     bool   `json:"active"`
	Connection string `json:"connection"`
	PeerHost   string `json:"peerHost"`
	PeerPort   int    `json:"peerPort"`
	User       string `json:"user"`
	PID        int    `json:"pid,omitempty"`
	Role       string `json:"role,omitempty"`
	Cmd        string `json:"cmd,omitempty"`
	Container  string `json:"container,omitempty"`
}

func (a *adapter) handleConsumers(w http.ResponseWriter, r *http.Request) {
	cl, _, err := a.client(r.Context())
	if err != nil {
		writeErr(w, err)
		return
	}
	cs, err := cl.Consumers(r.Context())
	if err != nil {
		writeErr(w, err)
		return
	}
	var mapping map[string]string
	if a.deps.Magento != nil {
		mapping, _ = a.deps.Magento.Consumers(r.Context())
	}
	rows := make([]consumerRow, 0, len(cs))
	for _, c := range cs {
		row := consumerRow{
			Queue:      c.Queue.Name,
			Consumer:   mapping[c.Queue.Name],
			Tag:        c.ConsumerTag,
			Prefetch:   val(c.PrefetchCount),
			Active:     c.Active == nil || *c.Active,
			Connection: c.ChannelDetails.ConnectionName,
			PeerHost:   c.ChannelDetails.PeerHost,
			PeerPort:   c.ChannelDetails.PeerPort,
			User:       c.ChannelDetails.User,
		}
		if a.deps.Proc != nil && row.PeerHost != "" {
			if p, ok := a.deps.Proc.Lookup(row.PeerHost, row.PeerPort); ok {
				row.PID, row.Role, row.Cmd, row.Container = p.PID, p.Role, p.Cmd, p.Container
			} else {
				a.deps.Proc.Miss(row.PeerHost, row.PeerPort)
			}
		}
		rows = append(rows, row)
	}
	writeJSON(w, http.StatusOK, map[string]any{"consumers": rows})
}

func (a *adapter) handleTopology(w http.ResponseWriter, r *http.Request) {
	cl, _, err := a.client(r.Context())
	if err != nil {
		writeErr(w, err)
		return
	}
	ex, err := cl.Exchanges(r.Context())
	if err != nil {
		writeErr(w, err)
		return
	}
	bs, err := cl.Bindings(r.Context())
	if err != nil {
		writeErr(w, err)
		return
	}
	exchanges := make([]Exchange, 0, len(ex))
	for _, e := range ex {
		if e.Name != "" {
			exchanges = append(exchanges, e)
		}
	}
	bindings := make([]Binding, 0, len(bs))
	for _, b := range bs {
		if b.Source != "" {
			bindings = append(bindings, b)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"exchanges": exchanges, "bindings": bindings})
}

func (a *adapter) handleDBQueues(w http.ResponseWriter, r *http.Request) {
	db, err := a.deps.Docker.Service(r.Context(), "db")
	if err != nil {
		writeErr(w, err)
		return
	}
	if db == nil {
		writeJSON(w, http.StatusNotFound, services.ErrorBody(http.StatusNotFound, "no db service in this environment"))
		return
	}
	env, err := a.deps.Docker.ContainerEnv(r.Context(), db.ID)
	if err != nil {
		writeErr(w, err)
		return
	}
	var dbName, prefix string
	if a.deps.Magento != nil {
		if m, err := a.deps.Magento.Load(r.Context()); err == nil && m != nil {
			dbName, prefix = m.DBName, m.TablePrefix
		}
	}
	cfg, err := dbConfig(env, dbName)
	if err != nil {
		writeErr(w, err)
		return
	}
	qs, err := readDBQueues(r.Context(), cfg, prefix)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, services.ErrorBody(http.StatusBadGateway, "reading db-backed queues: "+err.Error()))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"queues": qs})
}

func (a *adapter) handlePeek(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Queue string `json:"queue"`
		Count int    `json:"count"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&req); err != nil || req.Queue == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "queue is required"})
		return
	}
	req.Count = min(max(req.Count, 1), 20)
	cl, _, err := a.client(r.Context())
	if err != nil {
		writeErr(w, err)
		return
	}
	q, err := cl.Queue(r.Context(), req.Queue)
	if err != nil {
		writeErr(w, err)
		return
	}
	if q.Type == "quorum" {
		writeJSON(w, http.StatusConflict, map[string]string{"error": fmt.Sprintf("%s is a quorum queue: each peek requeues and raises x-delivery-count, and RabbitMQ drops or dead-letters a message once it passes the delivery limit (default 20)", req.Queue)})
		return
	}
	if q.ConsumerCount() > 0 {
		writeJSON(w, http.StatusConflict, map[string]string{"error": fmt.Sprintf("%s has %d consumer(s) attached; peeking would race them and sets the redelivered flag", req.Queue, q.ConsumerCount())})
		return
	}
	msgs, err := cl.Peek(r.Context(), req.Queue, req.Count)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"write":    true,
		"note":     "Peek is a write: messages are requeued with the redelivered flag set",
		"messages": msgs,
	})
}

func (a *adapter) Streams() []core.StreamSpec {
	return []core.StreamSpec{
		{Name: "rates", Title: "Publish rates", Start: a.rates},
		{Name: "firehose", Title: "Publish firehose", OptIn: true, Start: a.firehose},
	}
}

func (a *adapter) rates(ctx context.Context, _ core.Filter, emit core.Emit) error {
	cl, _, err := a.client(ctx)
	if err != nil {
		return err
	}
	var prev publishCounts
	tick := time.NewTicker(2 * time.Second)
	defer tick.Stop()
	for {
		qs, err := cl.Queues(ctx)
		if err == nil {
			cur := queuePublishCounts(qs)
			if prev != nil {
				now := time.Now()
				for name, n := range publishDeltas(prev, cur) {
					pub := core.MQPub{
						Time:       now,
						RoutingKey: name,
						Queues:     []string{name},
						Source:     "delta",
						Count:      int(n),
					}
					emit(core.Event{Type: "mq.pub", Time: now, Kind: core.RabbitMQ, Data: pub})
				}
			}
			prev = cur
		} else if errors.Is(err, ErrAuth) {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-tick.C:
		}
	}
}

func (a *adapter) Actions() []core.ActionSpec {
	return []core.ActionSpec{
		{
			Name:        "purge-queue",
			Title:       "Purge queue",
			Destructive: true,
			Confirm:     core.ConfirmTyped,
			Run:         a.purge,
		},
		{
			Name:    "tracing-off",
			Title:   "Switch tracing off",
			Confirm: core.ConfirmPlain,
			Run:     a.tracingOff,
		},
	}
}

func (a *adapter) purge(ctx context.Context, req core.ActionRequest) (core.ActionResult, error) {
	if req.Target == "" {
		return core.ActionResult{}, errors.New("queue name required")
	}
	cl, _, err := a.client(ctx)
	if err != nil {
		return core.ActionResult{}, err
	}
	q, err := cl.Queue(ctx, req.Target)
	if err != nil {
		return core.ActionResult{}, err
	}
	n := q.ReadyCount()
	if err := cl.Purge(ctx, req.Target); err != nil {
		return core.ActionResult{}, err
	}
	return core.ActionResult{
		Message: "Purged " + strconv.FormatInt(n, 10) + " ready messages from " + req.Target + ". Magento does not republish them, so the work they described is lost.",
	}, nil
}

func (a *adapter) tracingOff(ctx context.Context, _ core.ActionRequest) (core.ActionResult, error) {
	cl, _, err := a.client(ctx)
	if err != nil {
		return core.ActionResult{}, err
	}
	if err := cl.SetTracing(ctx, false); err != nil {
		return core.ActionResult{}, err
	}
	return core.ActionResult{Message: "Tracing switched off"}, nil
}
