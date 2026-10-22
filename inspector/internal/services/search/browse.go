package search

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"

	"github.com/dockergiant/images/inspector/internal/core"
	"github.com/dockergiant/images/inspector/internal/services"
)

const (
	maxHits     = 20
	maxRequest  = 1 << 20
	maxAnalyze  = 10000
	queryBudget = "5s"
)

var errUnknownIndex = errors.New("unknown index or alias")

func (a *adapter) Browse() []core.Route {
	return []core.Route{
		{Method: http.MethodGet, Path: "/metrics", Handler: a.history.ServeHTTP, Params: []core.Param{core.WindowParam}},
		{Method: http.MethodGet, Path: "/indices", Handler: a.handleIndices},
		{Method: http.MethodGet, Path: "/index/{name}/mapping", Handler: a.handleMapping, Params: []core.Param{
			{Name: "field", Type: "string", Description: "Only fields whose name contains this."},
		}},
		{Method: http.MethodGet, Path: "/index/{name}/analyzers", Handler: a.handleAnalyzers},
		{Method: http.MethodPost, Path: "/analyze", Handler: a.handleAnalyze},
		{Method: http.MethodPost, Path: "/search", Handler: a.handleSearch},
	}
}

func respond(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func fail(w http.ResponseWriter, err error) {
	status := http.StatusBadGateway
	var se *statusError
	switch {
	case errors.Is(err, errNotRunning):
		status = http.StatusServiceUnavailable
	case errors.Is(err, errUnknownIndex):
		status = http.StatusNotFound
	case errors.As(err, &se) && se.Status >= 400 && se.Status < 500:
		status = se.Status
	}
	respond(w, status, services.ErrorBody(status, err.Error()))
}

func badRequest(w http.ResponseWriter, msg string) {
	respond(w, http.StatusBadRequest, services.ErrorBody(http.StatusBadRequest, msg))
}

func (a *adapter) knownIndex(ctx context.Context, c *client, name string) (bool, error) {
	if name == "" {
		return false, nil
	}
	ix, al, err := c.catalog(ctx)
	if err != nil {
		return false, err
	}
	for _, x := range ix {
		if x.Index == name {
			return true, nil
		}
	}
	for _, x := range al {
		if x.Alias == name {
			return true, nil
		}
	}
	return false, nil
}

func (a *adapter) indexClient(ctx context.Context, name string) (*client, error) {
	c, _, err := a.client(ctx)
	if err != nil {
		return nil, err
	}
	ok, err := a.knownIndex(ctx, c, name)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, fmt.Errorf("%w: %q", errUnknownIndex, name)
	}
	return c, nil
}

func (a *adapter) handleIndices(w http.ResponseWriter, r *http.Request) {
	c, _, err := a.client(r.Context())
	if err != nil {
		fail(w, err)
		return
	}
	ix, al, err := c.catalog(r.Context())
	if err != nil {
		fail(w, err)
		return
	}
	st := a.stores(r.Context())
	resp := map[string]any{
		"indices":     BuildIndexViews(ix, al, st.Stores, st.Magento, a.now()),
		"aliases":     al,
		"magento":     st.Magento,
		"storesKnown": len(st.Stores) > 0,
	}
	if st.Err != nil {
		resp["storesError"] = st.Err.Error()
	}
	respond(w, http.StatusOK, resp)
}

type mappingField struct {
	Index          string   `json:"index"`
	Field          string   `json:"field"`
	Type           string   `json:"type"`
	Analyzer       string   `json:"analyzer,omitempty"`
	SearchAnalyzer string   `json:"searchAnalyzer,omitempty"`
	CopyTo         []string `json:"copyTo,omitempty"`
}

func flattenMapping(index, prefix string, props map[string]any, out *[]mappingField) {
	names := make([]string, 0, len(props))
	for k := range props {
		names = append(names, k)
	}
	sort.Strings(names)
	for _, name := range names {
		p, _ := props[name].(map[string]any)
		field := prefix + name
		if sub, ok := p["properties"].(map[string]any); ok {
			flattenMapping(index, field+".", sub, out)
			continue
		}
		f := mappingField{Index: index, Field: field, Type: catString(p["type"]), Analyzer: catString(p["analyzer"]), SearchAnalyzer: catString(p["search_analyzer"])}
		switch ct := p["copy_to"].(type) {
		case string:
			f.CopyTo = []string{ct}
		case []any:
			for _, v := range ct {
				f.CopyTo = append(f.CopyTo, catString(v))
			}
		}
		*out = append(*out, f)
		if multi, ok := p["fields"].(map[string]any); ok {
			flattenMapping(index, field+".", multi, out)
		}
	}
}

func (a *adapter) handleMapping(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	c, err := a.indexClient(r.Context(), name)
	if err != nil {
		fail(w, err)
		return
	}
	var resp map[string]struct {
		Mappings struct {
			Properties map[string]any `json:"properties"`
		} `json:"mappings"`
	}
	if err := c.getJSON(r.Context(), indexPath(name)+"/_mapping", &resp); err != nil {
		fail(w, err)
		return
	}
	filter := strings.ToLower(r.URL.Query().Get("field"))
	fields := []mappingField{}
	concrete := make([]string, 0, len(resp))
	for k := range resp {
		concrete = append(concrete, k)
	}
	sort.Strings(concrete)
	for _, k := range concrete {
		var all []mappingField
		flattenMapping(k, "", resp[k].Mappings.Properties, &all)
		for _, f := range all {
			if filter == "" || strings.Contains(strings.ToLower(f.Field), filter) {
				fields = append(fields, f)
			}
		}
	}
	respond(w, http.StatusOK, map[string]any{"index": name, "fields": fields})
}

func (a *adapter) handleAnalyzers(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	c, err := a.indexClient(r.Context(), name)
	if err != nil {
		fail(w, err)
		return
	}
	var resp map[string]struct {
		Settings struct {
			Index struct {
				Analysis json.RawMessage `json:"analysis"`
			} `json:"index"`
		} `json:"settings"`
	}
	if err := c.getJSON(r.Context(), indexPath(name)+"/_settings/index.analysis*", &resp); err != nil {
		fail(w, err)
		return
	}
	analysis := map[string]json.RawMessage{}
	for k, v := range resp {
		if len(v.Settings.Index.Analysis) > 0 {
			analysis[k] = v.Settings.Index.Analysis
		}
	}
	respond(w, http.StatusOK, map[string]any{"index": name, "analysis": analysis})
}

func decodeBody(w http.ResponseWriter, r *http.Request, v any) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxRequest))
	if err := dec.Decode(v); err != nil {
		badRequest(w, "invalid JSON body: "+err.Error())
		return false
	}
	return true
}

func (a *adapter) handleAnalyze(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Index    string `json:"index"`
		Analyzer string `json:"analyzer"`
		Field    string `json:"field"`
		Text     string `json:"text"`
	}
	if !decodeBody(w, r, &req) {
		return
	}
	if req.Text == "" || len(req.Text) > maxAnalyze {
		badRequest(w, fmt.Sprintf("text must hold 1 to %d bytes", maxAnalyze))
		return
	}
	if req.Field != "" && req.Index == "" {
		badRequest(w, "field needs an index")
		return
	}
	var c *client
	var err error
	path := "/_analyze"
	if req.Index != "" {
		c, err = a.indexClient(r.Context(), req.Index)
		path = indexPath(req.Index) + "/_analyze"
	} else {
		c, _, err = a.client(r.Context())
	}
	if err != nil {
		fail(w, err)
		return
	}
	body := map[string]any{"text": req.Text, "explain": true}
	if req.Analyzer != "" {
		body["analyzer"] = req.Analyzer
	}
	if req.Field != "" {
		body["field"] = req.Field
	}
	data, _, err := c.do(r.Context(), http.MethodPost, path, body)
	if err != nil {
		fail(w, err)
		return
	}
	respond(w, http.StatusOK, map[string]any{"request": body, "response": json.RawMessage(data)})
}

func termQuery(term string, magento bool) map[string]any {
	if !magento {
		return map[string]any{"query_string": map[string]any{"query": term}}
	}
	return map[string]any{"bool": map[string]any{
		"should": []any{
			map[string]any{"match": map[string]any{"_search": map[string]any{"query": term}}},
			map[string]any{"match": map[string]any{"name": map[string]any{"query": term, "boost": 5}}},
			map[string]any{"match": map[string]any{"sku": map[string]any{"query": term, "boost": 6}}},
		},
		"minimum_should_match": 1,
	}}
}

func buildSearch(body json.RawMessage, term string, size int, profile, explain, magento bool) (map[string]any, error) {
	q := map[string]any{}
	switch {
	case len(body) > 0 && string(body) != "null":
		var s string
		if json.Unmarshal(body, &s) == nil {
			body = json.RawMessage(s)
		}
		if err := json.Unmarshal(body, &q); err != nil || q == nil {
			return nil, errors.New("body must be a JSON object")
		}
	case strings.TrimSpace(term) != "":
		q["query"] = termQuery(strings.TrimSpace(term), magento)
	default:
		return nil, errors.New("give a query body to replay or a search term")
	}
	n := maxHits
	if size > 0 && size < n {
		n = size
	}
	if cur, ok := q["size"].(float64); ok && cur >= 0 && int(cur) < n {
		n = int(cur)
	}
	q["size"] = n
	q["timeout"] = queryBudget
	q["profile"] = profile
	q["explain"] = explain
	return q, nil
}

func (a *adapter) handleSearch(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Index   string          `json:"index"`
		Body    json.RawMessage `json:"body"`
		Term    string          `json:"term"`
		Size    int             `json:"size"`
		Profile bool            `json:"profile"`
		Explain bool            `json:"explain"`
	}
	if !decodeBody(w, r, &req) {
		return
	}
	if req.Index == "" {
		badRequest(w, "index is required")
		return
	}
	_, magento := MagentoName(req.Index, nil)
	body, err := buildSearch(req.Body, req.Term, req.Size, req.Profile, req.Explain, magento)
	if err != nil {
		badRequest(w, err.Error())
		return
	}
	c, err := a.indexClient(r.Context(), req.Index)
	if err != nil {
		fail(w, err)
		return
	}
	data, _, err := c.do(r.Context(), http.MethodPost, indexPath(req.Index)+"/_search?request_cache=false", body)
	if err != nil {
		fail(w, err)
		return
	}
	respond(w, http.StatusOK, map[string]any{"request": body, "response": json.RawMessage(data)})
}
