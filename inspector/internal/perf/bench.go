package perf

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/dockergiant/images/inspector/internal/apm"
)

const (
	restoreTimeout = time.Minute
	readyPoll      = 200 * time.Millisecond
	markerPath     = "/tmp/roll-inspector-benchmark.json"
)

type BenchConfig struct {
	// Base is where the requests go: nginx itself, so Varnish never answers
	// from cache.
	Base    string
	Warmups int
	Rounds  int
	// PerRound requests are measured per setting per round, and their median
	// is the round's sample: one request swings by 40% on a busy laptop.
	PerRound       int
	RequestTimeout time.Duration
	// ReadyWait bounds the wait for FPM after a reload; nginx answers 502
	// while the workers restart.
	ReadyWait time.Duration
	LoadPath  string
	// MarkerPath holds the settings a running benchmark has to put back.
	// It lives in the inspector container, so a crash or OOM kill between
	// arms is repaired by the next start (Recover).
	MarkerPath string
}

func (c BenchConfig) withDefaults() BenchConfig {
	if c.Base == "" {
		c.Base = "http://nginx"
	}
	if c.Warmups <= 0 {
		c.Warmups = 2
	}
	if c.Rounds <= 0 {
		c.Rounds = 5
	}
	if c.PerRound <= 0 {
		c.PerRound = 3
	}
	if c.RequestTimeout <= 0 {
		c.RequestTimeout = time.Minute
	}
	if c.ReadyWait <= 0 {
		c.ReadyWait = 20 * time.Second
	}
	if c.LoadPath == "" {
		c.LoadPath = loadPath
	}
	if c.MarkerPath == "" {
		c.MarkerPath = markerPath
	}
	return c
}

type arm struct {
	ID    string
	Label string
	Want  Want
}

var arms = []arm{
	{"off", "Tracing off", Want{Detail: "full"}},
	{"lean", "Lean", Want{Tracing: true, Detail: "lean"}},
	{"full", "Full", Want{Tracing: true, Detail: "full"}},
	{"full+profile", "Full + profiling", Want{Tracing: true, Detail: "full", Profiling: true}},
}

type ArmResult struct {
	ID       string      `json:"id"`
	Label    string      `json:"label"`
	Samples  []float64   `json:"samples"`
	MedianMs float64     `json:"medianMs"`
	MinMs    float64     `json:"minMs"`
	MaxMs    float64     `json:"maxMs"`
	DeltaPct *float64    `json:"deltaPct"`
	NonOK    int         `json:"nonOk"`
	Statuses map[int]int `json:"statuses"`
	// Skipped says why this arm was not measured, such as an image that
	// predates lean tracing.
	Skipped string `json:"skipped,omitempty"`
}

type BenchState struct {
	ID           int         `json:"id"`
	Running      bool        `json:"running"`
	Phase        string      `json:"phase"`
	URL          string      `json:"url"`
	Host         string      `json:"host"`
	Path         string      `json:"path"`
	Started      *time.Time  `json:"started,omitempty"`
	Finished     *time.Time  `json:"finished,omitempty"`
	Round        int         `json:"round"`
	Rounds       int         `json:"rounds"`
	Warmups      int         `json:"warmups"`
	PerRound     int         `json:"perRound"`
	Arm          string      `json:"arm"`
	Done         int         `json:"done"`
	Total        int         `json:"total"`
	Arms         []ArmResult `json:"arms"`
	Previous     *Want       `json:"previous,omitempty"`
	Restored     bool        `json:"restored"`
	RestoreError string      `json:"restoreError,omitempty"`
	Error        string      `json:"error,omitempty"`
	LoadBefore   *Load       `json:"loadBefore,omitempty"`
	LoadAfter    *Load       `json:"loadAfter,omitempty"`
}

func (s BenchState) clone() BenchState {
	out := s
	out.Arms = make([]ArmResult, len(s.Arms))
	for i, a := range s.Arms {
		a.Samples = append([]float64(nil), a.Samples...)
		st := make(map[int]int, len(a.Statuses))
		for k, v := range a.Statuses {
			st[k] = v
		}
		a.Statuses = st
		out.Arms[i] = a
	}
	return out
}

// Bench measures what tracing costs one page: it requests the page with
// tracing off, lean, full and full with profiling, in rounds that rotate the
// order, and puts the previous setting back however the run ends.
type Bench struct {
	ctl         *Controller
	cfg         BenchConfig
	domain      string
	defaultHost func(context.Context) string
	client      *http.Client

	mu         sync.Mutex
	state      BenchState
	cancel     context.CancelFunc
	done       chan struct{}
	closed     bool
	onProgress func(BenchState)

	// shutdown bounds every switch and the restore; Close cancels it once its
	// own deadline passes, so a restore never outlives the process unnoticed.
	shutdown     context.Context
	stopShutdown context.CancelFunc
}

func NewBench(ctl *Controller, cfg BenchConfig, domain string, defaultHost func(context.Context) string) *Bench {
	cfg = cfg.withDefaults()
	shutdown, stop := context.WithCancel(context.Background())
	return &Bench{
		shutdown:     shutdown,
		stopShutdown: stop,
		ctl:          ctl,
		cfg:          cfg,
		domain:       strings.ToLower(domain),
		defaultHost:  defaultHost,
		state:        BenchState{Arms: []ArmResult{}},
		client: &http.Client{
			// A redirect would time a 2 ms 302 instead of the page.
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
	}
}

func (b *Bench) OnProgress(fn func(BenchState)) {
	b.mu.Lock()
	b.onProgress = fn
	b.mu.Unlock()
}

func (b *Bench) State() BenchState {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.state.clone()
}

func (b *Bench) update(fn func(*BenchState)) {
	b.mu.Lock()
	fn(&b.state)
	snap := b.state.clone()
	cb := b.onProgress
	b.mu.Unlock()
	if cb != nil {
		cb(snap)
	}
}

// target turns what the page sent into a Host header and a request path. A
// path alone uses the shop's host; an absolute URL must stay on this env's
// domain, because Inspector sends nothing anywhere but nginx.
func (b *Bench) target(ctx context.Context, raw string) (string, string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", "", errors.New("give a URL or a path such as /women.html")
	}
	if strings.ContainsAny(raw, "\r\n\x00") {
		return "", "", errors.New("the URL holds a line break")
	}
	if strings.HasPrefix(raw, "/") && !strings.HasPrefix(raw, "//") {
		u, err := url.Parse(raw)
		if err != nil {
			return "", "", fmt.Errorf("bad path: %w", err)
		}
		host := ""
		if b.defaultHost != nil {
			host = b.defaultHost(ctx)
		}
		if host == "" {
			host = b.domain
		}
		return host, u.RequestURI(), nil
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", "", fmt.Errorf("bad URL: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", "", errors.New("the URL needs http or https, or start with / for a path on the shop")
	}
	host := strings.ToLower(u.Hostname())
	if host != b.domain && !strings.HasSuffix(host, "."+b.domain) {
		return "", "", fmt.Errorf("%s is not on this env's domain %s; Inspector only measures pages of this shop", host, b.domain)
	}
	if host == "inspector."+b.domain {
		return "", "", errors.New("that is Inspector itself; give a shop page")
	}
	return host, u.RequestURI(), nil
}

// Start validates the URL, takes the switches and runs in the background.
// The run outlives the request that started it; Cancel ends it.
func (b *Bench) Start(ctx context.Context, raw string) (BenchState, error) {
	host, path, err := b.target(ctx, raw)
	if err != nil {
		return b.State(), fmt.Errorf("%w: %w", ErrInvalid, err)
	}
	b.mu.Lock()
	closed := b.closed
	b.mu.Unlock()
	if closed {
		return b.State(), fmt.Errorf("%w: Inspector is stopping", ErrUnavailable)
	}
	// An Apply in flight finishes first, so prev is what the user just set.
	b.ctl.applyMu.Lock()
	release, ok := b.ctl.hold()
	if !ok {
		b.ctl.applyMu.Unlock()
		return b.State(), ErrBusy
	}
	st, id := b.ctl.read(ctx, Services[0], true)
	b.ctl.applyMu.Unlock()
	if !st.Available || id == "" {
		release()
		reason := st.Reason
		if reason == "" {
			reason = "php-fpm cannot be switched"
		}
		return b.State(), fmt.Errorf("%w: %s", ErrUnavailable, reason)
	}
	prev := st.want()
	if err := b.writeMarker(map[string]Want{Services[0]: prev}); err != nil {
		release()
		return b.State(), fmt.Errorf("%w: cannot write the restore marker, so a crash could leave php-fpm in a benchmark arm: %w", ErrUnavailable, err)
	}
	runCtx, cancel := context.WithCancel(context.Background())
	now := time.Now()
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		cancel()
		b.removeMarker()
		release()
		return b.State(), fmt.Errorf("%w: Inspector is stopping", ErrUnavailable)
	}
	results := make([]ArmResult, len(arms))
	var measured []int
	for i, a := range arms {
		results[i] = ArmResult{ID: a.ID, Label: a.Label, Samples: []float64{}, Statuses: map[int]int{}}
		// The old roll-inspector-ini ignores TRACE_DETAIL, so lean would
		// measure full tracing under the wrong label.
		if a.Want.Detail == "lean" && strings.Contains(st.DetailReason, "predates") {
			results[i].Skipped = "not supported by this php-fpm image: " + st.DetailReason
			continue
		}
		measured = append(measured, i)
	}
	b.state = BenchState{
		ID: b.state.ID + 1, Running: true, Phase: "probing", URL: "https://" + host + path, Host: host, Path: path,
		Started: &now, Rounds: b.cfg.Rounds, Warmups: b.cfg.Warmups, PerRound: b.cfg.PerRound, Total: b.cfg.Rounds * len(measured),
		Arms: results, Previous: &prev,
	}
	b.cancel = cancel
	done := make(chan struct{})
	b.done = done
	runID := b.state.ID
	snap := b.state.clone()
	b.mu.Unlock()
	go b.run(runCtx, runID, done, release, id, host, path, prev, measured)
	return snap, nil
}

func (b *Bench) Cancel() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if !b.state.Running || b.cancel == nil {
		return false
	}
	b.cancel()
	return true
}

// Close ends a running benchmark and waits for it to put the previous
// settings back. Once ctx ends the restore is cut off too, so the caller's
// shutdown budget holds; Close then reports that the restore did not finish.
func (b *Bench) Close(ctx context.Context) error {
	b.mu.Lock()
	b.closed = true
	done := b.done
	if b.cancel != nil {
		b.cancel()
	}
	b.mu.Unlock()
	if done == nil {
		return nil
	}
	stop := context.AfterFunc(ctx, b.stopShutdown)
	defer stop()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("the benchmark did not put the previous settings back in time: %w", ctx.Err())
	}
}

func armOrder(n, rounds int) [][]int {
	out := make([][]int, rounds)
	for r := range rounds {
		out[r] = make([]int, n)
		for k := range n {
			out[r][k] = (r + k) % n
		}
	}
	return out
}

func (b *Bench) run(ctx context.Context, runID int, done chan struct{}, release func(), id, host, path string, prev Want, measured []int) {
	before := readLoad(b.cfg.LoadPath)
	b.update(func(s *BenchState) { s.LoadBefore = &before })
	var runErr error
	defer func() {
		// The restore gets its own context: a cancel must not stop it, only
		// the end of Inspector's shutdown budget.
		rctx, cancel := context.WithTimeout(b.shutdown, restoreTimeout)
		restoreErr := b.ctl.switchTo(rctx, id, prev)
		b.ctl.forget()
		if restoreErr == nil {
			// The exec succeeding does not prove the setting stuck: a
			// `roll inspector trace` during the run can land after it.
			got, _ := b.ctl.read(rctx, Services[0], true)
			switch {
			case got.Error != "":
				restoreErr = fmt.Errorf("the restore ran, but php-fpm cannot be read back: %s", got.Error)
			case got.want() != prev:
				restoreErr = fmt.Errorf("php-fpm is now %s, not %s", describe(got.want()), describe(prev))
			}
		}
		cancel()
		// A failed restore keeps the marker, so the next start tries again.
		if restoreErr == nil {
			b.removeMarker()
		}
		after := readLoad(b.cfg.LoadPath)
		finished := time.Now()
		b.update(func(s *BenchState) {
			if s.ID != runID {
				return
			}
			s.Running = false
			s.Finished = &finished
			s.LoadAfter = &after
			s.Arm = ""
			s.Restored = restoreErr == nil
			if restoreErr != nil {
				s.RestoreError = restoreErr.Error()
			}
			switch {
			case ctx.Err() != nil:
				s.Phase = "cancelled"
			case runErr != nil:
				s.Phase = "failed"
				s.Error = runErr.Error()
			default:
				s.Phase = "done"
			}
			summarize(s.Arms)
		})
		b.mu.Lock()
		if b.state.ID == runID {
			b.cancel = nil
		}
		b.mu.Unlock()
		release()
		close(done)
	}()

	_, code, loc, err := b.get(ctx, host, path)
	switch {
	case err != nil:
		runErr = fmt.Errorf("the first request to %s failed: %w", host+path, err)
		return
	case code != http.StatusOK:
		runErr = errors.New(answered(host+path, code, loc) + "; measure a page that answers 200")
		return
	}

	// A switch is not cancelled halfway: a killed exec leaves
	// roll-inspector-ini running in the container, where it could finish
	// after the restore.
	switchCtx := b.shutdown
	count := 0
	for r, order := range armOrder(len(measured), b.cfg.Rounds) {
		for _, k := range order {
			i := measured[k]
			a := arms[i]
			b.update(func(s *BenchState) { s.Phase, s.Round, s.Arm = "switching", r+1, a.ID })
			if runErr = b.ctl.switchTo(switchCtx, id, a.Want); runErr != nil {
				return
			}
			if ctx.Err() != nil {
				return
			}
			if runErr = b.ready(ctx, host, path); runErr != nil {
				runErr = fmt.Errorf("with %s: %w", a.Label, runErr)
				return
			}
			b.update(func(s *BenchState) { s.Phase = "warming" })
			for range b.cfg.Warmups {
				if runErr = b.getOK(ctx, host, path, a.Label); runErr != nil {
					return
				}
			}
			b.update(func(s *BenchState) { s.Phase = "measuring" })
			times := make([]float64, 0, b.cfg.PerRound)
			for range b.cfg.PerRound {
				ms, code, loc, err := b.get(ctx, host, path)
				if err != nil {
					runErr = err
					return
				}
				// An error page or a redirect answers in a few ms, and timing
				// it would make the arm look faster than off.
				if code != http.StatusOK {
					runErr = fmt.Errorf("with %s %s; the run stops rather than time it", a.Label, answered("the page", code, loc))
					b.update(func(s *BenchState) { s.Arms[i].Statuses[code]++; s.Arms[i].NonOK++ })
					return
				}
				times = append(times, ms)
			}
			count++
			b.update(func(s *BenchState) {
				res := &s.Arms[i]
				res.Samples = append(res.Samples, median(times))
				res.Statuses[http.StatusOK] += len(times)
				s.Done = count
				summarize(s.Arms)
			})
		}
	}
}

func answered(what string, code int, loc string) string {
	msg := fmt.Sprintf("%s answered %d, not 200", what, code)
	if loc != "" {
		msg += " (Location: " + loc + ")"
	}
	return msg
}

func describe(w Want) string {
	switch {
	case !w.Tracing:
		return "tracing off"
	case w.Profiling:
		return w.Detail + " tracing with profiling"
	}
	return w.Detail + " tracing"
}

func (b *Bench) getOK(ctx context.Context, host, path, label string) error {
	_, code, loc, err := b.get(ctx, host, path)
	if err != nil {
		return err
	}
	if code != http.StatusOK {
		return fmt.Errorf("with %s %s", label, answered("the page", code, loc))
	}
	return nil
}

// ready waits for FPM to answer 200 after a reload; nginx answers 502 while
// the workers restart, and anything else after ReadyWait fails the run.
func (b *Bench) ready(ctx context.Context, host, path string) error {
	deadline := time.Now().Add(b.cfg.ReadyWait)
	for {
		_, code, loc, err := b.get(ctx, host, path)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err == nil && code == http.StatusOK {
			return nil
		}
		if time.Now().After(deadline) {
			if err != nil {
				return fmt.Errorf("php-fpm did not come back within %s after the reload: %w", b.cfg.ReadyWait, err)
			}
			return fmt.Errorf("%s for %s after the reload", answered("the page", code, loc), b.cfg.ReadyWait)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(readyPoll):
		}
	}
}

// get times one request including its whole body, since the page is not
// done for the browser before the last byte.
func (b *Bench) get(ctx context.Context, host, path string) (float64, int, string, error) {
	ctx, cancel := context.WithTimeout(ctx, b.cfg.RequestTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, b.cfg.Base+path, nil)
	if err != nil {
		return 0, 0, "", err
	}
	req.Host = host
	req.Header.Set("X-Forwarded-Proto", "https")
	req.Header.Set("X-Forwarded-Port", "443")
	req.Header.Set("User-Agent", apm.BenchmarkUserAgent)
	start := time.Now()
	res, err := b.client.Do(req)
	if err != nil {
		return 0, 0, "", err
	}
	_, err = io.Copy(io.Discard, res.Body)
	res.Body.Close()
	ms := float64(time.Since(start)) / float64(time.Millisecond)
	if err != nil {
		return ms, res.StatusCode, "", err
	}
	return ms, res.StatusCode, res.Header.Get("Location"), nil
}

func summarize(results []ArmResult) {
	var base float64
	for i := range results {
		r := &results[i]
		r.DeltaPct = nil
		if len(r.Samples) == 0 {
			r.MedianMs, r.MinMs, r.MaxMs = 0, 0, 0
			continue
		}
		r.MedianMs = median(r.Samples)
		r.MinMs, r.MaxMs = r.Samples[0], r.Samples[0]
		for _, v := range r.Samples {
			r.MinMs = min(r.MinMs, v)
			r.MaxMs = max(r.MaxMs, v)
		}
		if r.ID == "off" {
			base = r.MedianMs
		}
	}
	if base <= 0 {
		return
	}
	for i := range results {
		r := &results[i]
		if r.ID == "off" || len(r.Samples) == 0 {
			continue
		}
		d := (r.MedianMs - base) / base * 100
		r.DeltaPct = &d
	}
}

func median(v []float64) float64 {
	if len(v) == 0 {
		return 0
	}
	s := append([]float64(nil), v...)
	sort.Float64s(s)
	if n := len(s); n%2 == 1 {
		return s[n/2]
	}
	n := len(s)
	return (s[n/2-1] + s[n/2]) / 2
}
