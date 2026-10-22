package apm

import (
	"net/url"
	"sort"
	"strings"
)

const nPlusOneMin = 10

var (
	cacheSystems  = map[string]bool{"redis": true, "valkey": true, "memcached": true, "dragonfly": true}
	searchSystems = map[string]bool{"elasticsearch": true, "opensearch": true}
	queueTypes    = map[string]bool{"queue": true, "amqp": true, "rabbitmq": true, "kafka": true}
	sqlTypes      = map[string]bool{"sql": true, "db": true, "mysql": true}
	cacheTypes    = map[string]bool{"redis": true, "memcached": true, "cache": true}

	// Cm_Cache_Backend_Redis wraps every call in these two, which would double the cache count.
	cacheNoise = map[string]bool{"Redis.clearLastError": true, "Redis.getLastError": true}
)

func classify(s Span) string {
	name := strings.ToLower(s.Name)
	typ := strings.ToLower(s.Type)
	a := s.Attrs
	switch {
	case sqlTypes[typ]:
		return "sql"
	case cacheTypes[typ]:
		return "cache"
	case queueTypes[typ]:
		return "queue"
	case searchSystems[typ]:
		return "search"
	}
	if sys := strings.ToLower(a["db.system"]); sys != "" {
		switch {
		case cacheSystems[sys]:
			return "cache"
		case searchSystems[sys]:
			return "search"
		}
		return "sql"
	}
	if a["messaging.system"] != "" {
		return "queue"
	}
	switch {
	case strings.HasPrefix(name, "pdo"), strings.HasPrefix(name, "mysqli"):
		return "sql"
	case strings.HasPrefix(name, "redis."), strings.HasPrefix(name, "predis"), strings.HasPrefix(name, "credis"), strings.HasPrefix(name, "memcached"):
		return "cache"
	case strings.HasPrefix(name, "phpamqplib"), strings.HasPrefix(name, "amqp"):
		return "queue"
	case strings.HasPrefix(name, "elasticsearch"), strings.HasPrefix(name, "opensearch"):
		return "search"
	}
	if searchTarget(a) {
		return "search"
	}
	if typ == "http" || name == "curl_exec" || strings.HasPrefix(name, "guzzle") || (a["url.full"] != "" && a["otel.kind"] == "client") {
		return "http"
	}
	if strings.HasPrefix(name, "magento.") || strings.EqualFold(a["component"], "magento") {
		return "magento"
	}
	return "php"
}

// dd-trace's curl integration drops the port from http.url, so the port comes from network.destination.port.
func searchTarget(a map[string]string) bool {
	for _, k := range []string{"network.destination.name", "server.address", "out.host"} {
		if searchSystems[strings.ToLower(a[k])] {
			return true
		}
	}
	for _, k := range []string{"network.destination.port", "server.port", "out.port"} {
		if a[k] == "9200" {
			return true
		}
	}
	for _, k := range []string{"http.url", "url.full"} {
		if u, err := url.Parse(a[k]); err == nil && u.Port() == "9200" {
			return true
		}
	}
	return false
}

// The Excimer prepend posts its profile while the root span is still open, so the upload lands in the trace it measures.
func profilerUpload(s Span) bool {
	a := s.Attrs
	if strings.EqualFold(a["network.destination.name"], "inspector") || strings.EqualFold(a["server.address"], "inspector") {
		return true
	}
	return strings.HasPrefix(a["http.url"], "http://inspector:8126/") || strings.HasPrefix(a["url.full"], "http://inspector:8126/")
}

func counted(s Span) bool {
	switch s.Component {
	case "cache":
		return !cacheNoise[s.Name]
	case "sql":
		return sqlExecution(s)
	}
	return true
}

func sqlExecution(s Span) bool {
	name := strings.ToLower(s.Name)
	return !strings.HasSuffix(name, ".prepare") && !strings.Contains(name, "connect") && !strings.HasSuffix(name, ".__construct")
}

func nPlusOne(spans []Span) []string {
	counts := map[string]int{}
	for _, s := range spans {
		if s.Component != "sql" || !sqlExecution(s) {
			continue
		}
		q := s.Resource
		if st := first(s.Attrs, "db.statement", "db.query.text"); st != "" {
			q = st
		}
		if q = normalizeSQL(q); q != "" {
			counts[q]++
		}
	}
	var out []string
	for q, n := range counts {
		if n >= nPlusOneMin {
			out = append(out, q)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if counts[out[i]] != counts[out[j]] {
			return counts[out[i]] > counts[out[j]]
		}
		return out[i] < out[j]
	})
	return out
}
