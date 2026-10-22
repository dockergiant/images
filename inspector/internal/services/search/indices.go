package search

import (
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

const orphanMinAge = 10 * time.Minute

type CatIndex struct {
	Health    string    `json:"health"`
	Status    string    `json:"status"`
	Index     string    `json:"index"`
	UUID      string    `json:"uuid"`
	Primaries int       `json:"pri"`
	Replicas  int       `json:"rep"`
	Docs      int64     `json:"docs"`
	Deleted   int64     `json:"deleted"`
	Bytes     int64     `json:"bytes"`
	Created   time.Time `json:"created"`
}

type CatAlias struct {
	Alias   string `json:"alias"`
	Index   string `json:"index"`
	IsWrite bool   `json:"isWrite,omitempty"`
}

type Label struct {
	Title        string `json:"title"`
	Entity       string `json:"entity"`
	Prefix       string `json:"prefix"`
	StoreID      int    `json:"storeId,omitempty"`
	StoreCode    string `json:"storeCode,omitempty"`
	Version      string `json:"version,omitempty"`
	Elasticsuite bool   `json:"elasticsuite,omitempty"`
	StoreFound   bool   `json:"storeFound"`
}

type IndexView struct {
	CatIndex
	Label   *Label   `json:"label,omitempty"`
	Aliases []string `json:"aliases"`
	Orphan  bool     `json:"orphan"`
	System  bool     `json:"system"`
}

var (
	coreName  = regexp.MustCompile(`^(.+)_product_(\d+)(?:_v(\d+))?$`)
	suiteName = regexp.MustCompile(`^(.+)_(catalog_product|catalog_category|thesaurus|cms_page)(?:_(\d{8}_\d{6}|\d{8}))?$`)
)

var entityTitles = map[string]string{
	"product":   "Products",
	"category":  "Categories",
	"thesaurus": "Thesaurus",
	"cms_page":  "CMS pages",
}

func catString(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case json.Number:
		return x.String()
	case float64:
		return strconv.FormatFloat(x, 'f', -1, 64)
	}
	return ""
}

func catInt(v any) int64 {
	n, _ := strconv.ParseInt(catString(v), 10, 64)
	return n
}

func ParseCatIndices(b []byte) ([]CatIndex, error) {
	var rows []map[string]any
	if err := json.Unmarshal(b, &rows); err != nil {
		return nil, fmt.Errorf("parse _cat/indices: %w", err)
	}
	out := make([]CatIndex, 0, len(rows))
	for _, r := range rows {
		ix := CatIndex{
			Health:    catString(r["health"]),
			Status:    catString(r["status"]),
			Index:     catString(r["index"]),
			UUID:      catString(r["uuid"]),
			Primaries: int(catInt(r["pri"])),
			Replicas:  int(catInt(r["rep"])),
			Docs:      catInt(r["docs.count"]),
			Deleted:   catInt(r["docs.deleted"]),
			Bytes:     catInt(r["store.size"]),
		}
		if ms := catInt(r["creation.date"]); ms > 0 {
			ix.Created = time.UnixMilli(ms)
		}
		if ix.Index != "" {
			out = append(out, ix)
		}
	}
	return out, nil
}

func ParseCatAliases(b []byte) ([]CatAlias, error) {
	var rows []map[string]any
	if err := json.Unmarshal(b, &rows); err != nil {
		return nil, fmt.Errorf("parse _cat/aliases: %w", err)
	}
	out := make([]CatAlias, 0, len(rows))
	for _, r := range rows {
		a := CatAlias{Alias: catString(r["alias"]), Index: catString(r["index"]), IsWrite: catString(r["is_write_index"]) == "true"}
		if a.Alias != "" && a.Index != "" {
			out = append(out, a)
		}
	}
	return out, nil
}

func isSystemIndex(name string) bool {
	return strings.HasPrefix(name, ".") || strings.HasPrefix(name, "top_queries-") || strings.HasPrefix(name, "security-auditlog-")
}

func MagentoName(index string, stores map[int]string) (Label, bool) {
	if isSystemIndex(index) {
		return Label{}, false
	}
	if m := suiteName.FindStringSubmatch(index); m != nil {
		if l, ok := suiteLabel(m, stores); ok {
			return l, true
		}
	}
	m := coreName.FindStringSubmatch(index)
	if m == nil {
		return Label{}, false
	}
	id, err := strconv.Atoi(m[2])
	if err != nil {
		return Label{}, false
	}
	l := Label{Entity: "product", Prefix: m[1], StoreID: id}
	if m[3] != "" {
		l.Version = "v" + m[3]
	}
	if code, ok := stores[id]; ok {
		l.StoreCode, l.StoreFound = code, true
	}
	l.Title = title(l)
	return l, true
}

func suiteLabel(m []string, stores map[int]string) (Label, bool) {
	head := m[1]
	if !strings.Contains(head, "_") {
		return Label{}, false
	}
	byCode := make(map[string]int, len(stores))
	for id, code := range stores {
		byCode[code] = id
	}
	l := Label{Entity: strings.TrimPrefix(m[2], "catalog_"), Version: m[3], Elasticsuite: true}
	for i := 0; i < len(head); i++ {
		if head[i] != '_' || i == 0 || i == len(head)-1 {
			continue
		}
		if id, ok := byCode[head[i+1:]]; ok {
			l.Prefix, l.StoreCode, l.StoreID, l.StoreFound = head[:i], head[i+1:], id, true
			break
		}
	}
	if !l.StoreFound {
		l.Prefix, l.StoreCode, _ = strings.Cut(head, "_")
	}
	l.Title = title(l)
	return l, true
}

func title(l Label) string {
	name := entityTitles[l.Entity]
	if l.StoreCode != "" {
		return name + " · " + l.StoreCode
	}
	return fmt.Sprintf("%s · store %d", name, l.StoreID)
}

func BuildIndexViews(ix []CatIndex, al []CatAlias, stores map[int]string, magento bool, now time.Time) []IndexView {
	aliases := map[string][]string{}
	for _, a := range al {
		aliases[a.Index] = append(aliases[a.Index], a.Alias)
	}
	out := make([]IndexView, 0, len(ix))
	for _, x := range ix {
		v := IndexView{CatIndex: x, Aliases: aliases[x.Index], System: isSystemIndex(x.Index)}
		if v.Aliases == nil {
			v.Aliases = []string{}
		}
		sort.Strings(v.Aliases)
		if magento {
			if l, ok := MagentoName(x.Index, stores); ok {
				v.Label = &l
				v.Orphan = len(v.Aliases) == 0 && len(stores) > 0 && !l.StoreFound &&
					!x.Created.IsZero() && now.Sub(x.Created) >= orphanMinAge
			}
		}
		out = append(out, v)
	}
	rank := func(v IndexView) int {
		switch {
		case v.Label != nil:
			return 0
		case !v.System:
			return 1
		}
		return 2
	}
	sort.SliceStable(out, func(i, j int) bool {
		ri, rj := rank(out[i]), rank(out[j])
		if ri != rj {
			return ri < rj
		}
		if ri == 0 && out[i].Label.Title != out[j].Label.Title {
			return out[i].Label.Title < out[j].Label.Title
		}
		return out[i].Index < out[j].Index
	})
	return out
}

func captureTargets(ix []CatIndex, al []CatAlias) []string {
	set := map[string]bool{}
	for _, a := range al {
		if !isSystemIndex(a.Alias) && !isSystemIndex(a.Index) {
			set[a.Index] = true
		}
	}
	if len(set) == 0 {
		for _, x := range ix {
			if !isSystemIndex(x.Index) && x.Status == "open" {
				set[x.Index] = true
			}
		}
	}
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
