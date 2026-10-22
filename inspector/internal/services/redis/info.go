package redis

import (
	"sort"
	"strconv"
	"strings"
)

type Info map[string]map[string]string

func ParseInfo(s string) Info {
	info := Info{}
	section := "default"
	for _, line := range strings.Split(s, "\n") {
		line = strings.TrimRight(line, "\r")
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "#") {
			section = strings.ToLower(strings.TrimSpace(strings.TrimPrefix(line, "#")))
			continue
		}
		k, v, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		if info[section] == nil {
			info[section] = map[string]string{}
		}
		info[section][k] = v
	}
	return info
}

func (i Info) Get(key string) string {
	for _, sec := range i {
		if v, ok := sec[key]; ok {
			return v
		}
	}
	return ""
}

func (i Info) Int(key string) int64 {
	n, _ := strconv.ParseInt(i.Get(key), 10, 64)
	return n
}

func (i Info) Float(key string) float64 {
	f, _ := strconv.ParseFloat(i.Get(key), 64)
	return f
}

func fields(v string) map[string]string {
	out := map[string]string{}
	for _, part := range strings.Split(v, ",") {
		if k, val, ok := strings.Cut(part, "="); ok {
			out[k] = val
		}
	}
	return out
}

type Keyspace struct {
	DB      int   `json:"db"`
	Keys    int64 `json:"keys"`
	Expires int64 `json:"expires"`
	AvgTTL  int64 `json:"avgTtl"`
}

func ParseKeyspace(i Info) []Keyspace {
	var out []Keyspace
	for k, v := range i["keyspace"] {
		if !strings.HasPrefix(k, "db") {
			continue
		}
		db, err := strconv.Atoi(k[2:])
		if err != nil {
			continue
		}
		f := fields(v)
		ks := Keyspace{DB: db}
		ks.Keys, _ = strconv.ParseInt(f["keys"], 10, 64)
		ks.Expires, _ = strconv.ParseInt(f["expires"], 10, 64)
		ks.AvgTTL, _ = strconv.ParseInt(f["avg_ttl"], 10, 64)
		out = append(out, ks)
	}
	sort.Slice(out, func(a, b int) bool { return out[a].DB < out[b].DB })
	return out
}

type CommandStat struct {
	Cmd         string  `json:"cmd"`
	Calls       int64   `json:"calls"`
	Usec        int64   `json:"usec"`
	UsecPerCall float64 `json:"usecPerCall"`
	Failed      int64   `json:"failed"`
}

func ParseCommandStats(i Info) []CommandStat {
	var out []CommandStat
	for k, v := range i["commandstats"] {
		if !strings.HasPrefix(k, "cmdstat_") {
			continue
		}
		f := fields(v)
		s := CommandStat{Cmd: strings.TrimPrefix(k, "cmdstat_")}
		s.Calls, _ = strconv.ParseInt(f["calls"], 10, 64)
		s.Usec, _ = strconv.ParseInt(f["usec"], 10, 64)
		s.UsecPerCall, _ = strconv.ParseFloat(f["usec_per_call"], 64)
		s.Failed, _ = strconv.ParseInt(f["failed_calls"], 10, 64)
		out = append(out, s)
	}
	sort.Slice(out, func(a, b int) bool {
		if out[a].Usec != out[b].Usec {
			return out[a].Usec > out[b].Usec
		}
		return out[a].Cmd < out[b].Cmd
	})
	return out
}

type LatencyStat struct {
	Cmd  string  `json:"cmd"`
	P50  float64 `json:"p50"`
	P99  float64 `json:"p99"`
	P999 float64 `json:"p999"`
}

func ParseLatencyStats(i Info) []LatencyStat {
	var out []LatencyStat
	for k, v := range i["latencystats"] {
		if !strings.HasPrefix(k, "latency_percentiles_usec_") {
			continue
		}
		f := fields(v)
		s := LatencyStat{Cmd: strings.TrimPrefix(k, "latency_percentiles_usec_")}
		s.P50, _ = strconv.ParseFloat(f["p50"], 64)
		s.P99, _ = strconv.ParseFloat(f["p99"], 64)
		s.P999, _ = strconv.ParseFloat(f["p99.9"], 64)
		out = append(out, s)
	}
	sort.Slice(out, func(a, b int) bool { return out[a].P99 > out[b].P99 })
	return out
}

func ParseClientList(s string) []map[string]string {
	var out []map[string]string
	for _, line := range strings.Split(s, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		c := map[string]string{}
		for _, f := range strings.Fields(line) {
			if k, v, ok := strings.Cut(f, "="); ok {
				c[k] = v
			}
		}
		out = append(out, c)
	}
	return out
}

// DetectEngine never reports redis_version for Valkey, which pins it at 7.2.4
// for client compatibility.
func DetectEngine(i Info) (string, string) {
	if v := i.Get("dragonfly_version"); v != "" {
		return "dragonfly", strings.TrimPrefix(strings.TrimPrefix(v, "df-"), "v")
	}
	if v := i.Get("valkey_version"); v != "" {
		return "valkey", v
	}
	if strings.EqualFold(i.Get("server_name"), "valkey") {
		return "valkey", i.Get("redis_version")
	}
	return "redis", i.Get("redis_version")
}

func Tested(engine, version string) bool {
	switch engine {
	case "redis":
		return minor(version) == "7.2"
	case "valkey":
		m := minor(version)
		return m == "8.1" || m == "9.0"
	}
	return false
}

func minor(version string) string {
	parts := strings.SplitN(version, ".", 3)
	if len(parts) < 2 {
		return version
	}
	return parts[0] + "." + parts[1]
}

func atLeast(version string, major, min int) bool {
	parts := strings.SplitN(version, ".", 3)
	ma, _ := strconv.Atoi(parts[0])
	mi := 0
	if len(parts) > 1 {
		mi, _ = strconv.Atoi(parts[1])
	}
	return ma > major || ma == major && mi >= min
}

func Features(engine, version string) map[string]bool {
	if engine == "dragonfly" {
		return map[string]bool{
			"noTouch":          false,
			"latency":          false,
			"latencyHistogram": false,
			"latencyStats":     false,
			"objectEncoding":   false,
			"memoryUsage":      true,
			"scanType":         true,
			"unlink":           true,
		}
	}
	compat := version
	if engine == "valkey" {
		compat = "7.2"
	}
	return map[string]bool{
		"noTouch":          atLeast(compat, 7, 2),
		"latency":          true,
		"latencyHistogram": atLeast(compat, 7, 0),
		"latencyStats":     atLeast(compat, 7, 0),
		"objectEncoding":   true,
		"memoryUsage":      atLeast(compat, 4, 0),
		"scanType":         atLeast(compat, 6, 0),
		"unlink":           atLeast(compat, 4, 0),
	}
}
