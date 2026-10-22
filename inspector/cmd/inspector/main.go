package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/dockergiant/images/inspector/clients"
	"github.com/dockergiant/images/inspector/internal/api"
	"github.com/dockergiant/images/inspector/internal/apm"
	"github.com/dockergiant/images/inspector/internal/capture"
	"github.com/dockergiant/images/inspector/internal/core"
	"github.com/dockergiant/images/inspector/internal/docker"
	"github.com/dockergiant/images/inspector/internal/hub"
	"github.com/dockergiant/images/inspector/internal/magento"
	"github.com/dockergiant/images/inspector/internal/metrics"
	"github.com/dockergiant/images/inspector/internal/perf"
	"github.com/dockergiant/images/inspector/internal/procmap"
	"github.com/dockergiant/images/inspector/internal/services"
	"github.com/dockergiant/images/inspector/internal/services/nginx"
	"github.com/dockergiant/images/inspector/internal/services/rabbitmq"
	"github.com/dockergiant/images/inspector/internal/services/redis"
	"github.com/dockergiant/images/inspector/internal/services/search"
	"github.com/dockergiant/images/inspector/internal/services/varnish"
	"github.com/dockergiant/images/inspector/internal/timeline"
	"github.com/dockergiant/images/inspector/ui"
)

var version = "dev"

const (
	apiAddr    = ":8080"
	ddAddr     = ":8126"
	otlpAddr   = ":4318"
	healthURL  = "http://127.0.0.1:8126/info"
	healthWait = 2 * time.Second

	hubBuffer      = 1024
	sweepTimeout   = 30 * time.Second
	detectTimeout  = 20 * time.Second
	shutdownBudget = 15 * time.Second
	recoverTimeout = time.Minute
	envEvery       = 20 * time.Second
	envTimeout     = 30 * time.Second
	errorEvery     = time.Minute
)

func main() {
	os.Exit(run(os.Args[1:], os.Stderr))
}

func run(args []string, stderr io.Writer) int {
	flags := flag.NewFlagSet("inspector", flag.ContinueOnError)
	flags.SetOutput(stderr)
	health := flags.Bool("healthcheck", false, "check that the trace intake on 127.0.0.1:8126 answers, then exit 0 or 1")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if *health {
		return healthcheck(healthURL, healthWait)
	}

	cfg, err := loadConfig(os.Getenv)
	if err != nil {
		fmt.Fprintln(stderr, "inspector:", err)
		return 2
	}
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil)).With("env", cfg.env)
	if cfg.warning != "" {
		log.Warn(cfg.warning)
	}
	if err := serve(cfg, log); err != nil {
		log.Error("inspector stopped", "err", err)
		return 1
	}
	return 0
}

func serve(cfg config, log *slog.Logger) error {
	sig, stopSignals := signal.NotifyContext(context.Background(), syscall.SIGTERM, os.Interrupt)
	defer stopSignals()
	// Request contexts outlive the signal until the capture sessions are
	// restored, so an opt-in stream does not race the shutdown restore.
	base, cancelBase := context.WithCancel(context.Background())
	defer cancelBase()

	d, err := docker.New(cfg.env)
	if err != nil {
		return fmt.Errorf("docker client: %w", err)
	}
	defer d.Close()
	d.SetLogger(log)

	errs := newRateLog(log, errorEvery)
	reader := magento.NewReader(d)
	proc := procmap.New(d)
	proc.OnError = func(err error) { errs.Error("pid map refresh failed", err) }
	captures := capture.New(log)
	h := hub.New(hubBuffer)
	deps := services.Deps{Docker: d, Magento: reader, Proc: proc, Capture: captures, Log: log}
	adapters := []core.Adapter{varnish.New(deps), redis.New(deps), search.New(deps), rabbitmq.New(deps)}
	store := apm.NewStore(0, 0)
	store.SkipHost("inspector." + cfg.domain)
	tl := timeline.New(proc, store)
	sampler := metrics.NewSampler(cfg.sample)
	perfs := perf.New(perf.Options{
		Docker:        d,
		Env:           cfg.env,
		Domain:        cfg.domain,
		Store:         store,
		Sampler:       sampler,
		SampleDefault: cfg.sample,
		DefaultHost:   shopHost(reader, cfg),
	})

	uiFS, err := fs.Sub(ui.Dist, "dist")
	if err != nil {
		return fmt.Errorf("embedded ui: %w", err)
	}
	web := api.New(api.Config{
		Env:      cfg.env,
		Domain:   cfg.domain,
		Secret:   cfg.secret,
		Version:  version,
		Docker:   d,
		Adapters: adapters,
		Feeds:    []api.Feed{{Kind: nginx.Kind, Spec: nginx.Stream(d)}},
		Magento:  reader,
		Hub:      h,
		Timeline: tl,
		Store:    store,
		Capture:  captures,
		UI:       uiFS,
		Clients:  clients.Files,
		Perf:     perfs,
		Log:      log,
	})

	allow := newAllowList(d)
	servers := []*server{
		{name: "trace intake", addr: ddAddr, handler: apm.NewDDIntake(store, allow.allow)},
		{name: "otlp intake", addr: otlpAddr, handler: apm.NewOTLPIntake(store, allow.allow)},
		{name: "api", addr: apiAddr, handler: web.Handler()},
	}
	failed := make(chan error, len(servers))
	for _, s := range servers[:2] {
		if err := s.start(base, log, failed); err != nil {
			return err
		}
	}

	// Before the API listens: a sweep after a browser reconnects would kill
	// the varnishlog or MONITOR the new viewer just started.
	sctx, cancel := context.WithTimeout(sig, sweepTimeout)
	if err := d.Sweep(sctx); err != nil {
		log.Warn("sweep of leftover inspector execs failed", "err", err)
	}
	cancel()
	recoverBenchmark(sig, perfs.Bench, log)
	if err := servers[2].start(base, log, failed); err != nil {
		return err
	}
	log.Info("inspector started", "version", version, "domain", cfg.domain)

	// The samplers start once Detect has run and stop with the signal; serve
	// waits for them, so none outlives the Docker client.
	var samplers sync.WaitGroup
	samplers.Go(func() {
		detect(sig, adapters, log)
		sampleMetrics(sig, adapters, sampler, errs)
	})
	go proc.Run(sig)
	go forwardTraces(sig, store, tl)
	go watchFrontends(sig, reader, tl, errs, log)

	var runErr error
	select {
	case <-sig.Done():
		log.Info("shutting down")
	case runErr = <-failed:
	}
	stopSignals()
	shutdown(servers, captures, perfs.Bench, cancelBase, log)
	samplers.Wait()
	return runErr
}

type server struct {
	name    string
	addr    string
	handler http.Handler
	srv     *http.Server
}

func (s *server) start(base context.Context, log *slog.Logger, failed chan<- error) error {
	ln, err := net.Listen("tcp", s.addr)
	if err != nil {
		return fmt.Errorf("%s: listen %s: %w", s.name, s.addr, err)
	}
	s.srv = &http.Server{
		Handler:           s.handler,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       2 * time.Minute,
		BaseContext:       func(net.Listener) context.Context { return base },
		ErrorLog:          slog.NewLogLogger(log.Handler(), slog.LevelWarn),
	}
	go func() {
		if err := serveErr(s.srv.Serve(ln)); err != nil {
			failed <- fmt.Errorf("%s: %w", s.name, err)
		}
	}()
	return nil
}

func shutdown(servers []*server, captures *capture.Manager, bench *perf.Bench, cancelBase context.CancelFunc, log *slog.Logger) {
	ctx, cancel := context.WithTimeout(context.Background(), shutdownBudget)
	defer cancel()

	var wg sync.WaitGroup
	for _, s := range servers {
		if s.srv == nil {
			continue
		}
		wg.Go(func() {
			if err := s.srv.Shutdown(ctx); err != nil {
				_ = s.srv.Close()
			}
		})
	}
	// A benchmark between arms would leave php-fpm in that arm, perhaps
	// full tracing with profiling; it restores before the Docker client closes.
	wg.Go(func() {
		if err := bench.Close(ctx); err != nil {
			log.Warn("benchmark restore at shutdown", "err", err)
		}
	})
	captures.EndAll(ctx)
	cancelBase()
	wg.Wait()
	if ctx.Err() != nil {
		log.Warn("shutdown ran out of its budget", "budget", shutdownBudget.String())
	}
}

func detect(ctx context.Context, adapters []core.Adapter, log *slog.Logger) {
	var wg sync.WaitGroup
	for _, a := range adapters {
		wg.Go(func() {
			dctx, cancel := context.WithTimeout(ctx, detectTimeout)
			defer cancel()
			info, err := a.Detect(dctx)
			if err != nil && info.Error == "" {
				info.Error = err.Error()
			}
			attrs := []any{"kind", a.Kind(), "present", info.Present, "engine", info.Engine, "version", info.Version, "tested", info.Tested}
			if info.Error != "" {
				log.Warn("detect", append(attrs, "err", info.Error)...)
				return
			}
			log.Info("detect", attrs...)
		})
	}
	wg.Wait()
}

// sampleMetrics runs one sampler per adapter that keeps a history, and
// returns once ctx ends and every sampler has stopped.
func sampleMetrics(ctx context.Context, adapters []core.Adapter, sampler *metrics.Sampler, errs *rateLog) {
	var wg sync.WaitGroup
	for _, a := range adapters {
		src, ok := a.(core.MetricsSource)
		if !ok {
			continue
		}
		wg.Go(func() {
			sampler.Run(ctx, string(a.Kind()), src, metrics.SampleTimeout, func(err error) {
				errs.Error("metrics sample failed: "+string(a.Kind()), err)
			})
		})
	}
	wg.Wait()
}

// recoverBenchmark puts php-fpm back when the last Inspector died during a
// benchmark arm, before the API can start a new one.
func recoverBenchmark(ctx context.Context, b *perf.Bench, log *slog.Logger) {
	rctx, cancel := context.WithTimeout(ctx, recoverTimeout)
	defer cancel()
	restored, err := b.Recover(rctx)
	if len(restored) > 0 {
		log.Warn("restored the settings an interrupted benchmark left behind", "services", restored)
	}
	if err != nil {
		log.Error("could not restore the settings an interrupted benchmark left behind", "err", err)
	}
}

func forwardTraces(ctx context.Context, store *apm.Store, tl *timeline.Timeline) {
	ch, stop := store.Subscribe()
	defer stop()
	for {
		select {
		case <-ctx.Done():
			return
		case t := <-ch:
			tl.Ingest(core.Event{Type: "trace", Time: t.Start, Kind: core.PHP, Data: t})
		}
	}
}

func watchFrontends(ctx context.Context, r magento.Reader, tl *timeline.Timeline, errs *rateLog, log *slog.Logger) {
	loaded := false
	var mtime int64
	tick := time.NewTicker(envEvery)
	defer tick.Stop()
	for {
		lctx, cancel := context.WithTimeout(ctx, envTimeout)
		env, err := r.Load(lctx)
		cancel()
		switch {
		case err != nil && ctx.Err() == nil:
			errs.Error("read env.php failed", err)
		case err == nil && (!loaded || env.MTime != mtime):
			names := frontends(env)
			tl.SetFrontends(names)
			loaded, mtime = true, env.MTime
			log.Info("cache frontends", "databases", len(names), "mtime", env.MTime)
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}
