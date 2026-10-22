package redis

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	goredis "github.com/redis/go-redis/v9"

	"github.com/dockergiant/images/inspector/internal/core"
	"github.com/dockergiant/images/inspector/internal/services"
)

const (
	scanCount   = 200
	maxMembers  = 1000
	maxTags     = 2000
	scanRounds  = 50
	viewIDTag   = "_VIEW_ID_"
	unknownFind = "unknown"
)

func (a *adapter) Browse() []core.Route {
	return []core.Route{
		{Method: http.MethodGet, Path: "/metrics", Handler: a.history.ServeHTTP, Params: []core.Param{core.WindowParam}},
		{Method: http.MethodGet, Path: "/types", Handler: a.handleTypes},
		{Method: http.MethodGet, Path: "/keys", Handler: a.handleKeys, Params: []core.Param{dbParam,
			{Name: "cursor", Type: "integer", Description: "The SCAN cursor the last page returned as next; 0 or empty starts over."},
			{Name: "match", Type: "string", Description: "A SCAN glob such as zc:k:*; default *."},
			{Name: "type", Type: "string", Description: "Only keys of this type: string, hash, list, set, zset or stream."},
		}},
		{Method: http.MethodGet, Path: "/key", Handler: a.handleKey, Params: []core.Param{dbParam,
			{Name: "key", Type: "string", Required: true, Description: "The full key name."},
			{Name: "force", Type: "integer", Description: "1 loads a value larger than the preview limit."},
		}},
		{Method: http.MethodGet, Path: "/tags", Handler: a.handleTags, Params: []core.Param{dbParam,
			{Name: "prefix", Type: "string", Description: "Only tags that start with this, such as cat_p_."},
			{Name: "all", Type: "integer", Description: "1 also lists the per-view tags that are hidden by default."},
		}},
		{Method: http.MethodGet, Path: "/tag", Handler: a.handleTag, Params: []core.Param{dbParam,
			{Name: "tag", Type: "string", Required: true, Description: "A Magento cache tag, such as cat_p_42."},
		}},
		{Method: http.MethodGet, Path: "/clients", Handler: a.handleClients},
		{Method: http.MethodGet, Path: "/slowlog", Handler: a.handleSlowlog},
		{Method: http.MethodGet, Path: "/find", Handler: a.handleFind, Params: []core.Param{
			{Name: "url", Type: "string", Required: true, Description: "The page URL to find the full-page cache entries of."},
		}},
	}
}

var dbParam = core.Param{Name: "db", Type: "integer", Description: "The database number; default 0."}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, status int, err error) {
	writeJSON(w, status, services.ErrorBody(status, err.Error()))
}

type cacheType struct {
	Name           string `json:"name"`
	Label          string `json:"label"`
	DB             int    `json:"db"`
	Backend        string `json:"backend"`
	Prefix         string `json:"prefix"`
	Compression    string `json:"compression,omitempty"`
	Serializer     string `json:"serializer,omitempty"`
	SerializerNote string `json:"serializerNote,omitempty"`
	Keys           int64  `json:"keys"`
	Layout         string `json:"layout"`
}

var typeLabels = map[string]string{"default": "Default cache", "page_cache": "Page cache", "session": "Sessions"}

func label(name string) string {
	if l, ok := typeLabels[name]; ok {
		return l
	}
	return name
}

func (a *adapter) handleTypes(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	text, err := a.client(0).Info(ctx, "keyspace").Result()
	if err != nil {
		writeErr(w, http.StatusBadGateway, err)
		return
	}
	keyspace := ParseKeyspace(ParseInfo(text))
	keys := map[int]int64{}
	for _, ks := range keyspace {
		keys[ks.DB] = ks.Keys
	}
	resp := map[string]any{"generic": true, "types": []cacheType{}, "databases": keyspace}
	env := a.env(ctx)
	if env == nil {
		writeJSON(w, http.StatusOK, resp)
		return
	}
	var types []cacheType
	for name, fe := range env.Cache {
		if fe.Server == "" && !strings.Contains(strings.ToLower(fe.Backend), "redis") && !strings.Contains(strings.ToLower(fe.Backend), "valkey") {
			continue
		}
		t := cacheType{
			Name:       name,
			Label:      label(name),
			DB:         fe.Database,
			Backend:    fe.Backend,
			Prefix:     fe.IDPrefix,
			Serializer: fe.Serializer,
			Keys:       keys[fe.Database],
		}
		if fe.CompressData {
			t.Compression = fe.CompressionLib
			if t.Compression == "" {
				t.Compression = "gzip"
			}
		}
		if fe.Serializer == "igbinary" && !env.Igbinary {
			t.SerializerNote = "igbinary requested, php used"
		}
		t.Layout, _ = a.layoutOf(ctx, fe.Database)
		types = append(types, t)
	}
	if s := strings.ToLower(env.SessionSave); s == "redis" || s == "valkey" {
		types = append(types, cacheType{Name: "session", Label: label("session"), DB: env.SessionDB, Backend: env.SessionSave, Keys: keys[env.SessionDB]})
	}
	order := map[string]int{"default": 0, "page_cache": 1, "session": 2}
	sort.Slice(types, func(i, j int) bool {
		oi, ok := order[types[i].Name]
		if !ok {
			oi = 9
		}
		oj, ok := order[types[j].Name]
		if !ok {
			oj = 9
		}
		if oi != oj {
			return oi < oj
		}
		return types[i].Name < types[j].Name
	})
	resp["generic"] = false
	if types != nil {
		resp["types"] = types
	}
	writeJSON(w, http.StatusOK, resp)
}

type keyRow struct {
	Key    string `json:"key"`
	Type   string `json:"type"`
	TTL    int64  `json:"ttl"`
	Memory int64  `json:"memory,omitempty"`
	Family string `json:"family,omitempty"`
	ID     string `json:"id,omitempty"`
}

func family(layout, key string) string {
	switch layout {
	case LayoutSymfony:
		switch {
		case strings.HasPrefix(key, symfonyTags):
			return "tag"
		case strings.HasPrefix(key, symfonyIDTags):
			return "reverse-index"
		case key == symfonyAllIDs:
			return "index"
		case entryID(layout, key) != "":
			return "entry"
		}
	case LayoutCm:
		switch {
		case strings.HasPrefix(key, cmKey):
			return "entry"
		case strings.HasPrefix(key, cmTagIDs):
			return "tag"
		case key == cmTags || key == cmIDs:
			return "index"
		}
	}
	if strings.HasPrefix(key, "sess_") || strings.HasPrefix(key, "PHPREDIS_SESSION:") {
		return "session"
	}
	return ""
}

func ttlSeconds(d time.Duration) int64 {
	if d < 0 {
		return int64(d)
	}
	return int64((d + time.Second - 1) / time.Second)
}

func (a *adapter) handleKeys(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	q := r.URL.Query()
	db, err := parseDB(q.Get("db"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	var cursor uint64
	if v := q.Get("cursor"); v != "" {
		if cursor, err = strconv.ParseUint(v, 10, 64); err != nil {
			writeErr(w, http.StatusBadRequest, fmt.Errorf("invalid cursor %q", v))
			return
		}
	}
	match := q.Get("match")
	if match == "" {
		match = "*"
	}
	typ := q.Get("type")
	c := a.client(db)
	var keys []string
	var next uint64
	if typ != "" && a.feature("scanType") {
		keys, next, err = c.ScanType(ctx, cursor, match, scanCount, typ).Result()
	} else {
		keys, next, err = c.Scan(ctx, cursor, match, scanCount).Result()
	}
	if err != nil {
		writeErr(w, http.StatusBadGateway, err)
		return
	}
	layout, _ := a.layoutOf(ctx, db)
	rows := a.describe(ctx, c, layout, keys)
	if typ != "" {
		kept := rows[:0]
		for _, row := range rows {
			if row.Type == typ {
				kept = append(kept, row)
			}
		}
		rows = kept
	}
	writeJSON(w, http.StatusOK, map[string]any{"db": db, "cursor": strconv.FormatUint(next, 10), "layout": layout, "keys": rows})
}

func (a *adapter) describe(ctx context.Context, c *goredis.Client, layout string, keys []string) []keyRow {
	memory := a.feature("memoryUsage")
	pipe := c.Pipeline()
	types := make([]*goredis.StatusCmd, len(keys))
	ttls := make([]*goredis.DurationCmd, len(keys))
	mems := make([]*goredis.IntCmd, len(keys))
	for i, k := range keys {
		types[i] = pipe.Type(ctx, k)
		ttls[i] = pipe.PTTL(ctx, k)
		if memory {
			mems[i] = pipe.MemoryUsage(ctx, k)
		}
	}
	if len(keys) > 0 {
		_, _ = pipe.Exec(ctx)
	}
	rows := make([]keyRow, 0, len(keys))
	for i, k := range keys {
		row := keyRow{Key: shown(k), Type: types[i].Val(), TTL: ttlSeconds(ttls[i].Val()), Family: family(layout, k), ID: entryID(layout, k)}
		if mems[i] != nil {
			row.Memory = mems[i].Val()
		}
		rows = append(rows, row)
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].Key < rows[j].Key })
	return rows
}

func shown(s string) string {
	if printable(s) {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '\\':
			b.WriteString(`\\`)
		case c >= 0x20 && c < 0x7f:
			b.WriteByte(c)
		default:
			fmt.Fprintf(&b, `\x%02x`, c)
		}
	}
	return b.String()
}

type keyView struct {
	Key       string `json:"key"`
	DB        int    `json:"db"`
	Type      string `json:"type"`
	TTL       int64  `json:"ttl"`
	Size      int64  `json:"size"`
	Memory    int64  `json:"memory,omitempty"`
	Encoding  string `json:"encoding,omitempty"`
	Family    string `json:"family,omitempty"`
	TooLarge  bool   `json:"tooLarge,omitempty"`
	Entry     *Entry `json:"entry,omitempty"`
	Value     any    `json:"value,omitempty"`
	Truncated bool   `json:"truncated,omitempty"`
}

var errNoKey = errors.New("key not found")

func (a *adapter) loadKey(ctx context.Context, db int, key string, force bool) (keyView, error) {
	c := a.client(db)
	v := keyView{Key: shown(key), DB: db}
	typ, err := c.Type(ctx, key).Result()
	if err != nil {
		return v, err
	}
	if typ == "none" {
		return v, errNoKey
	}
	v.Type = typ
	layout, _ := a.layoutOf(ctx, db)
	v.Family = family(layout, key)

	pipe := c.Pipeline()
	ttl := pipe.PTTL(ctx, key)
	var mem *goredis.IntCmd
	var enc *goredis.StringCmd
	if a.feature("memoryUsage") {
		mem = pipe.MemoryUsage(ctx, key)
	}
	if a.feature("objectEncoding") {
		enc = pipe.ObjectEncoding(ctx, key)
	}
	var size *goredis.IntCmd
	switch {
	case typ == "string":
		size = pipe.StrLen(ctx, key)
	case typ == "hash" && strings.HasPrefix(key, cmKey):
		size = pipe.HStrLen(ctx, key, "d")
	}
	_, _ = pipe.Exec(ctx)
	v.TTL = ttlSeconds(ttl.Val())
	if mem != nil {
		v.Memory = mem.Val()
	}
	if enc != nil {
		v.Encoding = enc.Val()
	}
	v.Size = v.Memory
	if size != nil {
		v.Size = size.Val()
	}
	if v.Size > maxValueBytes && !force {
		v.TooLarge = true
		return v, nil
	}

	switch typ {
	case "string":
		raw, err := c.Get(ctx, key).Bytes()
		if err != nil {
			return v, err
		}
		var e Entry
		decoded := false
		if id := entryID(layout, key); layout == LayoutSymfony && id != "" {
			if e, err = DecodeSymfony(raw); err == nil {
				e.ID = id
				decoded = true
				if len(e.Tags) == 0 {
					ns, _, _ := strings.Cut(key, ":")
					e.Tags, _ = c.SMembers(ctx, symfonyIDTags+ns+id).Result()
					if e.Tags == nil {
						e.Tags = []string{}
					}
				}
			}
		}
		if !decoded {
			e = Entry{Size: len(raw), Tags: []string{}}
			classify(&e, raw)
		}
		e.Key = v.Key
		applyTTL(&e, ttl.Val())
		v.Entry = &e
	case "hash":
		h, err := c.HGetAll(ctx, key).Result()
		if err != nil {
			return v, err
		}
		if id := entryID(layout, key); id != "" || strings.HasPrefix(key, cmKey) {
			fields := make(map[string][]byte, len(h))
			for k, val := range h {
				fields[k] = []byte(val)
			}
			if e, err := DecodeCm(fields); err == nil {
				e.ID = strings.TrimPrefix(key, cmKey)
				e.Key = v.Key
				applyTTL(&e, ttl.Val())
				v.Entry = &e
				break
			}
		}
		out := map[string]string{}
		for k, val := range h {
			if len(out) >= maxMembers {
				v.Truncated = true
				break
			}
			out[shown(k)] = preview(val)
		}
		v.Value = out
	case "set":
		members, more, err := a.members(ctx, c, key, maxMembers)
		if err != nil {
			return v, err
		}
		for i := range members {
			members[i] = preview(members[i])
		}
		v.Value, v.Truncated = members, more
	case "list":
		items, err := c.LRange(ctx, key, 0, maxMembers-1).Result()
		if err != nil {
			return v, err
		}
		for i := range items {
			items[i] = preview(items[i])
		}
		v.Value = items
	case "zset":
		items, err := c.ZRangeWithScores(ctx, key, 0, maxMembers-1).Result()
		if err != nil {
			return v, err
		}
		out := make([]map[string]any, 0, len(items))
		for _, z := range items {
			out = append(out, map[string]any{"member": preview(fmt.Sprint(z.Member)), "score": z.Score})
		}
		v.Value = out
	case "stream":
		msgs, err := c.XRangeN(ctx, key, "-", "+", 100).Result()
		if err != nil {
			return v, err
		}
		v.Value = msgs
	}
	return v, nil
}

func preview(s string) string {
	out := shown(s)
	if len(out) > maxPreview {
		return out[:maxPreview] + "…"
	}
	return out
}

func applyTTL(e *Entry, ttl time.Duration) {
	switch {
	case ttl > 0:
		e.Expire = time.Now().Add(ttl).Unix()
		e.Infinite = false
	case ttl == -1 && e.Expire == 0:
		e.Infinite = true
	}
}

func (a *adapter) members(ctx context.Context, c *goredis.Client, key string, limit int) ([]string, bool, error) {
	var out []string
	var cursor uint64
	for i := 0; i < scanRounds; i++ {
		batch, next, err := c.SScan(ctx, key, cursor, "", 1000).Result()
		if err != nil {
			return nil, false, err
		}
		out = append(out, batch...)
		if len(out) >= limit {
			return out[:limit], true, nil
		}
		if next == 0 {
			return out, false, nil
		}
		cursor = next
	}
	return out, true, nil
}

func (a *adapter) handleKey(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	db, err := parseDB(q.Get("db"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	key := q.Get("key")
	if key == "" {
		writeErr(w, http.StatusBadRequest, errors.New("key is required"))
		return
	}
	v, err := a.loadKey(r.Context(), db, key, q.Get("force") == "1")
	if errors.Is(err, errNoKey) {
		writeErr(w, http.StatusNotFound, err)
		return
	}
	if err != nil {
		writeErr(w, http.StatusBadGateway, err)
		return
	}
	writeJSON(w, http.StatusOK, v)
}

func globEscape(s string) string {
	var b strings.Builder
	for _, r := range s {
		if strings.ContainsRune(`*?[]\`, r) {
			b.WriteByte('\\')
		}
		b.WriteRune(r)
	}
	return b.String()
}

type tagRow struct {
	Tag   string `json:"tag"`
	Key   string `json:"key"`
	Count int64  `json:"count"`
}

func (a *adapter) handleTags(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	q := r.URL.Query()
	db, err := parseDB(q.Get("db"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	layout, ns := a.layoutOf(ctx, db)
	resp := map[string]any{"db": db, "layout": layout, "namespace": ns, "tags": []tagRow{}, "hidden": 0, "partial": false}
	if layout == "" {
		writeJSON(w, http.StatusOK, resp)
		return
	}
	base := tagKey(layout, ns, "")
	pattern := globEscape(base) + globEscape(q.Get("prefix")) + "*"
	c := a.client(db)
	var keys []string
	var cursor uint64
	partial := false
	for i := 0; ; i++ {
		if i == scanRounds || len(keys) >= maxTags {
			partial = true
			break
		}
		batch, next, err := c.Scan(ctx, cursor, pattern, 1000).Result()
		if err != nil {
			writeErr(w, http.StatusBadGateway, err)
			return
		}
		keys = append(keys, batch...)
		if next == 0 {
			break
		}
		cursor = next
	}
	all := q.Get("all") == "1"
	hidden := 0
	var shownKeys []string
	for _, k := range keys {
		if !all && strings.Contains(k[len(base):], viewIDTag) {
			hidden++
			continue
		}
		shownKeys = append(shownKeys, k)
	}
	pipe := c.Pipeline()
	counts := make([]*goredis.IntCmd, len(shownKeys))
	for i, k := range shownKeys {
		counts[i] = pipe.SCard(ctx, k)
	}
	if len(shownKeys) > 0 {
		_, _ = pipe.Exec(ctx)
	}
	rows := make([]tagRow, 0, len(shownKeys))
	for i, k := range shownKeys {
		rows = append(rows, tagRow{Tag: shown(k[len(base):]), Key: shown(k), Count: counts[i].Val()})
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].Count != rows[j].Count {
			return rows[i].Count > rows[j].Count
		}
		return rows[i].Tag < rows[j].Tag
	})
	resp["tags"] = rows
	resp["hidden"] = hidden
	resp["partial"] = partial
	writeJSON(w, http.StatusOK, resp)
}

type tagMember struct {
	ID     string `json:"id"`
	Key    string `json:"key"`
	Exists bool   `json:"exists"`
}

func (a *adapter) handleTag(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	q := r.URL.Query()
	db, err := parseDB(q.Get("db"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	tag := q.Get("tag")
	if tag == "" {
		writeErr(w, http.StatusBadRequest, errors.New("tag is required"))
		return
	}
	layout, ns := a.layoutOf(ctx, db)
	if layout == "" {
		writeErr(w, http.StatusNotFound, fmt.Errorf("db %d holds no Magento cache layout", db))
		return
	}
	c := a.client(db)
	key := tagKey(layout, ns, tag)
	total, err := c.SCard(ctx, key).Result()
	if err != nil {
		writeErr(w, http.StatusBadGateway, err)
		return
	}
	ids, more, err := a.members(ctx, c, key, maxMembers)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err)
		return
	}
	sort.Strings(ids)
	pipe := c.Pipeline()
	exists := make([]*goredis.IntCmd, len(ids))
	for i, id := range ids {
		exists[i] = pipe.Exists(ctx, dataKey(layout, ns, id))
	}
	if len(ids) > 0 {
		_, _ = pipe.Exec(ctx)
	}
	members := make([]tagMember, 0, len(ids))
	stale := 0
	for i, id := range ids {
		m := tagMember{ID: shown(id), Key: shown(dataKey(layout, ns, id)), Exists: exists[i].Val() > 0}
		if !m.Exists {
			stale++
		}
		members = append(members, m)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"db": db, "tag": tag, "key": key, "layout": layout, "total": total,
		"members": members, "stale": stale, "truncated": more,
	})
}

func (a *adapter) handleClients(w http.ResponseWriter, r *http.Request) {
	text, err := a.client(0).ClientList(r.Context()).Result()
	if err != nil {
		writeErr(w, http.StatusBadGateway, err)
		return
	}
	clients := ParseClientList(text)
	out := make([]map[string]any, 0, len(clients))
	for _, c := range clients {
		row := make(map[string]any, len(c)+4)
		for k, v := range c {
			row[k] = v
		}
		row["self"] = c["name"] == inspectorTag
		if ip, port := splitAddr(c["addr"]); ip != "" && a.proc != nil {
			if p, ok := a.proc.Lookup(ip, port); ok {
				row["pid"] = p.PID
				row["container"] = p.Container
				row["role"] = p.Role
				row["process"] = p.Cmd
			} else if name, ok := a.proc.ContainerOf(ip); ok {
				row["container"] = name
			}
		}
		out = append(out, row)
	}
	writeJSON(w, http.StatusOK, map[string]any{"clients": out})
}

func (a *adapter) handleSlowlog(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	resp := map[string]any{"slowlog": a.slowlog(ctx, 128)}
	if text, err := a.client(0).Info(ctx, "commandstats").Result(); err == nil {
		resp["commandstats"] = ParseCommandStats(ParseInfo(text))
	}
	if a.feature("latencyStats") {
		if text, err := a.client(0).Info(ctx, "latencystats").Result(); err == nil {
			resp["latency"] = ParseLatencyStats(ParseInfo(text))
		}
	}
	writeJSON(w, http.StatusOK, resp)
}

func (a *adapter) handleFind(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	url := r.URL.Query().Get("url")
	if url == "" {
		writeErr(w, http.StatusBadRequest, errors.New("url is required"))
		return
	}
	resp := map[string]any{"url": url, "found": false}
	env := a.env(ctx)
	if env == nil {
		resp["reason"] = "Not a Magento 2 project"
		writeJSON(w, http.StatusOK, resp)
		return
	}
	fe, ok := env.Cache["page_cache"]
	if !ok || fe.Server == "" && !strings.Contains(strings.ToLower(fe.Backend), "redis") {
		resp["reason"] = "The page cache does not use Redis"
		writeJSON(w, http.StatusOK, resp)
		return
	}
	id, err := a.deps.Magento.FPCKey(ctx, url)
	if err != nil || id == "" {
		resp["reason"] = unknownFind
		writeJSON(w, http.StatusOK, resp)
		return
	}
	db := fe.Database
	layout, _ := a.layoutOf(ctx, db)
	id = cleanID(id)
	key := dataKey(layout, fe.IDPrefix, id)
	if layout == LayoutCm {
		key = cmKey + fe.IDPrefix + id
	}
	resp["id"] = id
	resp["key"] = key
	resp["db"] = db
	v, err := a.loadKey(ctx, db, key, false)
	if errors.Is(err, errNoKey) {
		resp["reason"] = "Not cached yet"
		writeJSON(w, http.StatusOK, resp)
		return
	}
	if err != nil {
		writeErr(w, http.StatusBadGateway, err)
		return
	}
	if v.Entry != nil {
		v.Entry.URL = url
	}
	resp["found"] = true
	resp["entry"] = v
	writeJSON(w, http.StatusOK, resp)
}
