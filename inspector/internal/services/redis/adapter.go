package redis

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	goredis "github.com/redis/go-redis/v9"

	"github.com/dockergiant/images/inspector/internal/core"
	"github.com/dockergiant/images/inspector/internal/magento"
	"github.com/dockergiant/images/inspector/internal/metrics"
	"github.com/dockergiant/images/inspector/internal/procmap"
	"github.com/dockergiant/images/inspector/internal/services"
)

const (
	defaultAddr   = "redis:6379"
	healthEvery   = 2 * time.Second
	sampleWindow  = 10 * time.Second
	hitSpan       = 5 * time.Minute
	sampleStale   = 5 * time.Minute
	layoutTTL     = 30 * time.Second
	maxValueBytes = 1 << 20
	deleteChunk   = 200
	maxDB         = 1 << 10
)

type procMap interface {
	Lookup(ip string, port int) (procmap.Proc, bool)
	Miss(ip string, port int)
	ContainerOf(ip string) (string, bool)
}

type MonitorStatus struct {
	Banner bool   `json:"banner"`
	Text   string `json:"text"`
}

type sample struct {
	at                       time.Time
	hits, misses, cmds       int64
	evicted, expired         int64
	ownHits, ownMisses, ownN int64
}

type layoutEntry struct {
	layout string
	ns     string
	at     time.Time
}

type adapter struct {
	deps services.Deps
	proc procMap
	log  *slog.Logger
	addr string

	ownHits, ownMisses, ownCmds atomic.Int64
	history                     *metrics.History

	sampleMu   sync.Mutex
	mu         sync.Mutex
	clients    map[int]*goredis.Client
	engine     string
	version    string
	features   map[string]bool
	prev       *sample
	lastHealth *core.Health
	lastAt     time.Time
	layouts    map[int]layoutEntry
}

func New(d services.Deps) core.Adapter {
	a := &adapter{
		deps:    d,
		log:     d.Log,
		addr:    defaultAddr,
		clients: map[int]*goredis.Client{},
		layouts: map[int]layoutEntry{},
		history: metrics.NewHistory(metricSpec),
	}
	if d.Proc != nil {
		a.proc = d.Proc
	}
	if a.log == nil {
		a.log = slog.New(slog.DiscardHandler)
	}
	return a
}

func (a *adapter) Kind() core.Kind {
	return core.Redis
}

func (a *adapter) client(db int) *goredis.Client {
	a.mu.Lock()
	defer a.mu.Unlock()
	if c := a.clients[db]; c != nil {
		return c
	}
	c := goredis.NewClient(&goredis.Options{
		Addr:            a.addr,
		DB:              db,
		ClientName:      inspectorTag,
		Protocol:        2,
		DisableIdentity: true,
		DialTimeout:     2 * time.Second,
		ReadTimeout:     10 * time.Second,
		WriteTimeout:    5 * time.Second,
		PoolSize:        4,
		MaxRetries:      1,
		OnConnect: func(ctx context.Context, cn *goredis.Conn) error {
			// Before 7.2 this errors; reads then bump LRU/LFU, which is acceptable.
			_ = cn.Do(ctx, "CLIENT", "NO-TOUCH", "ON").Err()
			return nil
		},
	})
	c.AddHook(ownHook{a: a})
	a.clients[db] = c
	return c
}

func (a *adapter) feature(name string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.features == nil {
		return Features("redis", "7.2")[name]
	}
	return a.features[name]
}

func (a *adapter) Detect(ctx context.Context) (core.Info, error) {
	info := core.Info{Kind: core.Redis}
	if a.deps.Docker != nil {
		c, err := a.deps.Docker.Service(ctx, "redis")
		if err != nil {
			info.Error = err.Error()
			return info, nil
		}
		if c == nil {
			return info, nil
		}
		info.Container = c.Name
	}
	info.Present = true
	text, err := a.client(0).Info(ctx, "server").Result()
	if err != nil {
		info.Error = err.Error()
		return info, nil
	}
	engine, version := DetectEngine(ParseInfo(text))
	features := Features(engine, version)
	a.probe(ctx, features)
	a.mu.Lock()
	a.engine, a.version, a.features = engine, version, features
	a.mu.Unlock()
	info.Engine = engine
	info.Version = version
	info.Tested = Tested(engine, version)
	info.Features = features
	return info, nil
}

// probe only switches features off: Dragonfly lists commands in COMMAND INFO
// that it does not fully implement.
func (a *adapter) probe(ctx context.Context, features map[string]bool) {
	names := []string{"unlink", "memory", "object", "latency"}
	res, err := a.client(0).Do(ctx, "COMMAND", "INFO", names[0], names[1], names[2], names[3]).Slice()
	if err != nil || len(res) != len(names) {
		return
	}
	keys := map[string][]string{
		"unlink":  {"unlink"},
		"memory":  {"memoryUsage"},
		"object":  {"objectEncoding"},
		"latency": {"latency", "latencyHistogram"},
	}
	for i, n := range names {
		if res[i] == nil {
			for _, k := range keys[n] {
				features[k] = false
			}
		}
	}
}

func (a *adapter) cachedHealth() (core.Health, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.lastHealth != nil && time.Since(a.lastAt) < healthEvery {
		return *a.lastHealth, true
	}
	return core.Health{}, false
}

// baseline picks the sample the deltas are measured against and the one to
// keep for the next call. The kept sample moves only once it is a full
// window old, so the span does not depend on how many tabs poll.
func baseline(prev, cur *sample) (use, keep *sample) {
	if prev != nil && cur.at.Sub(prev.at) > sampleStale {
		prev = nil
	}
	if prev == nil || cur.at.Sub(prev.at) >= sampleWindow {
		return prev, cur
	}
	return prev, prev
}

func (a *adapter) Health(ctx context.Context) (core.Health, error) {
	if h, ok := a.cachedHealth(); ok {
		return h, nil
	}
	a.sampleMu.Lock()
	defer a.sampleMu.Unlock()
	if h, ok := a.cachedHealth(); ok {
		return h, nil
	}

	a.mu.Lock()
	detected := a.engine != ""
	a.mu.Unlock()
	if !detected {
		if _, err := a.Detect(ctx); err != nil {
			return core.Health{}, err
		}
	}
	c := a.client(0)
	text, err := c.Info(ctx, "all").Result()
	if err != nil {
		text, err = c.Info(ctx).Result()
	}
	if err != nil {
		return core.Health{Status: core.Error, Summary: "Cannot reach Redis: " + err.Error(), Cards: []core.Card{}, Warnings: []core.Warning{}}, nil
	}
	info := ParseInfo(text)
	now := time.Now()
	cur := &sample{
		at:        now,
		hits:      info.Int("keyspace_hits"),
		misses:    info.Int("keyspace_misses"),
		cmds:      info.Int("total_commands_processed"),
		evicted:   info.Int("evicted_keys"),
		expired:   info.Int("expired_keys"),
		ownHits:   a.ownHits.Load(),
		ownMisses: a.ownMisses.Load(),
		ownN:      a.ownCmds.Load(),
	}
	a.mu.Lock()
	prev, keep := baseline(a.prev, cur)
	a.prev = keep
	engine, version := a.engine, a.version
	a.mu.Unlock()

	h := buildHealth(info, engine, version, prev, cur, a.hitWindow())
	h.Raw = a.healthRaw(ctx, info)
	a.mu.Lock()
	a.lastHealth = &h
	a.lastAt = now
	a.mu.Unlock()
	return h, nil
}

func engineLabel(engine, version string) string {
	switch engine {
	case "valkey":
		return "Valkey " + version
	case "dragonfly":
		return "Dragonfly " + version
	}
	return "Redis " + version
}

// hitWindow holds the keyspace lookups of the last hitSpan, read from the
// metrics history so the Health card divides exactly as the dashboard donut
// and the Overview tile do. ok is false until the window holds two samples.
type hitWindow struct {
	ok           bool
	hits, misses float64
}

func (a *adapter) hitWindow() hitWindow {
	r := a.history.Query(hitSpan)
	if r.Samples < 2 {
		return hitWindow{}
	}
	return hitWindow{ok: true, hits: float64(r.Totals["hits"]), misses: float64(r.Totals["misses"])}
}

func hitCard(w hitWindow, cur *sample) core.Card {
	hit := core.Card{Label: "Hit ratio, last 5 min", Value: "–", Tone: core.OK}
	hits, misses := w.hits, w.misses
	if !w.ok {
		hits = float64(max(cur.hits-cur.ownHits, 0))
		misses = float64(max(cur.misses-cur.ownMisses, 0))
	}
	switch {
	case hits+misses == 0 && w.ok:
		hit.Sub = "no key lookups in the last 5 min"
		return hit
	case hits+misses == 0:
		hit.Sub = "no key lookups yet"
		return hit
	}
	r := hits / (hits + misses)
	hit.Value = fmt.Sprintf("%.1f%%", 100*r)
	if r < 0.5 {
		hit.Tone = core.Warn
	}
	if w.ok {
		hit.Sub = plural(int(hits), "hit", "hits") + " · " + plural(int(misses), "miss", "misses") + ", without Inspector's reads"
	} else {
		hit.Sub = "since server start, without Inspector's reads, until 5 min are sampled"
	}
	return hit
}

func buildHealth(info Info, engine, version string, prev, cur *sample, win hitWindow) core.Health {
	if engine == "" {
		engine, version = DetectEngine(info)
	}
	var cards []core.Card
	var warnings []core.Warning
	status := core.OK

	badge := "tested"
	if !Tested(engine, version) {
		badge = "best-effort"
	}
	cards = append(cards, core.Card{Label: "Engine", Value: engineLabel(engine, version), Sub: badge, Tone: core.ToneInfo})

	used := info.Int("used_memory")
	max := info.Int("maxmemory")
	mem := core.Card{Label: "Memory", Value: memBytes(used), Sub: "no maxmemory limit", Tone: core.OK}
	if max > 0 {
		mem.Sub = fmt.Sprintf("of %s (%.0f%%)", memBytes(max), 100*float64(used)/float64(max))
		if float64(used) > 0.9*float64(max) {
			mem.Tone = core.Warn
		}
	}
	cards = append(cards, mem)

	hit := hitCard(win, cur)
	cards = append(cards, hit)

	ops := core.Card{Label: "Ops/s", Value: info.Get("instantaneous_ops_per_sec"), Sub: "reported by the server", Tone: core.OK}
	if prev != nil && cur.at.After(prev.at) {
		d := cur.cmds - prev.cmds - (cur.ownN - prev.ownN)
		if d < 0 {
			d = 0
		}
		ops.Value = fmt.Sprintf("%.0f", float64(d)/cur.at.Sub(prev.at).Seconds())
		ops.Sub = "without Inspector's commands"
	}
	cards = append(cards, ops)

	cards = append(cards, core.Card{
		Label: "Clients",
		Value: info.Get("connected_clients"),
		Sub:   fmt.Sprintf("%d blocked", info.Int("blocked_clients")),
		Tone:  core.OK,
	})

	ev := core.Card{Label: "Evictions", Value: strconv.FormatInt(cur.evicted, 10), Sub: "since server start", Tone: core.OK}
	if prev != nil {
		if d := cur.evicted - prev.evicted; d > 0 {
			ev.Sub = fmt.Sprintf("+%d in the last %s", d, roundDur(cur.at.Sub(prev.at)))
			ev.Tone = core.Warn
			warnings = append(warnings, core.Warning{
				ID:   "redis-evictions",
				Text: fmt.Sprintf("%s evicts keys because memory is full (maxmemory-policy %s). Magento recomputes evicted entries on the next request.", engineLabel(engine, version), info.Get("maxmemory_policy")),
				Fix:  "Raise maxmemory for the redis service, or flush caches you do not need",
				Tone: core.Warn,
			})
		}
	}
	cards = append(cards, ev)

	exp := core.Card{Label: "Expirations", Value: strconv.FormatInt(cur.expired, 10), Sub: "since server start", Tone: core.OK}
	if prev != nil && cur.at.After(prev.at) {
		exp.Sub = fmt.Sprintf("+%d in the last %s", cur.expired-prev.expired, roundDur(cur.at.Sub(prev.at)))
	}
	cards = append(cards, exp)

	for _, c := range cards {
		if c.Tone == core.Warn && status == core.OK {
			status = core.Warn
		}
		if c.Tone == core.Error {
			status = core.Error
		}
	}
	var keys int64
	for _, ks := range ParseKeyspace(info) {
		keys += ks.Keys
	}
	summary := fmt.Sprintf("%s · %d keys", engineLabel(engine, version), keys)
	if hit.Value != "–" {
		summary += " · " + hit.Value + " hits"
	}
	if warnings == nil {
		warnings = []core.Warning{}
	}
	return core.Health{Status: status, Summary: summary, Cards: cards, Warnings: warnings}
}

func roundDur(d time.Duration) time.Duration {
	if d > time.Second {
		return d.Round(time.Second)
	}
	return d.Round(time.Millisecond)
}

type slowEntry struct {
	ID         int64     `json:"id"`
	Time       time.Time `json:"time"`
	Micros     int64     `json:"micros"`
	Args       []string  `json:"args"`
	ClientAddr string    `json:"clientAddr,omitempty"`
	ClientName string    `json:"clientName,omitempty"`
}

func (a *adapter) slowlog(ctx context.Context, n int64) []slowEntry {
	logs, err := a.client(0).SlowLogGet(ctx, n).Result()
	out := []slowEntry{}
	if err != nil {
		return out
	}
	for _, l := range logs {
		c := truncateArgs(core.RedisCmd{Args: append([]string{""}, l.Args...)})
		out = append(out, slowEntry{
			ID:         l.ID,
			Time:       l.Time,
			Micros:     l.Duration.Microseconds(),
			Args:       c.Args[1:],
			ClientAddr: l.ClientAddr,
			ClientName: l.ClientName,
		})
	}
	return out
}

func (a *adapter) healthRaw(ctx context.Context, info Info) map[string]any {
	stats := ParseCommandStats(info)
	if len(stats) > 15 {
		stats = stats[:15]
	}
	raw := map[string]any{
		"info":         info,
		"slowlog":      a.slowlog(ctx, 10),
		"commandstats": stats,
	}
	if a.feature("latencyStats") {
		raw["latency"] = ParseLatencyStats(info)
	}
	if a.feature("latencyHistogram") {
		if v, err := a.client(0).Do(ctx, "LATENCY", "HISTOGRAM").Result(); err == nil {
			raw["latencyHistogram"] = histogram(v)
		}
	}
	return raw
}

func histogram(v any) map[string]any {
	out := map[string]any{}
	list, ok := v.([]any)
	if !ok {
		if m, ok := v.(map[any]any); ok {
			for k, val := range m {
				out[fmt.Sprint(k)] = pairs(val)
			}
		}
		return out
	}
	for i := 0; i+1 < len(list); i += 2 {
		out[fmt.Sprint(list[i])] = pairs(list[i+1])
	}
	return out
}

func pairs(v any) any {
	list, ok := v.([]any)
	if !ok || len(list)%2 != 0 {
		return v
	}
	m := map[string]any{}
	for i := 0; i+1 < len(list); i += 2 {
		m[fmt.Sprint(list[i])] = pairs(list[i+1])
	}
	return m
}

func (a *adapter) Streams() []core.StreamSpec {
	return []core.StreamSpec{{Name: "monitor", Title: "Monitor", OptIn: false, Filters: []string{"db", "match"}, Start: a.monitor}}
}

func (a *adapter) monitor(ctx context.Context, f core.Filter, emit core.Emit) error {
	db := -1
	if v := f["db"]; v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 || n >= maxDB {
			return fmt.Errorf("invalid db filter %q", v)
		}
		db = n
	}
	match := f["match"]

	m, err := dialMonitor(ctx, a.addr)
	if err != nil {
		return err
	}
	defer m.Close()
	stop := context.AfterFunc(ctx, func() { m.Close() })
	defer stop()

	own := m.ownIP()
	emit(core.Event{Type: "redis.monitor", Time: time.Now(), Kind: core.Redis, Data: MonitorStatus{
		Banner: true,
		Text:   "MONITOR runs while this view is open. It costs Redis throughput.",
	}})

	lines := make(chan string, 1024)
	var readErr error
	go func() {
		defer close(lines)
		for {
			l, err := m.readLine()
			if err != nil {
				readErr = err
				return
			}
			select {
			case lines <- l:
			case <-ctx.Done():
				return
			}
		}
	}()

	fold := newFolder()
	send := func(cmds []core.RedisCmd) {
		for _, c := range cmds {
			emit(core.Event{Type: "redis.cmd", Time: c.Time, Kind: core.Redis, Data: c})
		}
	}
	tick := time.NewTicker(2 * foldWindow)
	defer tick.Stop()
	var last time.Time
	for {
		select {
		case <-ctx.Done():
			send(fold.FlushAll())
			return ctx.Err()
		case l, ok := <-lines:
			if !ok {
				send(fold.FlushAll())
				if ctx.Err() != nil {
					return ctx.Err()
				}
				if readErr == nil {
					readErr = errors.New("MONITOR connection closed")
				}
				return readErr
			}
			last = time.Now()
			c, err := ParseMonitor(l)
			if err != nil || skip(c, own) {
				continue
			}
			if db >= 0 && c.DB != db {
				continue
			}
			if match != "" && (len(c.Args) == 0 || !glob(match, c.Args[0])) {
				continue
			}
			c = truncateArgs(c)
			a.resolve(&c)
			send(fold.Add(c))
		case <-tick.C:
			if !last.IsZero() && time.Since(last) >= foldWindow {
				send(fold.FlushAll())
			}
		}
	}
}

func (a *adapter) resolve(c *core.RedisCmd) {
	if a.proc == nil || c.IP == "" {
		return
	}
	if p, ok := a.proc.Lookup(c.IP, c.Port); ok {
		c.PID = p.PID
		c.Container = p.Container
		return
	}
	name, ok := a.proc.ContainerOf(c.IP)
	if !ok {
		return
	}
	c.Container = name
	if strings.Contains(name, "php-fpm") || strings.Contains(name, "php-debug") {
		a.proc.Miss(c.IP, c.Port)
	}
}

func (a *adapter) env(ctx context.Context) *magento.Env {
	if a.deps.Magento == nil {
		return nil
	}
	env, err := a.deps.Magento.Load(ctx)
	if err != nil || env == nil || env.Generic {
		return nil
	}
	return env
}

func (a *adapter) layoutOf(ctx context.Context, db int) (string, string) {
	a.mu.Lock()
	if l, ok := a.layouts[db]; ok && time.Since(l.at) < layoutTTL {
		a.mu.Unlock()
		return l.layout, l.ns
	}
	a.mu.Unlock()

	c := a.client(db)
	layout := ""
	if n, _ := c.Exists(ctx, symfonyAllIDs).Result(); n > 0 {
		layout = LayoutSymfony
	} else if n, _ := c.Exists(ctx, cmTags).Result(); n > 0 {
		layout = LayoutCm
	} else if keys, _, err := c.Scan(ctx, 0, "*", 1000).Result(); err == nil {
		layout = DetectLayout(keys)
	}

	ns := ""
	found := false
	if env := a.env(ctx); env != nil {
		for _, fe := range env.Cache {
			if fe.Database == db && fe.IDPrefix != "" {
				ns, found = fe.IDPrefix, true
				break
			}
		}
	}
	if !found && layout == LayoutSymfony {
		ns = a.guessNamespace(ctx, c)
	}

	a.mu.Lock()
	a.layouts[db] = layoutEntry{layout: layout, ns: ns, at: time.Now()}
	a.mu.Unlock()
	return layout, ns
}

func (a *adapter) guessNamespace(ctx context.Context, c *goredis.Client) string {
	var cursor uint64
	for i := 0; i < 5; i++ {
		keys, next, err := c.Scan(ctx, cursor, "*:*", 1000).Result()
		if err != nil {
			return ""
		}
		for _, k := range keys {
			if strings.HasPrefix(k, "cache:") {
				continue
			}
			if ns, _, ok := strings.Cut(k, ":"); ok {
				return ns
			}
		}
		if next == 0 {
			return ""
		}
		cursor = next
	}
	return ""
}

func (a *adapter) forgetLayout(db int) {
	a.mu.Lock()
	delete(a.layouts, db)
	a.mu.Unlock()
}

func (a *adapter) Actions() []core.ActionSpec {
	return []core.ActionSpec{
		{Name: "delete-key", Title: "Delete entry", Destructive: true, Confirm: core.ConfirmPlain, Run: a.deleteKey},
		{Name: "clean-tag", Title: "Clean tag", Destructive: true, Confirm: core.ConfirmPlain, Run: a.cleanTag},
		{Name: "clean-type", Title: "Clean cache type", Destructive: true, Confirm: core.ConfirmPlain, Run: a.cleanType},
		{Name: "flushdb", Title: "Flush database", Destructive: true, Confirm: core.ConfirmTyped, Run: a.flushDB},
	}
}

func parseDB(s string) (int, error) {
	if s == "" {
		return 0, nil
	}
	n, err := strconv.Atoi(s)
	if err != nil || n < 0 || n >= maxDB {
		return 0, fmt.Errorf("invalid database %q", s)
	}
	return n, nil
}

func (a *adapter) deleteKey(ctx context.Context, req core.ActionRequest) (core.ActionResult, error) {
	db, err := parseDB(req.Params["db"])
	if err != nil {
		return core.ActionResult{}, err
	}
	if req.Target == "" {
		return core.ActionResult{}, errors.New("no key given")
	}
	layout, _ := a.layoutOf(ctx, db)
	c := a.client(db)
	n, err := c.Exists(ctx, req.Target).Result()
	if err != nil {
		return core.ActionResult{}, err
	}
	if n == 0 {
		return core.ActionResult{}, fmt.Errorf("key %s does not exist in db %d", req.Target, db)
	}
	if err := a.deleteEntries(ctx, c, layout, []string{req.Target}); err != nil {
		return core.ActionResult{}, err
	}
	msg := fmt.Sprintf("Deleted %s from db %d", req.Target, db)
	if entryID(layout, req.Target) != "" {
		msg += " and removed it from its tag indexes"
	}
	return core.ActionResult{Message: msg}, nil
}

func entryID(layout, key string) string {
	switch layout {
	case LayoutSymfony:
		if strings.HasPrefix(key, "cache:") {
			return ""
		}
		if _, id, ok := strings.Cut(key, ":"); ok {
			return id
		}
	case LayoutCm:
		if strings.HasPrefix(key, cmKey) {
			return strings.TrimPrefix(key, cmKey)
		}
	}
	return ""
}

func dataKey(layout, ns, id string) string {
	if layout == LayoutCm {
		return cmKey + id
	}
	return ns + ":" + id
}

func tagKey(layout, ns, tag string) string {
	if layout == LayoutCm {
		return cmTagIDs + ns + tag
	}
	return symfonyTags + ns + tag
}

// deleteEntries removes entries plus the index entries each layout keeps, so
// a later tag clean in Magento does not trip over dangling ids.
func (a *adapter) deleteEntries(ctx context.Context, c *goredis.Client, layout string, keys []string) error {
	unlink := a.feature("unlink")
	for start := 0; start < len(keys); start += deleteChunk {
		end := min(start+deleteChunk, len(keys))
		chunk := keys[start:end]

		tags := make([][]string, len(chunk))
		read := c.Pipeline()
		sm := make([]*goredis.StringSliceCmd, len(chunk))
		hg := make([]*goredis.StringCmd, len(chunk))
		for i, k := range chunk {
			id := entryID(layout, k)
			if id == "" {
				continue
			}
			if layout == LayoutSymfony {
				ns, _, _ := strings.Cut(k, ":")
				sm[i] = read.SMembers(ctx, symfonyIDTags+ns+id)
			} else {
				hg[i] = read.HGet(ctx, k, "t")
			}
		}
		if read.Len() > 0 {
			if _, err := read.Exec(ctx); err != nil && !errors.Is(err, goredis.Nil) {
				return err
			}
		}
		for i := range chunk {
			switch {
			case sm[i] != nil:
				tags[i] = sm[i].Val()
			case hg[i] != nil:
				for _, t := range strings.Split(hg[i].Val(), ",") {
					if t != "" {
						tags[i] = append(tags[i], t)
					}
				}
			}
		}

		write := c.Pipeline()
		for i, k := range chunk {
			if unlink {
				write.Unlink(ctx, k)
			} else {
				write.Del(ctx, k)
			}
			id := entryID(layout, k)
			if id == "" {
				continue
			}
			if layout == LayoutSymfony {
				ns, _, _ := strings.Cut(k, ":")
				write.SRem(ctx, symfonyAllIDs, id)
				for _, t := range tags[i] {
					write.SRem(ctx, symfonyTags+ns+t, id)
				}
				write.Del(ctx, symfonyIDTags+ns+id)
			} else {
				write.SRem(ctx, cmIDs, id)
				for _, t := range tags[i] {
					write.SRem(ctx, cmTagIDs+t, id)
				}
			}
		}
		if _, err := write.Exec(ctx); err != nil {
			return err
		}
	}
	return nil
}

func (a *adapter) cleanTag(ctx context.Context, req core.ActionRequest) (core.ActionResult, error) {
	db, err := parseDB(req.Params["db"])
	if err != nil {
		return core.ActionResult{}, err
	}
	if req.Target == "" {
		return core.ActionResult{}, errors.New("no tag given")
	}
	layout, ns := a.layoutOf(ctx, db)
	if p, ok := req.Params["prefix"]; ok {
		ns = p
	}
	if layout == "" {
		return core.ActionResult{}, fmt.Errorf("db %d holds no Magento cache layout", db)
	}
	c := a.client(db)
	tag := req.Target
	key := tagKey(layout, ns, tag)
	ids, err := c.SMembers(ctx, key).Result()
	if err != nil {
		return core.ActionResult{}, err
	}
	if len(ids) == 0 {
		// Magento upper-cases and cleans tags before it stores them, so a tag
		// typed the way Varnish shows it (cat_c_41) lives under CAT_C_41.
		norm := magentoTag(tag)
		if norm == tag {
			return core.ActionResult{}, fmt.Errorf("no entries carry tag %s (looked in %s)", tag, key)
		}
		nkey := tagKey(layout, ns, norm)
		if ids, err = c.SMembers(ctx, nkey).Result(); err != nil {
			return core.ActionResult{}, err
		}
		if len(ids) == 0 {
			return core.ActionResult{}, fmt.Errorf("no entries carry tag %s (looked in %s and %s)", tag, key, nkey)
		}
		tag, key = norm, nkey
	}
	keys := make([]string, 0, len(ids))
	for _, id := range ids {
		keys = append(keys, dataKey(layout, ns, id))
	}
	if err := a.deleteEntries(ctx, c, layout, keys); err != nil {
		return core.ActionResult{}, err
	}
	pipe := c.Pipeline()
	pipe.Del(ctx, key)
	if layout == LayoutCm {
		pipe.SRem(ctx, cmTags, ns+tag)
	}
	if _, err := pipe.Exec(ctx); err != nil {
		return core.ActionResult{}, err
	}
	return core.ActionResult{
		Message: fmt.Sprintf("Cleaned tag %s in db %d: %s removed", tag, db, plural(len(keys), "entry", "entries")),
		Data:    map[string]any{"removed": len(keys), "key": key},
	}, nil
}

var nonTagChar = regexp.MustCompile(`[^A-Za-z0-9_]`)

func magentoTag(tag string) string {
	return nonTagChar.ReplaceAllString(strings.ReplaceAll(strings.ToUpper(tag), ".", "__"), "_")
}

var cacheTypeRe = regexp.MustCompile(`^[a-z0-9_]+$`)

func (a *adapter) cleanType(ctx context.Context, req core.ActionRequest) (core.ActionResult, error) {
	if a.deps.Magento == nil {
		return core.ActionResult{}, errors.New("cache types need a Magento project")
	}
	if !cacheTypeRe.MatchString(req.Target) {
		return core.ActionResult{}, fmt.Errorf("invalid cache type %q", req.Target)
	}
	if err := a.deps.Magento.CacheClean(ctx, []string{req.Target}); err != nil {
		return core.ActionResult{}, err
	}
	a.mu.Lock()
	a.layouts = map[int]layoutEntry{}
	a.mu.Unlock()
	return core.ActionResult{Message: "Ran bin/magento cache:clean " + req.Target}, nil
}

func (a *adapter) flushDB(ctx context.Context, req core.ActionRequest) (core.ActionResult, error) {
	if req.Target == "" {
		return core.ActionResult{}, errors.New("no database given")
	}
	db, err := parseDB(req.Target)
	if err != nil {
		return core.ActionResult{}, err
	}
	if err := a.client(db).FlushDB(ctx).Err(); err != nil {
		return core.ActionResult{}, err
	}
	a.forgetLayout(db)
	return core.ActionResult{Message: fmt.Sprintf("Flushed database %d", db)}, nil
}

var lookups = map[string]bool{
	"get": true, "hget": true, "hgetall": true, "strlen": true, "hstrlen": true, "type": true,
	"ttl": true, "pttl": true, "exists": true, "smembers": true, "sscan": true, "hscan": true,
	"scard": true, "hlen": true, "llen": true, "zcard": true, "lrange": true, "zrange": true,
	"object": true, "sismember": true, "xrange": true,
}

// ownHook counts Inspector's own key lookups, because CLIENT INFO has no
// per-connection keyspace_hits and the hit ratio must leave them out.
type ownHook struct {
	a *adapter
}

func (h ownHook) DialHook(next goredis.DialHook) goredis.DialHook {
	return next
}

func (h ownHook) ProcessHook(next goredis.ProcessHook) goredis.ProcessHook {
	return func(ctx context.Context, cmd goredis.Cmder) error {
		err := next(ctx, cmd)
		h.a.count(cmd)
		return err
	}
}

func (h ownHook) ProcessPipelineHook(next goredis.ProcessPipelineHook) goredis.ProcessPipelineHook {
	return func(ctx context.Context, cmds []goredis.Cmder) error {
		err := next(ctx, cmds)
		for _, c := range cmds {
			h.a.count(c)
		}
		return err
	}
}

func (a *adapter) count(cmd goredis.Cmder) {
	a.ownCmds.Add(1)
	if !lookups[strings.ToLower(cmd.Name())] {
		return
	}
	err := cmd.Err()
	if err != nil && !errors.Is(err, goredis.Nil) {
		return
	}
	if lookupHit(cmd) {
		a.ownHits.Add(1)
	} else {
		a.ownMisses.Add(1)
	}
}

func lookupHit(cmd goredis.Cmder) bool {
	if errors.Is(cmd.Err(), goredis.Nil) {
		return false
	}
	switch c := cmd.(type) {
	case *goredis.StatusCmd:
		return c.Val() != "none"
	case *goredis.IntCmd:
		return c.Val() > 0
	case *goredis.DurationCmd:
		return c.Val() != -2
	case *goredis.MapStringStringCmd:
		return len(c.Val()) > 0
	case *goredis.StringSliceCmd:
		return len(c.Val()) > 0
	case *goredis.ScanCmd:
		keys, _ := c.Val()
		return len(keys) > 0
	}
	return true
}

func memBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "KMGTPE"[exp])
}

func plural(n int, one, many string) string {
	if n == 1 {
		return "1 " + one
	}
	return strconv.Itoa(n) + " " + many
}
