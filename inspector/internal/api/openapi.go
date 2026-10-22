package api

import (
	"net/http"
	"regexp"
	"strings"
)

var nonWord = regexp.MustCompile(`[^A-Za-z0-9]+`)

// operationID turns /traces/{id}/analysis into getTracesIdAnalysis.
func operationID(p string) string {
	var b strings.Builder
	b.WriteString("get")
	for _, part := range nonWord.Split(p, -1) {
		if part == "" {
			continue
		}
		b.WriteString(strings.ToUpper(part[:1]) + part[1:])
	}
	if b.Len() == 3 {
		b.WriteString("Index")
	}
	return b.String()
}

func (s *Server) openAPI(table []v1Route) map[string]any {
	errorBody := map[string]any{
		"description": "An error, with a hint on what to do about it",
		"content": map[string]any{"application/json": map[string]any{"schema": map[string]any{
			"type":       "object",
			"properties": map[string]any{"error": map[string]any{"type": "string"}, "hint": map[string]any{"type": "string"}},
		}}},
	}
	paths := map[string]any{}
	tags := []map[string]string{}
	seen := map[string]bool{}
	for _, rt := range table {
		if !seen[rt.Tag] {
			seen[rt.Tag] = true
			tags = append(tags, map[string]string{"name": rt.Tag})
		}
		params := []map[string]any{}
		for _, p := range rt.Params {
			params = append(params, map[string]any{
				"name":        p.Name,
				"in":          p.In,
				"required":    p.Required,
				"description": p.Description,
				"schema":      map[string]string{"type": p.Type},
			})
		}
		media := "application/json"
		if rt.Stream {
			media = "text/event-stream"
		}
		paths[v1Prefix+rt.Path] = map[string]any{
			"get": map[string]any{
				"operationId": operationID(rt.Path),
				"summary":     rt.Summary,
				"tags":        []string{rt.Tag},
				"parameters":  params,
				"responses": map[string]any{
					"200": map[string]any{
						"description": rt.Returns,
						"content":     map[string]any{media: map[string]any{}},
					},
					"default": errorBody,
				},
			},
		}
	}
	return map[string]any{
		"openapi": "3.1.0",
		"info": map[string]any{
			"title":       "Roll Inspector read API",
			"version":     "1",
			"description": "Read-only API of Roll Inspector for " + s.cfg.Env + ". Every origin may read it: Access-Control-Allow-Origin is *, and no cookie, CSRF token or Origin is checked, so any website open in this browser can read the shop's traces and cache through it. Writes stay on /api behind the Inspector page. Inspector " + s.cfg.Version + ".",
		},
		"servers": []map[string]string{{"url": s.inspectorURL()}},
		"tags":    tags,
		"paths":   paths,
	}
}

func (s *Server) handleOpenAPI(table []v1Route) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, s.openAPI(table))
	}
}
