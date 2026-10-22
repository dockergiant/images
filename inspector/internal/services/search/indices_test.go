package search

import (
	"testing"
	"time"
)

func TestParseCatIndices(t *testing.T) {
	for _, name := range []string{"cat-indices-2.0.json", "cat-indices-2.6.json", "cat-indices-3.5.json"} {
		ix, err := ParseCatIndices(fixture(t, name))
		if err != nil || len(ix) == 0 {
			t.Fatalf("%s: %v %d", name, err, len(ix))
		}
	}
	ix, _ := ParseCatIndices(fixture(t, "cat-indices-3.5.json"))
	var got *CatIndex
	for i := range ix {
		if ix[i].Index == "magento2_product_1_v78" {
			got = &ix[i]
		}
	}
	if got == nil {
		t.Fatal("magento2_product_1_v78 missing")
	}
	want := CatIndex{Health: "yellow", Status: "open", Index: "magento2_product_1_v78", UUID: "h1epH1RmS7S_c_022hDN3A",
		Primaries: 1, Replicas: 1, Docs: 373, Deleted: 48, Bytes: 529919, Created: time.UnixMilli(1792525111375)}
	if *got != want {
		t.Errorf("got %+v", *got)
	}
}

func TestParseCatIndicesClosed(t *testing.T) {
	ix, err := ParseCatIndices([]byte(`[{"health":null,"status":"close","index":"old","uuid":"u","pri":"1","rep":"0","docs.count":null,"docs.deleted":null,"store.size":null,"creation.date":"1792525111375"}]`))
	if err != nil || len(ix) != 1 || ix[0].Docs != 0 || ix[0].Status != "close" {
		t.Fatalf("got %+v %v", ix, err)
	}
}

func TestParseCatAliases(t *testing.T) {
	al, err := ParseCatAliases(fixture(t, "cat-aliases-2.6.json"))
	if err != nil || len(al) != 1 {
		t.Fatalf("%+v %v", al, err)
	}
	if al[0] != (CatAlias{Alias: "shop2_product_1", Index: "shop2_product_1_v1"}) {
		t.Errorf("got %+v", al[0])
	}
	al, err = ParseCatAliases([]byte(`[{"alias":"a","index":"b","is_write_index":"true"}]`))
	if err != nil || !al[0].IsWrite {
		t.Errorf("is_write_index: %+v %v", al, err)
	}
}

func TestMagentoName(t *testing.T) {
	ml := map[int]string{0: "admin", 1: "default", 3: "en", 18: "outlet_nl", 19: "outlet_de"}
	shop3 := map[int]string{0: "admin", 1: "default", 2: "nl", 3: "de", 4: "fr", 5: "en", 6: "b2b_de", 7: "giftshop"}
	cases := []struct {
		index  string
		stores map[int]string
		ok     bool
		want   Label
	}{
		{"magento2_product_1_v78", ml, true, Label{Title: "Products · default", Entity: "product", Prefix: "magento2", StoreID: 1, StoreCode: "default", Version: "v78", StoreFound: true}},
		{"magento2_product_18_v69", ml, true, Label{Title: "Products · outlet_nl", Entity: "product", Prefix: "magento2", StoreID: 18, StoreCode: "outlet_nl", Version: "v69", StoreFound: true}},
		{"magento2_product_1", ml, true, Label{Title: "Products · default", Entity: "product", Prefix: "magento2", StoreID: 1, StoreCode: "default", StoreFound: true}},
		{"magento2_product_988_v2", ml, true, Label{Title: "Products · store 988", Entity: "product", Prefix: "magento2", StoreID: 988, Version: "v2"}},
		{"shop2_product_1_v2", map[int]string{1: "default"}, true, Label{Title: "Products · default", Entity: "product", Prefix: "shop2", StoreID: 1, StoreCode: "default", Version: "v2", StoreFound: true}},
		{"magento2_product_1_v1", nil, true, Label{Title: "Products · store 1", Entity: "product", Prefix: "magento2", StoreID: 1, Version: "v1"}},
		{"magento2_b2b_de_catalog_product_20261020_161253", shop3, true, Label{Title: "Products · b2b_de", Entity: "product", Prefix: "magento2", StoreID: 6, StoreCode: "b2b_de", Version: "20261020_161253", Elasticsuite: true, StoreFound: true}},
		{"magento2_b2b_de_catalog_product_20261020_161253", nil, true, Label{Title: "Products · b2b_de", Entity: "product", Prefix: "magento2", StoreCode: "b2b_de", Version: "20261020_161253", Elasticsuite: true}},
		{"magento2_giftshop_thesaurus_20261020_161531", shop3, true, Label{Title: "Thesaurus · giftshop", Entity: "thesaurus", Prefix: "magento2", StoreID: 7, StoreCode: "giftshop", Version: "20261020_161531", Elasticsuite: true, StoreFound: true}},
		{"magento2_nl_catalog_category_20261020_161526", shop3, true, Label{Title: "Categories · nl", Entity: "category", Prefix: "magento2", StoreID: 2, StoreCode: "nl", Version: "20261020_161526", Elasticsuite: true, StoreFound: true}},
		{"magento2_default_catalog_product", shop3, true, Label{Title: "Products · default", Entity: "product", Prefix: "magento2", StoreID: 1, StoreCode: "default", Elasticsuite: true, StoreFound: true}},
		{"magento2_default_catalog_product_20261020", shop3, true, Label{Title: "Products · default", Entity: "product", Prefix: "magento2", StoreID: 1, StoreCode: "default", Version: "20261020", Elasticsuite: true, StoreFound: true}},
		{"shop_a_b_catalog_product_20261020_161253", map[int]string{1: "b"}, true, Label{Title: "Products · b", Entity: "product", Prefix: "shop_a", StoreID: 1, StoreCode: "b", Version: "20261020_161253", Elasticsuite: true, StoreFound: true}},
		{".opensearch-observability", ml, false, Label{}},
		{".plugins-ml-config", ml, false, Label{}},
		{".roll-inspector", ml, false, Label{}},
		{"top_queries-2026.10.19-59499", ml, false, Label{}},
		{"products", ml, false, Label{}},
		{"catalog_product_20261020_161253", ml, false, Label{}},
	}
	for _, c := range cases {
		got, ok := MagentoName(c.index, c.stores)
		if ok != c.ok || got != c.want {
			t.Errorf("%s: got %+v %v", c.index, got, ok)
		}
	}
}

func TestOrphans(t *testing.T) {
	ix, _ := ParseCatIndices(fixture(t, "cat-indices-3.5.json"))
	al, _ := ParseCatAliases(fixture(t, "cat-aliases-3.5.json"))
	now := time.UnixMilli(1792527280614).Add(11 * time.Minute)
	stores := fixtureStores(t, "stores-3.5.json")

	if got := orphanNames(BuildIndexViews(ix, al, stores, true, now)); len(got) != 0 {
		t.Errorf("all stores present: got orphans %v", got)
	}

	delete(stores, 987)
	delete(stores, 988)
	got := orphanNames(BuildIndexViews(ix, al, stores, true, now))
	if len(got) != 2 || got[0] != "magento2_product_987_v2" || got[1] != "magento2_product_988_v2" {
		t.Errorf("stores 987 and 988 gone: got %v", got)
	}

	young := time.UnixMilli(1792527224430).Add(630 * time.Second)
	if got := orphanNames(BuildIndexViews(ix, al, stores, true, young)); len(got) != 1 || got[0] != "magento2_product_987_v2" {
		t.Errorf("younger than 10 minutes: got %v", got)
	}

	if got := orphanNames(BuildIndexViews(ix, al, nil, true, now)); len(got) != 0 {
		t.Errorf("unknown stores: got %v", got)
	}
	if got := orphanNames(BuildIndexViews(ix, al, stores, false, now)); len(got) != 0 {
		t.Errorf("generic mode: got %v", got)
	}
}

func TestOrphanAliasedIndex(t *testing.T) {
	ix, _ := ParseCatIndices(fixture(t, "cat-indices-2.6.json"))
	al, _ := ParseCatAliases(fixture(t, "cat-aliases-2.6.json"))
	views := BuildIndexViews(ix, al, fixtureStores(t, "stores-2.6.json"), true, time.UnixMilli(1792527531860).Add(time.Hour))
	if got := orphanNames(views); len(got) != 0 {
		t.Errorf("unaliased index of an existing store is no orphan: %v", got)
	}
	if views[0].Index != "shop2_product_1_v1" || views[0].Label == nil || len(views[0].Aliases) != 1 {
		t.Errorf("Magento indices sort first: %+v", views[0])
	}
	last := views[len(views)-1]
	if last.Index != ".opensearch-observability" || !last.System || last.Label != nil {
		t.Errorf("system index last: %+v", last)
	}
}

func TestOrphanElasticsuite(t *testing.T) {
	ix, _ := ParseCatIndices(fixture(t, "cat-indices-2.0.json"))
	al, _ := ParseCatAliases(fixture(t, "cat-aliases-2.0.json"))
	stores := map[int]string{1: "default", 2: "nl", 3: "de", 4: "fr", 5: "en", 6: "b2b_de", 7: "giftshop"}
	now := time.UnixMilli(1792512931765).Add(time.Hour)
	if got := orphanNames(BuildIndexViews(ix, al, stores, true, now)); len(got) != 0 {
		t.Errorf("shop3: got %v", got)
	}
	ix = append(ix, CatIndex{Index: "magento2_be_catalog_product_20261001_101010", Status: "open", Created: now.Add(-48 * time.Hour)})
	if got := orphanNames(BuildIndexViews(ix, al, stores, true, now)); len(got) != 1 || got[0] != "magento2_be_catalog_product_20261001_101010" {
		t.Errorf("deleted store be: got %v", got)
	}
}

func TestCaptureTargets(t *testing.T) {
	ix, _ := ParseCatIndices(fixture(t, "cat-indices-3.5.json"))
	al, _ := ParseCatAliases(fixture(t, "cat-aliases-3.5.json"))
	got := captureTargets(ix, al)
	want := []string{"magento2_product_18_v69", "magento2_product_19_v69", "magento2_product_1_v78", "magento2_product_3_v73", "magento2_product_987_v1", "magento2_product_988_v1"}
	if len(got) != len(want) {
		t.Fatalf("got %v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("got %v", got)
		}
	}
	got = captureTargets([]CatIndex{{Index: "products", Status: "open"}, {Index: ".hidden", Status: "open"}, {Index: "top_queries-1", Status: "open"}, {Index: "closed", Status: "close"}}, nil)
	if len(got) != 1 || got[0] != "products" {
		t.Errorf("without aliases: got %v", got)
	}
}

func orphanNames(v []IndexView) []string {
	var out []string
	for _, x := range v {
		if x.Orphan {
			out = append(out, x.Index)
		}
	}
	return out
}
