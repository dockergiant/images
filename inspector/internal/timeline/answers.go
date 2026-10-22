package timeline

import (
	"fmt"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/dockergiant/images/inspector/internal/core"
)

const maxAnswers = 3

// The Redis adapter cuts arguments at this length, so a value that reaches it
// was longer on the wire.
const argCut = 256

func (t *Timeline) answers(e *entry, j *joined) []core.Card {
	cards := []core.Card{}
	if c, ok := t.whyCard(e); ok {
		cards = append(cards, c)
	}
	if c, ok := t.cacheCard(j); ok {
		cards = append(cards, c)
	}
	if c, ok := searchCard(j); ok {
		cards = append(cards, c)
	}
	if len(cards) > maxAnswers {
		cards = cards[:maxAnswers]
	}
	return cards
}

func (t *Timeline) whyCard(e *entry) (core.Card, bool) {
	v := e.varnish
	if v == nil {
		return core.Card{}, false
	}
	h := strings.ToUpper(v.Handling)
	switch h {
	case "HITMISS":
		h = "MISS"
	case "HITPASS":
		h = "PASS"
	}
	if h != "MISS" && h != "PASS" {
		return core.Card{}, false
	}
	c := core.Card{Label: "Why a " + h + "?", Tone: core.ToneInfo}
	if h == "MISS" {
		if b, tag, ok := t.banFor(v); ok {
			ago := agoText(v.Start.Sub(b.at))
			if tag == "" {
				c.Value = "All Magento pages were purged " + ago
			} else {
				c.Value = fmt.Sprintf("Tag %s was purged %s", tag, ago)
			}
			c.Sub = "PURGE X-Magento-Tags-Pattern: " + b.pattern
			c.Tone = core.Warn
			return c, true
		}
	}
	switch {
	case v.Reason != "":
		c.Value = capitalize(v.Reason)
	case h == "MISS":
		c.Value = "Not in cache yet"
	default:
		c.Value = "Passed in VCL"
	}
	if v.TTL != "" {
		c.Sub = "TTL " + v.TTL
	}
	return c, true
}

func (t *Timeline) banFor(v *core.VarnishReq) (ban, string, bool) {
	for i := len(t.bans) - 1; i >= 0; i-- {
		b := t.bans[i]
		if b.at.After(v.Start) || v.Start.Sub(b.at) > banKeep {
			continue
		}
		if b.pattern == ".*" || b.pattern == "^.*$" {
			return b, "", true
		}
		for _, tag := range v.Tags {
			if b.re.MatchString(tag) {
				return b, tag, true
			}
		}
	}
	return ban{}, "", false
}

func agoText(d time.Duration) string {
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%d s ago", int(d/time.Second))
	case d < time.Hour:
		return fmt.Sprintf("%d min ago", int(d/time.Minute))
	}
	return fmt.Sprintf("%d h ago", int(d/time.Hour))
}

func (t *Timeline) cacheCard(j *joined) (core.Card, bool) {
	c := core.Card{Label: "Cache calls", Tone: core.ToneInfo}
	if len(j.cache) == 0 {
		if j.cacheSpans.Count == 0 {
			return core.Card{}, false
		}
		c.Value = plural(j.cacheSpans.Count, "cache call", "cache calls") + " in the trace"
		c.Sub = fmtDur(j.cacheSpans.Total) + " in cache calls"
		return c, true
	}

	reads, writes, size, cut := cacheStats(j.cache)
	perDB := map[int]int{}
	for _, rc := range j.cache {
		if isRead(rc.cmd.Cmd) {
			perDB[rc.cmd.DB] += max(1, rc.cmd.Folded)
		}
	}
	dbs := make([]int, 0, len(perDB))
	for db := range perDB {
		dbs = append(dbs, db)
	}
	sort.Ints(dbs)
	parts := make([]string, 0, len(dbs))
	for _, db := range dbs {
		parts = append(parts, fmt.Sprintf("%s: %s", t.frontendName(db), plural(perDB[db], "read", "reads")))
	}
	if reads == 0 {
		parts = append(parts, "no reads")
	}
	c.Value = strings.Join(parts, " · ")

	if writes == 0 {
		c.Sub = "no writes"
	} else {
		approx := ""
		if cut {
			approx = "≥"
		}
		c.Sub = fmt.Sprintf("%s · %s%s", plural(writes, "write", "writes"), approx, fmtSize(size))
	}
	if j.cacheJoin == joinProbable {
		c.Sub += " · probable"
	}
	return c, true
}

func (t *Timeline) frontendName(db int) string {
	if name, ok := t.frontends[db]; ok && name != "" {
		return name
	}
	return fmt.Sprintf("db %d", db)
}

func cacheStats(cmds []resolvedCmd) (reads, writes int, size int64, cut bool) {
	for _, rc := range cmds {
		n := max(1, rc.cmd.Folded)
		switch {
		case isRead(rc.cmd.Cmd):
			reads += n
		case isWrite(rc.cmd.Cmd):
			writes += n
			for i, a := range rc.cmd.Args {
				if i == 0 {
					continue
				}
				size += int64(len(a))
				if len(a) >= argCut {
					cut = true
				}
			}
		}
	}
	return
}

var readCmds = set("GET", "MGET", "GETEX", "HGET", "HMGET", "HGETALL", "HEXISTS", "HKEYS", "HLEN", "EXISTS",
	"SMEMBERS", "SISMEMBER", "SMISMEMBER", "SSCAN", "SCARD", "SUNION", "SINTER", "TTL", "PTTL", "TYPE", "STRLEN",
	"ZRANGE", "ZRANGEBYSCORE", "ZSCORE", "ZCARD", "LRANGE", "LLEN", "SCAN", "HSCAN", "ZSCAN", "KEYS")

var writeCmds = set("SET", "SETEX", "PSETEX", "SETNX", "MSET", "HSET", "HMSET", "HSETNX", "HDEL", "SADD", "SREM",
	"DEL", "UNLINK", "EXPIRE", "PEXPIRE", "EXPIREAT", "PEXPIREAT", "ZADD", "ZREM", "LPUSH", "RPUSH", "LPOP", "RPOP",
	"INCR", "INCRBY", "DECR", "HINCRBY", "APPEND", "GETSET", "GETDEL", "EVAL", "EVALSHA", "FCALL")

func set(names ...string) map[string]bool {
	m := make(map[string]bool, len(names))
	for _, n := range names {
		m[n] = true
	}
	return m
}

func isRead(cmd string) bool  { return readCmds[strings.ToUpper(cmd)] }
func isWrite(cmd string) bool { return writeCmds[strings.ToUpper(cmd)] }

func searchCard(j *joined) (core.Card, bool) {
	c := core.Card{Label: "Search", Tone: core.ToneInfo}
	if len(j.queries) == 0 {
		if j.searchSpans.Count == 0 {
			return core.Card{}, false
		}
		c.Value = plural(j.searchSpans.Count, "search call", "search calls") + " in the trace"
		c.Sub = "took " + fmtDur(j.searchSpans.Total)
		return c, true
	}
	var took time.Duration
	summary := ""
	for _, q := range j.queries {
		took += q.Took
		if summary == "" {
			summary = q.Summary
		}
	}
	parts := []string{plural(len(j.queries), "query", "queries")}
	if labels := indexLabels(j.queries); len(labels) > 0 {
		parts = append(parts, strings.Join(labels, ", "))
	}
	c.Value = strings.Join(parts, " · ")
	c.Sub = "took " + fmtDur(took)
	if summary != "" {
		c.Sub += " · " + summary
	}
	return c, true
}

func plural(n int, one, many string) string {
	if n == 1 {
		return "1 " + one
	}
	return fmt.Sprintf("%d %s", n, many)
}

func capitalize(s string) string {
	r, size := utf8.DecodeRuneInString(s)
	if r == utf8.RuneError {
		return s
	}
	return string(unicode.ToUpper(r)) + s[size:]
}

func fmtDur(d time.Duration) string {
	switch {
	case d < time.Millisecond:
		return fmt.Sprintf("%.1f ms", float64(d)/float64(time.Millisecond))
	case d < time.Second:
		return fmt.Sprintf("%d ms", (d+time.Millisecond/2)/time.Millisecond)
	}
	return fmt.Sprintf("%.2f s", d.Seconds())
}

func fmtSize(n int64) string {
	switch {
	case n < 1024:
		return fmt.Sprintf("%d B", n)
	case n < 1024*1024:
		return fmt.Sprintf("%.1f KB", float64(n)/1024)
	}
	return fmt.Sprintf("%.1f MB", float64(n)/(1024*1024))
}
