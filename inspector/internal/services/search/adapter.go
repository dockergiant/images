package search

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/dockergiant/images/inspector/internal/core"
	"github.com/dockergiant/images/inspector/internal/docker"
	"github.com/dockergiant/images/inspector/internal/metrics"
	"github.com/dockergiant/images/inspector/internal/services"
)

const (
	probeTTL       = 30 * time.Second
	restoreTimeout = 20 * time.Second
	seenCap        = 4096
)

var errNotRunning = errors.New("no opensearch or elasticsearch service is running: set ROLL_OPENSEARCH=1 in .env.roll and run roll env up")

type sample struct {
	at      time.Time
	queries int64
	millis  int64
}

type adapter struct {
	deps        services.Deps
	log         *slog.Logger
	http        *http.Client
	baseURL     func(*docker.Container) string
	now         func() time.Time
	pollEvery   time.Duration
	followEvery time.Duration
	stores      func(ctx context.Context) storeSet
	history     *metrics.History

	mu       sync.Mutex
	flavor   Flavor
	insights bool
	probed   time.Time
	prev     sample

	// captureMu orders session setup, session teardown and the reconcile in
	// Detect; captureGen names the session that owns the markers.
	captureMu  sync.Mutex
	captureGen uint64
}

func New(d services.Deps) core.Adapter {
	return newAdapter(d)
}

func newAdapter(d services.Deps) *adapter {
	a := &adapter{
		deps:        d,
		log:         d.Log,
		http:        newHTTPClient(),
		now:         time.Now,
		pollEvery:   2 * time.Second,
		followEvery: 10 * time.Second,
		baseURL:     func(c *docker.Container) string { return "http://" + c.Service + ":9200" },
	}
	if a.log == nil {
		a.log = slog.New(slog.DiscardHandler)
	}
	loader := &storeLoader{docker: d.Docker, magento: d.Magento}
	a.stores = loader.load
	a.history = metrics.NewHistory(metricSpec).WithClock(func() time.Time { return a.now() })
	return a
}

func (a *adapter) Kind() core.Kind {
	return core.Search
}

func (a *adapter) container(ctx context.Context) (*docker.Container, error) {
	if a.deps.Docker == nil {
		return nil, nil
	}
	for _, svc := range []string{"opensearch", "elasticsearch"} {
		ct, err := a.deps.Docker.Service(ctx, svc)
		if err != nil || ct != nil {
			return ct, err
		}
	}
	return nil, nil
}

func (a *adapter) client(ctx context.Context) (*client, *docker.Container, error) {
	ct, err := a.container(ctx)
	if err != nil {
		return nil, nil, err
	}
	if ct == nil {
		return nil, nil, errNotRunning
	}
	return &client{base: a.baseURL(ct), http: a.http}, ct, nil
}

func (a *adapter) probe(ctx context.Context, c *client, force bool) (Flavor, bool, error) {
	a.mu.Lock()
	if !force && !a.probed.IsZero() && time.Since(a.probed) < probeTTL {
		f, ins := a.flavor, a.insights
		a.mu.Unlock()
		return f, ins, nil
	}
	a.mu.Unlock()
	body, h, err := c.do(ctx, http.MethodGet, "/", nil)
	if err != nil {
		return Flavor{}, false, err
	}
	f, err := DetectFlavor(body, h)
	if err != nil {
		return Flavor{}, false, err
	}
	insights := false
	if f.Engine == EngineOpenSearch {
		if plugins, _, err := c.do(ctx, http.MethodGet, "/_cat/plugins?format=json", nil); err == nil {
			insights, _ = HasInsights(plugins)
		}
	}
	a.mu.Lock()
	a.flavor, a.insights, a.probed = f, insights, time.Now()
	a.mu.Unlock()
	return f, insights, nil
}

func (a *adapter) captureActive() bool {
	return a.deps.Capture != nil && a.deps.Capture.Active(captureKey)
}

func (a *adapter) Detect(ctx context.Context) (core.Info, error) {
	info := core.Info{Kind: core.Search, Features: map[string]bool{}}
	c, ct, err := a.client(ctx)
	if errors.Is(err, errNotRunning) {
		return info, nil
	}
	if err != nil {
		return info, err
	}
	info.Present, info.Container = true, ct.Name
	f, insights, err := a.probe(ctx, c, true)
	if err != nil {
		info.Error = err.Error()
		return info, nil
	}
	info.Engine, info.Version, info.Tested = f.Engine, f.Version, f.Tested()
	info.Features = map[string]bool{
		"insights":     insights,
		"slowlog":      true,
		"capture":      true,
		"analyze":      true,
		"profile":      true,
		"deleteOrphan": true,
	}
	a.captureMu.Lock()
	defer a.captureMu.Unlock()
	if !a.captureActive() {
		n, err := reconcile(ctx, c)
		if err != nil {
			a.log.Warn("restoring slowlog thresholds of an earlier capture failed", "err", err)
			info.Error = "restoring the slowlog thresholds of an earlier capture failed: " + err.Error()
		} else if n > 0 {
			a.log.Info("restored slowlog thresholds left by an earlier capture", "indices", n)
		}
	}
	return info, nil
}

func (a *adapter) labelFor(ctx context.Context, index string) string {
	first, _, _ := strings.Cut(index, ",")
	st := a.stores(ctx)
	if !st.Magento {
		return ""
	}
	if l, ok := MagentoName(first, st.Stores); ok {
		return l.Title
	}
	return ""
}

var matchFilters = []string{"index", "q"}

func matches(f core.Filter, q core.SearchQuery) bool {
	if v := strings.ToLower(f["index"]); v != "" &&
		!strings.Contains(strings.ToLower(q.Index), v) && !strings.Contains(strings.ToLower(q.Label), v) {
		return false
	}
	if v := strings.ToLower(f["q"]); v != "" &&
		!strings.Contains(strings.ToLower(q.Body), v) && !strings.Contains(strings.ToLower(q.Summary), v) {
		return false
	}
	return true
}

func searchEvent(q core.SearchQuery) core.Event {
	return core.Event{Type: "search.query", Time: q.Time, Kind: core.Search, Data: q}
}

type seenSet struct {
	keys  map[string]bool
	order []string
}

func (s *seenSet) add(k string) bool {
	if s.keys == nil {
		s.keys = map[string]bool{}
	}
	if s.keys[k] {
		return false
	}
	s.keys[k] = true
	s.order = append(s.order, k)
	if len(s.order) > seenCap {
		delete(s.keys, s.order[0])
		s.order = s.order[1:]
	}
	return true
}

func (a *adapter) Streams() []core.StreamSpec {
	return []core.StreamSpec{
		{Name: "insights", Title: "Top queries (Query Insights, top 10 per 5 minutes)", Filters: matchFilters, Start: a.runInsights},
		{Name: "slowlog", Title: "Slowlog tail", Filters: matchFilters, Start: a.runSlowlog},
		{Name: "capture", Title: "Capture all queries (slowlog at 0 ms)", OptIn: true, Filters: matchFilters, Start: a.runCapture},
	}
}

func (a *adapter) runInsights(ctx context.Context, f core.Filter, emit core.Emit) error {
	c, _, err := a.client(ctx)
	if err != nil {
		return err
	}
	var seen seenSet
	t := time.NewTicker(a.pollEvery)
	defer t.Stop()
	for {
		data, _, err := c.do(ctx, http.MethodGet, "/_insights/top_queries?type=latency", nil)
		switch {
		case isStatus(err, http.StatusBadRequest, http.StatusNotFound, http.StatusMethodNotAllowed):
			emit(core.Event{Type: "stream.error", Time: a.now(), Kind: core.Search,
				Data: "Query Insights needs OpenSearch 2.12 or later; use the slowlog capture for a live query lane"})
			<-ctx.Done()
			return ctx.Err()
		case err != nil:
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return err
		}
		recs, err := ParseInsights(data)
		if err != nil {
			return err
		}
		for _, r := range recs {
			if !seen.add(r.key()) || r.opaqueID() == opaqueID || r.systemOnly() {
				continue
			}
			q := r.query()
			q.Label = a.labelFor(ctx, q.Index)
			if matches(f, q) {
				emit(searchEvent(q))
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
		}
	}
}

func (a *adapter) runSlowlog(ctx context.Context, f core.Filter, emit core.Emit) error {
	_, ct, err := a.client(ctx)
	if err != nil {
		return err
	}
	return a.tail(ctx, ct, f, emit)
}

func (a *adapter) tail(ctx context.Context, ct *docker.Container, f core.Filter, emit core.Emit) error {
	err := a.deps.Docker.Logs(ctx, ct.ID, a.now(), true, func(l docker.LogLine) {
		t := l.Time
		if t.IsZero() {
			t = a.now()
		}
		q, ok := ParseSlowlog(l.Line, t)
		if !ok || q.OpaqueID == opaqueID || isSystemIndex(q.Index) {
			return
		}
		q.Label = a.labelFor(ctx, q.Index)
		if matches(f, q) {
			emit(searchEvent(q))
		}
	})
	if ctx.Err() != nil {
		return ctx.Err()
	}
	// The hub only restarts a stream that fails, and a log that ends means
	// the search container restarted, so a clean end is reported as an error.
	if err == nil {
		err = errors.New("log stream ended")
	}
	return fmt.Errorf("slowlog: %w", err)
}

type captureState struct {
	Capturing bool      `json:"capturing"`
	Indices   []string  `json:"indices"`
	Started   time.Time `json:"started"`
	Until     time.Time `json:"until"`
	Reason    string    `json:"reason,omitempty"`
}

func (a *adapter) runCapture(parent context.Context, f core.Filter, emit core.Emit) error {
	if a.deps.Capture == nil {
		return errors.New("capture sessions are not available")
	}
	c, ct, err := a.client(parent)
	if err != nil {
		return err
	}
	sess := newCaptureSession(c, a.now)
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	var gen uint64
	restore := func(rctx context.Context) error {
		cancel()
		a.captureMu.Lock()
		defer a.captureMu.Unlock()
		if a.captureGen != gen {
			// A newer session reused these markers; it restores the original values.
			return nil
		}
		return sess.restore(rctx)
	}
	if err := a.deps.Capture.Begin(captureKey, captureMax, restore); err != nil {
		return err
	}
	end := func() {
		rctx, rcancel := context.WithTimeout(context.Background(), restoreTimeout)
		defer rcancel()
		if err := a.deps.Capture.End(rctx, captureKey); err != nil {
			a.log.Error("restoring slowlog thresholds failed", "err", err)
		}
	}
	defer end()
	a.captureMu.Lock()
	a.captureGen++
	gen = a.captureGen
	err = sess.begin(ctx)
	a.captureMu.Unlock()
	if err != nil {
		end()
		if !errors.Is(err, errNothingToCapture) {
			return err
		}
		emit(core.Event{Type: "stream.error", Time: a.now(), Kind: core.Search, Data: err.Error()})
		<-parent.Done()
		return parent.Err()
	}
	state := func(on bool, reason string) core.Event {
		return core.Event{Type: "search.capture", Time: a.now(), Kind: core.Search, Data: captureState{
			Capturing: on, Indices: sess.indices(), Started: sess.started, Until: sess.started.Add(captureMax), Reason: reason,
		}}
	}
	emit(state(true, ""))

	errc := make(chan error, 1)
	go func() { errc <- a.tail(ctx, ct, f, emit) }()
	t := time.NewTicker(a.followEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			<-errc
			if parent.Err() != nil {
				return parent.Err()
			}
			emit(state(false, "capture stopped after 30 minutes; thresholds restored"))
			<-parent.Done()
			return parent.Err()
		case err := <-errc:
			if ctx.Err() == nil {
				return err
			}
			errc <- err
		case <-t.C:
			before := len(sess.indices())
			if err := sess.sync(ctx); err != nil && ctx.Err() == nil {
				a.log.Warn("following alias moves during capture failed", "err", err)
			}
			if len(sess.indices()) != before {
				emit(state(true, ""))
			}
		}
	}
}

func (a *adapter) Actions() []core.ActionSpec {
	return []core.ActionSpec{{
		Name:        "delete-orphan",
		Title:       "Delete orphan index",
		Destructive: true,
		Confirm:     core.ConfirmTyped,
		Run:         a.deleteOrphan,
	}}
}

func (a *adapter) deleteOrphan(ctx context.Context, req core.ActionRequest) (core.ActionResult, error) {
	name := req.Target
	if name == "" {
		return core.ActionResult{}, errors.New("no index given")
	}
	c, _, err := a.client(ctx)
	if err != nil {
		return core.ActionResult{}, err
	}
	ix, al, err := c.catalog(ctx)
	if err != nil {
		return core.ActionResult{}, err
	}
	st := a.stores(ctx)
	if !st.Magento {
		return core.ActionResult{}, errors.New("orphan detection needs a Magento project")
	}
	if len(st.Stores) == 0 {
		return core.ActionResult{}, fmt.Errorf("the store list is unavailable, so %s cannot be proven an orphan", name)
	}
	for _, v := range BuildIndexViews(ix, al, st.Stores, true, a.now()) {
		if v.Index != name {
			continue
		}
		if !v.Orphan {
			return core.ActionResult{}, fmt.Errorf("%s is not an orphan index: only indices without an alias, older than 10 minutes, whose store is gone can be deleted", name)
		}
		if _, _, err := c.do(ctx, http.MethodDelete, indexPath(name), nil); err != nil {
			return core.ActionResult{}, err
		}
		return core.ActionResult{Message: fmt.Sprintf("Deleted orphan index %s (%s)", name, v.Label.Title), Data: v}, nil
	}
	return core.ActionResult{}, fmt.Errorf("no index named %s", name)
}
