package search

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
)

const (
	EngineOpenSearch    = "opensearch"
	EngineElasticsearch = "elasticsearch"
)

var testedOpenSearch = []string{"2.0", "2.6", "3.5"}

type Flavor struct {
	Engine  string `json:"engine"`
	Version string `json:"version"`
}

func (f Flavor) Tested() bool {
	if f.Engine != EngineOpenSearch {
		return false
	}
	for _, v := range testedOpenSearch {
		if f.Version == v || strings.HasPrefix(f.Version, v+".") {
			return true
		}
	}
	return false
}

func (f Flavor) Name() string {
	if f.Engine == EngineElasticsearch {
		return "Elasticsearch"
	}
	return "OpenSearch"
}

func DetectFlavor(body []byte, h http.Header) (Flavor, error) {
	var root struct {
		Version struct {
			Distribution string `json:"distribution"`
			Number       string `json:"number"`
		} `json:"version"`
		Tagline string `json:"tagline"`
	}
	if err := json.Unmarshal(body, &root); err != nil {
		return Flavor{}, fmt.Errorf("unexpected answer to GET /: %w", err)
	}
	if root.Version.Number == "" {
		return Flavor{}, errors.New("GET / carries no version number")
	}
	f := Flavor{Version: root.Version.Number}
	switch {
	case strings.EqualFold(root.Version.Distribution, "opensearch"):
		f.Engine = EngineOpenSearch
	case h.Get("X-Elastic-Product") == "Elasticsearch", strings.Contains(root.Tagline, "You Know, for Search"):
		f.Engine = EngineElasticsearch
	case strings.HasPrefix(h.Get("X-OpenSearch-Version"), "OpenSearch/"), strings.Contains(root.Tagline, "OpenSearch"):
		f.Engine = EngineOpenSearch
	default:
		return Flavor{}, fmt.Errorf("unknown search engine %s", root.Version.Number)
	}
	return f, nil
}

func HasInsights(plugins []byte) (bool, error) {
	var rows []struct {
		Component string `json:"component"`
	}
	if err := json.Unmarshal(plugins, &rows); err != nil {
		return false, err
	}
	for _, r := range rows {
		if r.Component == "query-insights" {
			return true, nil
		}
	}
	return false, nil
}
