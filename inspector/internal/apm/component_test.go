package apm

import "testing"

func TestClassify(t *testing.T) {
	cases := []struct {
		name string
		span Span
		want string
	}{
		{"pdo", Span{Name: "PDOStatement.execute", Type: "sql"}, "sql"},
		{"pdo without type", Span{Name: "PDO.query"}, "sql"},
		{"mysqli", Span{Name: "mysqli_query"}, "sql"},
		{"otel db", Span{Name: "SELECT shop.t", Attrs: map[string]string{"db.system": "mysql"}}, "sql"},
		{"phpredis", Span{Name: "Redis.mGet", Type: "redis"}, "cache"},
		{"predis", Span{Name: "Predis.Client.executeCommand"}, "cache"},
		{"otel valkey", Span{Name: "GET", Attrs: map[string]string{"db.system": "valkey"}}, "cache"},
		{"curl to opensearch port", Span{Name: "curl_exec", Type: "http", Attrs: map[string]string{"network.destination.name": "search-node", "network.destination.port": "9200", "http.url": "http://search-node/x/_search"}}, "search"},
		{"curl to opensearch name", Span{Name: "curl_exec", Type: "http", Attrs: map[string]string{"network.destination.name": "opensearch"}}, "search"},
		{"otel server.port", Span{Name: "POST", Attrs: map[string]string{"server.address": "es", "server.port": "9200"}}, "search"},
		{"url with port", Span{Name: "curl_exec", Type: "http", Attrs: map[string]string{"http.url": "http://es:9200/_search"}}, "search"},
		{"elasticsearch type", Span{Name: "Elasticsearch.Client.search", Type: "elasticsearch"}, "search"},
		{"curl elsewhere", Span{Name: "curl_exec", Type: "http", Attrs: map[string]string{"network.destination.name": "api.example.com", "network.destination.port": "443"}}, "http"},
		{"guzzle", Span{Name: "GuzzleHttp\\Client.transfer"}, "http"},
		{"amqp", Span{Name: "PhpAmqpLib\\Channel\\AMQPChannel.basic_publish", Type: "queue"}, "queue"},
		{"amqp by name", Span{Name: "amqp.basic.publish"}, "queue"},
		{"otel messaging", Span{Name: "publish", Attrs: map[string]string{"messaging.system": "rabbitmq"}}, "queue"},
		{"magento event", Span{Name: "magento.event.execute", Type: "web"}, "magento"},
		{"magento component", Span{Name: "magento.request", Type: "web", Attrs: map[string]string{"component": "magento"}}, "magento"},
		{"cli root", Span{Name: "Standard input code", Type: "cli"}, "php"},
		{"exec", Span{Name: "command_execution", Type: "system"}, "php"},
	}
	for _, c := range cases {
		if got := classify(c.span); got != c.want {
			t.Errorf("%s: classify = %s, want %s", c.name, got, c.want)
		}
	}
}

func TestProfilerUpload(t *testing.T) {
	yes := []Span{
		{Name: "curl_exec", Attrs: map[string]string{"network.destination.name": "inspector", "network.destination.port": "8126"}},
		{Name: "curl_exec", Attrs: map[string]string{"http.url": "http://inspector:8126/profiles/6ad7d4d30000000025a83c7ee41d7769"}},
		{Name: "POST", Attrs: map[string]string{"url.full": "http://inspector:8126/profiles/x"}},
	}
	for _, s := range yes {
		if !profilerUpload(s) {
			t.Errorf("not recognised: %+v", s.Attrs)
		}
	}
	no := []Span{
		{Name: "curl_exec", Attrs: map[string]string{"network.destination.name": "opensearch"}},
		{Name: "magento.request", Attrs: map[string]string{"http.url": "https://app.test/inspector:8126/"}},
	}
	for _, s := range no {
		if profilerUpload(s) {
			t.Errorf("wrongly recognised: %+v", s.Attrs)
		}
	}
}
