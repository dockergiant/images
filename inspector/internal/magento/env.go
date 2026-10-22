package magento

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
)

type Frontend struct {
	Backend        string `json:"backend"`
	Server         string `json:"server"`
	Port           int    `json:"port"`
	Database       int    `json:"database"`
	IDPrefix       string `json:"idPrefix"`
	CompressData   bool   `json:"compressData"`
	CompressionLib string `json:"compressionLib"`
	Serializer     string `json:"serializer"`
}

type AMQP struct {
	Host     string `json:"host"`
	Port     int    `json:"port"`
	VHost    string `json:"vhost"`
	User     string `json:"-"`
	Password string `json:"-"`
}

type Env struct {
	Generic          bool                `json:"generic"`
	Framework        string              `json:"framework"`
	FrameworkVersion string              `json:"frameworkVersion"`
	Mode             string              `json:"mode"`
	Cache            map[string]Frontend `json:"cache"`
	SessionSave      string              `json:"sessionSave"`
	SessionHost      string              `json:"sessionHost"`
	SessionDB        int                 `json:"sessionDb"`
	AMQP             *AMQP               `json:"amqp,omitempty"`
	HTTPCacheHosts   []string            `json:"httpCacheHosts"`
	CacheTypes       map[string]bool     `json:"cacheTypes"`
	DBHost           string              `json:"dbHost"`
	DBName           string              `json:"dbName"`
	TablePrefix      string              `json:"tablePrefix"`
	BaseURLs         map[string]string   `json:"baseUrls"`
	FPCApplication   string              `json:"fpcApplication"`
	SearchEngine     string              `json:"searchEngine"`
	Igbinary         bool                `json:"igbinary"`
	MTime            int64               `json:"mtime"`
}

const jsonMarker = "ROLL_INSPECTOR_JSON "

type envOutput struct {
	Generic          bool           `json:"generic"`
	MTime            int64          `json:"mtime"`
	Igbinary         bool           `json:"igbinary"`
	Framework        string         `json:"framework"`
	FrameworkVersion string         `json:"frameworkVersion"`
	Env              map[string]any `json:"env"`
}

var frameworks = map[string]string{
	"mage-os/framework": "mage-os",
	"magento/framework": "magento",
}

var fpcApplications = map[string]string{
	"1": "builtin",
	"2": "varnish",
}

func parseEnv(out []byte) (*Env, error) {
	payload, err := markedJSON(out)
	if err != nil {
		return nil, err
	}
	var raw envOutput
	if err := json.Unmarshal(payload, &raw); err != nil {
		return nil, fmt.Errorf("env.php reader: %w", err)
	}
	e := raw.Env
	env := &Env{
		Generic:          raw.Generic,
		Framework:        frameworks[raw.Framework],
		FrameworkVersion: raw.FrameworkVersion,
		Mode:             str(dig(e, "MAGE_MODE")),
		Cache:            map[string]Frontend{},
		CacheTypes:       map[string]bool{},
		BaseURLs:         map[string]string{},
		DBHost:           str(dig(e, "db", "connection", "default", "host")),
		DBName:           str(dig(e, "db", "connection", "default", "dbname")),
		TablePrefix:      str(dig(e, "db", "table_prefix")),
		SearchEngine:     str(dig(e, "system", "default", "catalog", "search", "engine")),
		Igbinary:         raw.Igbinary,
		MTime:            raw.MTime,
	}
	if raw.Framework != "" && env.Framework == "" {
		env.Framework = raw.Framework
	}

	if fes, ok := dig(e, "cache", "frontend").(map[string]any); ok {
		for name, v := range fes {
			fe, _ := v.(map[string]any)
			env.Cache[name] = Frontend{
				Backend:        str(dig(fe, "backend")),
				IDPrefix:       str(dig(fe, "id_prefix")),
				Server:         str(dig(fe, "backend_options", "server")),
				Port:           num(dig(fe, "backend_options", "port")),
				Database:       num(dig(fe, "backend_options", "database")),
				CompressData:   flag(dig(fe, "backend_options", "compress_data")),
				CompressionLib: str(dig(fe, "backend_options", "compression_lib")),
				Serializer:     str(dig(fe, "backend_options", "serializer")),
			}
		}
	}

	env.SessionSave = str(dig(e, "session", "save"))
	if env.SessionSave != "" {
		env.SessionHost = str(dig(e, "session", env.SessionSave, "host"))
		env.SessionDB = num(dig(e, "session", env.SessionSave, "database"))
	}

	if amqp, ok := dig(e, "queue", "amqp").(map[string]any); ok && str(amqp["host"]) != "" {
		env.AMQP = &AMQP{
			Host:     str(amqp["host"]),
			Port:     num(amqp["port"]),
			VHost:    str(amqp["virtualhost"]),
			User:     str(amqp["user"]),
			Password: str(amqp["password"]),
		}
	}

	if hosts, ok := dig(e, "http_cache_hosts").([]any); ok {
		for _, h := range hosts {
			host := str(dig(h, "host"))
			if host == "" {
				continue
			}
			if port := str(dig(h, "port")); port != "" {
				host = net.JoinHostPort(host, port)
			}
			env.HTTPCacheHosts = append(env.HTTPCacheHosts, host)
		}
	}

	if types, ok := dig(e, "cache_types").(map[string]any); ok {
		for name, v := range types {
			env.CacheTypes[name] = flag(v)
		}
	}

	if system, ok := dig(e, "system").(map[string]any); ok {
		baseURLs(env.BaseURLs, "default", system["default"])
		for _, scope := range []string{"websites", "stores"} {
			codes, _ := system[scope].(map[string]any)
			for code, cfg := range codes {
				baseURLs(env.BaseURLs, scope+"/"+code, cfg)
			}
		}
	}

	app := str(dig(e, "system", "default", "system", "full_page_cache", "caching_application"))
	if name, ok := fpcApplications[app]; ok {
		env.FPCApplication = name
	} else {
		env.FPCApplication = app
	}
	return env, nil
}

func baseURLs(out map[string]string, scope string, cfg any) {
	for _, kind := range []string{"unsecure", "secure"} {
		if u := str(dig(cfg, "web", kind, "base_url")); u != "" {
			out[scope+"/web/"+kind+"/base_url"] = u
		}
	}
}

func markedJSON(out []byte) ([]byte, error) {
	var found []byte
	for _, line := range bytes.Split(out, []byte("\n")) {
		if rest, ok := bytes.CutPrefix(bytes.TrimRight(line, "\r"), []byte(jsonMarker)); ok {
			found = rest
		}
	}
	if found == nil {
		return nil, errors.New("php script printed no result")
	}
	return found, nil
}

func dig(v any, path ...string) any {
	for _, k := range path {
		m, ok := v.(map[string]any)
		if !ok {
			return nil
		}
		v = m[k]
	}
	return v
}

func str(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case float64:
		return strconv.FormatFloat(t, 'f', -1, 64)
	case bool:
		if t {
			return "1"
		}
		return "0"
	}
	return ""
}

func num(v any) int {
	switch t := v.(type) {
	case float64:
		return int(t)
	case string:
		n, _ := strconv.Atoi(strings.TrimSpace(t))
		return n
	case bool:
		if t {
			return 1
		}
	}
	return 0
}

func flag(v any) bool {
	switch t := v.(type) {
	case bool:
		return t
	case float64:
		return t != 0
	case string:
		switch strings.ToLower(strings.TrimSpace(t)) {
		case "", "0", "false", "no", "off":
			return false
		}
		return true
	}
	return false
}
