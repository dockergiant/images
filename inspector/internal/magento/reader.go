package magento

import (
	"bufio"
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	neturl "net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/dockergiant/images/inspector/internal/docker"
)

type Reader interface {
	Load(ctx context.Context) (*Env, error)
	CacheTypeTags(ctx context.Context) (map[string]string, error)
	FPCKey(ctx context.Context, url string) (string, error)
	ConfigShow(ctx context.Context, paths []string) (map[string]string, error)
	Consumers(ctx context.Context) (map[string]string, error)
	CacheClean(ctx context.Context, types []string) error
}

var (
	//go:embed envphp.php
	envScript []byte
	//go:embed cachetypes.php
	cacheTypesScript []byte
	//go:embed fpckey.php
	fpcKeyScript []byte
	//go:embed consumers.php
	consumersScript []byte
)

var ErrNoPHP = errors.New("php-fpm is not running")

// The entrypoint remaps www-data to the host uid, so execs as this user
// leave files owned the way php-fpm leaves them.
const PHPUser = "www-data"

const (
	phpService = "php-fpm"

	statEvery    = 5 * time.Second
	metadataTTL  = 10 * time.Minute
	configTTL    = 5 * time.Minute
	statTimeout  = 10 * time.Second
	envTimeout   = 30 * time.Second
	bootTimeout  = 90 * time.Second
	cleanTimeout = 5 * time.Minute
	maxURL       = 8192
)

var (
	configPath = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_-]*(/[A-Za-z0-9_-]+)*$`)
	dumpLine   = regexp.MustCompile(`^([A-Za-z0-9_-]+(?:/[A-Za-z0-9_-]+)+) -(?: (.*))?$`)
	cacheType  = regexp.MustCompile(`^[a-z0-9_]+$`)
)

type slot struct {
	mu        sync.Mutex
	value     map[string]string
	at        time.Time
	container string
}

type reader struct {
	docker docker.API
	now    func() time.Time

	mu           sync.Mutex
	env          *Env
	envContainer string
	envStat      fileTimes
	checked      time.Time

	tags      slot
	consumers slot
	config    slot
}

func NewReader(d docker.API) Reader {
	return &reader{docker: d, now: time.Now}
}

func (r *reader) Load(ctx context.Context) (*Env, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := r.now()
	if r.env != nil && now.Sub(r.checked) < statEvery {
		return r.env, nil
	}
	c, err := r.php(ctx)
	if err != nil {
		return nil, err
	}
	st, err := r.stat(ctx, c.ID)
	if err != nil {
		return nil, err
	}
	if r.env != nil && r.envContainer == c.ID && r.envStat == st {
		r.checked = now
		return r.env, nil
	}
	out, err := r.script(ctx, c.ID, envTimeout, envScript)
	if err != nil {
		return nil, fmt.Errorf("read env.php: %w", err)
	}
	env, err := parseEnv(out)
	if err != nil {
		return nil, err
	}
	r.env, r.envContainer, r.envStat, r.checked = env, c.ID, st, now
	return env, nil
}

const (
	envFile       = "app/etc/env.php"
	installedFile = "vendor/composer/installed.json"
)

// Generic mode depends on installed.json as much as on env.php, so a
// composer install with env.php untouched must also trigger a reread.
type fileTimes struct {
	env, installed int64
}

// stat exits non-zero when either file is missing but still prints the other,
// so the output is read whatever the exit code and a missing file stays 0.
func (r *reader) stat(ctx context.Context, container string) (fileTimes, error) {
	ctx, cancel := context.WithTimeout(ctx, statTimeout)
	defer cancel()
	res, err := r.docker.Exec(ctx, docker.ExecSpec{
		Container: container,
		User:      PHPUser,
		Cmd:       []string{"stat", "-c", "%n %Y", envFile, installedFile},
	})
	if err != nil {
		return fileTimes{}, fmt.Errorf("stat env.php: %w", err)
	}
	var st fileTimes
	for _, line := range strings.Split(string(res.Stdout), "\n") {
		name, val, ok := strings.Cut(strings.TrimSpace(line), " ")
		if !ok {
			continue
		}
		n, err := strconv.ParseInt(val, 10, 64)
		if err != nil {
			return fileTimes{}, fmt.Errorf("stat env.php: unexpected output %q", truncate(line))
		}
		switch name {
		case envFile:
			st.env = n
		case installedFile:
			st.installed = n
		}
	}
	return st, nil
}

func (r *reader) CacheTypeTags(ctx context.Context) (map[string]string, error) {
	return r.cached(ctx, &r.tags, metadataTTL, func(ctx context.Context, container string) (map[string]string, error) {
		return r.scriptMap(ctx, container, cacheTypesScript)
	})
}

func (r *reader) Consumers(ctx context.Context) (map[string]string, error) {
	return r.cached(ctx, &r.consumers, metadataTTL, func(ctx context.Context, container string) (map[string]string, error) {
		return r.scriptMap(ctx, container, consumersScript)
	})
}

func (r *reader) FPCKey(ctx context.Context, url string) (string, error) {
	if err := validURL(url); err != nil {
		return "", err
	}
	c, err := r.php(ctx)
	if err != nil {
		return "", err
	}
	out, err := r.script(ctx, c.ID, bootTimeout, fpcKeyScript, url)
	if err != nil {
		return "", fmt.Errorf("fpc key: %w", err)
	}
	payload, err := markedJSON(out)
	if err != nil {
		return "", fmt.Errorf("fpc key: %w", err)
	}
	var res struct {
		Key string `json:"key"`
	}
	if err := json.Unmarshal(payload, &res); err != nil {
		return "", fmt.Errorf("fpc key: %w", err)
	}
	if res.Key == "" {
		return "", errors.New("fpc key: Magento returned an empty identifier")
	}
	return res.Key, nil
}

func validURL(raw string) error {
	if raw == "" || len(raw) > maxURL {
		return errors.New("url is empty or too long")
	}
	for _, ch := range raw {
		if ch < 0x20 || ch == 0x7f {
			return errors.New("url contains control characters")
		}
	}
	u, err := neturl.Parse(raw)
	if err != nil {
		return fmt.Errorf("invalid url: %w", err)
	}
	if (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return errors.New("url must be an absolute http or https URL")
	}
	return nil
}

func (r *reader) ConfigShow(ctx context.Context, paths []string) (map[string]string, error) {
	for _, p := range paths {
		if !configPath.MatchString(p) {
			return nil, fmt.Errorf("invalid config path %q", p)
		}
	}
	if len(paths) == 0 {
		return map[string]string{}, nil
	}
	all, err := r.cached(ctx, &r.config, configTTL, r.configDump)
	if err != nil {
		return nil, err
	}
	out := map[string]string{}
	for path, value := range all {
		for _, p := range paths {
			if path == p || strings.HasPrefix(path, p+"/") {
				out[path] = value
				break
			}
		}
	}
	return out, nil
}

func (r *reader) configDump(ctx context.Context, container string) (map[string]string, error) {
	out, err := r.magento(ctx, container, bootTimeout, "config:show")
	if err != nil {
		return nil, err
	}
	return parseConfigDump(out), nil
}

func parseConfigDump(out []byte) map[string]string {
	values := map[string]string{}
	last := ""
	sc := bufio.NewScanner(bytes.NewReader(out))
	sc.Buffer(make([]byte, 64*1024), 16*1024*1024)
	for sc.Scan() {
		line := strings.TrimRight(sc.Text(), "\r")
		if m := dumpLine.FindStringSubmatch(line); m != nil {
			last = m[1]
			values[last] = strings.TrimRight(m[2], " ")
			continue
		}
		if last != "" {
			values[last] += "\n" + line
		}
	}
	for path := range values {
		if isSecret(path[strings.LastIndexByte(path, '/')+1:]) {
			values[path] = redacted
		}
	}
	return values
}

func (r *reader) CacheClean(ctx context.Context, types []string) error {
	for _, t := range types {
		if !cacheType.MatchString(t) {
			return fmt.Errorf("invalid cache type %q", t)
		}
	}
	c, err := r.php(ctx)
	if err != nil {
		return err
	}
	_, err = r.magento(ctx, c.ID, cleanTimeout, "cache:clean", types...)
	return err
}

func (r *reader) php(ctx context.Context) (*docker.Container, error) {
	c, err := r.docker.Service(ctx, phpService)
	if err != nil {
		return nil, err
	}
	if c == nil {
		return nil, ErrNoPHP
	}
	return c, nil
}

func (r *reader) cached(ctx context.Context, s *slot, ttl time.Duration, fetch func(ctx context.Context, container string) (map[string]string, error)) (map[string]string, error) {
	c, err := r.php(ctx)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.value == nil || s.container != c.ID || r.now().Sub(s.at) >= ttl {
		v, err := fetch(ctx, c.ID)
		if err != nil {
			return nil, err
		}
		s.value, s.container, s.at = v, c.ID, r.now()
	}
	out := make(map[string]string, len(s.value))
	for k, v := range s.value {
		out[k] = v
	}
	return out, nil
}

func (r *reader) scriptMap(ctx context.Context, container string, script []byte) (map[string]string, error) {
	out, err := r.script(ctx, container, bootTimeout, script)
	if err != nil {
		return nil, err
	}
	payload, err := markedJSON(out)
	if err != nil {
		return nil, err
	}
	m := map[string]string{}
	if err := json.Unmarshal(payload, &m); err != nil {
		return nil, err
	}
	return m, nil
}

func (r *reader) script(ctx context.Context, container string, timeout time.Duration, script []byte, args ...string) ([]byte, error) {
	cmd := []string{"php"}
	if len(args) > 0 {
		cmd = append(append(cmd, "--"), args...)
	}
	return r.run(ctx, timeout, docker.ExecSpec{
		Container:    container,
		User:         PHPUser,
		Cmd:          cmd,
		Stdin:        bytes.NewReader(script),
		KillOnCancel: true,
	})
}

func (r *reader) magento(ctx context.Context, container string, timeout time.Duration, command string, args ...string) ([]byte, error) {
	cmd := append([]string{"php", "bin/magento", command, "--no-ansi", "--no-interaction"}, args...)
	out, err := r.run(ctx, timeout, docker.ExecSpec{Container: container, User: PHPUser, Cmd: cmd, KillOnCancel: true})
	if err != nil {
		return nil, fmt.Errorf("bin/magento %s: %w", command, err)
	}
	return out, nil
}

func (r *reader) run(ctx context.Context, timeout time.Duration, spec docker.ExecSpec) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	res, err := r.docker.Exec(ctx, spec)
	if err != nil {
		return nil, err
	}
	if res.ExitCode != 0 {
		msg := strings.TrimSpace(string(res.Stderr))
		if msg == "" {
			msg = strings.TrimSpace(string(res.Stdout))
		}
		return nil, fmt.Errorf("exit %d: %s", res.ExitCode, truncate(msg))
	}
	return res.Stdout, nil
}

func truncate(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if len(s) > 300 {
		s = s[:300] + "…"
	}
	return s
}
