package workbuddy

import (
	"embed"
	"encoding/json"
	"sync"

	wbp "daydayup/internal/provider"
)

// The bundled curated model snapshots align with the official desktop picker:
// virtual aliases, variant builds and paid siblings are already removed, and
// per-model capability metadata rides along. See catalog_intl.json /
// catalog_cn.json (17 international, 14 CN models).

//go:embed catalog_intl.json catalog_cn.json
var catalogFS embed.FS

var (
	once        sync.Once
	catalogIntl []wbp.Model
	catalogCN   []wbp.Model
	allModels   []wbp.Model
)

func loadCatalogs() {
	once.Do(func() {
		catalogIntl = parseCatalog("catalog_intl.json")
		catalogCN = parseCatalog("catalog_cn.json")
		allModels = append(append([]wbp.Model{}, catalogIntl...), catalogCN...)
	})
}

func parseCatalog(name string) []wbp.Model {
	b, err := catalogFS.ReadFile(name)
	if err != nil {
		panic("workbuddy: embedded catalog missing: " + err.Error())
	}
	var out []wbp.Model
	if err := json.Unmarshal(b, &out); err != nil {
		panic("workbuddy: embedded catalog corrupt: " + err.Error())
	}
	return out
}

// bundledCatalog returns the curated snapshot for a realm (deep copy so
// callers may mutate without corrupting the cache).
func bundledCatalog(cn bool) []wbp.Model {
	loadCatalogs()
	src := catalogIntl
	if cn {
		src = catalogCN
	}
	out := make([]wbp.Model, len(src))
	for i := range src {
		c := wbp.Model{}
		for k, v := range src[i] {
			c[k] = v
		}
		c["source"] = "catalog"
		out[i] = c
	}
	return out
}

// bundledAll returns every bundled model from both realms.
func bundledAll() []wbp.Model {
	loadCatalogs()
	return allModels
}
