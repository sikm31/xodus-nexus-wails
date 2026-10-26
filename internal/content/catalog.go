// Package content ports content-catalog.ts and game-content.ts: discovery,
// installation and validation of add-on content for installed games.
package content

import (
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"xodus-nexus-wails/internal/catalog"
	"xodus-nexus-wails/internal/shared"
)

// Product is untyped public catalog metadata.
type Product = map[string]any

var validIDPattern = regexp.MustCompile(`^[A-Z0-9]{12}$`)

func validID(value any) bool {
	text, ok := value.(string)
	return ok && validIDPattern.MatchString(text)
}

func list(value any) []any {
	items, _ := value.([]any)
	return items
}

func object(value any) map[string]any {
	record, _ := value.(map[string]any)
	return record
}

// ContentSkus returns non-trial, released SKU properties.
func ContentSkus(product Product) []map[string]any {
	var out []map[string]any
	for _, row := range list(product["DisplaySkuAvailabilities"]) {
		props := object(object(object(row)["Sku"])["Properties"])
		if props != nil && props["IsTrial"] == false && props["IsPreOrder"] != true {
			out = append(out, props)
		}
	}
	return out
}

// DesktopContentPackages returns x64 Windows.Desktop MSIXVC packages.
func DesktopContentPackages(product Product) []map[string]any {
	var out []map[string]any
	for _, props := range ContentSkus(product) {
		for _, entry := range list(props["Packages"]) {
			pkg := object(entry)
			if pkg == nil || pkg["PackageFormat"] != "MSIXVC" || !slices.Contains(list(pkg["Architectures"]), any("x64")) {
				continue
			}
			if slices.ContainsFunc(list(pkg["PlatformDependencies"]), func(dep any) bool { return object(dep)["PlatformName"] == "Windows.Desktop" }) {
				out = append(out, pkg)
			}
		}
	}
	return out
}

func relationships(product Product, kind string) []string {
	var out []string
	for _, market := range list(product["MarketProperties"]) {
		for _, entry := range list(object(market)["RelatedProducts"]) {
			row := object(entry)
			if row["RelationshipType"] == kind && validID(row["RelatedProductId"]) {
				out = append(out, row["RelatedProductId"].(string))
			}
		}
	}
	return out
}

func bundleChildren(product Product) []string {
	var out []string
	for _, props := range ContentSkus(product) {
		for _, entry := range list(props["BundledSkus"]) {
			if row := object(entry); validID(row["BigId"]) {
				out = append(out, row["BigId"].(string))
			}
		}
	}
	return out
}

func pick(record map[string]any, keys ...string) map[string]any {
	out := map[string]any{}
	for _, key := range keys {
		if value, ok := record[key]; ok {
			out[key] = value
		}
	}
	return out
}

// compact keeps public metadata as discovery, never a license, and keeps large
// artwork/URLs out of the cache.
func compact(product Product) Product {
	localized := []any{}
	if rows := list(product["LocalizedProperties"]); len(rows) > 0 {
		title, _ := object(rows[0])["ProductTitle"].(string)
		localized = append(localized, map[string]any{"ProductTitle": shared.TruncateUTF16(title, 300)})
	}
	markets := []any{}
	for _, row := range list(product["MarketProperties"]) {
		relations := []any{}
		for _, relation := range list(object(row)["RelatedProducts"]) {
			relations = append(relations, pick(object(relation), "RelatedProductId", "RelationshipType"))
		}
		markets = append(markets, map[string]any{"RelatedProducts": relations})
	}
	skus := []any{}
	for _, row := range list(product["DisplaySkuAvailabilities"]) {
		props := object(object(object(row)["Sku"])["Properties"])
		_, declared := props["Packages"]
		bundled := []any{}
		for _, child := range list(props["BundledSkus"]) {
			bundled = append(bundled, pick(object(child), "BigId"))
		}
		var packages any
		if raw, present := props["Packages"]; !(present && raw == nil) {
			compacted := []any{}
			for _, pkg := range list(raw) {
				compacted = append(compacted, pick(object(pkg), "ContentId", "PackageFormat", "Architectures", "PlatformDependencies",
					"PackageFamilyName", "MainPackageFamilyNameForDlc", "MaxDownloadSizeInBytes"))
			}
			packages = compacted
		}
		properties := pick(props, "IsTrial", "IsPreOrder")
		properties["PackagesDeclared"] = declared
		properties["BundledSkus"] = bundled
		properties["Packages"] = packages
		skus = append(skus, map[string]any{"Sku": map[string]any{"Properties": properties}})
	}
	return Product{"ProductId": product["ProductId"], "ProductType": product["ProductType"], "LocalizedProperties": localized,
		"MarketProperties": markets, "DisplaySkuAvailabilities": skus}
}

type cacheEntry struct {
	product Product // nil when the catalog had no such product
	until   time.Time
	bytes   int
}

// Base identifies the installed game being scanned.
type Base struct {
	StoreID string
	Name    string
	Related []string
}

type Discovery struct {
	Products []Product
	Related  []string
	Verify   []string
}

type Catalog struct {
	fetch catalog.Fetcher
	mu    sync.Mutex
	cache map[string]cacheEntry
	order []string
	bytes int
}

func NewCatalog(fetch catalog.Fetcher) *Catalog {
	if fetch == nil {
		fetch = catalog.HTTPFetcher(nil)
	}
	return &Catalog{fetch: fetch, cache: map[string]cacheEntry{}}
}

var marketPattern = regexp.MustCompile(`^[A-Z]{2}$`)

func (c *Catalog) store(key string, entry cacheEntry) {
	if old, ok := c.cache[key]; ok {
		c.bytes -= old.bytes
		c.order = slices.DeleteFunc(c.order, func(k string) bool { return k == key })
	}
	c.cache[key] = entry
	c.order = append(c.order, key)
	c.bytes += entry.bytes
	for c.bytes > 16_000_000 || len(c.cache) > 10_256 {
		oldest := c.order[0]
		c.order = c.order[1:]
		c.bytes -= c.cache[oldest].bytes
		delete(c.cache, oldest)
	}
}

type productSet struct {
	order []string
	byID  map[string]Product
}

func (s *productSet) set(id string, product Product) {
	if _, ok := s.byID[id]; !ok {
		s.order = append(s.order, id)
	}
	s.byID[id] = product
}

func uniqueStrings(values []string) []string {
	var out []string
	seen := map[string]bool{}
	for _, value := range values {
		if !seen[value] {
			seen[value] = true
			out = append(out, value)
		}
	}
	return out
}

// Discover finds content related to base. Bundle membership only permits an
// actual license check (Verify), never an ownership grant.
func (c *Catalog) Discover(ctx context.Context, base Base, access map[string]shared.LibraryAccess, market, offerID string) (Discovery, error) {
	if !validID(base.StoreID) || len(base.Related) > 256 || slices.ContainsFunc(base.Related, func(id string) bool { return !validID(id) }) ||
		len(access) > 10_000 {
		return Discovery{}, errors.New("Invalid content discovery scope.")
	}
	owned := make([]string, 0, len(access))
	for id := range access {
		if !validID(id) {
			return Discovery{}, errors.New("Invalid content discovery scope.")
		}
		owned = append(owned, id)
	}
	slices.Sort(owned)
	market = strings.ToUpper(market)
	if !marketPattern.MatchString(market) {
		market = "US"
	}
	ctx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	if err := ctx.Err(); err != nil {
		return Discovery{}, err
	}
	products := productSet{byID: map[string]Product{}}
	fetched := map[string]bool{}
	var transferredMu sync.Mutex
	transferred := 0
	load := func(ids []string) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		var pending []string
		for _, id := range uniqueStrings(ids) {
			if !fetched[id] {
				pending = append(pending, id)
			}
		}
		c.mu.Lock()
		for _, id := range pending {
			if entry, ok := c.cache[market+":"+id]; ok && entry.until.After(time.Now()) {
				fetched[id] = true
				if entry.product != nil {
					products.set(id, entry.product)
				}
			}
		}
		c.mu.Unlock()
		var needed []string
		for _, id := range pending {
			if !fetched[id] {
				needed = append(needed, id)
			}
		}
		var batches [][]string
		for i := 0; i < len(needed); i += 20 {
			batches = append(batches, needed[i:min(len(needed), i+20)])
		}
		var (
			mu       sync.Mutex
			next     int
			failed   bool
			firstErr error
			workers  sync.WaitGroup
		)
		fail := func(err error) {
			mu.Lock()
			failed = true
			if firstErr == nil {
				firstErr = err
			}
			mu.Unlock()
		}
		for range min(4, len(batches)) {
			workers.Add(1)
			go func() {
				defer workers.Done()
				for {
					mu.Lock()
					if failed || next >= len(batches) {
						mu.Unlock()
						return
					}
					batch := batches[next]
					next++
					mu.Unlock()
					if err := c.loadBatch(ctx, batch, market, &products, fetched, &mu, &transferredMu, &transferred); err != nil {
						fail(err)
						return
					}
				}
			}()
		}
		workers.Wait()
		return firstErr
	}

	initial := append([]string{base.StoreID}, base.Related...)
	if validID(offerID) {
		initial = append(initial, offerID)
	}
	if err := load(append(initial, owned...)); err != nil {
		return Discovery{}, err
	}
	families := map[string]bool{}
	for _, pkg := range DesktopContentPackages(products.byID[base.StoreID]) {
		if family, ok := pkg["PackageFamilyName"].(string); ok && strings.HasPrefix(family, base.Name+"_") {
			families[family] = true
		}
	}
	scoped := func(product Product) bool {
		kind, _ := product["ProductType"].(string)
		id, _ := product["ProductId"].(string)
		if kind != "Durable" && kind != "GameAddOn" {
			return false
		}
		return slices.Contains(base.Related, id) || slices.Contains(relationships(product, "addOnParent"), base.StoreID) ||
			slices.ContainsFunc(DesktopContentPackages(product), func(pkg map[string]any) bool {
				family, _ := pkg["MainPackageFamilyNameForDlc"].(string)
				return families[family]
			})
	}
	var related []string
	relatedSet := map[string]bool{}
	addRelated := func(id string) {
		if !relatedSet[id] {
			relatedSet[id] = true
			related = append(related, id)
		}
	}
	for _, id := range base.Related {
		if id != base.StoreID {
			addRelated(id)
		}
	}
	frontier := initial
	for _, id := range products.order {
		product := products.byID[id]
		if scoped(product) || (access[id] != "" && slices.Contains(bundleChildren(product), base.StoreID)) {
			frontier = append(frontier, id)
		}
	}
	frontier = uniqueStrings(frontier)
	graph := map[string]bool{}
	var graphOrder []string
	for depth := 0; len(frontier) > 0; depth++ {
		if depth > 6 {
			return Discovery{}, errors.New("Content relationship graph is too deep.")
		}
		var next []string
		nextSet := map[string]bool{}
		for _, id := range frontier {
			if graph[id] {
				continue
			}
			graph[id] = true
			graphOrder = append(graphOrder, id)
			if len(graph) > 256 {
				return Discovery{}, errors.New("Content relationship graph is too large.")
			}
			product := products.byID[id]
			if product == nil {
				continue
			}
			if scoped(product) {
				addRelated(id)
			}
			children := bundleChildren(product)
			if id == base.StoreID {
				children = append(children, relationships(product, "Bundle")...)
			}
			for _, child := range children {
				if !graph[child] && !nextSet[child] {
					nextSet[child] = true
					next = append(next, child)
				}
			}
		}
		next = slices.DeleteFunc(next, func(id string) bool { return graph[id] })
		if len(graph)+len(next) > 256 {
			return Discovery{}, errors.New("Content relationship graph is too large.")
		}
		if err := load(next); err != nil {
			return Discovery{}, err
		}
		frontier = next
	}
	if len(related) > 128 {
		return Discovery{}, errors.New("Too many related content products.")
	}
	var verify []string
	verifySet := map[string]bool{}
	for _, root := range graphOrder {
		if access[root] == "" {
			continue
		}
		visited := map[string]bool{root: true}
		pending := bundleChildren(products.byID[root])
		for len(pending) > 0 {
			id := pending[len(pending)-1]
			pending = pending[:len(pending)-1]
			if visited[id] {
				continue
			}
			visited[id] = true
			if len(visited) > 256 {
				return Discovery{}, errors.New("Content relationship graph is too large.")
			}
			if relatedSet[id] && !verifySet[id] {
				verifySet[id] = true
				verify = append(verify, id)
			}
			pending = append(pending, bundleChildren(products.byID[id])...)
		}
	}
	result := Discovery{Products: []Product{}, Related: []string{}, Verify: []string{}}
	if product := products.byID[base.StoreID]; product != nil {
		result.Products = append(result.Products, product)
	}
	for _, id := range related {
		if product := products.byID[id]; product != nil {
			result.Products = append(result.Products, product)
		}
	}
	result.Related = append(result.Related, related...)
	result.Verify = append(result.Verify, verify...)
	return result, nil
}

var errUnexpected = errors.New("Unexpected content metadata.")

func (c *Catalog) loadBatch(ctx context.Context, batch []string, market string, products *productSet, fetched map[string]bool,
	mu, transferredMu *sync.Mutex, transferred *int) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	query := url.Values{"bigIds": {strings.Join(batch, ",")}, "market": {market}, "languages": {"en-us,neutral"}}
	requestCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	response, err := c.fetch(requestCtx, "https://displaycatalog.mp.microsoft.com/v7.0/products?"+query.Encode(), 8_000_000)
	if err != nil {
		return err
	}
	if !response.OK() {
		return errors.New("Microsoft content metadata could not be loaded.")
	}
	transferredMu.Lock()
	*transferred += len(response.Body)
	total := *transferred
	transferredMu.Unlock()
	if len(response.Body) > 8_000_000 || total > 64_000_000 {
		return errors.New("Content metadata response is too large.")
	}
	if !utf8.Valid(response.Body) {
		return errors.New("Content metadata is not valid UTF-8.")
	}
	var data map[string]any
	if err := json.Unmarshal(response.Body, &data); err != nil {
		return err
	}
	rows, ok := data["Products"].([]any)
	if !ok || len(rows) > len(batch) {
		return errUnexpected
	}
	seen := map[string]bool{}
	loaded := map[string]Product{}
	for _, raw := range rows {
		record := object(raw)
		id, _ := record["ProductId"].(string)
		if record == nil || !slices.Contains(batch, id) || seen[id] {
			return errUnexpected
		}
		seen[id] = true
		loaded[id] = compact(record)
	}
	mu.Lock()
	defer mu.Unlock()
	for id, product := range loaded {
		products.set(id, product)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, id := range batch {
		fetched[id] = true
		product := loaded[id]
		encoded, _ := json.Marshal(product)
		if len(encoded) > 1_000_000 {
			return errors.New("Content metadata response is too large.")
		}
		ttl := 30 * time.Second
		if product != nil {
			ttl = 5 * time.Minute
		}
		c.store(market+":"+id, cacheEntry{product: product, until: time.Now().Add(ttl), bytes: len(encoded)})
	}
	return nil
}
