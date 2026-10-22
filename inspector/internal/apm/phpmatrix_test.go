//go:build phpmatrix

package apm

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// TestPHPMatrix checks the captures scripts/php-matrix leaves in PHPMATRIX_DIR,
// one directory per version and run: full, lean, off and debug.
func TestPHPMatrix(t *testing.T) {
	dir := os.Getenv("PHPMATRIX_DIR")
	if dir == "" {
		t.Skip("PHPMATRIX_DIR is not set; scripts/php-matrix sets it")
	}
	runs := loadMatrixRuns(t, dir)
	if len(runs) == 0 {
		t.Fatalf("no run.json under %s", dir)
	}
	full := map[string]*matrixRun{}
	for _, r := range runs {
		if r.Mode == "full" {
			full[r.Version] = r
		}
	}
	for _, r := range runs {
		t.Run(r.Version+"/"+r.Mode, func(t *testing.T) {
			c := &checker{t: t, label: r.Version + "/" + r.Mode}
			switch r.Mode {
			case "off":
				checkOff(c, r)
			default:
				checkTraced(c, r, full[r.Version])
			}
			checkLog(c, r)
		})
	}
}

type matrixRun struct {
	Version string `json:"version"`
	Mode    string `json:"mode"`
	Image   string `json:"image"`

	dir       string
	requests  []capturedRequest
	responses []matrixResponse
	ini       string
	traces    map[string]Trace
}

type capturedRequest struct {
	Method  string            `json:"method"`
	Path    string            `json:"path"`
	Headers map[string]string `json:"headers"`
	body    []byte
}

type matrixResponse struct {
	PHP               string `json:"php"`
	SAPI              string `json:"sapi"`
	TraceID           string `json:"trace_id"`
	DDTraceLoaded     bool   `json:"ddtrace_loaded"`
	DDTraceVersion    string `json:"ddtrace_version"`
	XdebugLoaded      bool   `json:"xdebug_loaded"`
	XdebugStart       string `json:"xdebug_start"`
	MagentoEnabledIni string `json:"magento_enabled_ini"`
	AutoPrependFile   string `json:"auto_prepend_file"`
	DB                string `json:"db"`
	DBRows            int    `json:"db_rows"`
	RedisHits         int    `json:"redis_hits"`
	Ping              string `json:"ping"`

	n      int
	xid    string
	status int
	err    error
}

type checker struct {
	t     *testing.T
	label string
}

// check prints one line per check for the driver and fails the test on a miss.
func (c *checker) check(ok bool, name, detail string) {
	c.t.Helper()
	status := "ok"
	if !ok {
		status = "FAIL"
		c.t.Errorf("%s: %s", name, detail)
	}
	if detail != "" {
		name += ": " + detail
	}
	fmt.Printf("phpmatrix: %-4s %-10s %s\n", status, c.label, name)
}

func loadMatrixRuns(t *testing.T, dir string) []*matrixRun {
	t.Helper()
	var runs []*matrixRun
	err := filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || d.Name() != "run.json" {
			return err
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		r := &matrixRun{dir: filepath.Dir(p)}
		if err := json.Unmarshal(data, r); err != nil {
			return fmt.Errorf("%s: %w", p, err)
		}
		r.load(t)
		runs = append(runs, r)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	order := map[string]int{"full": 0, "lean": 1, "off": 2, "debug": 3}
	sort.Slice(runs, func(i, j int) bool {
		if runs[i].Version != runs[j].Version {
			return runs[i].Version > runs[j].Version
		}
		return order[runs[i].Mode] < order[runs[j].Mode]
	})
	return runs
}

func (r *matrixRun) load(t *testing.T) {
	t.Helper()
	metas, _ := filepath.Glob(filepath.Join(r.dir, "capture", "req-*.json"))
	sort.Strings(metas)
	for _, m := range metas {
		data, err := os.ReadFile(m)
		if err != nil {
			t.Fatal(err)
		}
		var req capturedRequest
		if err := json.Unmarshal(data, &req); err != nil {
			t.Fatalf("%s: %v", m, err)
		}
		req.body, err = os.ReadFile(strings.TrimSuffix(m, ".json") + ".body")
		if err != nil {
			t.Fatal(err)
		}
		r.requests = append(r.requests, req)
	}
	if ini, err := os.ReadFile(filepath.Join(r.dir, "ini.txt")); err == nil {
		r.ini = string(ini)
	}
	f, err := os.Open(filepath.Join(r.dir, "requests.tsv"))
	if err != nil {
		return
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		cols := strings.Split(sc.Text(), "\t")
		if len(cols) != 3 {
			continue
		}
		var resp matrixResponse
		resp.n, _ = strconv.Atoi(cols[0])
		resp.xid = cols[1]
		resp.status, _ = strconv.Atoi(cols[2])
		body, err := os.ReadFile(filepath.Join(r.dir, "response-"+cols[0]+".json"))
		if err == nil {
			resp.err = json.Unmarshal(body, &resp)
		} else {
			resp.err = err
		}
		r.responses = append(r.responses, resp)
	}
}

func (r *matrixRun) posts(prefix string) []capturedRequest {
	var out []capturedRequest
	for _, q := range r.requests {
		if (q.Method == "POST" || q.Method == "PUT") && strings.HasPrefix(q.Path, prefix) {
			out = append(out, q)
		}
	}
	return out
}

var traceIDPattern = regexp.MustCompile(`^[0-9a-f]{32}$`)

func checkResponses(c *checker, r *matrixRun, traced bool) bool {
	ok := len(r.responses) == 3
	var bad []string
	for _, resp := range r.responses {
		switch {
		case resp.status != 200:
			bad = append(bad, fmt.Sprintf("request %d answered %d", resp.n, resp.status))
		case resp.err != nil:
			bad = append(bad, fmt.Sprintf("request %d body: %v", resp.n, resp.err))
		case resp.SAPI != "fpm-fcgi":
			bad = append(bad, fmt.Sprintf("request %d ran on %q", resp.n, resp.SAPI))
		case resp.DBRows != 10 || resp.RedisHits != 10 || resp.Ping != "pong":
			bad = append(bad, fmt.Sprintf("request %d did not finish its work: rows %d, redis %d, ping %q", resp.n, resp.DBRows, resp.RedisHits, resp.Ping))
		case traced && !traceIDPattern.MatchString(resp.TraceID):
			bad = append(bad, fmt.Sprintf("request %d echoed trace id %q", resp.n, resp.TraceID))
		}
	}
	ok = ok && len(bad) == 0
	detail := fmt.Sprintf("%d requests", len(r.responses))
	if len(r.responses) > 0 && r.responses[0].err == nil {
		detail += fmt.Sprintf(", PHP %s, %s, ddtrace %s", r.responses[0].PHP, r.responses[0].DB, orNone(r.responses[0].DDTraceVersion))
	}
	if len(bad) > 0 {
		detail = strings.Join(bad, "; ")
	}
	c.check(ok, "script ran under FPM with PDO, Redis and curl", detail)
	return ok
}

func orNone(s string) string {
	if s == "" {
		return "none"
	}
	return s
}

func checkTraced(c *checker, r *matrixRun, full *matrixRun) {
	if !checkResponses(c, r, true) {
		return
	}
	first := r.responses[0]
	c.check(first.DDTraceLoaded, "ddtrace loaded", "")
	c.check(first.AutoPrependFile == "/usr/local/etc/php/inspector-prepend.php", "Excimer prepend configured", first.AutoPrependFile)
	c.check(strings.Contains(r.ini, "datadog.trace.agent_url=http://inspector:8126") &&
		strings.Contains(r.ini, "datadog.trace.header_tags=x-varnish:varnish.xid"),
		"rendered ini points at inspector:8126 with the x-varnish header tag", "")

	payloads := r.posts("/v0.4/traces")
	r.traces = map[string]Trace{}
	var decodeErrs []string
	for _, p := range payloads {
		traces, err := decodeDD(p.body)
		if err != nil {
			decodeErrs = append(decodeErrs, err.Error())
			continue
		}
		for _, tr := range traces {
			if old, ok := r.traces[tr.ID]; ok {
				tr.Spans = append(old.Spans, tr.Spans...)
			}
			r.traces[tr.ID] = tr
		}
	}
	c.check(len(payloads) > 0 && len(decodeErrs) == 0, "trace payloads decode",
		fmt.Sprintf("%d payloads, %d traces%s", len(payloads), len(r.traces), joinErrs(decodeErrs)))

	maxSpans := 0
	for _, resp := range r.responses {
		id := resp.TraceID
		tr, found := r.traces[id]
		c.check(found, fmt.Sprintf("request %d trace %s arrived", resp.n, id), "")
		if !found {
			continue
		}
		if len(tr.Spans) > maxSpans {
			maxSpans = len(tr.Spans)
		}
		root := tr.Spans[0]
		c.check(tr.Method == "GET" && strings.Contains(tr.URL, "/matrix.php") && tr.Status == 200 && root.Type == "web",
			fmt.Sprintf("request %d root is a web request", resp.n),
			fmt.Sprintf("%s %s %d, type %s, service %s", tr.Method, tr.URL, tr.Status, root.Type, root.Service))
		c.check(root.Service == "matrix", fmt.Sprintf("request %d service is ROLL_ENV_NAME", resp.n), root.Service)
		c.check(tr.VarnishXID == resp.xid, fmt.Sprintf("request %d varnish.xid tag", resp.n),
			fmt.Sprintf("got %q, sent %s", tr.VarnishXID, resp.xid))
		pdo, redis, curl := spanCount(tr, "PDO"), spanCount(tr, "Redis."), spanCount(tr, "curl_exec")
		c.check(pdo > 0 && redis > 0 && curl > 0 && tr.Counts["sql"] > 0 && tr.Counts["cache"] > 0 && tr.Counts["http"] > 0,
			fmt.Sprintf("request %d has PDO, Redis and curl spans", resp.n),
			fmt.Sprintf("%d spans: PDO %d, Redis %d, curl %d; sql %d, cache %d, http %d",
				len(tr.Spans), pdo, redis, curl, tr.Counts["sql"], tr.Counts["cache"], tr.Counts["http"]))
		checkProfile(c, r, resp)
	}

	switch r.Mode {
	case "full":
		c.check(isOn(first.MagentoEnabledIni), "Magento integration on in full", first.MagentoEnabledIni)
	case "lean":
		c.check(isOff(first.MagentoEnabledIni) && strings.Contains(r.ini, "datadog.trace.magento_enabled=0"),
			"lean sets datadog.trace.magento_enabled=0", first.MagentoEnabledIni)
		magento := 0
		for _, tr := range r.traces {
			magento += tr.Counts["magento"]
		}
		c.check(magento == 0, "lean has no Magento spans", strconv.Itoa(magento))
		if full != nil && len(full.traces) > 0 {
			fullMax := 0
			for _, tr := range full.traces {
				if len(tr.Spans) > fullMax {
					fullMax = len(tr.Spans)
				}
			}
			c.check(maxSpans <= fullMax, "lean has no more spans than full",
				fmt.Sprintf("lean %d, full %d (a plain script has no Magento spans to drop)", maxSpans, fullMax))
		}
	case "debug":
		c.check(first.XdebugLoaded && first.DDTraceLoaded, "Xdebug and ddtrace loaded together", "")
		c.check(first.XdebugStart == "trigger", "Xdebug waits for a trigger", first.XdebugStart)
	}
}

func joinErrs(errs []string) string {
	if len(errs) == 0 {
		return ""
	}
	return "; " + strings.Join(errs, "; ")
}

func isOn(v string) bool {
	switch strings.ToLower(v) {
	case "1", "on", "true", "yes":
		return true
	}
	return false
}

func isOff(v string) bool {
	switch strings.ToLower(v) {
	case "0", "off", "false", "no":
		return true
	}
	return false
}

func spanCount(tr Trace, prefix string) int {
	n := 0
	for _, s := range tr.Spans {
		if strings.HasPrefix(s.Name, prefix) {
			n++
		}
	}
	return n
}

type speedscope struct {
	Schema string `json:"$schema"`
	Shared struct {
		Frames []struct {
			Name string `json:"name"`
		} `json:"frames"`
	} `json:"shared"`
	Profiles []struct {
		Type    string    `json:"type"`
		Samples [][]int   `json:"samples"`
		Weights []float64 `json:"weights"`
	} `json:"profiles"`
}

func checkProfile(c *checker, r *matrixRun, resp matrixResponse) {
	name := fmt.Sprintf("request %d profile", resp.n)
	posts := r.posts("/profiles/" + resp.TraceID)
	if len(posts) == 0 {
		c.check(false, name, "no POST /profiles/"+resp.TraceID)
		return
	}
	var p speedscope
	if err := json.Unmarshal(posts[0].body, &p); err != nil {
		c.check(false, name, "not JSON: "+err.Error())
		return
	}
	samples, burn := 0, false
	for _, pr := range p.Profiles {
		samples += len(pr.Samples)
	}
	for _, f := range p.Shared.Frames {
		if f.Name == "matrix_burn" {
			burn = true
		}
	}
	c.check(strings.Contains(p.Schema, "speedscope") && len(p.Shared.Frames) > 0 && samples > 0 && burn, name,
		fmt.Sprintf("speedscope, %d frames, %d samples, matrix_burn sampled: %t", len(p.Shared.Frames), samples, burn))
}

func checkOff(c *checker, r *matrixRun) {
	if checkResponses(c, r, false) {
		first := r.responses[0]
		c.check(!first.DDTraceLoaded, "ddtrace not loaded", "")
		c.check(first.AutoPrependFile == "", "no prepend", first.AutoPrependFile)
	}
	posts := r.posts("/")
	paths := make([]string, 0, len(posts))
	for _, p := range posts {
		paths = append(paths, p.Path)
	}
	c.check(len(posts) == 0, "nothing posted to inspector", strings.Join(paths, ", "))
	c.check(strings.TrimSpace(r.ini) == "", "no inspector ini rendered", strings.TrimSpace(r.ini))
}

var logProblems = []*regexp.Regexp{
	regexp.MustCompile(`(?i)PHP (Fatal error|Warning|Deprecated|Notice|Parse error)`),
	regexp.MustCompile(`(?i)\[ddtrace\].*\b(error|warn(ing)?)\b`),
	regexp.MustCompile(`(?i)(fatal|warning|deprecated).*(inspector-prepend|ddtrace|datadog|excimer)`),
	regexp.MustCompile(`(?i)xdebug.*(could not connect|step debug)`),
	regexp.MustCompile(`(?i)roll-inspector-ini:`),
}

func checkLog(c *checker, r *matrixRun) {
	data, err := os.ReadFile(filepath.Join(r.dir, "php-fpm.log"))
	if err != nil {
		c.check(false, "php-fpm log clean", err.Error())
		return
	}
	var hits []string
	for _, line := range strings.Split(string(data), "\n") {
		for _, re := range logProblems {
			if re.MatchString(line) {
				hits = append(hits, strings.TrimSpace(line))
				break
			}
		}
	}
	detail := fmt.Sprintf("%d lines", strings.Count(string(data), "\n"))
	if len(hits) > 0 {
		if len(hits) > 5 {
			hits = append(hits[:5], fmt.Sprintf("and %d more", len(hits)-5))
		}
		detail = strings.Join(hits, " | ")
	}
	c.check(len(hits) == 0, "php-fpm log has no fatal, warning or deprecation", detail)
}
