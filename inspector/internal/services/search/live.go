package search

import (
	"bytes"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/dockergiant/images/inspector/internal/core"
)

type insightRecord struct {
	ID           string                     `json:"id"`
	Timestamp    int64                      `json:"timestamp"`
	Indices      []string                   `json:"indices"`
	Source       json.RawMessage            `json:"source"`
	Labels       map[string]any             `json:"labels"`
	Latency      json.RawMessage            `json:"latency"`
	Measurements map[string]json.RawMessage `json:"measurements"`
	PhaseLatency map[string]float64         `json:"phase_latency_map"`
}

func ParseInsights(b []byte) ([]insightRecord, error) {
	var resp struct {
		TopQueries []insightRecord `json:"top_queries"`
	}
	if err := json.Unmarshal(b, &resp); err != nil {
		return nil, fmt.Errorf("parse top_queries: %w", err)
	}
	sort.SliceStable(resp.TopQueries, func(i, j int) bool { return resp.TopQueries[i].Timestamp < resp.TopQueries[j].Timestamp })
	return resp.TopQueries, nil
}

func (r insightRecord) key() string {
	if r.ID != "" {
		return r.ID
	}
	sum := sha1.Sum(r.Source)
	return strconv.FormatInt(r.Timestamp, 10) + ":" + hex.EncodeToString(sum[:8])
}

func (r insightRecord) opaqueID() string {
	return catString(r.Labels["X-Opaque-Id"])
}

func latencyMillis(raw json.RawMessage) (float64, bool) {
	if len(raw) == 0 {
		return 0, false
	}
	var n float64
	if json.Unmarshal(raw, &n) == nil {
		return n, true
	}
	var m struct {
		Number *float64 `json:"number"`
	}
	if json.Unmarshal(raw, &m) == nil && m.Number != nil {
		return *m.Number, true
	}
	return 0, false
}

func (r insightRecord) took() time.Duration {
	ms, ok := latencyMillis(r.Measurements["latency"])
	if !ok {
		ms, ok = latencyMillis(r.Latency)
	}
	if !ok {
		for _, v := range r.PhaseLatency {
			ms += v
		}
	}
	return time.Duration(ms * float64(time.Millisecond))
}

func (r insightRecord) body() string {
	var s string
	if json.Unmarshal(r.Source, &s) == nil {
		return s
	}
	var buf bytes.Buffer
	if json.Compact(&buf, r.Source) == nil {
		return buf.String()
	}
	return string(r.Source)
}

func (r insightRecord) systemOnly() bool {
	if len(r.Indices) == 0 {
		return false
	}
	for _, ix := range r.Indices {
		if !isSystemIndex(ix) {
			return false
		}
	}
	return true
}

func (r insightRecord) query() core.SearchQuery {
	t := time.UnixMilli(r.Timestamp)
	body := r.body()
	return core.SearchQuery{
		Time:     t,
		Start:    t,
		Took:     r.took(),
		Index:    strings.Join(r.Indices, ","),
		Phase:    "request",
		Summary:  Summarize(body),
		Body:     body,
		OpaqueID: r.opaqueID(),
		Source:   "insights",
	}
}

var tookUnits = []struct {
	suffix string
	unit   time.Duration
}{
	{"nanos", time.Nanosecond},
	{"micros", time.Microsecond},
	{"ms", time.Millisecond},
	{"s", time.Second},
	{"m", time.Minute},
	{"h", time.Hour},
	{"d", 24 * time.Hour},
}

func parseTook(s string) (time.Duration, bool) {
	for _, u := range tookUnits {
		if num, ok := strings.CutSuffix(s, u.suffix); ok {
			f, err := strconv.ParseFloat(num, 64)
			if err != nil {
				return 0, false
			}
			return time.Duration(f * float64(u.unit)), true
		}
	}
	return 0, false
}

func between(s, open, close string) (string, bool) {
	i := strings.Index(s, open)
	if i < 0 {
		return "", false
	}
	s = s[i+len(open):]
	j := strings.Index(s, close)
	if j < 0 {
		return "", false
	}
	return s[:j], true
}

func slowPhase(logger string) string {
	switch {
	case strings.Contains(logger, "s.s.query"), strings.Contains(logger, "slowlog.query"):
		return "query"
	case strings.Contains(logger, "s.s.fetch"), strings.Contains(logger, "slowlog.fetch"):
		return "fetch"
	}
	return ""
}

func ParseSlowlog(line string, t time.Time) (core.SearchQuery, bool) {
	line = strings.TrimSpace(line)
	if strings.HasPrefix(line, "{") {
		return parseSlowlogJSON(line, t)
	}
	i := strings.Index(line, " took[")
	if i < 0 || !strings.Contains(line, "took_millis[") {
		return core.SearchQuery{}, false
	}
	head, rest := line[:i], line[i+1:]
	phase := slowPhase(head)
	if phase == "" {
		return core.SearchQuery{}, false
	}
	j := strings.LastIndex(head, "][")
	if j < 0 {
		return core.SearchQuery{}, false
	}
	k := strings.LastIndex(head[:j], "[")
	if k < 0 {
		return core.SearchQuery{}, false
	}
	q := core.SearchQuery{Index: head[k+1 : j], Phase: phase, Source: "slowlog", Time: t}
	q.Took = slowTook(between(rest, "took[", "]"))
	if q.Took == 0 {
		if ms, ok := between(rest, "took_millis[", "]"); ok {
			n, _ := strconv.ParseInt(ms, 10, 64)
			q.Took = time.Duration(n) * time.Millisecond
		}
	}
	if s := strings.Index(rest, "source["); s >= 0 {
		start := s + len("source[")
		if e := strings.LastIndex(rest, "], id["); e >= start {
			q.Body = rest[start:e]
			q.OpaqueID, _ = between(rest[e:], "id[", "]")
		} else if e := strings.LastIndex(rest, "], "); e >= start {
			q.Body = rest[start:e]
		}
	}
	q.Start = t.Add(-q.Took)
	q.Summary = Summarize(q.Body)
	return q, true
}

func slowTook(s string, ok bool) time.Duration {
	if !ok {
		return 0
	}
	d, _ := parseTook(s)
	return d
}

func parseSlowlogJSON(line string, t time.Time) (core.SearchQuery, bool) {
	var m map[string]any
	if json.Unmarshal([]byte(line), &m) != nil {
		return core.SearchQuery{}, false
	}
	get := func(keys ...string) (string, bool) {
		for _, k := range keys {
			if v, ok := m[k]; ok {
				return catString(v), true
			}
		}
		return "", false
	}
	millis, ok := get("took_millis", "elasticsearch.slowlog.took_millis")
	if !ok {
		return core.SearchQuery{}, false
	}
	logger, _ := get("component", "log.logger", "type")
	phase := slowPhase(logger)
	if phase == "" {
		return core.SearchQuery{}, false
	}
	q := core.SearchQuery{Phase: phase, Source: "slowlog", Time: t}
	msg, _ := get("message", "elasticsearch.slowlog.message")
	q.Index, _ = between(msg, "[", "]")
	q.Took = slowTook(get("took", "elasticsearch.slowlog.took"))
	if q.Took == 0 {
		n, _ := strconv.ParseFloat(millis, 64)
		q.Took = time.Duration(n * float64(time.Millisecond))
	}
	q.Body, _ = get("source", "elasticsearch.slowlog.source")
	q.OpaqueID, _ = get("id", "elasticsearch.slowlog.id")
	q.Start = t.Add(-q.Took)
	q.Summary = Summarize(q.Body)
	return q, true
}

const maxClauses = 4

var skipKeys = map[string]bool{"boost": true, "_name": true, "minimum_should_match": true, "adjust_pure_negative": true}

func Summarize(body string) string {
	var m map[string]any
	if body == "" || json.Unmarshal([]byte(body), &m) != nil {
		return clip(body, 120)
	}
	var clauses []string
	if q, ok := m["query"]; ok {
		collectClauses(q, "", &clauses)
	}
	s := strings.Join(clauses[:min(len(clauses), maxClauses)], ", ")
	if len(clauses) > maxClauses {
		s += fmt.Sprintf(" +%d", len(clauses)-maxClauses)
	}
	if s == "" {
		s = "match_all"
	}
	parts := []string{s}
	if size, ok := m["size"]; ok {
		parts = append(parts, "size "+catString(size))
	}
	if sorts := sortFields(m["sort"]); len(sorts) > 0 {
		parts = append(parts, "sort "+strings.Join(sorts, ","))
	}
	aggs, ok := m["aggregations"].(map[string]any)
	if !ok {
		aggs, _ = m["aggs"].(map[string]any)
	}
	if n := len(aggs); n == 1 {
		parts = append(parts, "1 agg")
	} else if n > 1 {
		parts = append(parts, fmt.Sprintf("%d aggs", n))
	}
	return clip(strings.Join(parts, " · "), 240)
}

func sortedKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		if !skipKeys[k] {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	return keys
}

func sortFields(v any) []string {
	var out []string
	switch x := v.(type) {
	case string:
		out = append(out, x)
	case map[string]any:
		out = append(out, sortedKeys(x)...)
	case []any:
		for _, e := range x {
			out = append(out, sortFields(e)...)
		}
	}
	return out
}

func fieldValue(v any) (string, any) {
	m, ok := v.(map[string]any)
	if !ok {
		return "", v
	}
	keys := sortedKeys(m)
	if len(keys) == 0 {
		return "", nil
	}
	val := m[keys[0]]
	if inner, ok := val.(map[string]any); ok {
		if q, ok := inner["query"]; ok {
			val = q
		} else if q, ok := inner["value"]; ok {
			val = q
		}
	}
	return keys[0], val
}

func collectClauses(n any, neg string, out *[]string) {
	switch v := n.(type) {
	case []any:
		for _, e := range v {
			collectClauses(e, neg, out)
		}
	case map[string]any:
		for _, k := range sortedKeys(v) {
			c := v[k]
			switch k {
			case "bool":
				b, _ := c.(map[string]any)
				for _, occ := range []string{"must", "filter", "should", "must_not"} {
					if child, ok := b[occ]; ok {
						p := neg
						if occ == "must_not" {
							p = "not "
						}
						collectClauses(child, p, out)
					}
				}
			case "match", "match_phrase", "match_phrase_prefix", "match_bool_prefix":
				f, val := fieldValue(c)
				*out = append(*out, fmt.Sprintf("%s%s %s=%q", neg, k, f, catString(val)))
			case "term", "prefix", "wildcard", "fuzzy", "regexp":
				f, val := fieldValue(c)
				*out = append(*out, fmt.Sprintf("%s%s %s=%s", neg, k, f, catString(val)))
			case "terms":
				f, val := fieldValue(c)
				*out = append(*out, fmt.Sprintf("%s%s %s=%v", neg, k, f, val))
			case "range":
				f, _ := fieldValue(c)
				*out = append(*out, neg+"range "+f)
			case "exists":
				m, _ := c.(map[string]any)
				*out = append(*out, neg+"exists "+catString(m["field"]))
			case "ids":
				m, _ := c.(map[string]any)
				vals, _ := m["values"].([]any)
				*out = append(*out, fmt.Sprintf("%sids %d", neg, len(vals)))
			case "multi_match", "query_string", "simple_query_string":
				m, _ := c.(map[string]any)
				*out = append(*out, fmt.Sprintf("%s%s %q", neg, k, catString(m["query"])))
			case "nested", "constant_score", "function_score", "dis_max", "boosting", "has_child", "has_parent":
				m, _ := c.(map[string]any)
				for _, inner := range []string{"query", "filter", "queries", "positive", "negative"} {
					if child, ok := m[inner]; ok {
						collectClauses(child, neg, out)
					}
				}
			default:
				*out = append(*out, neg+k)
			}
		}
	}
}
