package api

import (
	"context"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/dockergiant/images/inspector/internal/apm"
	"github.com/dockergiant/images/inspector/internal/core"
	"github.com/dockergiant/images/inspector/internal/magento"
	"github.com/dockergiant/images/inspector/internal/perf"
)

const (
	defaultTimeline = 100
	maxTimeline     = 500
	defaultTraces   = 100
	defaultHotspots = 100
	maxHotspots     = 500
)

func (s *Server) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/session", s.handleSession)
	mux.HandleFunc("GET /api/overview", s.handleOverview)
	mux.HandleFunc("GET /api/timeline", s.handleTimeline)
	mux.HandleFunc("GET /api/timeline/{key}", s.handleTimelineGet)
	mux.HandleFunc("GET /api/traces", s.handleTraces)
	// The literal segment outranks {id}, so a trace can never shadow this route.
	mux.HandleFunc("GET /api/traces/hotspots", s.handleHotspots)
	mux.HandleFunc("GET /api/traces/metrics", s.handleTraceMetrics)
	mux.HandleFunc("GET /api/traces/{id}", s.handleTrace)
	mux.HandleFunc("GET /api/traces/{id}/analysis", s.handleAnalysis)
	mux.HandleFunc("GET /api/traces/{id}/profile", s.handleProfile)
	mux.HandleFunc("POST /api/confirm", s.handleConfirm)
	mux.HandleFunc("POST /api/actions/{kind}/{name}", s.handleAction)
	mux.HandleFunc("GET /api/performance", s.handlePerformance)
	mux.HandleFunc("POST /api/performance/settings", s.handlePerfSettings)
	mux.HandleFunc("POST /api/performance/benchmark", s.handleBenchStart)
	mux.HandleFunc("POST /api/performance/benchmark/cancel", s.handleBenchCancel)
	mux.HandleFunc("DELETE /api/performance/benchmark", s.handleBenchCancel)
	mux.HandleFunc("POST /api/performance/live/stop", s.handleLiveStop)
	for _, a := range s.cfg.Adapters {
		prefix := "/api/" + string(a.Kind())
		mux.HandleFunc("GET "+prefix+"/info", s.handleInfo(a))
		mux.HandleFunc("GET "+prefix+"/health", s.handleHealth(a))
		for _, rt := range a.Browse() {
			mux.HandleFunc(rt.Method+" "+prefix+rt.Path, rt.Handler)
		}
	}
	s.mountV1(mux, s.v1Table())
	mux.HandleFunc("GET "+userscriptPath, s.handleUserscript)
	mux.HandleFunc("GET "+extensionPath, s.handleExtension)
	notFound := func(w http.ResponseWriter, r *http.Request) {
		writeError(w, http.StatusNotFound, "no such API route: "+r.Method+" "+r.URL.Path)
	}
	mux.HandleFunc("GET /api/", notFound)
	mux.HandleFunc("POST /api/", notFound)
	if s.cfg.UI != nil {
		mux.Handle("GET /", http.FileServerFS(s.cfg.UI))
	}
	// The stream sits outside the mux: a GET pattern also takes HEAD, and the
	// POST catch-all would answer other methods with 404 instead of 405.
	return noStoreAPI(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/stream":
			s.serveStream(w, r, false)
			return
		case v1Prefix + "/stream":
			s.serveStream(w, r, true)
			return
		}
		mux.ServeHTTP(w, r)
	}))
}

func noStoreAPI(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if len(r.URL.Path) >= 5 && r.URL.Path[:5] == "/api/" {
			w.Header().Set("Cache-Control", "no-store")
		} else {
			// The embedded files carry no modification time, so without this a
			// browser keeps the old UI after the image is updated.
			w.Header().Set("Cache-Control", "no-cache")
		}
		next.ServeHTTP(w, r)
	})
}

type streamInfo struct {
	Name  string `json:"name"`
	Title string `json:"title"`
	OptIn bool   `json:"optIn"`
}

type actionInfo struct {
	Name        string            `json:"name"`
	Title       string            `json:"title"`
	Destructive bool              `json:"destructive"`
	Confirm     core.ConfirmLevel `json:"confirm"`
}

type adapterSession struct {
	Kind    core.Kind    `json:"kind"`
	Info    core.Info    `json:"info"`
	Streams []streamInfo `json:"streams"`
	Actions []actionInfo `json:"actions"`
}

type session struct {
	CSRF    string `json:"csrf"`
	Env     string `json:"env"`
	Domain  string `json:"domain"`
	Version string `json:"version"`
	// UIHash changes with any file of the embedded UI, so an open page can
	// tell that the image under it was updated.
	UIHash    string           `json:"uiHash"`
	LAN       bool             `json:"lan"`
	LANReason string           `json:"lanReason,omitempty"`
	Adapters  []adapterSession `json:"adapters"`
	Captures  []string         `json:"captures"`
	// Performance feeds the top-bar pill from the page's state cache, read at
	// most once a minute for the pill alone.
	Performance *perf.Summary `json:"performance,omitempty"`
}

func (s *Server) handleSession(w http.ResponseWriter, r *http.Request) {
	infos := s.detectAll(r.Context(), r.URL.Query().Get("refresh") == "1")
	out := session{
		CSRF:     s.csrf,
		Env:      s.cfg.Env,
		Domain:   s.cfg.Domain,
		Version:  s.cfg.Version,
		UIHash:   s.uiHash(),
		Adapters: make([]adapterSession, 0, len(s.cfg.Adapters)),
		Captures: []string{},
	}
	out.LAN, out.LANReason = s.lan(r.Context())
	if s.cfg.Capture != nil {
		out.Captures = s.cfg.Capture.Keys()
	}
	if s.cfg.Perf != nil {
		sum := s.cfg.Perf.Controller().Summary(r.Context())
		out.Performance = &sum
	}
	for i, a := range s.cfg.Adapters {
		as := adapterSession{Kind: a.Kind(), Info: infos[i], Streams: []streamInfo{}, Actions: []actionInfo{}}
		for _, st := range a.Streams() {
			as.Streams = append(as.Streams, streamInfo{Name: st.Name, Title: st.Title, OptIn: st.OptIn})
		}
		for _, ac := range a.Actions() {
			confirm := ac.Confirm
			if confirm == "" {
				confirm = core.ConfirmNone
			}
			if ac.Destructive && confirm == core.ConfirmNone {
				confirm = core.ConfirmPlain
			}
			as.Actions = append(as.Actions, actionInfo{Name: ac.Name, Title: ac.Title, Destructive: ac.Destructive, Confirm: confirm})
		}
		out.Adapters = append(out.Adapters, as)
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) detectAll(ctx context.Context, refresh bool) []core.Info {
	out := make([]core.Info, len(s.cfg.Adapters))
	var wg sync.WaitGroup
	for i, a := range s.cfg.Adapters {
		wg.Add(1)
		go func() {
			defer wg.Done()
			out[i] = s.info(ctx, a, refresh)
		}()
	}
	wg.Wait()
	return out
}

func (s *Server) health(ctx context.Context, a core.Adapter) core.Health {
	ctx, cancel := context.WithTimeout(ctx, readTimeout)
	defer cancel()
	h, err := a.Health(ctx)
	if err != nil {
		return core.Health{Status: core.Error, Summary: err.Error(), Cards: []core.Card{}, Warnings: []core.Warning{}}
	}
	return h
}

type overviewAdapter struct {
	Kind   core.Kind    `json:"kind"`
	Info   core.Info    `json:"info"`
	Health *core.Health `json:"health,omitempty"`
}

type overview struct {
	Adapters     []overviewAdapter `json:"adapters"`
	Magento      any               `json:"magento"`
	MagentoError string            `json:"magentoError,omitempty"`
	Warnings     []core.Warning    `json:"warnings"`
	Captures     []string          `json:"captures"`
}

func (s *Server) handleOverview(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	infos := s.detectAll(ctx, false)
	out := overview{Adapters: make([]overviewAdapter, len(s.cfg.Adapters)), Captures: []string{}}
	var env *magento.Env
	var wg sync.WaitGroup
	for i, a := range s.cfg.Adapters {
		out.Adapters[i] = overviewAdapter{Kind: a.Kind(), Info: infos[i]}
		if !infos[i].Present {
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			h := s.health(ctx, a)
			out.Adapters[i].Health = &h
		}()
	}
	if s.cfg.Magento != nil {
		wg.Add(1)
		go func() {
			defer wg.Done()
			lctx, cancel := context.WithTimeout(ctx, readTimeout)
			defer cancel()
			e, err := s.cfg.Magento.Load(lctx)
			if err != nil {
				out.MagentoError = err.Error()
				return
			}
			env = e
		}()
	}
	wg.Wait()

	present := map[core.Kind]bool{}
	for _, info := range infos {
		present[info.Kind] = info.Present
	}
	if env != nil {
		out.Magento = magento.Redact(env)
	}
	out.Warnings = magento.Warnings(env, present, s.hasRequestSource(present))
	if out.Warnings == nil {
		out.Warnings = []core.Warning{}
	}
	if s.cfg.Capture != nil {
		out.Captures = s.cfg.Capture.Keys()
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) hasRequestSource(present map[core.Kind]bool) bool {
	if present[core.Varnish] {
		return true
	}
	if s.cfg.Store != nil && len(s.cfg.Store.List(apm.ListFilter{Limit: 1})) > 0 {
		return true
	}
	return s.cfg.Timeline != nil && len(s.cfg.Timeline.Recent(1)) > 0
}

func (s *Server) handleInfo(a core.Adapter) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, s.info(r.Context(), a, r.URL.Query().Get("refresh") == "1"))
	}
}

func (s *Server) handleHealth(a core.Adapter) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		info := s.info(r.Context(), a, false)
		if !info.Present {
			writeJSON(w, http.StatusOK, core.Health{Status: core.ToneInfo, Summary: string(a.Kind()) + " is not part of this env", Cards: []core.Card{}, Warnings: []core.Warning{}})
			return
		}
		writeJSON(w, http.StatusOK, s.health(r.Context(), a))
	}
}

func intParam(r *http.Request, name string, def, max int) int {
	n, err := strconv.Atoi(r.URL.Query().Get(name))
	if err != nil || n <= 0 {
		return def
	}
	if max > 0 && n > max {
		return max
	}
	return n
}

func (s *Server) handleTimeline(w http.ResponseWriter, r *http.Request) {
	if s.cfg.Timeline == nil {
		writeJSON(w, http.StatusOK, []any{})
		return
	}
	list := s.cfg.Timeline.Recent(intParam(r, "n", defaultTimeline, maxTimeline))
	if list == nil {
		writeJSON(w, http.StatusOK, []any{})
		return
	}
	writeJSON(w, http.StatusOK, list)
}

func (s *Server) handleTimelineGet(w http.ResponseWriter, r *http.Request) {
	if s.cfg.Timeline != nil {
		if req, ok := s.cfg.Timeline.Get(r.PathValue("key")); ok {
			writeJSON(w, http.StatusOK, req)
			return
		}
	}
	writeHint(w, http.StatusNotFound, "request not in the timeline (it keeps the last 30 s of side events and 500 requests)", "GET "+v1Prefix+"/requests lists the requests it still holds")
}

func (s *Server) handleTraces(w http.ResponseWriter, r *http.Request) {
	if s.cfg.Store == nil {
		writeJSON(w, http.StatusOK, []any{})
		return
	}
	q := r.URL.Query()
	f := apm.ListFilter{Route: q.Get("route"), Sort: q.Get("sort"), Limit: intParam(r, "limit", defaultTraces, 0)}
	if v := q.Get("min"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			ms, merr := strconv.Atoi(v)
			if merr != nil {
				writeError(w, http.StatusBadRequest, "min must be a duration such as 250ms")
				return
			}
			d = time.Duration(ms) * time.Millisecond
		}
		f.MinDuration = d
	}
	writeJSON(w, http.StatusOK, s.cfg.Store.List(f))
}

const traceHint = "GET " + v1Prefix + "/traces lists the stored traces"

func (s *Server) handleTrace(w http.ResponseWriter, r *http.Request) {
	if s.cfg.Store != nil {
		if t, ok := s.cfg.Store.Get(r.PathValue("id")); ok {
			writeJSON(w, http.StatusOK, t)
			return
		}
	}
	writeError(w, http.StatusNotFound, "trace not found; the store keeps the last 500")
}

func (s *Server) handleAnalysis(w http.ResponseWriter, r *http.Request) {
	if s.cfg.Store != nil {
		if a, ok := s.cfg.Store.Analysis(r.PathValue("id")); ok {
			writeJSON(w, http.StatusOK, a)
			return
		}
	}
	writeHint(w, http.StatusNotFound, "trace not found; the store keeps the last 500", traceHint)
}

func (s *Server) handleHotspots(w http.ResponseWriter, r *http.Request) {
	if s.cfg.Store == nil {
		writeJSON(w, http.StatusOK, apm.EmptyHotspots())
		return
	}
	writeJSON(w, http.StatusOK, s.cfg.Store.Hotspots(intParam(r, "limit", defaultHotspots, maxHotspots)))
}

func (s *Server) handleProfile(w http.ResponseWriter, r *http.Request) {
	if s.cfg.Store != nil {
		if data, ok := s.cfg.Store.Profile(r.PathValue("id")); ok {
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Content-Disposition", "attachment")
			_, _ = w.Write(data)
			return
		}
	}
	writeHint(w, http.StatusNotFound, "no profile for this trace", "profiles exist only with profiling on (ROLL_INSPECTOR_PROFILE=excimer, or the Performance page); hasProfile in the trace summary says whether one arrived")
}
