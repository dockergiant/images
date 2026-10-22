package apm

import "testing"

func TestNormalizeSQLLiterals(t *testing.T) {
	cases := map[string]string{
		"SELECT `url_rewrite`.* FROM `url_rewrite` WHERE (`request_path` IN ('category-1.html', 'category-1.html/')) AND (`store_id` IN ('1'))": "SELECT `url_rewrite`.* FROM `url_rewrite` WHERE (`request_path` IN (?)) AND (`store_id` IN (?))",
		"SET NAMES utf8mb4 COLLATE utf8mb4_general_ci":                        "SET NAMES utf8mb4 COLLATE utf8mb4_general_ci",
		"SELECT `t1`.`a2` FROM `table_1` AS `t1` LIMIT 10 OFFSET 20":          "SELECT `t1`.`a2` FROM `table_1` AS `t1` LIMIT ? OFFSET ?",
		"SELECT * FROM `123` WHERE x = -5":                                    "SELECT * FROM `123` WHERE x = -?",
		"SET time_zone = '+00:00'":                                            "SET time_zone = ?",
		"  SELECT\t1  ":                                                       "SELECT ?",
		"SELECT 'it\\'s', \"a\"\"b\" FROM t":                                  "SELECT ?, ? FROM t",
		"SELECT * FROM t WHERE id IN ( ?, ? ,? ) OR id IN (1)":                "SELECT * FROM t WHERE id IN (?) OR id IN (?)",
		"INSERT INTO t (a) VALUES (1), (2),(3) ON DUPLICATE KEY UPDATE a = 4": "INSERT INTO t (a) VALUES (?) ON DUPLICATE KEY UPDATE a = ?",
		"SELECT 1abc, 0xZZ FROM t":                                            "SELECT 1abc, 0xZZ FROM t",
		"SELECT * FROM t WHERE (a, b) IN ((1, 2), (3, 4))":                    "SELECT * FROM t WHERE (a, b) IN ((?))",
		"SELECT 'unterminated":                                                "SELECT ?",
		"":                                                                    "",
	}
	for in, want := range cases {
		if got := normalizeSQL(in); got != want {
			t.Errorf("normalizeSQL(%q)\n got %q\nwant %q", in, got, want)
		}
	}
}

func TestNormalizeCache(t *testing.T) {
	cases := map[string]string{
		"mGet 69d_:RESOLVED_STORES_D1BEFA03C79CA0B84ECC488DEA96BC68":                        "mGet *_:RESOLVED_STORES_*",
		"setEx 69d_:EAV_ENTITY_ATTRIBUTES_3_1_3 7200 a:4:{s:4:\"data\";s:2166:\"CACHE_COMP": "setEx *_:EAV_ENTITY_ATTRIBUTES_*_*_*",
		"sAdd cache:tags:69d_TRANSLATE TRANSLATE_EN_US_FRONTEND_DEFAULT_MAGENTO_LUMA_":      "sAdd cache:tags:*_TRANSLATE",
		"sAdd cache:all_ids TRANSLATE_EN_US":                                                "sAdd cache:all_ids",
		"hGet zc:k:a1f_CONFIG_SCOPES d":                                                     "hGet zc:k:*_CONFIG_SCOPES",
		"get DEFAULT_FACADE":                                                                "get DEFAULT_FACADE",
		"get session:category12":                                                            "get session:category*",
		"get sess_0123456789abcdef0123":                                                     "get sess_*",
		"exec":                                                                              "exec",
		"Redis.mGet":                                                                        "mGet",
		"  GET   key:42  ":                                                                  "GET key:*",
		"":                                                                                  "",
	}
	for in, want := range cases {
		if got := normalizeCache(in); got != want {
			t.Errorf("normalizeCache(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestCacheCallDropsValue(t *testing.T) {
	s := Span{Name: "Redis.setEx", Resource: "Redis.setEx", Attrs: map[string]string{"redis.raw_command": "setEx 69d_:EAV 7200 a:4:{s:4:\"data\";}"}}
	if got := cacheCall(s); got != "setEx 69d_:EAV" {
		t.Errorf("cacheCall = %q", got)
	}
	s = Span{Name: "Redis.exec", Resource: "Redis.exec"}
	if got := cacheCall(s); got != "Redis.exec" {
		t.Errorf("cacheCall without raw command = %q", got)
	}
	s = Span{Name: "GET", Attrs: map[string]string{"db.statement": "GET foo"}}
	if got := cacheCall(s); got != "GET foo" {
		t.Errorf("cacheCall from db.statement = %q", got)
	}
}

func TestKindOf(t *testing.T) {
	cases := []struct {
		s    Span
		want string
	}{
		{Span{Name: "PDOStatement.execute", Component: "sql"}, "sql"},
		{Span{Name: "Redis.mGet", Component: "cache"}, "cache"},
		{Span{Name: "curl_exec", Component: "search"}, "search"},
		{Span{Name: "curl_exec", Component: "http"}, "http"},
		{Span{Name: "AMQPChannel.basic_publish", Component: "queue"}, "php"},
		{Span{Name: "magento.plugin.around", Component: "magento"}, "plugin"},
		{Span{Name: "magento.event.execute", Component: "magento"}, "observer"},
		{Span{Name: "magento.event.dispatch", Component: "magento"}, "event"},
		{Span{Name: "magento.block.render", Component: "magento"}, "block"},
		{Span{Name: "magento.template.render", Component: "magento"}, "template"},
		{Span{Name: "magento.launch", Component: "magento"}, "php"},
		{Span{Name: "Foo::bar", Component: "php"}, "php"},
		{Span{Name: "PDO.prepare", Type: "sql"}, "sql"},
	}
	for _, c := range cases {
		if got := kindOf(c.s); got != c.want {
			t.Errorf("kindOf(%s/%s) = %s, want %s", c.s.Name, c.s.Component, got, c.want)
		}
	}
}
