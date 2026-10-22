package varnish

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/dockergiant/images/inspector/internal/core"
	"github.com/dockergiant/images/inspector/internal/docker"
	"github.com/dockergiant/images/inspector/internal/metrics"
	"github.com/dockergiant/images/inspector/internal/services"
)

const (
	service       = "varnish"
	ringSize      = 2000
	hitWindow     = 5 * time.Minute
	sampleEvery   = time.Second
	execTimeout   = 10 * time.Second
	searchTimeout = 20 * time.Second
	searchLimit   = 2000
	searchKeep    = 100
	topMisses     = 10
	recentBans    = 10
	// healthTTL dedupes the Overview poll, the Health view's poll and every
	// other tab: each Health is three execs into the varnish container.
	healthTTL = 5 * time.Second
	// statReuse lets Health and the metrics sampler share one varnishstat
	// without skewing the sampler's per-second rates.
	statReuse = time.Second
)

var (
	errNotRunning = errors.New("varnish is not running in this environment")
	versionRe     = regexp.MustCompile(`varnish-(\d+\.\d+(?:\.\d+)?)`)
	tagBanRe      = regexp.MustCompile(`^obj\.http\.X-Magento-Tags ~ (.*)$`)
	urlBanRe      = regexp.MustCompile(`^req\.url ~ (.*)$`)
)

var windowCounters = []string{
	"MAIN.cache_hit", "MAIN.cache_miss", "MAIN.cache_hitpass", "MAIN.cache_hitmiss",
	"MAIN.s_pass", "MAIN.client_req", "MAIN.backend_fail", "MAIN.n_lru_nuked",
}

type sample struct {
	at     time.Time
	values map[string]uint64
}

type adapter struct {
	deps     services.Deps
	purgeURL string
	client   *http.Client
	now      func() time.Time
	history  *metrics.History

	mu       sync.Mutex
	info     core.Info
	detected bool
	samples  []sample
	ring     []core.VarnishReq
	ringNext int
	watchers int
	envs     map[string]map[string]string

	lastHealth *core.Health
	healthAt   time.Time
	lastStat   map[string]uint64
	statAt     time.Time
	// healthMu lets one Health run at a time, so concurrent polls share it.
	healthMu sync.Mutex
	statMu   sync.Mutex
}

func New(d services.Deps) core.Adapter {
	return newAdapter(d)
}

func newAdapter(d services.Deps) *adapter {
	a := &adapter{
		deps:     d,
		purgeURL: "http://varnish:80/",
		client:   &http.Client{Timeout: execTimeout},
		now:      time.Now,
		envs:     map[string]map[string]string{},
	}
	a.history = metrics.NewHistory(metricSpec).WithClock(func() time.Time { return a.now() })
	return a
}

func (a *adapter) Kind() core.Kind {
	return core.Varnish
}

func (a *adapter) container(ctx context.Context) (*docker.Container, error) {
	c, err := a.deps.Docker.Service(ctx, service)
	if err != nil {
		return nil, err
	}
	if c == nil {
		return nil, errNotRunning
	}
	return c, nil
}

func (a *adapter) exec(ctx context.Context, timeout time.Duration, cmd ...string) (docker.ExecResult, error) {
	c, err := a.container(ctx)
	if err != nil {
		return docker.ExecResult{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	return a.deps.Docker.Exec(ctx, docker.ExecSpec{Container: c.ID, Cmd: cmd})
}

func (a *adapter) adm(ctx context.Context, args ...string) ([]byte, error) {
	res, err := a.exec(ctx, execTimeout, append([]string{"varnishadm"}, args...)...)
	if err != nil {
		return nil, err
	}
	if res.ExitCode != 0 {
		return nil, fmt.Errorf("varnishadm %s: %s", args[0], outputText(res))
	}
	return res.Stdout, nil
}

func outputText(res docker.ExecResult) string {
	text := strings.TrimSpace(strings.TrimSpace(string(res.Stdout)) + "\n" + strings.TrimSpace(string(res.Stderr)))
	if text == "" {
		return fmt.Sprintf("exit code %d", res.ExitCode)
	}
	return text
}

func (a *adapter) Detect(ctx context.Context) (core.Info, error) {
	info := core.Info{Kind: core.Varnish, Engine: "Varnish", Features: map[string]bool{}}
	c, err := a.deps.Docker.Service(ctx, service)
	if err != nil {
		return info, err
	}
	if c == nil {
		a.setInfo(info)
		return info, nil
	}
	info.Present = true
	info.Container = c.Name

	res, err := a.exec(ctx, execTimeout, "varnishd", "-V")
	if err != nil {
		info.Error = err.Error()
		a.setInfo(info)
		return info, nil
	}
	if m := versionRe.FindStringSubmatch(string(res.Stdout) + string(res.Stderr)); m != nil {
		info.Version = m[1]
		info.Tested = tested(info.Version)
	} else {
		info.Error = "varnishd -V: " + outputText(res)
	}

	help := func(cmd ...string) string {
		r, err := a.exec(ctx, execTimeout, cmd...)
		if err != nil {
			return ""
		}
		return string(r.Stdout) + string(r.Stderr)
	}
	info.Features["unbuffered"] = strings.Contains(help("varnishlog", "-h"), "[-u]")
	info.Features["ncsaJson"] = strings.Contains(help("varnishncsa", "-h"), "[-j]")
	adm := help("varnishadm", "help")
	info.Features["varnishadm"] = strings.Contains(adm, "ban.list")
	info.Features["admJson"] = strings.Contains(adm, "ban.list [-j]")
	a.setInfo(info)
	return info, nil
}

func tested(version string) bool {
	parts := strings.SplitN(version, ".", 3)
	if len(parts) < 2 {
		return false
	}
	mm := parts[0] + "." + parts[1]
	return mm == "7.1" || mm == "8.0"
}

func (a *adapter) setInfo(info core.Info) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.info = info
	a.detected = true
}

func (a *adapter) currentInfo(ctx context.Context) core.Info {
	a.mu.Lock()
	info, ok := a.info, a.detected
	a.mu.Unlock()
	if ok {
		return info
	}
	info, _ = a.Detect(ctx)
	return info
}

func (a *adapter) containerEnv(ctx context.Context, c *docker.Container) map[string]string {
	a.mu.Lock()
	env, ok := a.envs[c.ID]
	a.mu.Unlock()
	if ok {
		return env
	}
	list, err := a.deps.Docker.ContainerEnv(ctx, c.ID)
	if err != nil {
		return map[string]string{}
	}
	env = map[string]string{}
	for _, kv := range list {
		if k, v, ok := strings.Cut(kv, "="); ok {
			env[k] = v
		}
	}
	a.mu.Lock()
	a.envs[c.ID] = env
	a.mu.Unlock()
	return env
}

func (a *adapter) remember(r core.VarnishReq) {
	r.Raw = nil
	a.mu.Lock()
	defer a.mu.Unlock()
	if len(a.ring) < ringSize {
		a.ring = append(a.ring, r)
		return
	}
	a.ring[a.ringNext] = r
	a.ringNext = (a.ringNext + 1) % ringSize
}

func (a *adapter) recent() ([]core.VarnishReq, int) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return slices.Clone(a.ring), a.watchers
}

func (a *adapter) watch(delta int) {
	a.mu.Lock()
	a.watchers += delta
	a.mu.Unlock()
}

func (a *adapter) addSample(values map[string]uint64) (newest, oldest sample, prev sample, havePrev bool) {
	now := a.now()
	keep := make(map[string]uint64, len(windowCounters))
	for _, k := range windowCounters {
		if v, ok := values[k]; ok {
			keep[k] = v
		}
	}
	cur := sample{at: now, values: keep}

	a.mu.Lock()
	defer a.mu.Unlock()
	if n := len(a.samples); n > 0 {
		prev, havePrev = a.samples[n-1], true
		if now.Sub(prev.at) < sampleEvery {
			if n > 1 {
				prev = a.samples[n-2]
			} else {
				havePrev = false
			}
			a.samples[n-1] = cur
		} else {
			a.samples = append(a.samples, cur)
		}
	} else {
		a.samples = append(a.samples, cur)
	}
	cut := 0
	for cut < len(a.samples)-1 && now.Sub(a.samples[cut+1].at) >= hitWindow {
		cut++
	}
	a.samples = a.samples[cut:]
	return cur, a.samples[0], prev, havePrev
}

type MissRow struct {
	URL    string `json:"url"`
	Misses int    `json:"misses"`
	Reason string `json:"reason"`
}

type BanRow struct {
	Ban
	FullFlush bool `json:"fullFlush"`
}

type HealthRaw struct {
	Counters  map[string]uint64  `json:"counters"`
	Rates     map[string]float64 `json:"rates"`
	TopMisses []MissRow          `json:"topMisses"`
	Live      bool               `json:"live"`
	Seen      int                `json:"seen"`
	Bans      []BanRow           `json:"bans"`
	Backends  []Backend          `json:"backends"`
}

func (a *adapter) Health(ctx context.Context) (core.Health, error) {
	if h, ok := a.cachedHealth(); ok {
		return h, nil
	}
	a.healthMu.Lock()
	defer a.healthMu.Unlock()
	if h, ok := a.cachedHealth(); ok {
		return h, nil
	}
	h, err := a.health(ctx)
	if err == nil {
		a.mu.Lock()
		a.lastHealth, a.healthAt = &h, a.now()
		a.mu.Unlock()
	}
	return h, err
}

func (a *adapter) cachedHealth() (core.Health, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.lastHealth == nil || a.now().Sub(a.healthAt) >= healthTTL {
		return core.Health{}, false
	}
	return *a.lastHealth, true
}

// forgetHealth drops the cached Health, so Recent bans shows a purge at once.
func (a *adapter) forgetHealth() {
	a.mu.Lock()
	a.lastHealth = nil
	a.mu.Unlock()
}

func (a *adapter) health(ctx context.Context) (core.Health, error) {
	c, err := a.container(ctx)
	if errors.Is(err, errNotRunning) {
		return core.Health{Status: core.ToneInfo, Summary: "No varnish service in this environment"}, nil
	}
	if err != nil {
		return core.Health{}, err
	}
	counters, err := a.statWithin(ctx, statReuse)
	if err != nil {
		return core.Health{}, err
	}

	var backends []Backend
	if out, err := a.adm(ctx, "backend.list", "-j"); err == nil {
		backends, _ = ParseBackends(out)
	}
	var bans []Ban
	if out, err := a.adm(ctx, "ban.list", "-j"); err == nil {
		bans, _ = ParseBans(out)
	}
	env := a.containerEnv(ctx, c)
	cur, oldest, prev, havePrev := a.addSample(counters)
	reqs, watchers := a.recent()

	raw := HealthRaw{Counters: counters, Rates: map[string]float64{}, Live: watchers > 0, Seen: len(reqs), Backends: backends}
	if havePrev {
		raw.Rates = Rates(prev.values, cur.values, cur.at.Sub(prev.at), windowCounters...)
	}
	raw.TopMisses = missRows(reqs, bans)
	raw.Bans = banRows(bans)

	h := core.Health{Status: core.OK, Raw: raw}
	windowed := cur.at.Sub(oldest.at) > 0
	window := func(name string) uint64 {
		if windowed {
			return delta(oldest.values, cur.values, name)
		}
		return counters[name]
	}

	// The same ratio as the hit_ratio series; see metricSpec.
	hits := window("MAIN.cache_hit")
	misses := window("MAIN.cache_miss")
	passes := window("MAIN.s_pass")
	hitCard := core.Card{Label: "Hit rate, last 5 min", Value: "–", Tone: core.ToneInfo}
	rate := ""
	if all := hits + misses + passes; all > 0 {
		pct := formatPct(float64(hits) / float64(all))
		hitCard.Value = pct
		hitCard.Sub = fmt.Sprintf("%s hits · %s misses · %s passes (not cacheable)", formatInt(hits), formatInt(misses), formatInt(passes))
		rate = pct + " hit rate"
	} else {
		hitCard.Sub = "no requests in the last 5 min"
	}
	if !windowed {
		hitCard.Sub += " since Varnish started"
		if hits+misses+passes == 0 {
			hitCard.Sub = "no requests since Varnish started"
		}
	}

	used := counters["SMA.s0.g_bytes"]
	size, ok := parseSize(env["CACHE_SIZE"])
	if !ok {
		size = used + counters["SMA.s0.g_space"]
	}
	pagesCard := core.Card{Label: "Cached pages", Value: formatInt(counters["MAIN.n_object"]), Tone: core.ToneInfo}
	if size > 0 {
		pagesCard.Sub = fmt.Sprintf("%s of %s", formatBytes(used), formatBytes(size))
	} else {
		pagesCard.Sub = formatBytes(used)
	}

	backendCard := core.Card{Label: "Backend", Value: "Unknown", Tone: core.ToneInfo}
	if host := env["BACKEND_HOST"]; host != "" {
		backendCard.Label = "Backend (" + host
		if port := env["BACKEND_PORT"]; port != "" {
			backendCard.Label += ":" + port
		}
		backendCard.Label += ")"
	}
	backendState := ""
	if len(backends) > 0 {
		backendCard.Value, backendCard.Tone, backendState = "Healthy", core.OK, "backend healthy"
		for _, b := range backends {
			if !b.Healthy() {
				backendCard.Value, backendCard.Tone, backendState = "Sick", core.Error, "backend sick"
				h.Status = core.Error
				h.Warnings = append(h.Warnings, core.Warning{
					ID:   "varnish.backend-sick." + b.Name,
					Text: fmt.Sprintf("Backend %s is sick (%d of %d probes good)", b.Name, b.Good, b.Window),
					Tone: core.Error,
				})
			}
		}
	}
	if avg, n := avgFetch(reqs); n > 0 {
		backendCard.Sub = "avg fetch " + formatMillis(avg)
	} else {
		backendCard.Sub = "no fetches seen yet"
	}
	if fails := window("MAIN.backend_fail"); fails > 0 && windowed {
		backendCard.Sub = fmt.Sprintf("%s connection failures", formatInt(fails))
		h.Warnings = append(h.Warnings, core.Warning{
			ID:   "varnish.backend-fail",
			Text: fmt.Sprintf("Varnish failed to connect to the backend %s times in the last 5 min", formatInt(fails)),
			Tone: core.Warn,
		})
	}

	nuked := counters["MAIN.n_lru_nuked"]
	lruCard := core.Card{Label: "Pushed out (LRU)", Value: formatInt(nuked), Sub: "cache is big enough", Tone: core.OK}
	if recentNuked := window("MAIN.n_lru_nuked"); windowed && recentNuked > 0 {
		lruCard.Sub = fmt.Sprintf("%s in the last 5 min, cache is full", formatInt(recentNuked))
		lruCard.Tone = core.Warn
		h.Warnings = append(h.Warnings, core.Warning{
			ID:   "varnish.lru",
			Text: fmt.Sprintf("Varnish pushed %s objects out of a full cache in the last 5 min. Raise CACHE_SIZE on the varnish service.", formatInt(recentNuked)),
			Tone: core.Warn,
		})
	} else if nuked > 0 {
		lruCard.Sub = "none in the last 5 min"
	}

	h.Cards = []core.Card{hitCard, pagesCard, backendCard, lruCard}
	if h.Status == core.OK && len(h.Warnings) > 0 {
		h.Status = core.Warn
	}
	parts := []string{"Varnish"}
	if v := a.currentInfo(ctx).Version; v != "" {
		parts[0] += " " + v
	}
	for _, p := range []string{rate, backendState} {
		if p != "" {
			parts = append(parts, p)
		}
	}
	h.Summary = strings.Join(parts, " · ")
	return h, nil
}

func missRows(reqs []core.VarnishReq, bans []Ban) []MissRow {
	type agg struct {
		count   int
		reasons map[string]int
	}
	byURL := map[string]*agg{}
	for i := range reqs {
		r := &reqs[i]
		if r.Handling != "MISS" && r.Handling != "PASS" {
			continue
		}
		if r.Method != "GET" && r.Method != "HEAD" {
			continue
		}
		path, _, _ := strings.Cut(r.URL, "?")
		if assetPath(path) {
			continue
		}
		g := byURL[path]
		if g == nil {
			g = &agg{reasons: map[string]int{}}
			byURL[path] = g
		}
		g.count++
		g.reasons[explainBan(r, bans)]++
	}
	rows := make([]MissRow, 0, len(byURL))
	for url, g := range byURL {
		best, bestN := "", -1
		for reason, n := range g.reasons {
			if n > bestN || (n == bestN && reason < best) {
				best, bestN = reason, n
			}
		}
		rows = append(rows, MissRow{URL: url, Misses: g.count, Reason: best})
	}
	slices.SortFunc(rows, func(x, y MissRow) int {
		if x.Misses != y.Misses {
			return y.Misses - x.Misses
		}
		return strings.Compare(x.URL, y.URL)
	})
	if len(rows) > topMisses {
		rows = rows[:topMisses]
	}
	return rows
}

// Static and media files pass through Varnish by design in Magento's VCL, so
// they would bury the pages this table is meant to show.
func assetPath(path string) bool {
	path = strings.TrimPrefix(path, "/pub")
	return strings.HasPrefix(path, "/static/") || strings.HasPrefix(path, "/media/")
}

func explainBan(r *core.VarnishReq, bans []Ban) string {
	if r.Reason != "banned by a purge" {
		return r.Reason
	}
	var best *Ban
	for i := range bans {
		b := &bans[i]
		if b.Spec == "" || (!r.Start.IsZero() && b.Time.After(r.Start)) {
			continue
		}
		if best != nil && !b.Time.After(best.Time) {
			continue
		}
		if banMatches(b.Spec, r) != "" {
			best = b
		}
	}
	if best == nil {
		return r.Reason
	}
	return banMatches(best.Spec, r)
}

func banMatches(spec string, r *core.VarnishReq) string {
	if m := tagBanRe.FindStringSubmatch(spec); m != nil {
		if fullFlush(spec) {
			return "banned by a full flush"
		}
		re, err := regexp.Compile(m[1])
		if err != nil {
			return ""
		}
		for _, t := range r.Tags {
			if re.MatchString(t) {
				return "banned by tag " + t
			}
		}
		return ""
	}
	if m := urlBanRe.FindStringSubmatch(spec); m != nil {
		if m[1] == "." {
			return "banned by a full flush"
		}
		re, err := regexp.Compile(m[1])
		if err == nil && re.MatchString(r.URL) {
			return "banned by URL ban " + m[1]
		}
	}
	return ""
}

func fullFlush(spec string) bool {
	switch spec {
	case "obj.http.X-Magento-Tags ~ .*", "obj.http.X-Magento-Tags ~ ((^|,).*(,|$))", "req.url ~ .":
		return true
	}
	return false
}

func banRows(bans []Ban) []BanRow {
	rows := make([]BanRow, 0, len(bans))
	for _, b := range bans {
		if b.Spec == "" {
			continue
		}
		rows = append(rows, BanRow{Ban: b, FullFlush: fullFlush(b.Spec)})
	}
	slices.SortFunc(rows, func(x, y BanRow) int { return y.Time.Compare(x.Time) })
	if len(rows) > recentBans {
		rows = rows[:recentBans]
	}
	return rows
}

func avgFetch(reqs []core.VarnishReq) (time.Duration, int) {
	var total time.Duration
	n := 0
	for _, r := range reqs {
		if r.BereqVXID == "" || r.BereqStart.IsZero() || r.BereqEnd.IsZero() || r.Handling == "PIPE" {
			continue
		}
		total += r.BereqEnd.Sub(r.BereqStart)
		n++
	}
	if n == 0 {
		return 0, 0
	}
	return total / time.Duration(n), n
}

func (a *adapter) Browse() []core.Route {
	return []core.Route{
		{Method: http.MethodGet, Path: "/metrics", Handler: a.history.ServeHTTP, Params: []core.Param{core.WindowParam}},
		{Method: http.MethodGet, Path: "/bans", Handler: a.handleAdm("ban.list", func(b []byte) (any, error) { return ParseBans(b) })},
		{Method: http.MethodGet, Path: "/vcl", Handler: a.handleAdm("vcl.list", func(b []byte) (any, error) { return ParseVCLs(b) })},
		{Method: http.MethodGet, Path: "/vcl/{name}", Handler: a.handleVCLShow},
		{Method: http.MethodGet, Path: "/backends", Handler: a.handleAdm("backend.list", func(b []byte) (any, error) { return ParseBackends(b) })},
		{Method: http.MethodGet, Path: "/storage", Handler: a.handleAdm("storage.list", func(b []byte) (any, error) { return ParseStorages(b) })},
		{Method: http.MethodGet, Path: "/params", Handler: a.handleParams},
		{Method: http.MethodGet, Path: "/panic", Handler: a.handlePanic},
		{Method: http.MethodGet, Path: "/search", Handler: a.handleSearch, Params: []core.Param{
			{Name: "q", Type: "string", Description: "A varnishlog query such as ReqURL ~ \"^/checkout\". One of q, url or tag is required."},
			{Name: "url", Type: "string", Description: "Only requests whose URL contains this text."},
			{Name: "tag", Type: "string", Description: "Only responses carrying this Magento cache tag."},
		}},
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, err error) {
	status := http.StatusBadGateway
	var bad badRequest
	switch {
	case errors.Is(err, errNotRunning):
		status = http.StatusServiceUnavailable
	case errors.As(err, &bad):
		status = http.StatusBadRequest
	case errors.Is(err, errUnknownVCL):
		status = http.StatusNotFound
	}
	writeJSON(w, status, services.ErrorBody(status, err.Error()))
}

type badRequest struct{ msg string }

func (b badRequest) Error() string { return b.msg }

var errUnknownVCL = errors.New("unknown VCL name")

func (a *adapter) handleAdm(cmd string, parse func([]byte) (any, error)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		out, err := a.adm(r.Context(), cmd, "-j")
		if err != nil {
			writeErr(w, err)
			return
		}
		v, err := parse(out)
		if err != nil {
			writeErr(w, err)
			return
		}
		writeJSON(w, http.StatusOK, v)
	}
}

func (a *adapter) handleParams(w http.ResponseWriter, r *http.Request) {
	out, err := a.adm(r.Context(), "param.show", "-j", "changed")
	if err != nil {
		writeErr(w, err)
		return
	}
	ps, err := ParseParams(out)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, ps)
}

func (a *adapter) handleVCLShow(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	out, err := a.adm(r.Context(), "vcl.list", "-j")
	if err != nil {
		writeErr(w, err)
		return
	}
	vcls, err := ParseVCLs(out)
	if err != nil {
		writeErr(w, err)
		return
	}
	if !slices.ContainsFunc(vcls, func(v VCL) bool { return v.Name == name }) {
		writeErr(w, fmt.Errorf("%w: %q", errUnknownVCL, name))
		return
	}
	quoted, err := QuoteCLI(name)
	if err != nil {
		writeErr(w, badRequest{err.Error()})
		return
	}
	src, err := a.adm(r.Context(), "vcl.show", "-v", quoted)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"name": name, "files": SplitVCLShow(string(src))})
}

func (a *adapter) handlePanic(w http.ResponseWriter, r *http.Request) {
	res, err := a.exec(r.Context(), execTimeout, "varnishadm", "panic.show")
	if err != nil {
		writeErr(w, err)
		return
	}
	panicked, text := panicState(res.ExitCode, string(res.Stdout), string(res.Stderr))
	if res.ExitCode != 0 && panicked {
		writeErr(w, fmt.Errorf("varnishadm panic.show: %s", outputText(res)))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"panic": panicked, "text": text})
}

func (a *adapter) handleSearch(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	query, err := buildQuery(q.Get("q"), q.Get("url"), q.Get("tag"))
	if err != nil {
		writeErr(w, err)
		return
	}
	if query == "" {
		writeErr(w, badRequest{"give q, url or tag"})
		return
	}
	reqs, truncated, err := a.search(r.Context(), query)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"query": query, "requests": reqs, "truncated": truncated})
}

func (a *adapter) search(ctx context.Context, query string) ([]core.VarnishReq, bool, error) {
	res, err := a.exec(ctx, searchTimeout, "varnishlog", "-d", "-g", "request", "-v", "-k", strconv.Itoa(searchLimit), "-q", query)
	if err != nil {
		return nil, false, err
	}
	if res.ExitCode != 0 {
		return nil, false, badRequest{"varnishlog: " + outputText(res)}
	}
	var reqs []core.VarnishReq
	ParseGroups(strings.NewReader(string(res.Stdout)), func(r core.VarnishReq) { reqs = append(reqs, r) })
	truncated := len(reqs) >= searchLimit
	slices.SortStableFunc(reqs, func(x, y core.VarnishReq) int { return y.Start.Compare(x.Start) })
	if len(reqs) > searchKeep {
		reqs = reqs[:searchKeep]
	}
	if reqs == nil {
		reqs = []core.VarnishReq{}
	}
	return reqs, truncated, nil
}

func buildQuery(q, url, tag string) (string, error) {
	var parts []string
	q = strings.TrimSpace(q)
	if q != "" {
		if strings.Contains(q, "&&") || strings.Contains(q, "||") {
			return "", badRequest{"VSL queries use and, or and not; && and || are syntax errors"}
		}
		if strings.ContainsFunc(q, unicode.IsControl) {
			return "", badRequest{"query contains a control character"}
		}
		parts = append(parts, "("+q+")")
	}
	if url != "" {
		s, err := QuoteCLI(regexp.QuoteMeta(url))
		if err != nil {
			return "", badRequest{"url: " + err.Error()}
		}
		parts = append(parts, "ReqURL ~ "+s)
	}
	if tag != "" {
		s, err := QuoteCLI("(^|,)" + regexp.QuoteMeta(tag) + "(,|$)")
		if err != nil {
			return "", badRequest{"tag: " + err.Error()}
		}
		parts = append(parts, "BerespHeader:X-Magento-Tags ~ "+s)
	}
	return strings.Join(parts, " and "), nil
}

func (a *adapter) Streams() []core.StreamSpec {
	return []core.StreamSpec{{Name: "log", Title: "Varnish log", Filters: []string{"q", "url"}, Start: a.startLog}}
}

func (a *adapter) startLog(ctx context.Context, f core.Filter, emit core.Emit) error {
	query, err := buildQuery(f["q"], f["url"], "")
	if err != nil {
		return err
	}
	c, err := a.container(ctx)
	if err != nil {
		return err
	}
	cmd := []string{"varnishlog", "-g", "request", "-v"}
	if a.currentInfo(ctx).Features["unbuffered"] {
		cmd = append(cmd, "-u")
	}
	if query != "" {
		cmd = append(cmd, "-q", query)
	}
	record := query == ""
	if record {
		a.watch(1)
		defer a.watch(-1)
	}
	p := newGroupParser(func(r core.VarnishReq) {
		if record {
			a.remember(r)
		}
		at := r.Start
		if at.IsZero() {
			at = a.now()
		}
		emit(core.Event{Type: "varnish.req", Time: at, Kind: core.Varnish, Data: r})
	})
	return a.deps.Docker.Stream(ctx, docker.ExecSpec{Container: c.ID, Cmd: cmd}, p.Line)
}

func (a *adapter) Actions() []core.ActionSpec {
	return []core.ActionSpec{
		{Name: "purge-tag", Title: "Purge pages by tag", Destructive: true, Confirm: core.ConfirmPlain, Run: a.thenForget(a.purgeTag)},
		{Name: "ban-url", Title: "Ban URLs by regex", Destructive: true, Confirm: core.ConfirmPlain, Run: a.thenForget(a.banURL)},
		{Name: "purge-all", Title: "Purge all Magento pages", Destructive: true, Confirm: core.ConfirmTyped, Run: a.thenForget(a.purgeAll)},
		{Name: "ban-all", Title: "Ban everything", Destructive: true, Confirm: core.ConfirmTyped, Run: a.thenForget(a.banAll)},
	}
}

func (a *adapter) thenForget(run func(context.Context, core.ActionRequest) (core.ActionResult, error)) func(context.Context, core.ActionRequest) (core.ActionResult, error) {
	return func(ctx context.Context, req core.ActionRequest) (core.ActionResult, error) {
		defer a.forgetHealth()
		return run(ctx, req)
	}
}

func actionValue(req core.ActionRequest, param string) string {
	if v := strings.TrimSpace(req.Target); v != "" {
		return v
	}
	return strings.TrimSpace(req.Params[param])
}

func (a *adapter) purgeTag(ctx context.Context, req core.ActionRequest) (core.ActionResult, error) {
	tag := actionValue(req, "tag")
	if tag == "" {
		return core.ActionResult{}, errors.New("purge-tag needs a tag")
	}
	if strings.ContainsFunc(tag, func(r rune) bool { return unicode.IsControl(r) || r == ',' || r == ' ' }) {
		return core.ActionResult{}, errors.New("a cache tag has no spaces, commas or control characters")
	}
	pattern := "((^|,)" + regexp.QuoteMeta(tag) + "(,|$))"
	call, err := a.sendPurge(ctx, pattern)
	if err != nil {
		return core.ActionResult{}, err
	}
	return core.ActionResult{Message: "Purged pages tagged " + tag, Data: map[string]string{"call": call}}, nil
}

func (a *adapter) purgeAll(ctx context.Context, req core.ActionRequest) (core.ActionResult, error) {
	if req.Target != "all-pages" {
		return core.ActionResult{}, errors.New(`purge-all needs target "all-pages"`)
	}
	call, err := a.sendPurge(ctx, ".*")
	if err != nil {
		return core.ActionResult{}, err
	}
	return core.ActionResult{Message: "Purged all Magento pages", Data: map[string]string{"call": call}}, nil
}

func (a *adapter) sendPurge(ctx context.Context, pattern string) (string, error) {
	if _, err := a.container(ctx); err != nil {
		return "", err
	}
	call := "PURGE " + a.purgeURL + " X-Magento-Tags-Pattern: " + pattern
	r, err := http.NewRequestWithContext(ctx, "PURGE", a.purgeURL, nil)
	if err != nil {
		return "", err
	}
	r.Header.Set("X-Magento-Tags-Pattern", pattern)
	resp, err := a.client.Do(r)
	if err != nil {
		return "", fmt.Errorf("purge: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("varnish answered %s to %s: %s", resp.Status, call, strings.TrimSpace(string(body)))
	}
	return call, nil
}

func (a *adapter) banURL(ctx context.Context, req core.ActionRequest) (core.ActionResult, error) {
	expr := actionValue(req, "regex")
	if expr == "" {
		return core.ActionResult{}, errors.New("ban-url needs a regex")
	}
	if _, err := regexp.Compile(expr); err != nil {
		return core.ActionResult{}, fmt.Errorf("invalid regex: %w", err)
	}
	if strings.Contains(expr, `\Q`) {
		return core.ActionResult{}, errors.New(`\Q…\E is not supported in a ban regex`)
	}
	// varnishd splits a ban argument on spaces even inside quotes, so a space goes in as the PCRE escape \x20.
	quoted, err := QuoteCLI(strings.ReplaceAll(expr, " ", `\x20`))
	if err != nil {
		return core.ActionResult{}, err
	}
	if _, err := a.adm(ctx, "ban", "req.url", "~", quoted); err != nil {
		return core.ActionResult{}, err
	}
	return core.ActionResult{
		Message: "Banned URLs matching " + expr + ". The ban lurker cannot clean req.url bans, so this ban stays in ban.list until every older object has been looked up or has expired.",
		Data:    map[string]string{"call": "varnishadm ban req.url ~ " + quoted},
	}, nil
}

func (a *adapter) banAll(ctx context.Context, req core.ActionRequest) (core.ActionResult, error) {
	if req.Target != "everything" {
		return core.ActionResult{}, errors.New(`ban-all needs target "everything"`)
	}
	if _, err := a.adm(ctx, "ban", "req.url", "~", "."); err != nil {
		return core.ActionResult{}, err
	}
	return core.ActionResult{Message: "Banned every object in the cache", Data: map[string]string{"call": "varnishadm ban req.url ~ ."}}, nil
}

func parseSize(s string) (uint64, bool) {
	s = strings.TrimSpace(strings.ToLower(s))
	if s == "" {
		return 0, false
	}
	mult := uint64(1)
	switch s[len(s)-1] {
	case 'k':
		mult = 1 << 10
	case 'm':
		mult = 1 << 20
	case 'g':
		mult = 1 << 30
	case 't':
		mult = 1 << 40
	}
	if mult > 1 {
		s = s[:len(s)-1]
	}
	n, err := strconv.ParseUint(s, 10, 64)
	if err != nil {
		return 0, false
	}
	return n * mult, true
}

func formatInt(n uint64) string {
	s := strconv.FormatUint(n, 10)
	if len(s) <= 3 {
		return s
	}
	var b strings.Builder
	lead := len(s) % 3
	if lead > 0 {
		b.WriteString(s[:lead])
	}
	for i := lead; i < len(s); i += 3 {
		if b.Len() > 0 {
			b.WriteByte(',')
		}
		b.WriteString(s[i : i+3])
	}
	return b.String()
}

func formatBytes(n uint64) string {
	switch {
	case n >= 1<<30:
		return strconv.FormatFloat(float64(n)/(1<<30), 'f', 1, 64) + " GB"
	case n >= 1<<20:
		return strconv.FormatUint(n>>20, 10) + " MB"
	case n >= 1<<10:
		return strconv.FormatUint(n>>10, 10) + " KB"
	}
	return strconv.FormatUint(n, 10) + " B"
}

func formatMillis(d time.Duration) string {
	return strconv.FormatInt(d.Round(time.Millisecond).Milliseconds(), 10) + " ms"
}

// formatPct shows one decimal, like the hit ratio donut.
func formatPct(ratio float64) string {
	return strconv.FormatFloat(ratio*100, 'f', 1, 64) + "%"
}
