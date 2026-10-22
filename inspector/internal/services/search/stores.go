package search

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/go-sql-driver/mysql"

	"github.com/dockergiant/images/inspector/internal/docker"
	"github.com/dockergiant/images/inspector/internal/magento"
)

const storesTTL = time.Minute

var tablePrefixRe = regexp.MustCompile(`^[A-Za-z0-9_]*$`)

type storeSet struct {
	Magento bool
	Stores  map[int]string
	Err     error
}

type storeLoader struct {
	docker  docker.API
	magento magento.Reader

	mu   sync.Mutex
	at   time.Time
	last storeSet
}

func (l *storeLoader) load(ctx context.Context) storeSet {
	l.mu.Lock()
	defer l.mu.Unlock()
	if !l.at.IsZero() && time.Since(l.at) < storesTTL {
		return l.last
	}
	l.last = l.fetch(ctx)
	l.at = time.Now()
	return l.last
}

func (l *storeLoader) fetch(ctx context.Context) storeSet {
	if l.magento == nil {
		return storeSet{}
	}
	env, err := l.magento.Load(ctx)
	if err != nil {
		return storeSet{Magento: true, Err: err}
	}
	if env == nil || env.Generic {
		return storeSet{}
	}
	stores, err := l.query(ctx, env)
	return storeSet{Magento: true, Stores: stores, Err: err}
}

func dbCredentials(env []string) (user, pass, db string) {
	for _, kv := range env {
		k, v, _ := strings.Cut(kv, "=")
		switch k {
		case "MYSQL_USER", "MARIADB_USER":
			user = v
		case "MYSQL_PASSWORD", "MARIADB_PASSWORD":
			pass = v
		case "MYSQL_DATABASE", "MARIADB_DATABASE":
			db = v
		}
	}
	return user, pass, db
}

func (l *storeLoader) query(ctx context.Context, env *magento.Env) (map[int]string, error) {
	if !tablePrefixRe.MatchString(env.TablePrefix) {
		return nil, fmt.Errorf("unexpected table prefix %q", env.TablePrefix)
	}
	ct, err := l.docker.Service(ctx, "db")
	if err != nil {
		return nil, err
	}
	if ct == nil {
		return nil, errors.New("db is not running")
	}
	vars, err := l.docker.ContainerEnv(ctx, ct.ID)
	if err != nil {
		return nil, err
	}
	user, pass, name := dbCredentials(vars)
	if env.DBName != "" {
		name = env.DBName
	}
	if user == "" || name == "" {
		return nil, errors.New("db container has no MYSQL_USER or MYSQL_DATABASE")
	}
	cfg := mysql.NewConfig()
	cfg.User, cfg.Passwd, cfg.Net, cfg.Addr, cfg.DBName = user, pass, "tcp", "db:3306", name
	cfg.Timeout, cfg.ReadTimeout = 3*time.Second, 5*time.Second
	db, err := sql.Open("mysql", cfg.FormatDSN())
	if err != nil {
		return nil, err
	}
	defer db.Close()
	ctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	conn, err := db.Conn(ctx)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, "SET SESSION TRANSACTION READ ONLY"); err != nil {
		return nil, err
	}
	rows, err := conn.QueryContext(ctx, "SELECT store_id, code FROM `"+env.TablePrefix+"store`")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int]string{}
	for rows.Next() {
		var id int
		var code string
		if err := rows.Scan(&id, &code); err != nil {
			return nil, err
		}
		out[id] = code
	}
	return out, rows.Err()
}
