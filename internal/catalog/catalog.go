// Package catalog ports catalog.ts: Microsoft Store search, featured lists,
// licensed product details and artwork.
package catalog

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"xodus-nexus-wails/internal/shared"
)

var imageHosts = map[string]bool{"store-images.s-microsoft.com": true, "store-images.microsoft.com": true}
var storeHosts = map[string]bool{"www.xbox.com": true, "xbox.com": true, "apps.microsoft.com": true}

const cacheTTL = 10 * time.Minute
const maxResponse = 32 << 20

// Box art is square and carries the game's title branding, matching the
// square library tiles; promotional square art is the textless fallback.
var posterPurposes = []string{"BoxArt", "TitledPoster", "Poster", "FeaturePromotionalSquareArt"}
var heroPurposes = []string{"TitledHeroArt", "SuperHeroArt", "Hero", "Screenshot"}

var (
	twelveID      = regexp.MustCompile(`^[A-Z0-9]{12}$`)
	artworkID     = regexp.MustCompile(`^[A-Z0-9]{4,30}$`)
	marketCode    = regexp.MustCompile(`^[A-Z]{2}$`)
	marketRegion  = regexp.MustCompile(`(?:^|-)([A-Z]{2})$`)
	cursorPattern = regexp.MustCompile(`^[A-Za-z0-9+/=_-]+$`)
)

type Artwork struct{ CoverURL, HeroURL string }

func displayImage(images []any, purposes []string) string {
	for _, purpose := range purposes {
		for _, entry := range images {
			image := object(entry)
			if image == nil || image["ImagePurpose"] != purpose {
				continue
			}
			uri := str(image["Uri"])
			if strings.HasPrefix(uri, "//") {
				uri = "https:" + uri
			}
			if found := safeURL(uri, imageHosts); found != "" {
				return found
			}
		}
	}
	return ""
}

func NormalizeMarket(input string) string {
	market := strings.ToUpper(strings.TrimSpace(input))
	if marketCode.MatchString(market) {
		return market
	}
	if match := marketRegion.FindStringSubmatch(market); match != nil {
		return match[1]
	}
	return "US"
}

func findImage(product map[string]any, types []string) string {
	return sizedImage(findOriginalImage(product, types), coverWidth)
}

func findOriginalImage(product map[string]any, types []string) string {
	// Box art carries the game's title branding; poster art is often textless
	// promotional key art, so it is only a fallback.
	if boxArt := safeURL(product["boxArtUrl"], imageHosts); boxArt != "" {
		return boxArt
	}
	if slices.Contains(types, "Poster") {
		if direct := safeURL(product["posterArtUrl"], imageHosts); direct != "" {
			return direct
		}
	}
	images := list(product["images"])
	for _, kind := range types {
		for _, entry := range images {
			image := object(entry)
			if image != nil && strings.EqualFold(str(image["imageType"]), kind) {
				if found := safeURL(image["url"], imageHosts); found != "" {
					return found
				}
				break
			}
		}
	}
	return ""
}

func findHero(product map[string]any) string {
	for _, entry := range list(product["previews"]) {
		image := object(entry)
		kind := strings.ToLower(str(image["imageType"]))
		if image != nil && (kind == "superheroart" || kind == "hero") {
			return sizedImage(safeURL(image["url"], imageHosts), heroWidth)
		}
	}
	return ""
}

func includedWith(product map[string]any) string {
	if direct := str(product["plaintextPassName"]); direct != "" {
		return direct
	}
	for _, summary := range list(product["skusSummary"]) {
		for _, entry := range list(object(summary)["includedWith"]) {
			if name := str(object(entry)["name"]); name != "" {
				return name
			}
		}
	}
	return ""
}

func purchasePrice(product map[string]any) string {
	price := str(object(product["priceInfo"])["displayPrice"])
	if price != "" && strings.ToLower(price) != "included" {
		return price
	}
	for _, summary := range list(product["skusSummary"]) {
		if display := str(object(summary)["displayMSRP"]); display != "" {
			return display
		}
	}
	return price
}

type packageSupport struct {
	installability   string
	installProductID string
	packageFormat    string
	supportNote      string
	distributor      string
}

const metadataUnavailable = "Microsoft package metadata was unavailable. You can still use manual Product ID installation."

func directPackageSupport(product map[string]any) *packageSupport {
	if product == nil {
		return &packageSupport{installability: "unknown", supportNote: metadataUnavailable}
	}
	var formats []string
	seen := map[string]bool{}
	for _, availability := range list(product["DisplaySkuAvailabilities"]) {
		properties := object(object(object(availability)["Sku"])["Properties"])
		for _, entry := range list(properties["Packages"]) {
			pkg := object(entry)
			desktop := slices.ContainsFunc(list(pkg["PlatformDependencies"]), func(dependency any) bool {
				return str(object(dependency)["PlatformName"]) == "Windows.Desktop"
			})
			if desktop {
				format := strings.ToUpper(str(pkg["PackageFormat"]))
				if format == "" {
					format = "UNKNOWN"
				}
				if !seen[format] {
					seen[format] = true
					formats = append(formats, format)
				}
			}
		}
	}
	if seen["MSIXVC"] {
		return &packageSupport{installability: "supported", packageFormat: "MSIXVC",
			supportNote: "A direct Windows.Desktop MSIXVC package was verified. Ownership is checked by Xodus when installation starts."}
	}
	if len(formats) > 0 {
		joined := strings.Join(formats, ", ")
		return &packageSupport{installability: "unsupported", packageFormat: joined,
			supportNote: fmt.Sprintf("The PC package uses %s, which the current Xodus streaming path does not support.", joined)}
	}
	if eaLauncherProduct(product) {
		return &packageSupport{installability: "unsupported", distributor: "ea-app",
			supportNote: "This EA Play title has no Microsoft Store package. Nexus installs it with Maxima (Settings → Maxima), signed in to the EA account linked to Xbox."}
	}
	return nil
}

// EA "Rosebud" products carry a license but no Store package: Windows hands
// the install over to the EA app.
func eaLauncherProduct(product map[string]any) bool {
	isRosebud := func(name string) bool { return strings.HasPrefix(name, "ElectronicArtsMobile.Rosebud-") }
	properties := object(product["Properties"])
	// Some titles (Dead Space 2023) keep an older package name but belong to the same product group.
	if isRosebud(str(properties["PackageFamilyName"])) || str(properties["ProductGroupName"]) == "Rosebud" {
		return true
	}
	return slices.ContainsFunc(list(product["DisplaySkuAvailabilities"]), func(availability any) bool {
		properties := object(object(object(availability)["Sku"])["Properties"])
		return isRosebud(str(object(properties["FulfillmentData"])["PackageFamilyName"]))
	})
}

type bundleReference struct {
	productID string
	primary   bool
}

func bundleReferences(product map[string]any) []bundleReference {
	var references []bundleReference
	index := map[string]int{}
	for _, availability := range list(product["DisplaySkuAvailabilities"]) {
		properties := object(object(object(availability)["Sku"])["Properties"])
		for _, entry := range list(properties["BundledSkus"]) {
			record := object(entry)
			productID := strings.ToUpper(str(record["BigId"]))
			if productID == "" {
				continue
			}
			primary := record["IsPrimary"] == true
			if i, ok := index[productID]; ok {
				references[i].primary = references[i].primary || primary
			} else {
				index[productID] = len(references)
				references = append(references, bundleReference{productID, primary})
			}
		}
	}
	return references
}

func lowerFirst(value string) string {
	r, size := utf8.DecodeRuneInString(value)
	if size == 0 {
		return value
	}
	return strings.ToLower(string(r)) + value[size:]
}

func resolveSupport(productID string, details map[string]map[string]any, visited map[string]bool, budget *int) packageSupport {
	if len(visited) > 6 || func() bool { *budget--; return *budget+1 <= 0 }() {
		return packageSupport{installability: "unknown", supportNote: "The Store bundle metadata could not be fully resolved."}
	}
	product := details[productID]
	if direct := directPackageSupport(product); direct != nil {
		return *direct
	}
	if visited[productID] {
		return packageSupport{installability: "unsupported", supportNote: "The Store bundle contains a circular product reference and cannot be resolved safely."}
	}
	nextVisited := map[string]bool{productID: true}
	for id := range visited {
		nextVisited[id] = true
	}
	references := bundleReferences(product)
	if len(references) > 1 {
		var resolved []packageSupport
		for _, reference := range references {
			resolved = append(resolved, resolveSupport(reference.productID, details, nextVisited, budget))
		}
		if slices.ContainsFunc(resolved, func(item packageSupport) bool { return item.installability == "supported" }) {
			return packageSupport{installability: "supported", packageFormat: "MSIXVC",
				supportNote: "This Store offer contains multiple products. Choose a verified PC package before installing. Ownership is checked by Xodus."}
		}
		installability := "unsupported"
		if slices.ContainsFunc(resolved, func(item packageSupport) bool { return item.installability == "unknown" }) {
			installability = "unknown"
		}
		return packageSupport{installability: installability, supportNote: "No supported PC package could be verified in this Store bundle."}
	}
	var primary []bundleReference
	for _, reference := range references {
		if reference.primary {
			primary = append(primary, reference)
		}
	}
	candidates := []bundleReference{}
	if len(primary) == 1 {
		candidates = primary
	} else if len(references) == 1 {
		candidates = references
	}
	if len(candidates) == 0 {
		note := "No Windows.Desktop MSIXVC package or resolvable primary bundle product was found. It may be an add-on or console-only product."
		if len(references) > 1 {
			note = "This Store offer contains multiple packages without one unambiguous primary PC product. Nexus will not guess which one to install."
		}
		return packageSupport{installability: "unsupported", supportNote: note}
	}
	resolved := resolveSupport(candidates[0].productID, details, nextVisited, budget)
	if resolved.installability != "supported" {
		resolved.supportNote = fmt.Sprintf("The Store offer points to %s, but %s", candidates[0].productID, lowerFirst(resolved.supportNote))
		return resolved
	}
	if resolved.installProductID == "" {
		resolved.installProductID = candidates[0].productID
	}
	resolved.supportNote = fmt.Sprintf("The Store bundle was resolved to its primary Windows.Desktop MSIXVC product %s. Ownership is checked by Xodus when installation starts.", resolved.installProductID)
	return resolved
}

// ParseProducts combines a search listing with display-catalog package metadata.
func ParseProducts(search map[string]any, catalog map[string]any) []shared.CatalogProduct {
	details := map[string]map[string]any{}
	for _, detail := range list(catalog["Products"]) {
		record := object(detail)
		if id := strings.ToUpper(str(record["ProductId"])); id != "" && record != nil {
			details[id] = record
		}
	}
	out := []shared.CatalogProduct{}
	for _, entry := range list(search["productsList"]) {
		product := object(entry)
		productID := strings.ToUpper(str(product["productId"]))
		title := str(product["title"])
		if productID == "" || title == "" || product["isGame"] == false {
			continue
		}
		budget := 512
		support := resolveSupport(productID, details, map[string]bool{}, &budget)
		description := str(product["description"])
		if description == "" {
			description = "Microsoft Store did not provide a description."
		}
		categories := []string{}
		for _, category := range list(product["categories"]) {
			if value := str(category); value != "" {
				categories = append(categories, value)
			}
		}
		var rating *float64
		if value, ok := product["averageRating"].(float64); ok {
			rating = &value
		}
		out = append(out, shared.CatalogProduct{
			ProductID: productID, Title: title, Description: description, Publisher: str(product["publisherName"]),
			PosterURL: findImage(product, []string{"BoxArt", "Poster"}), HeroURL: findHero(product),
			StoreURL: safeURL(product["redirectUrl"], storeHosts), Categories: categories,
			PurchasePrice: purchasePrice(product), IncludedWith: includedWith(product), ReleaseDate: str(product["releaseDateUtc"]),
			AverageRating: rating, Installability: support.installability, InstallProductID: support.installProductID,
			PackageFormat: support.packageFormat, SupportNote: support.supportNote, Distributor: support.distributor,
		})
	}
	return out
}

type cached[T any] struct {
	until time.Time
	value T
}

type Service struct {
	fetch Fetcher
	Now   func() time.Time

	mu        sync.Mutex
	gameCache map[string]cached[shared.GameStoreDetails]
	gameOrder []string
	cache     map[string]cached[shared.CatalogSearchResult]
}

func NewService(fetch Fetcher) *Service {
	if fetch == nil {
		fetch = HTTPFetcher(nil)
	}
	return &Service{fetch: fetch, Now: time.Now, gameCache: map[string]cached[shared.GameStoreDetails]{}, cache: map[string]cached[shared.CatalogSearchResult]{}}
}

func (s *Service) getJSON(ctx context.Context, target string) (Response, map[string]any, error) {
	ctx, cancel := withTimeout(ctx, 20*time.Second)
	defer cancel()
	response, err := s.fetch(ctx, target, maxResponse)
	if err != nil {
		return Response{}, nil, err
	}
	if !response.OK() {
		return response, nil, nil
	}
	var value any
	if err := json.Unmarshal(response.Body, &value); err != nil {
		return response, nil, err
	}
	return response, object(value), nil
}

// GameDetails reads descriptive data for an installed game, independently of package support.
func (s *Service) GameDetails(ctx context.Context, productID, rawMarket string) (*shared.GameStoreDetails, error) {
	if !twelveID.MatchString(productID) {
		return nil, errors.New("Invalid Store product ID.")
	}
	market := NormalizeMarket(rawMarket)
	key := market + ":" + productID
	s.mu.Lock()
	if entry, ok := s.gameCache[key]; ok && entry.until.After(s.Now()) {
		s.mu.Unlock()
		value := shared.Clone(entry.value)
		return &value, nil
	}
	s.mu.Unlock()
	catalog := s.details(ctx, []string{productID}, market)
	var product map[string]any
	for _, entry := range catalog {
		if record := object(entry); record != nil && record["ProductId"] == productID {
			product = record
			break
		}
	}
	if product == nil || product["ProductType"] != "Game" {
		return nil, nil
	}
	localizedList := list(product["LocalizedProperties"])
	var localized map[string]any
	if len(localizedList) > 0 {
		localized = object(localizedList[0])
	}
	title := shared.TruncateUTF16(str(localized["ProductTitle"]), 512)
	if title == "" {
		return nil, nil
	}
	images := list(localized["Images"])
	if len(images) > 200 {
		images = images[:200]
	}
	var screenshots []string
	for _, image := range images {
		if object(image)["ImagePurpose"] != "Screenshot" {
			continue
		}
		if found := displayImage([]any{image}, []string{"Screenshot"}); found != "" && !slices.Contains(screenshots, found) {
			screenshots = append(screenshots, found)
		}
	}
	if len(screenshots) > 24 {
		screenshots = screenshots[:24]
	}
	categories := []string{}
	for _, value := range list(object(product["Properties"])["Categories"]) {
		if category := shared.TruncateUTF16(str(value), 80); category != "" {
			categories = append(categories, category)
		}
	}
	if len(categories) > 12 {
		categories = categories[:12]
	}
	var objects []any
	for _, image := range images {
		if object(image) != nil {
			objects = append(objects, image)
		}
	}
	var marketProperties map[string]any
	if markets := list(product["MarketProperties"]); len(markets) > 0 {
		marketProperties = object(markets[0])
	}
	if screenshots == nil {
		screenshots = []string{}
	}
	value := shared.GameStoreDetails{ProductID: productID, Title: title, Description: shared.TruncateUTF16(str(localized["ProductDescription"]), 64_000),
		Publisher: shared.TruncateUTF16(str(localized["PublisherName"]), 512), HeroURL: sizedImage(displayImage(objects, heroPurposes), heroWidth),
		Categories: categories, Screenshots: screenshots, ReleaseDate: str(marketProperties["OriginalReleaseDate"])}
	s.mu.Lock()
	if _, exists := s.gameCache[key]; !exists {
		if len(s.gameOrder) >= 100 {
			delete(s.gameCache, s.gameOrder[0])
			s.gameOrder = s.gameOrder[1:]
		}
		s.gameOrder = append(s.gameOrder, key)
	}
	s.gameCache[key] = cached[shared.GameStoreDetails]{s.Now().Add(cacheTTL), value}
	s.mu.Unlock()
	clone := shared.Clone(value)
	return &clone, nil
}

// LicensedProducts resolves licensed IDs, never search results or an install-history guess.
func (s *Service) LicensedProducts(ctx context.Context, ids []string, rawMarket string) ([]shared.CatalogProduct, error) {
	if len(ids) > 60 || slices.ContainsFunc(ids, func(id string) bool { return !twelveID.MatchString(id) }) {
		return nil, errors.New("Invalid licensed product list.")
	}
	products, err := s.licensedDetails(ctx, ids, NormalizeMarket(rawMarket))
	if err != nil {
		return nil, err
	}
	wanted := map[string]bool{}
	for _, id := range ids {
		wanted[id] = true
	}
	var rows []any
	for _, entry := range products {
		product := object(entry)
		id := str(product["ProductId"])
		if product == nil || id == "" || !wanted[id] || product["ProductType"] != "Game" {
			continue
		}
		var localized map[string]any
		if items := list(product["LocalizedProperties"]); len(items) > 0 {
			localized = object(items[0])
		}
		images := list(localized["Images"])
		find := func(purposes []string) any {
			for _, purpose := range purposes {
				for _, image := range images {
					if record := object(image); record != nil && record["ImagePurpose"] == purpose {
						uri := str(record["Uri"])
						if strings.HasPrefix(uri, "//") {
							return "https:" + uri
						}
						return uri
					}
				}
			}
			return nil
		}
		title := str(localized["ProductTitle"])
		if title == "" {
			title = id
		}
		rows = append(rows, map[string]any{"productId": id, "title": title, "description": localized["ProductDescription"],
			"publisherName": localized["PublisherName"], "boxArtUrl": find(posterPurposes), "isGame": true})
	}
	// Ownership and installation support are independent. Preserve the game's
	// real package status for the library badge and Ready to install filter.
	return ParseProducts(map[string]any{"productsList": rows}, map[string]any{"Products": products}), nil
}

func (s *Service) fetchDetails(ctx context.Context, batch []string, market string) ([]map[string]any, error) {
	response, catalog, err := s.getJSON(ctx, productsURL(batch, market))
	if err != nil {
		return nil, err
	}
	if !response.OK() {
		return nil, errors.New("Catalog request failed.")
	}
	products, ok := catalog["Products"].([]any)
	if catalog == nil || !ok {
		return nil, errors.New("Invalid catalog response.")
	}
	var out []map[string]any
	for _, entry := range products {
		record := object(entry)
		id := strings.ToUpper(str(record["ProductId"]))
		if record != nil && id != "" && slices.Contains(batch, id) {
			out = append(out, record)
		}
	}
	return out, nil
}

// orderedProducts is a Map<string, record> with insertion order.
type orderedProducts struct {
	order []string
	byID  map[string]map[string]any
}

func (o *orderedProducts) set(id string, record map[string]any) {
	if o.byID == nil {
		o.byID = map[string]map[string]any{}
	}
	if _, ok := o.byID[id]; !ok {
		o.order = append(o.order, id)
	}
	o.byID[id] = record
}

func (o *orderedProducts) values() []any {
	out := make([]any, len(o.order))
	for i, id := range o.order {
		out[i] = o.byID[id]
	}
	return out
}

func unique(values []string) []string {
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

// licensedDetails gives each licensed root its own bounded graph; unrelated
// editions cannot starve its PC package. Shared children are still fetched only once.
func (s *Service) licensedDetails(ctx context.Context, ids []string, market string) ([]any, error) {
	var details orderedProducts
	fetched := map[string]bool{}
	roots := unique(ids)
	seen := make([]map[string]bool, len(roots))
	frontiers := make([][]string, len(roots))
	for i, root := range roots {
		seen[i] = map[string]bool{root: true}
		frontiers[i] = []string{root}
	}
	active := func() bool {
		return slices.ContainsFunc(frontiers, func(frontier []string) bool { return len(frontier) > 0 })
	}
	for depth := 0; depth <= 6 && active(); depth++ {
		var pending []string
		for _, id := range unique(slices.Concat(frontiers...)) {
			if !fetched[id] {
				pending = append(pending, id)
			}
		}
		var batches [][]string
		for offset := 0; offset < len(pending); offset += 25 {
			batches = append(batches, pending[offset:min(len(pending), offset+25)])
		}
		var (
			mu       sync.Mutex
			next     int
			stopped  bool
			firstErr error
			workers  sync.WaitGroup
		)
		for range min(4, len(batches)) {
			workers.Add(1)
			go func() {
				defer workers.Done()
				for {
					mu.Lock()
					if stopped || next >= len(batches) {
						mu.Unlock()
						return
					}
					batch := batches[next]
					next++
					mu.Unlock()
					products, err := s.fetchDetails(ctx, batch, market)
					mu.Lock()
					if err != nil {
						stopped = true
						if firstErr == nil {
							firstErr = err
						}
						mu.Unlock()
						return
					}
					for _, product := range products {
						details.set(strings.ToUpper(str(product["ProductId"])), product)
					}
					for _, id := range batch {
						fetched[id] = true
					}
					mu.Unlock()
				}
			}()
		}
		workers.Wait()
		if firstErr != nil {
			return nil, errors.New("Microsoft Store game details could not be loaded. Check your connection and retry the library.")
		}
		for root, frontier := range frontiers {
			var following []string
			if depth < 6 {
				for _, id := range frontier {
					product := details.byID[id]
					// A direct desktop package defines the product, even if unsupported.
					if product == nil || directPackageSupport(product) != nil {
						continue
					}
					for _, child := range bundleReferences(product) {
						if len(seen[root]) >= 48 {
							break
						}
						if !seen[root][child.productID] {
							seen[root][child.productID] = true
							following = append(following, child.productID)
						}
					}
				}
			}
			frontiers[root] = following
		}
	}
	return details.values(), nil
}

// details enriches cards; failures leave cards visible without package metadata.
func (s *Service) details(ctx context.Context, ids []string, market string) []any {
	var details orderedProducts
	pending := unique(ids)
	for depth := 0; depth < 4 && len(pending) > 0 && len(details.order) < 100; depth++ {
		var next []string
		nextSet := map[string]bool{}
		for offset := 0; offset < len(pending); offset += 25 {
			var batch []string
			for _, id := range pending[offset:min(len(pending), offset+25)] {
				if _, ok := details.byID[id]; !ok {
					batch = append(batch, id)
				}
			}
			if len(batch) == 0 {
				continue
			}
			records, err := s.fetchDetails(ctx, batch, market)
			if err != nil {
				// Cards remain visible even when display catalog enrichment is unavailable.
				continue
			}
			for _, record := range records {
				id := strings.ToUpper(str(record["ProductId"]))
				details.set(id, record)
				for _, reference := range bundleReferences(record) {
					if _, ok := details.byID[reference.productID]; !ok && !nextSet[reference.productID] && len(details.order)+len(next) < 100 {
						nextSet[reference.productID] = true
						next = append(next, reference.productID)
					}
				}
			}
		}
		pending = next
	}
	return details.values()
}

func (s *Service) result(ctx context.Context, search map[string]any, query, market string, includePaging bool) shared.CatalogSearchResult {
	var ids []string
	for _, entry := range list(search["productsList"]) {
		if id := strings.ToUpper(str(object(entry)["productId"])); id != "" {
			ids = append(ids, id)
		}
	}
	catalog := s.details(ctx, unique(ids), market)
	result := shared.CatalogSearchResult{Query: query, Market: market, Products: ParseProducts(search, map[string]any{"Products": catalog}),
		FetchedAt: shared.JSISOString(s.Now()), Source: "Microsoft Store"}
	if includePaging {
		result.Cursor = str(search["cursor"])
		result.HasMore = search["hasMorePages"] == true && result.Cursor != ""
	}
	return result
}

func (s *Service) cachedResult(key string) (shared.CatalogSearchResult, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if entry, ok := s.cache[key]; ok && entry.until.After(s.Now()) {
		return shared.Clone(entry.value), true
	}
	return shared.CatalogSearchResult{}, false
}

func (s *Service) store(key string, value shared.CatalogSearchResult) shared.CatalogSearchResult {
	s.mu.Lock()
	s.cache[key] = cached[shared.CatalogSearchResult]{s.Now().Add(cacheTTL), value}
	s.mu.Unlock()
	return shared.Clone(value)
}

func (s *Service) Search(ctx context.Context, rawQuery, rawMarket, cursor string) (shared.CatalogSearchResult, error) {
	query := strings.TrimSpace(rawQuery)
	if shared.UTF16Len(query) < 2 {
		return shared.CatalogSearchResult{}, errors.New("Enter at least two characters to search Microsoft Store.")
	}
	if shared.UTF16Len(query) > 100 {
		return shared.CatalogSearchResult{}, errors.New("The Store search is limited to 100 characters.")
	}
	if cursor != "" && (len(cursor) > 4096 || !cursorPattern.MatchString(cursor)) {
		return shared.CatalogSearchResult{}, errors.New("The Store paging cursor is invalid.")
	}
	market := NormalizeMarket(rawMarket)
	key := market + "\x00" + strings.ToLower(query) + "\x00" + cursor
	if value, ok := s.cachedResult(key); ok {
		return value, nil
	}
	params := url.Values{}
	for k, v := range map[string]string{"query": query, "mediaType": "games", "age": "all", "price": "all", "category": "all", "subscription": "all", "gl": market, "hl": "en-us"} {
		params.Set(k, v)
	}
	if cursor != "" {
		params.Set("cursor", cursor)
	}
	response, search, err := s.getJSON(ctx, "https://apps.microsoft.com/api/products/search?"+params.Encode())
	if err != nil && response.Status == 0 {
		return shared.CatalogSearchResult{}, err
	}
	if !response.OK() {
		return shared.CatalogSearchResult{}, httpError("Microsoft Store search", response.Status)
	}
	if err != nil {
		return shared.CatalogSearchResult{}, err
	}
	return s.store(key, s.result(ctx, search, query, market, true)), nil
}

func (s *Service) Featured(ctx context.Context, rawMarket string) (shared.CatalogSearchResult, error) {
	market := NormalizeMarket(rawMarket)
	key := market + "\x00featured"
	if value, ok := s.cachedResult(key); ok {
		return value, nil
	}
	params := url.Values{}
	for k, v := range map[string]string{"listName": "TopTrending", "pgNo": "1", "noItems": "12", "filteredCategories": "AllProducts",
		"mediaType": "Games", "gl": market, "hl": "en-us", "exp": "0"} {
		params.Set(k, v)
	}
	response, search, err := s.getJSON(ctx, "https://apps.microsoft.com/api/Reco/GetComputedProductsList?"+params.Encode())
	if err != nil && response.Status == 0 {
		return shared.CatalogSearchResult{}, err
	}
	if !response.OK() {
		return shared.CatalogSearchResult{}, httpError("Microsoft Store featured collection", response.Status)
	}
	if err != nil {
		return shared.CatalogSearchResult{}, err
	}
	title := str(search["title"])
	if title == "" {
		title = "Trending games"
	}
	return s.store(key, s.result(ctx, search, title, market, false)), nil
}

func (s *Service) Posters(ctx context.Context, ids []string, rawMarket string) map[string]string {
	posters := map[string]string{}
	for id, images := range s.Artwork(ctx, ids, rawMarket) {
		if images.CoverURL != "" {
			posters[id] = images.CoverURL
		}
	}
	return posters
}

// Artwork fetches tile and landscape images by genuine product ID. Installed
// package logos are not covers; the local logo stays an offline fallback.
func (s *Service) Artwork(ctx context.Context, ids []string, rawMarket string) map[string]Artwork {
	market := NormalizeMarket(rawMarket)
	var candidates []string
	for _, id := range ids {
		if id = strings.ToUpper(strings.TrimSpace(id)); artworkID.MatchString(id) {
			candidates = append(candidates, id)
		}
	}
	candidates = unique(candidates)
	artwork := map[string]Artwork{}
	for offset := 0; offset < len(candidates); offset += 25 {
		batch := candidates[offset:min(len(candidates), offset+25)]
		response, catalog, err := s.getJSON(ctx, productsURL(batch, market))
		if err != nil || !response.OK() {
			// Covers stay hidden when the display catalog is unavailable.
			continue
		}
		for _, entry := range list(catalog["Products"]) {
			record := object(entry)
			id := strings.ToUpper(str(record["ProductId"]))
			if id == "" || record == nil || !slices.Contains(batch, id) {
				continue
			}
			var first map[string]any
			if localized := list(record["LocalizedProperties"]); len(localized) > 0 {
				first = object(localized[0])
			}
			images := list(first["Images"])
			cover, hero := sizedImage(displayImage(images, posterPurposes), coverWidth), sizedImage(displayImage(images, heroPurposes), heroWidth)
			if cover != "" || hero != "" {
				artwork[id] = Artwork{cover, hero}
			}
		}
	}
	return artwork
}
