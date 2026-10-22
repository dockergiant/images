package api

import (
	"errors"
	"net/http"

	"github.com/dockergiant/images/inspector/internal/core"
	"github.com/dockergiant/images/inspector/internal/perf"
)

const benchKey = "perf:benchmark"

func (s *Server) perfReady(w http.ResponseWriter) bool {
	if s.cfg.Perf == nil {
		writeError(w, http.StatusServiceUnavailable, "the performance controls are not set up in this Inspector")
		return false
	}
	return true
}

// relayBenchmark sends every benchmark step to the stream, so the page shows
// progress without polling.
func (s *Server) relayBenchmark() {
	if s.cfg.Perf == nil {
		return
	}
	s.cfg.Perf.Bench.OnProgress(func(st perf.BenchState) {
		s.record(benchKey, core.Event{Type: "benchmark", Time: s.now(), Kind: core.Kind("perf"), Data: st})
	})
}

func (s *Server) snapshot(r *http.Request) perf.Snapshot {
	snap := s.cfg.Perf.Snapshot(r.Context())
	snap.Live = s.liveSources()
	return snap
}

// liveSources lists the running streams and who keeps each one running: a
// drawer streams it itself, a client of /api/v1/stream (any page in the
// browser) streams it, the timeline follows it for the Overview, and a v1
// page lookup leases varnishlog for a few minutes.
func (s *Server) liveSources() []perf.LiveSource {
	active := s.cfg.Hub.Active()
	s.wantMu.Lock()
	want := make(map[string]int, len(s.want))
	for k, v := range s.want {
		want[k] = v
	}
	api := make(map[string]int, len(s.apiWant))
	for k, v := range s.apiWant {
		api[k] = v
	}
	s.wantMu.Unlock()
	out := make([]perf.LiveSource, 0, len(active))
	for _, key := range active {
		src := perf.LiveSource{Key: key, Holders: []string{}}
		switch {
		case key == timelineKey:
			src.Holders = append(src.Holders, "overview")
		case want[key]-api[key] > 0:
			src.Holders = append(src.Holders, "drawer")
		}
		if api[key] > 0 {
			src.Holders = append(src.Holders, "api")
		}
		if key != timelineKey {
			src.Holders = append(src.Holders, s.feedHolders(key)...)
		}
		out = append(out, src)
	}
	return out
}

func (s *Server) handlePerformance(w http.ResponseWriter, r *http.Request) {
	if !s.perfReady(w) {
		return
	}
	writeJSON(w, http.StatusOK, s.snapshot(r))
}

type perfSettings struct {
	Service   string  `json:"service"`
	Tracing   *bool   `json:"tracing"`
	Detail    *string `json:"detail"`
	Profiling *bool   `json:"profiling"`
	Sample    *string `json:"sample"`
}

func perfStatus(err error) int {
	switch {
	case errors.Is(err, perf.ErrInvalid):
		return http.StatusBadRequest
	case errors.Is(err, perf.ErrBusy), errors.Is(err, perf.ErrUnavailable):
		return http.StatusConflict
	}
	return http.StatusBadGateway
}

// handlePerfSettings changes no data, so it needs no confirm; checkHeaders
// still asks the CSRF header and Origin, as for every write.
func (s *Server) handlePerfSettings(w http.ResponseWriter, r *http.Request) {
	if !s.perfReady(w) {
		return
	}
	var body perfSettings
	if !decodeBody(w, r, &body) {
		return
	}
	php := body.Tracing != nil || body.Detail != nil || body.Profiling != nil || body.Service != ""
	if !php && body.Sample == nil {
		writeError(w, http.StatusBadRequest, "nothing to change: send service with tracing, detail or profiling, or sample")
		return
	}
	if body.Sample != nil {
		if err := s.cfg.Perf.SetSample(*body.Sample); err != nil {
			writeError(w, perfStatus(err), err.Error())
			return
		}
		s.cfg.Log.Info("performance: metrics sampling", "sample", *body.Sample)
	}
	if php {
		_, err := s.cfg.Perf.Controller().Apply(r.Context(), body.Service, perf.Change{Tracing: body.Tracing, Detail: body.Detail, Profiling: body.Profiling})
		if err != nil {
			writeError(w, perfStatus(err), err.Error())
			return
		}
		s.cfg.Log.Info("performance: tracing switched", "service", body.Service, "tracing", body.Tracing, "detail", body.Detail, "profiling", body.Profiling)
	}
	writeJSON(w, http.StatusOK, s.snapshot(r))
}

func (s *Server) handleBenchStart(w http.ResponseWriter, r *http.Request) {
	if !s.perfReady(w) {
		return
	}
	var body struct {
		URL string `json:"url"`
	}
	if !decodeBody(w, r, &body) {
		return
	}
	st, err := s.cfg.Perf.Bench.Start(r.Context(), body.URL)
	if err != nil {
		writeError(w, perfStatus(err), err.Error())
		return
	}
	s.cfg.Log.Info("performance: benchmark started", "url", st.URL)
	writeJSON(w, http.StatusAccepted, st)
}

func (s *Server) handleBenchCancel(w http.ResponseWriter, r *http.Request) {
	if !s.perfReady(w) {
		return
	}
	if !s.cfg.Perf.Bench.Cancel() {
		writeError(w, http.StatusConflict, "no benchmark is running")
		return
	}
	writeJSON(w, http.StatusOK, s.cfg.Perf.Bench.State())
}

// handleLiveStop ends one running source at once, whoever holds it. A v1
// client cannot start it again for a while; the Inspector page can.
func (s *Server) handleLiveStop(w http.ResponseWriter, r *http.Request) {
	if !s.perfReady(w) {
		return
	}
	var body struct {
		Key string `json:"key"`
	}
	if !decodeBody(w, r, &body) {
		return
	}
	if body.Key == "" {
		writeError(w, http.StatusBadRequest, "key is required: one of the live sources the Performance page lists")
		return
	}
	s.wantMu.Lock()
	s.stopped[body.Key] = s.now()
	s.wantMu.Unlock()
	if !s.cfg.Hub.Stop(body.Key) {
		s.wantMu.Lock()
		delete(s.stopped, body.Key)
		s.wantMu.Unlock()
		writeError(w, http.StatusNotFound, body.Key+" is not running")
		return
	}
	s.cfg.Log.Info("performance: live source stopped", "key", body.Key)
	writeJSON(w, http.StatusOK, s.snapshot(r))
}
