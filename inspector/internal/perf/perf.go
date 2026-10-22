package perf

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/dockergiant/images/inspector/internal/apm"
	"github.com/dockergiant/images/inspector/internal/docker"
	"github.com/dockergiant/images/inspector/internal/metrics"
)

const costWindow = 5 * time.Minute

type Options struct {
	Docker        docker.API
	Env           string
	Domain        string
	Store         *apm.Store
	Sampler       *metrics.Sampler
	SampleDefault time.Duration
	Bench         BenchConfig
	DefaultHost   func(context.Context) string
}

// Perf is what the Performance page reads and changes.
type Perf struct {
	Bench   *Bench
	Sampler *metrics.Sampler

	ctl           *Controller
	self          *self
	docker        docker.API
	store         *apm.Store
	domain        string
	sampleDefault time.Duration
	loadPath      string
}

func New(o Options) *Perf {
	ctl := NewController(o.Docker, o.Env)
	cfg := o.Bench.withDefaults()
	return &Perf{
		Bench:         NewBench(ctl, cfg, o.Domain, o.DefaultHost),
		Sampler:       o.Sampler,
		ctl:           ctl,
		self:          newSelf(o.Docker),
		docker:        o.Docker,
		store:         o.Store,
		domain:        strings.ToLower(o.Domain),
		sampleDefault: o.SampleDefault,
		loadPath:      cfg.LoadPath,
	}
}

func (p *Perf) Controller() *Controller { return p.ctl }

type SamplerState struct {
	Interval      int                 `json:"interval"`
	Paused        bool                `json:"paused"`
	Default       int                 `json:"default"`
	DefaultPaused bool                `json:"defaultPaused"`
	Options       []string            `json:"options"`
	Permanent     string              `json:"permanent"`
	Ticks         []metrics.TickStats `json:"ticks"`
	ExecsPerMin   float64             `json:"execsPerMin"`
}

// LiveSource is a running stream and who keeps it running.
type LiveSource struct {
	Key     string   `json:"key"`
	Holders []string `json:"holders"`
}

type Snapshot struct {
	PHP        []PHPState     `json:"php"`
	Summary    Summary        `json:"summary"`
	Sampler    SamplerState   `json:"sampler"`
	Inspector  SelfStats      `json:"inspector"`
	Intake     apm.IntakeCost `json:"intake"`
	Load       Load           `json:"load"`
	Benchmark  BenchState     `json:"benchmark"`
	DefaultURL string         `json:"defaultUrl"`
	Live       []LiveSource   `json:"live"`
}

func sampleValue(d time.Duration) string {
	if d <= 0 {
		return "off"
	}
	return strconv.Itoa(int(d / time.Second))
}

func (p *Perf) Snapshot(ctx context.Context) Snapshot {
	var s Snapshot
	var wg sync.WaitGroup
	wg.Go(func() { s.PHP = p.ctl.States(ctx) })
	wg.Go(func() { s.Inspector = p.self.Read(ctx) })
	wg.Wait()
	s.Summary = p.ctl.Summary(ctx)
	s.Sampler = p.samplerState()
	if p.store != nil {
		s.Intake = p.store.Cost(costWindow)
	}
	s.Load = readLoad(p.loadPath)
	s.Benchmark = p.Bench.State()
	s.DefaultURL = p.defaultURL()
	s.Live = []LiveSource{}
	return s
}

func (p *Perf) samplerState() SamplerState {
	st := SamplerState{Default: int(p.sampleDefault / time.Second), DefaultPaused: p.sampleDefault <= 0, Ticks: []metrics.TickStats{}}
	for _, d := range metrics.Intervals {
		st.Options = append(st.Options, sampleValue(d))
	}
	st.Options = append(st.Options, "off")
	if p.Sampler != nil {
		iv := p.Sampler.Interval()
		st.Interval = int(iv / time.Second)
		st.Paused = iv <= 0
		st.Permanent = "ROLL_INSPECTOR_SAMPLE=" + sampleValue(iv)
		st.Ticks = p.Sampler.Ticks(costWindow)
	}
	if p.docker != nil {
		st.ExecsPerMin = float64(p.docker.ExecsSince(time.Now().Add(-costWindow))) / costWindow.Minutes()
	}
	return st
}

func (p *Perf) SetSample(v string) error {
	d, err := metrics.ParseInterval(v)
	if err == nil {
		err = p.Sampler.SetInterval(d)
	}
	if err != nil {
		return fmt.Errorf("%w: %w", ErrInvalid, err)
	}
	return nil
}

// defaultURL is the slowest route's worst page, limited to GETs on the shop
// that answered 200, since that is what the benchmark can request.
func (p *Perf) defaultURL() string {
	if p.store == nil {
		return ""
	}
	best, bestMs := "", -1.0
	for _, r := range p.store.Hotspots(0).Routes {
		t, ok := p.store.Get(r.WorstID)
		if !ok || t.Method != http.MethodGet || t.Status != http.StatusOK {
			continue
		}
		u, err := url.Parse(t.URL)
		if err != nil {
			continue
		}
		h := strings.ToLower(u.Hostname())
		if h == "inspector."+p.domain || (h != p.domain && !strings.HasSuffix(h, "."+p.domain)) {
			continue
		}
		if r.P95Ms > bestMs {
			best, bestMs = t.URL, r.P95Ms
		}
	}
	return best
}
