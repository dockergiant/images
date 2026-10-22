package redis

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	goredis "github.com/redis/go-redis/v9"

	"github.com/dockergiant/images/inspector/internal/core"
	"github.com/dockergiant/images/inspector/internal/services"
)

// INSPECTOR_TEST_REDIS points at a throwaway Redis or Valkey; the test flushes
// databases 0 to 2.
func liveAdapter(t *testing.T) (*adapter, func(db int) *goredis.Client) {
	addr := os.Getenv("INSPECTOR_TEST_REDIS")
	if addr == "" {
		t.Skip("INSPECTOR_TEST_REDIS not set")
	}
	seed := func(db int) *goredis.Client {
		c := goredis.NewClient(&goredis.Options{Addr: addr, DB: db, Protocol: 2})
		t.Cleanup(func() { c.Close() })
		return c
	}
	ctx := context.Background()
	sym, cm := loadEntries(t)
	for db := 0; db < 3; db++ {
		if err := seed(db).FlushDB(ctx).Err(); err != nil {
			t.Fatal(err)
		}
	}
	s := seed(0)
	s.Set(ctx, "69d_:PAGE1", sym["page-json"], 0)
	s.Set(ctx, "69d_:CFG", sym["config-php"], time.Hour)
	s.SAdd(ctx, symfonyAllIDs, "PAGE1", "CFG")
	s.SAdd(ctx, "cache:tags:69d_FPC", "PAGE1", "GONE")
	s.SAdd(ctx, "cache:tags:69d_CMS_P_2", "PAGE1")
	s.SAdd(ctx, "cache:tags:69d_CONFIG", "CFG")
	s.SAdd(ctx, "cache:tags:69d_CAT_P_VIEW_ID_1", "CFG")
	s.SAdd(ctx, "cache:id_tags:69d_PAGE1", "FPC", "CMS_P_2")
	s.SAdd(ctx, "cache:id_tags:69d_CFG", "CONFIG", "CAT_P_VIEW_ID_1")

	c := seed(1)
	h := map[string]any{}
	for k, v := range cm["page-gz"] {
		h[k] = v
	}
	c.HSet(ctx, "zc:k:69d_PAGE", h)
	c.SAdd(ctx, "zc:ti:69d_FPC", "69d_PAGE")
	c.SAdd(ctx, "zc:ti:69d_CAT_C_41", "69d_PAGE", "69d_OTHER")
	c.SAdd(ctx, cmTags, "69d_FPC", "69d_CAT_C_41")
	c.SAdd(ctx, cmIDs, "69d_PAGE", "69d_OTHER")
	c.HSet(ctx, "zc:k:69d_OTHER", "d", "x", "t", "69d_CAT_C_41", "m", "1", "i", "0")

	seed(2).Set(ctx, "big", strings.Repeat("b", 2<<20), 0)

	a := New(services.Deps{}).(*adapter)
	a.addr = addr
	return a, seed
}

func get(t *testing.T, a *adapter, path string, out any) int {
	t.Helper()
	mux := http.NewServeMux()
	for _, r := range a.Browse() {
		mux.HandleFunc(r.Method+" "+r.Path, r.Handler)
	}
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	if err := json.Unmarshal(rec.Body.Bytes(), out); err != nil {
		t.Fatalf("%s: %v: %s", path, err, rec.Body.String())
	}
	return rec.Code
}

func TestLiveBrowse(t *testing.T) {
	a, _ := liveAdapter(t)
	ctx := context.Background()
	info, _ := a.Detect(ctx)
	if !info.Present || info.Engine == "" || info.Error != "" {
		t.Fatalf("detect %+v", info)
	}

	var keys struct {
		Cursor string   `json:"cursor"`
		Layout string   `json:"layout"`
		Keys   []keyRow `json:"keys"`
	}
	get(t, a, "/keys?db=0", &keys)
	if keys.Layout != LayoutSymfony || len(keys.Keys) != 9 {
		t.Fatalf("keys %+v", keys)
	}
	fam := map[string]string{}
	for _, k := range keys.Keys {
		fam[k.Key] = k.Family + "/" + k.ID
	}
	if fam["69d_:PAGE1"] != "entry/PAGE1" || fam["cache:tags:69d_FPC"] != "tag/" || fam[symfonyAllIDs] != "index/" {
		t.Errorf("families %v", fam)
	}

	var page keyView
	get(t, a, "/key?db=0&key=69d_:PAGE1", &page)
	if page.Entry == nil || page.Entry.DataForm != "page" || page.Entry.ID != "PAGE1" || !page.Entry.Infinite || page.Encoding == "" {
		t.Errorf("symfony page %+v %+v", page, page.Entry)
	}
	var cfg keyView
	get(t, a, "/key?db=0&key=69d_:CFG", &cfg)
	if cfg.Entry == nil || cfg.Entry.Infinite || cfg.TTL <= 0 || cfg.Entry.Expire == 0 {
		t.Errorf("symfony config %+v", cfg)
	}

	var cmPage keyView
	get(t, a, "/key?db=1&key=zc:k:69d_PAGE", &cmPage)
	if cmPage.Entry == nil || cmPage.Entry.DataForm != "page" || cmPage.Entry.Compression != "gzip" || cmPage.Entry.ID != "69d_PAGE" {
		t.Errorf("cm page %+v", cmPage)
	}

	var big keyView
	get(t, a, "/key?db=2&key=big", &big)
	if !big.TooLarge || big.Entry != nil || big.Size != 2<<20 {
		t.Errorf("big %+v", big)
	}
	get(t, a, "/key?db=2&key=big&force=1", &big)
	if big.Entry == nil || !big.Entry.Truncated {
		t.Errorf("forced big %+v", big.Entry)
	}
	var missing map[string]string
	if code := get(t, a, "/key?db=2&key=nope", &missing); code != http.StatusNotFound {
		t.Errorf("missing key: %d", code)
	}

	var tags struct {
		Namespace string   `json:"namespace"`
		Tags      []tagRow `json:"tags"`
		Hidden    int      `json:"hidden"`
	}
	get(t, a, "/tags?db=0", &tags)
	if tags.Namespace != "69d_" || tags.Hidden != 1 || len(tags.Tags) != 3 || tags.Tags[0].Tag != "FPC" || tags.Tags[0].Count != 2 {
		t.Errorf("tags %+v", tags)
	}
	get(t, a, "/tags?db=0&all=1", &tags)
	if len(tags.Tags) != 4 {
		t.Errorf("all tags %+v", tags)
	}
	var tag struct {
		Total   int64       `json:"total"`
		Stale   int         `json:"stale"`
		Members []tagMember `json:"members"`
	}
	get(t, a, "/tag?db=0&tag=FPC", &tag)
	if tag.Total != 2 || tag.Stale != 1 || len(tag.Members) != 2 {
		t.Errorf("tag %+v", tag)
	}

	var cmTagsResp struct {
		Layout string   `json:"layout"`
		Tags   []tagRow `json:"tags"`
	}
	get(t, a, "/tags?db=1", &cmTagsResp)
	if cmTagsResp.Layout != LayoutCm || len(cmTagsResp.Tags) != 2 || cmTagsResp.Tags[0].Tag != "69d_CAT_C_41" {
		t.Errorf("cm tags %+v", cmTagsResp)
	}

	var clients struct {
		Clients []map[string]any `json:"clients"`
	}
	get(t, a, "/clients", &clients)
	self := false
	for _, c := range clients.Clients {
		if c["self"] == true {
			self = true
		}
	}
	if !self {
		t.Errorf("own client not marked: %v", clients.Clients)
	}

	var types struct {
		Generic   bool       `json:"generic"`
		Databases []Keyspace `json:"databases"`
	}
	get(t, a, "/types", &types)
	if !types.Generic || len(types.Databases) != 3 {
		t.Errorf("types %+v", types)
	}

	var slow map[string]any
	get(t, a, "/slowlog", &slow)
	if _, ok := slow["slowlog"]; !ok {
		t.Errorf("slowlog %v", slow)
	}

	h, err := a.Health(ctx)
	if err != nil || h.Status == core.Error || len(h.Cards) != 7 {
		t.Errorf("health %+v %v", h, err)
	}
}

func TestLiveActions(t *testing.T) {
	a, seed := liveAdapter(t)
	ctx := context.Background()
	if _, err := a.Detect(ctx); err != nil {
		t.Fatal(err)
	}
	run := func(name string, req core.ActionRequest) core.ActionResult {
		t.Helper()
		for _, s := range a.Actions() {
			if s.Name == name {
				res, err := s.Run(ctx, req)
				if err != nil {
					t.Fatalf("%s: %v", name, err)
				}
				return res
			}
		}
		t.Fatalf("no action %s", name)
		return core.ActionResult{}
	}

	s := seed(0)
	run("delete-key", core.ActionRequest{Target: "69d_:PAGE1", Params: map[string]string{"db": "0"}})
	if n := s.Exists(ctx, "69d_:PAGE1", "cache:id_tags:69d_PAGE1").Val(); n != 0 {
		t.Errorf("entry or reverse index left: %d", n)
	}
	if s.SIsMember(ctx, "cache:tags:69d_FPC", "PAGE1").Val() || s.SIsMember(ctx, "cache:tags:69d_CMS_P_2", "PAGE1").Val() || s.SIsMember(ctx, symfonyAllIDs, "PAGE1").Val() {
		t.Error("PAGE1 still indexed")
	}
	if !s.SIsMember(ctx, symfonyAllIDs, "CFG").Val() {
		t.Error("CFG lost its index entry")
	}

	run("clean-tag", core.ActionRequest{Target: "CONFIG", Params: map[string]string{"db": "0"}})
	if s.Exists(ctx, "69d_:CFG", "cache:tags:69d_CONFIG", "cache:id_tags:69d_CFG").Val() != 0 || s.SIsMember(ctx, "cache:tags:69d_CAT_P_VIEW_ID_1", "CFG").Val() {
		t.Error("symfony tag clean left data")
	}

	c := seed(1)
	res := run("clean-tag", core.ActionRequest{Target: "69d_FPC", Params: map[string]string{"db": "1"}})
	if !strings.Contains(res.Message, "1 entry removed") {
		t.Errorf("message %q", res.Message)
	}
	if c.Exists(ctx, "zc:k:69d_PAGE", "zc:ti:69d_FPC").Val() != 0 {
		t.Error("cm entry or tag set left")
	}
	if c.SIsMember(ctx, "zc:ti:69d_CAT_C_41", "69d_PAGE").Val() || c.SIsMember(ctx, cmTags, "69d_FPC").Val() || c.SIsMember(ctx, cmIDs, "69d_PAGE").Val() {
		t.Error("cm indexes still list the cleaned entry")
	}
	if !c.SIsMember(ctx, "zc:ti:69d_CAT_C_41", "69d_OTHER").Val() || c.Exists(ctx, "zc:k:69d_OTHER").Val() != 1 {
		t.Error("unrelated cm entry touched")
	}

	run("flushdb", core.ActionRequest{Target: "2"})
	if seed(2).DBSize(ctx).Val() != 0 {
		t.Error("db 2 not flushed")
	}
	if seed(1).DBSize(ctx).Val() == 0 {
		t.Error("flushdb hit another database")
	}
}

func TestLiveMonitorDropsOwnTraffic(t *testing.T) {
	a, seed := liveAdapter(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	events := make(chan core.Event, 100)
	go a.monitor(ctx, core.Filter{}, func(ev core.Event) { events <- ev })
	select {
	case ev := <-events:
		if ev.Type != "redis.monitor" {
			t.Fatalf("first event %+v", ev)
		}
	case <-ctx.Done():
		t.Fatal("no banner")
	}
	seed(0).Set(ctx, "own:write", "x", 0)
	a.client(0).Get(ctx, "own:read")
	select {
	case ev := <-events:
		t.Errorf("own traffic leaked: %+v", ev)
	case <-time.After(300 * time.Millisecond):
	}
}
