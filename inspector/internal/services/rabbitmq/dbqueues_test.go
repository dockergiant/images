package rabbitmq

import (
	"database/sql"
	"strings"
	"testing"
	"time"
)

func TestDBConfigFromContainerEnv(t *testing.T) {
	env := []string{"MYSQL_USER=magento", "MYSQL_PASSWORD=s3cret", "MYSQL_DATABASE=fallback", "OTHER=x=y"}
	cfg, err := dbConfig(env, "")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.User != "magento" || cfg.Passwd != "s3cret" || cfg.Addr != "db:3306" || cfg.DBName != "fallback" {
		t.Fatalf("%+v", cfg)
	}
	cfg, _ = dbConfig(env, "magento_db")
	if cfg.DBName != "magento_db" {
		t.Fatalf("env.php database name should win: %s", cfg.DBName)
	}
	if _, err := dbConfig([]string{"X=1"}, ""); err == nil {
		t.Fatal("missing MYSQL_USER should fail")
	}
}

func TestDBQueueSQLIsReadOnlySelectWithPrefix(t *testing.T) {
	q, err := dbQueueSQL("pre_")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(q, "SELECT ") || !strings.Contains(q, "`pre_queue_message_status`") || !strings.Contains(q, "`pre_queue_message`") {
		t.Fatal(q)
	}
	if _, err := dbQueueSQL("x`; DROP TABLE y; --"); err == nil {
		t.Fatal("hostile prefix must be refused")
	}
}

func TestGroupDBQueues(t *testing.T) {
	early := time.Unix(100, 0)
	late := time.Unix(900, 0)
	got := groupDBQueues([]dbQueueRow{
		{Name: "b", Status: 2, Count: 3, Oldest: sql.NullTime{Time: late, Valid: true}},
		{Name: "b", Status: 6, Count: 1, Oldest: sql.NullTime{Time: early, Valid: true}},
		{Name: "a", Status: 99, Count: 2},
	})
	if len(got) != 2 || got[0].Name != "a" {
		t.Fatalf("%+v", got)
	}
	b := got[1]
	if b.Total != 4 || b.Statuses["new"] != 3 || b.Statuses["error"] != 1 || b.Oldest == nil || !b.Oldest.Equal(early) {
		t.Fatalf("%+v", b)
	}
	if got[0].Statuses["status_99"] != 2 || got[0].Oldest != nil {
		t.Fatalf("%+v", got[0])
	}
}
