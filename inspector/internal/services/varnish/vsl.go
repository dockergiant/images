package varnish

import (
	"bufio"
	"fmt"
	"io"
	"math"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/dockergiant/images/inspector/internal/core"
)

var recordRe = regexp.MustCompile(`^(-+)\s+(\d+)\s+(\S+)\s+([cb-])\s?(.*)$`)

const maxRawLines = 4000

type record struct {
	depth int
	vxid  string
	tag   string
	side  byte
	value string
}

func parseRecord(line string) (record, bool) {
	m := recordRe.FindStringSubmatch(line)
	if m == nil {
		return record{}, false
	}
	return record{depth: len(m[1]), vxid: m[2], tag: m[3], side: m[4][0], value: m[5]}, true
}

func ParseGroups(r io.Reader, emit func(core.VarnishReq)) {
	p := newGroupParser(emit)
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	for sc.Scan() {
		p.Line(sc.Text())
	}
	p.Flush()
}

type groupParser struct {
	emit    func(core.VarnishReq)
	raw     []string
	records []record
}

func newGroupParser(emit func(core.VarnishReq)) *groupParser {
	return &groupParser{emit: emit}
}

func (p *groupParser) Line(line string) {
	line = strings.TrimRight(line, "\r")
	if strings.TrimSpace(line) == "" {
		p.Flush()
		return
	}
	if len(p.raw) < maxRawLines {
		p.raw = append(p.raw, line)
	}
	if rec, ok := parseRecord(line); ok {
		p.records = append(p.records, rec)
	}
}

func (p *groupParser) Flush() {
	if len(p.records) > 0 {
		if req, ok := buildRequest(p.records, p.raw); ok {
			p.emit(req)
		}
	}
	p.raw = nil
	p.records = nil
}

type txn struct {
	records []record
}

func (t *txn) each(tag string, fn func(string)) {
	for _, r := range t.records {
		if r.tag == tag {
			fn(r.value)
		}
	}
}

func (t *txn) last(tag string) (string, bool) {
	v, ok := "", false
	t.each(tag, func(s string) { v, ok = s, true })
	return v, ok
}

func (t *txn) has(tag string) bool {
	_, ok := t.last(tag)
	return ok
}

func (t *txn) headers(tag, name string) []string {
	var out []string
	t.each(tag, func(s string) {
		if v, ok := headerValue(s, name); ok {
			out = append(out, v)
		}
	})
	return out
}

func (t *txn) timestamp(label string) (abs float64, sinceStart float64, ok bool) {
	t.each("Timestamp", func(s string) {
		f := strings.Fields(s)
		if len(f) < 3 || f[0] != label+":" {
			return
		}
		a, err1 := strconv.ParseFloat(f[1], 64)
		b, err2 := strconv.ParseFloat(f[2], 64)
		if err1 == nil && err2 == nil {
			abs, sinceStart, ok = a, b, true
		}
	})
	return
}

func headerValue(s, name string) (string, bool) {
	k, v, ok := strings.Cut(s, ":")
	if !ok || !strings.EqualFold(strings.TrimSpace(k), name) {
		return "", false
	}
	return strings.TrimSpace(v), true
}

func buildRequest(records []record, raw []string) (core.VarnishReq, bool) {
	var client *txn
	byVXID := map[string]*txn{}
	for _, r := range records {
		t := byVXID[r.vxid]
		if t == nil {
			t = &txn{}
			byVXID[r.vxid] = t
		}
		t.records = append(t.records, r)
		if client == nil && r.depth == 1 && r.side == 'c' {
			client = t
		}
	}
	if client == nil {
		return core.VarnishReq{}, false
	}

	req := core.VarnishReq{VXID: client.records[0].vxid, Raw: raw}
	req.Method, _ = client.last("ReqMethod")
	req.URL, _ = client.last("ReqURL")
	if hosts := client.headers("ReqHeader", "Host"); len(hosts) > 0 {
		req.Host = hosts[0]
	}
	if s, ok := client.last("RespStatus"); ok {
		req.Status, _ = strconv.Atoi(strings.TrimSpace(s))
	}

	var calls []string
	recvReturn := ""
	afterRecv := false
	for _, r := range client.records {
		switch r.tag {
		case "VCL_call":
			calls = append(calls, strings.TrimSpace(r.value))
			afterRecv = strings.TrimSpace(r.value) == "RECV"
		case "VCL_return":
			if afterRecv {
				recvReturn = strings.TrimSpace(r.value)
				afterRecv = false
			}
		}
	}
	req.Handling = handling(calls)

	// ESI subrequests add "req <vxid> esi" Links after the bereq Link.
	var linkKind string
	client.each("Link", func(l string) {
		f := strings.Fields(l)
		if req.BereqVXID != "" || len(f) < 2 || f[0] != "bereq" {
			return
		}
		req.BereqVXID = f[1]
		if len(f) >= 3 {
			linkKind = f[2]
		}
	})
	if linkKind == "bgfetch" {
		req.Grace = true
	}
	if h, ok := client.last("Hit"); ok {
		f := strings.Fields(h)
		if len(f) >= 2 {
			if left, err := strconv.ParseFloat(f[1], 64); err == nil && left < 0 {
				req.Grace = true
			}
		}
	}

	if abs, _, ok := client.timestamp("Start"); ok {
		req.Start = floatTime(abs)
	}
	if _, d, ok := client.timestamp("Process"); ok {
		req.TTFB = seconds(d)
	}
	if _, d, ok := client.timestamp("Resp"); ok {
		req.Duration = seconds(d)
	} else if _, d, ok := client.timestamp("PipeSess"); ok {
		req.Duration = seconds(d)
	}
	if req.TTFB == 0 {
		req.TTFB = req.Duration
	}

	backend := byVXID[req.BereqVXID]
	if req.BereqVXID == "" {
		backend = nil
	}
	if backend != nil {
		if abs, _, ok := backend.timestamp("Start"); ok {
			req.BereqStart = floatTime(abs)
		}
		for _, label := range []string{"BerespBody", "Beresp", "Bereq"} {
			if abs, _, ok := backend.timestamp(label); ok {
				req.BereqEnd = floatTime(abs)
				break
			}
		}
		if ttl, ok := backend.last("TTL"); ok {
			req.TTL = strings.TrimSpace(ttl)
		}
		if tags := backend.headers("BerespHeader", "X-Magento-Tags"); len(tags) > 0 {
			req.Tags = splitTags(tags[len(tags)-1])
		}
	}
	if len(req.Tags) == 0 {
		if tags := client.headers("RespHeader", "X-Magento-Tags"); len(tags) > 0 {
			req.Tags = splitTags(tags[len(tags)-1])
		}
	}

	req.Reason = reason(&req, client, backend, recvReturn)
	return req, true
}

func handling(calls []string) string {
	out := ""
	for _, c := range calls {
		switch c {
		case "HIT", "MISS", "PASS", "PIPE":
			out = c
		}
	}
	if out != "" {
		return out
	}
	for _, c := range calls {
		if c == "SYNTH" {
			return "SYNTH"
		}
	}
	return ""
}

func reason(req *core.VarnishReq, client, backend *txn, recvReturn string) string {
	switch req.Handling {
	case "PASS":
		if client.has("HitPass") {
			if r := backendReason(req, backend); r != "" {
				return "hit-for-pass: " + r
			}
			return "hit-for-pass: an earlier response was uncacheable"
		}
		if recvReturn == "pass" {
			if req.Method != "" && req.Method != "GET" && req.Method != "HEAD" {
				return req.Method + " is never cached (PASS in VCL)"
			}
			return "never cached (PASS in VCL)"
		}
		if r := backendReason(req, backend); r != "" {
			return r
		}
		return "passed in VCL"
	case "MISS":
		if client.has("HitMiss") {
			if r := backendReason(req, backend); r != "" {
				return "hit-for-miss: " + r
			}
			return "hit-for-miss: an earlier response was uncacheable"
		}
		if client.has("ExpBan") {
			return "banned by a purge"
		}
		if r := backendReason(req, backend); r != "" {
			return r
		}
		return "not in cache yet"
	}
	return ""
}

func backendReason(req *core.VarnishReq, backend *txn) string {
	if backend == nil || !uncacheable(backend) {
		return ""
	}
	if cc := backend.headers("BerespHeader", "Cache-Control"); len(cc) > 0 {
		joined := strings.Join(cc, ", ")
		lower := strings.ToLower(joined)
		if strings.Contains(lower, "private") || strings.Contains(lower, "no-cache") || strings.Contains(lower, "no-store") {
			return "Cache-Control: " + joined + " from backend"
		}
	}
	if sc := backend.headers("BerespHeader", "Set-Cookie"); len(sc) > 0 {
		return "Set-Cookie from backend"
	}
	for _, v := range backend.headers("BerespHeader", "Vary") {
		if strings.TrimSpace(v) == "*" {
			return "Vary: * from backend"
		}
	}
	for _, v := range backend.headers("BerespHeader", "Surrogate-Control") {
		if strings.Contains(strings.ToLower(v), "no-store") {
			return "Surrogate-Control: " + v + " from backend"
		}
	}
	status := 0
	if s, ok := backend.last("BerespStatus"); ok {
		status, _ = strconv.Atoi(strings.TrimSpace(s))
	}
	if rfc, ok := rfcTTL(backend); ok && rfc <= 0 {
		if status != 0 && !cacheableStatus(status) {
			return fmt.Sprintf("status %d is not cacheable by default", status)
		}
		if len(backend.headers("BerespHeader", "Expires")) > 0 {
			return "Expires in the past from backend"
		}
		return "backend sent TTL 0"
	}
	return "uncacheable in VCL"
}

func uncacheable(backend *txn) bool {
	ttl, ok := backend.last("TTL")
	if !ok {
		return false
	}
	f := strings.Fields(ttl)
	if len(f) == 0 {
		return false
	}
	if f[len(f)-1] == "uncacheable" {
		return true
	}
	if len(f) >= 2 {
		if v, err := strconv.ParseFloat(f[1], 64); err == nil && v <= 0 {
			return true
		}
	}
	return false
}

func rfcTTL(backend *txn) (float64, bool) {
	var out float64
	found := false
	backend.each("TTL", func(s string) {
		f := strings.Fields(s)
		if len(f) >= 2 && f[0] == "RFC" {
			if v, err := strconv.ParseFloat(f[1], 64); err == nil {
				out, found = v, true
			}
		}
	})
	return out, found
}

func cacheableStatus(s int) bool {
	switch s {
	case 200, 203, 204, 300, 301, 302, 304, 307, 308, 404, 410, 414:
		return true
	}
	return false
}

func splitTags(s string) []string {
	var out []string
	for t := range strings.SplitSeq(s, ",") {
		if t = strings.TrimSpace(t); t != "" {
			out = append(out, t)
		}
	}
	return out
}

func floatTime(f float64) time.Time {
	sec, frac := math.Modf(f)
	return time.Unix(int64(sec), int64(math.Round(frac*1e6))*1000).UTC()
}

func seconds(f float64) time.Duration {
	return time.Duration(math.Round(f*1e6)) * time.Microsecond
}
