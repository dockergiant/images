package rabbitmq

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/go-sql-driver/mysql"
)

var tableIdent = regexp.MustCompile(`^[A-Za-z0-9_]*$`)

var dbStatusNames = map[int]string{
	2: "new",
	3: "in_progress",
	4: "complete",
	5: "retry_required",
	6: "error",
	7: "to_be_deleted",
}

type dbQueue struct {
	Name     string         `json:"name"`
	Total    int64          `json:"total"`
	Statuses map[string]int `json:"statuses"`
	Oldest   *time.Time     `json:"oldestUpdate,omitempty"`
}

type dbQueueRow struct {
	Name   string
	Status int
	Count  int
	Oldest sql.NullTime
}

func parseEnv(env []string) map[string]string {
	out := map[string]string{}
	for _, kv := range env {
		if k, v, ok := strings.Cut(kv, "="); ok {
			out[k] = v
		}
	}
	return out
}

func dbConfig(env []string, database string) (*mysql.Config, error) {
	vars := parseEnv(env)
	user, pass := vars["MYSQL_USER"], vars["MYSQL_PASSWORD"]
	if user == "" {
		return nil, errors.New("db container has no MYSQL_USER")
	}
	if database == "" {
		database = vars["MYSQL_DATABASE"]
	}
	cfg := mysql.NewConfig()
	cfg.User = user
	cfg.Passwd = pass
	cfg.Net = "tcp"
	cfg.Addr = "db:3306"
	cfg.DBName = database
	cfg.Timeout = 3 * time.Second
	cfg.ReadTimeout = 5 * time.Second
	cfg.ParseTime = true
	return cfg, nil
}

func dbQueueSQL(prefix string) (string, error) {
	if !tableIdent.MatchString(prefix) {
		return "", fmt.Errorf("table prefix %q is not a plain identifier", prefix)
	}
	return fmt.Sprintf(
		"SELECT q.name, s.status, COUNT(*), MIN(s.updated_at) "+
			"FROM `%[1]squeue` q "+
			"JOIN `%[1]squeue_message_status` s ON s.queue_id = q.id "+
			"JOIN `%[1]squeue_message` m ON m.id = s.message_id "+
			"GROUP BY q.name, s.status", prefix), nil
}

func groupDBQueues(rows []dbQueueRow) []dbQueue {
	byName := map[string]*dbQueue{}
	for _, r := range rows {
		q := byName[r.Name]
		if q == nil {
			q = &dbQueue{Name: r.Name, Statuses: map[string]int{}}
			byName[r.Name] = q
		}
		label, ok := dbStatusNames[r.Status]
		if !ok {
			label = fmt.Sprintf("status_%d", r.Status)
		}
		q.Statuses[label] += r.Count
		q.Total += int64(r.Count)
		if r.Oldest.Valid && (q.Oldest == nil || r.Oldest.Time.Before(*q.Oldest)) {
			t := r.Oldest.Time
			q.Oldest = &t
		}
	}
	out := make([]dbQueue, 0, len(byName))
	for _, q := range byName {
		out = append(out, *q)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func readDBQueues(ctx context.Context, cfg *mysql.Config, prefix string) ([]dbQueue, error) {
	query, err := dbQueueSQL(prefix)
	if err != nil {
		return nil, err
	}
	connector, err := mysql.NewConnector(cfg)
	if err != nil {
		return nil, err
	}
	db := sql.OpenDB(connector)
	defer db.Close()
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	conn, err := db.Conn(ctx)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, "SET SESSION TRANSACTION READ ONLY"); err != nil {
		return nil, err
	}
	rows, err := conn.QueryContext(ctx, query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var all []dbQueueRow
	for rows.Next() {
		var r dbQueueRow
		if err := rows.Scan(&r.Name, &r.Status, &r.Count, &r.Oldest); err != nil {
			return nil, err
		}
		all = append(all, r)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return groupDBQueues(all), nil
}
