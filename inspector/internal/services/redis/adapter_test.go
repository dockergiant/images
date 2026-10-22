package redis

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dockergiant/images/inspector/internal/core"
	"github.com/dockergiant/images/inspector/internal/docker"
	"github.com/dockergiant/images/inspector/internal/procmap"
	"github.com/dockergiant/images/inspector/internal/services"
	goredis "github.com/redis/go-redis/v9"
)

type fakeProc struct {
	mu         sync.Mutex
	procs      map[string]procmap.Proc
	containers map[string]string
	misses     []string
}

func (f *fakeProc) Lookup(ip string, port int) (procmap.Proc, bool) {
	p, ok := f.procs[net.JoinHostPort(ip, strconv.Itoa(port))]
	return p, ok
}

func (f *fakeProc) Miss(ip string, port int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.misses = append(f.misses, net.JoinHostPort(ip, strconv.Itoa(port)))
}

func (f *fakeProc) ContainerOf(ip string) (string, bool) {
	c, ok := f.containers[ip]
	return c, ok
}

type fakeServer struct {
	ln      net.Listener
	replies map[string]func(args []string) string
	monitor []string
	refuse  bool

	mu       sync.Mutex
	commands [][]string
}

func newFakeServer(t *testing.T) *fakeServer {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s := &fakeServer{ln: ln, replies: map[string]func([]string) string{}}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go s.serve(conn)
		}
	}()
	return s
}

func (s *fakeServer) addr() string {
	return s.ln.Addr().String()
}

func (s *fakeServer) seen() [][]string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([][]string(nil), s.commands...)
}

func readCommand(r *bufio.Reader) ([]string, error) {
	line, err := r.ReadString('\n')
	if err != nil {
		return nil, err
	}
	n, err := strconv.Atoi(strings.TrimSpace(line[1:]))
	if err != nil || line[0] != '*' {
		return nil, fmt.Errorf("bad request %q", line)
	}
	args := make([]string, n)
	for i := range args {
		hdr, err := r.ReadString('\n')
		if err != nil {
			return nil, err
		}
		size, _ := strconv.Atoi(strings.TrimSpace(hdr[1:]))
		buf := make([]byte, size+2)
		if _, err := io.ReadFull(r, buf); err != nil {
			return nil, err
		}
		args[i] = string(buf[:size])
	}
	return args, nil
}

func bulk(s string) string {
	return fmt.Sprintf("$%d\r\n%s\r\n", len(s), s)
}

func (s *fakeServer) serve(conn net.Conn) {
	defer conn.Close()
	r := bufio.NewReader(conn)
	for {
		args, err := readCommand(r)
		if err != nil {
			return
		}
		s.mu.Lock()
		s.commands = append(s.commands, args)
		s.mu.Unlock()
		name := strings.ToUpper(args[0])
		if name == "CLIENT" && len(args) > 1 {
			name += " " + strings.ToUpper(args[1])
		}
		switch {
		case name == "MONITOR" && s.refuse:
			io.WriteString(conn, "-ERR MONITOR is disabled\r\n")
		case name == "MONITOR":
			io.WriteString(conn, "+OK\r\n")
			for _, l := range s.monitor {
				io.WriteString(conn, "+"+l+"\r\n")
			}
			io.Copy(io.Discard, r)
			return
		case name == "CLIENT SETNAME" || name == "SELECT" || name == "CLIENT NO-TOUCH":
			io.WriteString(conn, "+OK\r\n")
		case s.replies[name] != nil:
			io.WriteString(conn, s.replies[name](args))
		default:
			io.WriteString(conn, "-ERR unknown command '"+args[0]+"'\r\n")
		}
	}
}

func newTestAdapter(addr string, d services.Deps, proc procMap) *adapter {
	a := New(d).(*adapter)
	a.addr = addr
	a.proc = proc
	return a
}

func collect(t *testing.T, a *adapter, f core.Filter, want int) ([]core.Event, error) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var mu sync.Mutex
	var events []core.Event
	cmds := 0
	done := make(chan error, 1)
	go func() {
		done <- a.monitor(ctx, f, func(ev core.Event) {
			mu.Lock()
			defer mu.Unlock()
			events = append(events, ev)
			if ev.Type == "redis.cmd" {
				cmds++
			}
		})
	}()
	deadline := time.After(3 * time.Second)
	for {
		mu.Lock()
		n := cmds
		mu.Unlock()
		if n >= want {
			break
		}
		select {
		case err := <-done:
			return nil, err
		case <-deadline:
			mu.Lock()
			defer mu.Unlock()
			t.Fatalf("got %d commands, want %d: %+v", n, want, events)
		case <-time.After(5 * time.Millisecond):
		}
	}
	time.Sleep(30 * time.Millisecond)
	cancel()
	err := <-done
	mu.Lock()
	defer mu.Unlock()
	return append([]core.Event(nil), events...), err
}

var monitorLines = []string{
	`1792527779.100000 [0 127.0.0.1:40000] "INFO" "all"`,
	`1792527779.200000 [0 172.20.0.5:41000] "MGET" "69d_:SYSTEM_DEFAULT"`,
	`1792527779.300000 [0 172.20.0.5:41002] "ECHO" "phpredis:6ad7cda38850a:00000058"`,
	`1792527779.300100 [0 172.20.0.5:41002] "GET" "69d_:DICONFIG"`,
	`1792527779.400000 [3 172.20.0.5:41000] "GET" "sess_abc"`,
	`1792527779.500000 [0 172.20.0.5:41002] "SET" "69d_:BURST" "` + strings.Repeat("x", 600) + `"`,
	`1792527779.500500 [0 172.20.0.5:41002] "SADD" "cache:all_ids" "BURST"`,
	`1792527779.501000 [0 172.20.0.5:41002] "SADD" "cache:tags:69d_CMS_P_2" "BURST"`,
	`1792527779.501500 [0 172.20.0.5:41002] "EXPIRE" "69d_:BURST" "7200"`,
	`1792527779.600000 [0 172.20.0.8:50000] "PING"`,
}

func TestMonitorStream(t *testing.T) {
	srv := newFakeServer(t)
	srv.monitor = monitorLines
	proc := &fakeProc{
		procs:      map[string]procmap.Proc{"172.20.0.5:41000": {PID: 42, Container: "php-fpm", Role: "fpm-worker"}},
		containers: map[string]string{"172.20.0.5": "php-fpm", "172.20.0.8": "nginx"},
	}
	a := newTestAdapter(srv.addr(), services.Deps{}, proc)
	events, err := collect(t, a, core.Filter{}, 5)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("stream ended with %v", err)
	}
	if events[0].Type != "redis.monitor" || !events[0].Data.(MonitorStatus).Banner {
		t.Fatalf("first event %+v", events[0])
	}
	var cmds []core.RedisCmd
	for _, ev := range events[1:] {
		if ev.Type != "redis.cmd" || ev.Kind != core.Redis {
			t.Fatalf("event %+v", ev)
		}
		cmds = append(cmds, ev.Data.(core.RedisCmd))
	}
	if len(cmds) != 5 {
		t.Fatalf("%d commands: %+v", len(cmds), cmds)
	}
	if cmds[0].Cmd != "MGET" || cmds[0].PID != 42 || cmds[0].Container != "php-fpm" {
		t.Errorf("mapped worker: %+v", cmds[0])
	}
	if cmds[1].Cmd != "GET" || cmds[1].PID != 0 || cmds[1].Container != "php-fpm" {
		t.Errorf("unmapped worker: %+v", cmds[1])
	}
	if cmds[2].DB != 3 {
		t.Errorf("db 3: %+v", cmds[2])
	}
	burst := cmds[3]
	if burst.Cmd != "SET" || burst.Folded != 4 || burst.Args[0] != "69d_:BURST" || len(burst.Args[1]) > maxArg+32 {
		t.Errorf("burst: %+v", burst)
	}
	if cmds[4].Cmd != "PING" || cmds[4].Container != "nginx" {
		t.Errorf("ping from another container: %+v", cmds[4])
	}
	proc.mu.Lock()
	misses := strings.Join(proc.misses, ",")
	proc.mu.Unlock()
	if !strings.Contains(misses, "172.20.0.5:41002") || strings.Contains(misses, "172.20.0.8") || strings.Contains(misses, "41000") {
		t.Errorf("misses %s", misses)
	}
	seen := srv.seen()
	if len(seen) < 2 || strings.Join(seen[0], " ") != "CLIENT SETNAME roll-inspector" || seen[1][0] != "MONITOR" {
		t.Errorf("handshake %v", seen)
	}
}

func TestMonitorStreamFilters(t *testing.T) {
	srv := newFakeServer(t)
	srv.monitor = monitorLines
	a := newTestAdapter(srv.addr(), services.Deps{}, nil)
	events, err := collect(t, a, core.Filter{"db": "0", "match": "69d_:*"}, 3)
	if !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	var keys []string
	for _, ev := range events {
		if c, ok := ev.Data.(core.RedisCmd); ok {
			keys = append(keys, c.Args[0])
		}
	}
	if strings.Join(keys, ",") != "69d_:SYSTEM_DEFAULT,69d_:DICONFIG,69d_:BURST" {
		t.Errorf("keys %v", keys)
	}
	if err := a.monitor(context.Background(), core.Filter{"db": "x"}, func(core.Event) {}); err == nil {
		t.Error("bad db filter accepted")
	}
}

func TestMonitorRefused(t *testing.T) {
	srv := newFakeServer(t)
	srv.refuse = true
	a := newTestAdapter(srv.addr(), services.Deps{}, nil)
	err := a.monitor(context.Background(), core.Filter{}, func(core.Event) {})
	if err == nil || !strings.Contains(err.Error(), "MONITOR refused") {
		t.Fatalf("err %v", err)
	}
}

func infoReplies(t *testing.T, s *fakeServer, file string) {
	text := readFixture(t, file)
	s.replies["INFO"] = func([]string) string { return bulk(text) }
	s.replies["COMMAND"] = func([]string) string {
		return "*4\r\n*1\r\n" + bulk("unlink") + "*-1\r\n*1\r\n" + bulk("object") + "*1\r\n" + bulk("latency")
	}
	s.replies["SLOWLOG"] = func([]string) string {
		return "*1\r\n*6\r\n:7\r\n:1792528775\r\n:15000\r\n*2\r\n" + bulk("KEYS") + bulk("*") + bulk("172.20.0.5:41000") + bulk("")
	}
	s.replies["LATENCY"] = func([]string) string {
		return "*2\r\n" + bulk("get") + "*4\r\n" + bulk("calls") + ":3\r\n" + bulk("histogram_usec") + "*2\r\n:2\r\n:3\r\n"
	}
}

func TestDetect(t *testing.T) {
	fake := &docker.Fake{EnvName: "shop"}
	a := newTestAdapter("127.0.0.1:1", services.Deps{Docker: fake}, nil)
	info, err := a.Detect(context.Background())
	if err != nil || info.Present || info.Kind != core.Redis {
		t.Fatalf("absent service: %+v %v", info, err)
	}

	srv := newFakeServer(t)
	infoReplies(t, srv, "info-valkey90.txt")
	fake.SetContainers([]docker.Container{{ID: "abc", Name: "shop-valkey-1", Service: "redis", Running: true}})
	a = newTestAdapter(srv.addr(), services.Deps{Docker: fake}, nil)
	info, err = a.Detect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !info.Present || info.Engine != "valkey" || info.Version != "9.0.6" || !info.Tested || info.Container != "shop-valkey-1" {
		t.Errorf("info %+v", info)
	}
	if info.Features["memoryUsage"] || !info.Features["unlink"] || !info.Features["latencyHistogram"] {
		t.Errorf("COMMAND INFO probe: %v", info.Features)
	}
	var named bool
	for _, c := range srv.seen() {
		if strings.EqualFold(strings.Join(c, " "), "client setname roll-inspector") {
			named = true
		}
	}
	if !named {
		t.Errorf("go-redis clients are not named: %v", srv.seen())
	}

	a = newTestAdapter("127.0.0.1:1", services.Deps{Docker: fake}, nil)
	info, _ = a.Detect(context.Background())
	if !info.Present || info.Error == "" {
		t.Errorf("unreachable: %+v", info)
	}
}

func TestHealth(t *testing.T) {
	srv := newFakeServer(t)
	infoReplies(t, srv, "info-valkey81.txt")
	a := newTestAdapter(srv.addr(), services.Deps{}, nil)
	h, err := a.Health(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	labels := map[string]core.Card{}
	for _, c := range h.Cards {
		labels[c.Label] = c
	}
	for _, l := range []string{"Engine", "Memory", "Hit ratio, last 5 min", "Ops/s", "Clients", "Evictions", "Expirations"} {
		if _, ok := labels[l]; !ok {
			t.Errorf("card %s missing", l)
		}
	}
	if labels["Engine"].Value != "Valkey 8.1.10" || labels["Engine"].Sub != "tested" {
		t.Errorf("engine card %+v", labels["Engine"])
	}
	if labels["Memory"].Value != "1.1 MB" {
		t.Errorf("memory card %+v", labels["Memory"])
	}
	if !strings.HasPrefix(h.Summary, "Valkey 8.1.10 · 5 keys") {
		t.Errorf("summary %q", h.Summary)
	}
	raw := h.Raw.(map[string]any)
	if slow := raw["slowlog"].([]slowEntry); len(slow) != 1 || slow[0].Micros != 15000 || slow[0].Args[0] != "KEYS" {
		t.Errorf("slowlog %+v", raw["slowlog"])
	}
	if hist := raw["latencyHistogram"].(map[string]any); hist["get"] == nil {
		t.Errorf("histogram %+v", raw["latencyHistogram"])
	}
	again, _ := a.Health(context.Background())
	if again.Summary != h.Summary {
		t.Error("second call within 2 s should reuse the sample")
	}

	srv.ln.Close()
	b := newTestAdapter(srv.addr(), services.Deps{}, nil)
	h, err = b.Health(context.Background())
	if err != nil || h.Status != core.Error {
		t.Errorf("unreachable health %+v %v", h, err)
	}
}

func TestBuildHealthDeltas(t *testing.T) {
	info := ParseInfo(readFixture(t, "info-valkey90.txt"))
	t0 := time.Unix(1792528000, 0)
	prev := &sample{at: t0, hits: 100, misses: 0, cmds: 1000, evicted: 2, expired: 10}
	cur := &sample{at: t0.Add(2 * time.Second), hits: 190, misses: 30, cmds: 1300, evicted: 7, expired: 14, ownHits: 40, ownMisses: 10, ownN: 100}
	h := buildHealth(info, "valkey", "9.0.6", prev, cur, hitWindow{ok: true, hits: 997, misses: 3})
	cards := map[string]core.Card{}
	for _, c := range h.Cards {
		cards[c.Label] = c
	}
	if hit := cards["Hit ratio, last 5 min"]; hit.Value != "99.7%" || hit.Sub != "997 hits · 3 misses, without Inspector's reads" {
		t.Errorf("hit ratio %+v", hit)
	}
	if !strings.HasSuffix(h.Summary, " · 99.7% hits") {
		t.Errorf("summary %q", h.Summary)
	}
	if cards["Ops/s"].Value != "100" {
		t.Errorf("ops %+v", cards["Ops/s"])
	}
	if cards["Evictions"].Tone != core.Warn || h.Status != core.Warn || len(h.Warnings) != 1 {
		t.Errorf("evictions %+v status %s warnings %v", cards["Evictions"], h.Status, h.Warnings)
	}
	if cards["Expirations"].Sub != "+4 in the last 2s" {
		t.Errorf("expirations %+v", cards["Expirations"])
	}
}

func TestActionsSpec(t *testing.T) {
	a := New(services.Deps{})
	specs := map[string]core.ActionSpec{}
	for _, s := range a.Actions() {
		specs[s.Name] = s
	}
	want := map[string]core.ConfirmLevel{"delete-key": core.ConfirmPlain, "clean-tag": core.ConfirmPlain, "clean-type": core.ConfirmPlain, "flushdb": core.ConfirmTyped}
	for name, level := range want {
		s, ok := specs[name]
		if !ok || s.Confirm != level || !s.Destructive || s.Run == nil {
			t.Errorf("%s: %+v", name, s)
		}
	}
	if len(a.Streams()) != 1 || a.Streams()[0].Name != "monitor" || a.Streams()[0].OptIn {
		t.Errorf("streams %+v", a.Streams())
	}
	paths := map[string]bool{}
	for _, r := range a.Browse() {
		paths[r.Path] = true
	}
	for _, p := range []string{"/types", "/keys", "/key", "/tags", "/tag", "/clients", "/slowlog", "/find"} {
		if !paths[p] {
			t.Errorf("route %s missing", p)
		}
	}
	if _, err := specs["clean-type"].Run(context.Background(), core.ActionRequest{Target: "full_page"}); err == nil {
		t.Error("clean-type without Magento")
	}
	if _, err := specs["flushdb"].Run(context.Background(), core.ActionRequest{Target: "x"}); err == nil {
		t.Error("flushdb with a bad db")
	}
}

func TestCountSkipsMemoryUsage(t *testing.T) {
	a := &adapter{}
	ctx := context.Background()
	for i := 0; i < 3; i++ {
		typ := goredis.NewStatusCmd(ctx, "type", "k")
		typ.SetVal("string")
		ttl := goredis.NewDurationCmd(ctx, time.Millisecond, "pttl", "k")
		ttl.SetVal(time.Second)
		mem := goredis.NewIntCmd(ctx, "memory", "usage", "k")
		mem.SetVal(64)
		for _, c := range []goredis.Cmder{typ, ttl, mem} {
			a.count(c)
		}
	}
	if a.ownHits.Load() != 6 || a.ownMisses.Load() != 0 || a.ownCmds.Load() != 9 {
		t.Errorf("hits %d misses %d cmds %d", a.ownHits.Load(), a.ownMisses.Load(), a.ownCmds.Load())
	}
}

func TestBaselineKeepsTheWindowStable(t *testing.T) {
	t0 := time.Unix(1792528000, 0)
	first := &sample{at: t0}
	if use, keep := baseline(nil, first); use != nil || keep != first {
		t.Fatal("first sample becomes the baseline")
	}
	soon := &sample{at: t0.Add(3 * time.Second)}
	if use, keep := baseline(first, soon); use != first || keep != first {
		t.Error("a second tab's call must not move the baseline")
	}
	due := &sample{at: t0.Add(11 * time.Second)}
	if use, keep := baseline(first, due); use != first || keep != due {
		t.Error("a full window old baseline moves forward")
	}
	late := &sample{at: t0.Add(time.Hour)}
	if use, keep := baseline(first, late); use != nil || keep != late {
		t.Error("an hour-old baseline reads as since server start")
	}
}

func TestBuildHealthIgnoresReversedSamples(t *testing.T) {
	info := ParseInfo(readFixture(t, "info-valkey90.txt"))
	t0 := time.Unix(1792528000, 0)
	prev := &sample{at: t0.Add(2 * time.Second), expired: 14}
	cur := &sample{at: t0, expired: 10}
	h := buildHealth(info, "valkey", "9.0.6", prev, cur, hitWindow{})
	for _, c := range h.Cards {
		if c.Label == "Expirations" && c.Sub != "since server start" {
			t.Errorf("expirations %+v", c)
		}
	}
}

func TestHealthConcurrentCallersShareOneSample(t *testing.T) {
	srv := newFakeServer(t)
	infoReplies(t, srv, "info-valkey81.txt")
	a := newTestAdapter(srv.addr(), services.Deps{}, nil)
	var wg sync.WaitGroup
	sums := make([]string, 8)
	for i := range sums {
		wg.Add(1)
		go func() {
			defer wg.Done()
			h, _ := a.Health(context.Background())
			sums[i] = h.Summary
		}()
	}
	wg.Wait()
	for _, s := range sums {
		if s != sums[0] || s == "" {
			t.Fatalf("summaries differ: %q", sums)
		}
	}
	if a.prev == nil {
		t.Error("no baseline stored")
	}
}

func TestMagentoTag(t *testing.T) {
	for in, want := range map[string]string{"cat_c_41": "CAT_C_41", "CAT_P": "CAT_P", "a.b-c": "A__B_C", "cat_c_p_1": "CAT_C_P_1"} {
		if got := magentoTag(in); got != want {
			t.Errorf("magentoTag(%q) = %q, want %q", in, got, want)
		}
	}
}

// The card divides like the dashboard donut: the history's window, with
// Inspector's own lookups left out, and the lifetime only until it fills.
func TestBuildHealthHitRatioWindow(t *testing.T) {
	info := ParseInfo(readFixture(t, "info-valkey90.txt"))
	t0 := time.Unix(1792528000, 0)
	cur := &sample{at: t0, hits: 190, misses: 30, ownHits: 40, ownMisses: 10}
	card := func(w hitWindow) core.Card {
		for _, c := range buildHealth(info, "valkey", "9.0.6", nil, cur, w).Cards {
			if strings.HasPrefix(c.Label, "Hit ratio") {
				return c
			}
		}
		t.Fatal("no hit ratio card")
		return core.Card{}
	}
	if c := card(hitWindow{ok: true}); c.Value != "–" || c.Sub != "no key lookups in the last 5 min" {
		t.Errorf("idle window %+v", c)
	}
	if c := card(hitWindow{ok: true, hits: 1, misses: 3}); c.Value != "25.0%" || c.Tone != core.Warn || c.Sub != "1 hit · 3 misses, without Inspector's reads" {
		t.Errorf("low window %+v", c)
	}
	if c := card(hitWindow{}); c.Value != "88.2%" || c.Sub != "since server start, without Inspector's reads, until 5 min are sampled" {
		t.Errorf("lifetime fallback %+v", c)
	}
	empty := &sample{at: t0, ownHits: 5}
	for _, c := range buildHealth(info, "valkey", "9.0.6", nil, empty, hitWindow{}).Cards {
		if strings.HasPrefix(c.Label, "Hit ratio") && (c.Value != "–" || c.Sub != "no key lookups yet") {
			t.Errorf("no lookups %+v", c)
		}
	}
}

func TestHealthReadsTheHitRatioFromTheHistory(t *testing.T) {
	srv := newFakeServer(t)
	infoReplies(t, srv, "info-valkey81.txt")
	a := newTestAdapter(srv.addr(), services.Deps{}, nil)
	now := time.Now()
	a.history.Add(now.Add(-2*time.Minute), map[string]float64{"hits": 100, "misses": 0})
	a.history.Add(now.Add(-time.Minute), map[string]float64{"hits": 150, "misses": 5})
	a.history.Add(now, map[string]float64{"hits": 190, "misses": 10})
	h, err := a.Health(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range h.Cards {
		if c.Label == "Hit ratio, last 5 min" {
			if c.Value != "90.0%" || c.Sub != "90 hits · 10 misses, without Inspector's reads" {
				t.Errorf("card %+v", c)
			}
			return
		}
	}
	t.Errorf("no hit ratio card in %+v", h.Cards)
}
