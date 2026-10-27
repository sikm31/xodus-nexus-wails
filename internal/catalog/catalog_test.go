package catalog

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
	"xodus-nexus-wails/internal/shared"
)

// decode turns a JSON literal into the untyped shape the parser receives.
func decode(t *testing.T, text string) map[string]any {
	t.Helper()
	var value map[string]any
	if err := json.Unmarshal([]byte(text), &value); err != nil {
		t.Fatal(err)
	}
	return value
}

const searchJSON = `{"productsList":[{"productId":"9game123","title":"Purchased Game","description":"A real Store description.","publisherName":"Studio","isGame":true,
"posterArtUrl":"https://store-images.s-microsoft.com/image/poster","redirectUrl":"https://www.xbox.com/games/store/game/9game123","priceInfo":{"displayPrice":"$29.99"},"categories":["Action"]},
{"productId":"9bundle1","title":"Bundle Game","description":"Bundle","isGame":true,"plaintextPassName":"Xbox Game Pass"}],"cursor":"next_page","hasMorePages":true}`

const catalogJSON = `{"Products":[{"ProductId":"9GAME123","DisplaySkuAvailabilities":[{"Sku":{"Properties":{"Packages":[{"PackageFormat":"MSIXVC","PlatformDependencies":[{"PlatformName":"Windows.Desktop"}]}]}}}]},
{"ProductId":"9BUNDLE1","DisplaySkuAvailabilities":[{"Sku":{"Properties":{"Packages":[]}}}]}]}`

func respond(body string) Fetcher {
	return func(context.Context, string, int64) (Response, error) { return Response{200, []byte(body)}, nil }
}

func TestParseProducts(t *testing.T) {
	products := ParseProducts(decode(t, searchJSON), decode(t, catalogJSON))
	if p := products[0]; p.ProductID != "9GAME123" || p.PurchasePrice != "$29.99" || p.IncludedWith != "" || p.Installability != "supported" || p.PackageFormat != "MSIXVC" {
		t.Errorf("first = %+v", p)
	}
	if p := products[1]; p.IncludedWith != "Xbox Game Pass" || p.Installability != "unsupported" {
		t.Errorf("second = %+v", p)
	}
	foreign := decode(t, searchJSON)
	first := list(foreign["productsList"])[0].(map[string]any)
	first["posterArtUrl"] = "https://example.com/tracker.png"
	if ParseProducts(map[string]any{"productsList": []any{first}}, decode(t, catalogJSON))[0].PosterURL != "" {
		t.Error("foreign image host accepted")
	}
	first["boxArtUrl"] = "https://store-images.s-microsoft.com/image/boxart"
	if got := ParseProducts(map[string]any{"productsList": []any{first}}, decode(t, catalogJSON))[0].PosterURL; got != "https://store-images.s-microsoft.com/image/boxart?w=600" {
		t.Errorf("box art = %q", got)
	}
}

func TestMultiProductEditionAndEAppx(t *testing.T) {
	edition := ParseProducts(decode(t, `{"productsList":[{"productId":"9edition","title":"Game Standard Edition","isGame":true}]}`), decode(t, `{"Products":[
{"ProductId":"9EDITION","DisplaySkuAvailabilities":[{"Sku":{"Properties":{"Packages":[],"BundledSkus":[{"BigId":"9package","IsPrimary":true},{"BigId":"9bonus","IsPrimary":false}]}}}]},
{"ProductId":"9PACKAGE","DisplaySkuAvailabilities":[{"Sku":{"Properties":{"Packages":[{"PackageFormat":"MSIXVC","PlatformDependencies":[{"PlatformName":"Windows.Desktop"}]}]}}}]},
{"ProductId":"9BONUS","DisplaySkuAvailabilities":[{"Sku":{"Properties":{"Packages":[]}}}]}]}`))[0]
	if edition.ProductID != "9EDITION" || edition.Installability != "supported" || edition.PackageFormat != "MSIXVC" || edition.InstallProductID != "" {
		t.Errorf("edition = %+v", edition)
	}
	eappx := ParseProducts(decode(t, `{"productsList":[{"productId":"9eappx","title":"Legacy PC Game","isGame":true}]}`), decode(t, `{"Products":[
{"ProductId":"9EAPPX","DisplaySkuAvailabilities":[{"Sku":{"Properties":{"Packages":[{"PackageFormat":"EAppx","PlatformDependencies":[{"PlatformName":"Windows.Desktop"}]}],"BundledSkus":[{"BigId":"9package","IsPrimary":true}]}}}]}]}`))[0]
	if eappx.Installability != "unsupported" || eappx.PackageFormat != "EAPPX" || eappx.InstallProductID != "" {
		t.Errorf("eappx = %+v", eappx)
	}
}

func TestLicensedProducts(t *testing.T) {
	data := `{"Products":[{"ProductId":"9AAAAAAAAAAA","ProductType":"Game","LocalizedProperties":[{"ProductTitle":"Licensed PC Game","PublisherName":"Studio"}],
"DisplaySkuAvailabilities":[{"Sku":{"Properties":{"Packages":[{"PackageFormat":"MSIXVC","PlatformDependencies":[{"PlatformName":"Windows.Desktop"}]}]}}}]},
{"ProductId":"9BBBBBBBBBBB","ProductType":"Durable","LocalizedProperties":[{"ProductTitle":"DLC"}],"DisplaySkuAvailabilities":[{"Sku":{"Properties":{"Packages":[{"PackageFormat":"MSIXVC","PlatformDependencies":[{"PlatformName":"Windows.Desktop"}]}]}}}]},
{"ProductId":"9CCCCCCCCCCC","ProductType":"Game","LocalizedProperties":[{"ProductTitle":"Console game"}],"DisplaySkuAvailabilities":[]}]}`
	result, err := NewService(respond(data)).LicensedProducts(context.Background(), []string{"9AAAAAAAAAAA", "9BBBBBBBBBBB", "9CCCCCCCCCCC"}, "US")
	if err != nil || len(result) != 2 || result[0].Title != "Licensed PC Game" || result[1].Title != "Console game" ||
		result[0].Installability != "supported" || result[1].Installability != "unsupported" {
		t.Errorf("result = %+v, %v", result, err)
	}
	legacy := `{"Products":[{"ProductId":"9AAAAAAAAAAA","ProductType":"Game","LocalizedProperties":[{"ProductTitle":"Legacy PC game"}],
"DisplaySkuAvailabilities":[{"Sku":{"Properties":{"Packages":[{"PackageFormat":"EAppxBundle","PlatformDependencies":[{"PlatformName":"Windows.Desktop"}]}]}}}]},
{"ProductId":"9BBBBBBBBBBB","ProductType":"Game","DisplaySkuAvailabilities":[{"Sku":{"Properties":{"BundledSkus":[{"BigId":"9CCCCCCCCCCC"}]}}}]}]}`
	result, _ = NewService(respond(legacy)).LicensedProducts(context.Background(), []string{"9AAAAAAAAAAA", "9BBBBBBBBBBB"}, "US")
	if len(result) != 2 || result[0].Installability != "unsupported" || result[0].PackageFormat != "EAPPXBUNDLE" ||
		result[1].Title != "9BBBBBBBBBBB" || result[1].Installability != "unknown" {
		t.Errorf("legacy = %+v", result)
	}
}

func TestSearchAndFeatured(t *testing.T) {
	var urls []string
	fetch := func(ctx context.Context, target string, max int64) (Response, error) {
		urls = append(urls, target)
		if strings.Contains(target, "displaycatalog") {
			return Response{200, []byte(catalogJSON)}, nil
		}
		return Response{200, []byte(strings.Replace(searchJSON, `"cursor"`, `"title":"Trending games","cursor"`, 1))}, nil
	}
	result, err := NewService(fetch).Search(context.Background(), "gears", "neutral", "")
	if err != nil || result.Market != "US" || !result.HasMore || result.Products[0].Installability != "supported" ||
		!strings.Contains(urls[0], "mediaType=games") || !strings.Contains(urls[1], "bigIds=9GAME123%2C9BUNDLE1") {
		t.Errorf("search = %+v %v %v", result, err, urls)
	}
	urls = nil
	featured, err := NewService(fetch).Featured(context.Background(), "CZ")
	if err != nil || featured.Query != "Trending games" || featured.Market != "CZ" || featured.HasMore ||
		!strings.Contains(urls[0], "GetComputedProductsList") || !strings.Contains(urls[0], "mediaType=Games") {
		t.Errorf("featured = %+v %v", featured, err)
	}
	if _, err := NewService(fetch).Search(context.Background(), "g", "US", ""); err == nil {
		t.Error("one-character query accepted")
	}
	failing := func(context.Context, string, int64) (Response, error) { return Response{503, nil}, nil }
	if _, err := NewService(failing).Search(context.Background(), "gears", "US", ""); err == nil || !strings.Contains(err.Error(), "HTTP 503") {
		t.Errorf("HTTP failure: %v", err)
	}
}

func TestArtwork(t *testing.T) {
	posters := NewService(respond(`{"Products":[{"ProductId":"9GAME123","LocalizedProperties":[{"Images":[
{"ImagePurpose":"Logo","Uri":"//store-images.s-microsoft.com/image/logo"},
{"ImagePurpose":"FeaturePromotionalSquareArt","Uri":"//store-images.s-microsoft.com/image/promo"},
{"ImagePurpose":"BoxArt","Uri":"//store-images.s-microsoft.com/image/boxart"}]}]},
{"ProductId":"9FOREIGN","LocalizedProperties":[{"Images":[{"ImagePurpose":"BoxArt","Uri":"//tracker.example.com/image/boxart"}]}]}]}`)).
		Posters(context.Background(), []string{"9game123", "9foreign", "bad id"}, "US")
	if posters["9GAME123"] != "https://store-images.s-microsoft.com/image/boxart?w=600" || posters["9FOREIGN"] != "" {
		t.Errorf("posters = %v", posters)
	}
	artwork := NewService(respond(`{"Products":[{"ProductId":"9GAME123","LocalizedProperties":[{"Images":[
{"ImagePurpose":"Logo","Uri":"//store-images.s-microsoft.com/logo"},{"ImagePurpose":"BoxArt","Uri":"//foreign.example/box"},
{"ImagePurpose":"BoxArt","Uri":"//store-images.s-microsoft.com/box"},{"ImagePurpose":"SuperHeroArt","Uri":"//store-images.s-microsoft.com/hero"},
{"ImagePurpose":"TitledHeroArt","Uri":"//store-images.s-microsoft.com/titled-hero"}]}]},
{"ProductId":"9OTHER","LocalizedProperties":[{"Images":[{"ImagePurpose":"BoxArt","Uri":"//store-images.s-microsoft.com/other"}]}]}]}`)).
		Artwork(context.Background(), []string{"9game123"}, "US")
	if artwork["9GAME123"] != (Artwork{"https://store-images.s-microsoft.com/box?w=600", "https://store-images.s-microsoft.com/titled-hero?w=1920"}) || len(artwork) != 1 {
		t.Errorf("artwork = %v", artwork)
	}
	offline := func(context.Context, string, int64) (Response, error) { return Response{}, errors.New("Offline") }
	if len(NewService(offline).Artwork(context.Background(), []string{"9GAME123"}, "US")) != 0 {
		t.Error("offline artwork")
	}
	logo := respond(`{"Products":[{"ProductId":"9GAME123","LocalizedProperties":[{"Images":[{"ImagePurpose":"Logo","Uri":"//store-images.s-microsoft.com/logo"}]}]}]}`)
	if len(NewService(logo).Artwork(context.Background(), []string{"9GAME123"}, "US")) != 0 {
		t.Error("a logo is not artwork")
	}
}

func bundleID(n int) string {
	return "9" + fmt.Sprintf("%011s", strings.ToUpper(strconv.FormatInt(int64(n), 36)))
}

func bundleGame(productID string, children ...string) map[string]any {
	properties := map[string]any{"Packages": []any{map[string]any{"PackageFormat": "MSIXVC", "PlatformDependencies": []any{map[string]any{"PlatformName": "Windows.Desktop"}}}}}
	if len(children) > 0 {
		var bundled []any
		for _, child := range children {
			bundled = append(bundled, map[string]any{"BigId": child})
		}
		properties = map[string]any{"Packages": []any{}, "BundledSkus": bundled}
	}
	return map[string]any{"ProductId": productID, "ProductType": "Game", "LocalizedProperties": []any{map[string]any{"ProductTitle": productID}},
		"DisplaySkuAvailabilities": []any{map[string]any{"Sku": map[string]any{"Properties": properties}}}}
}

type bundleFixture struct {
	mu       sync.Mutex
	requests [][]string
	active   int
	peak     int
}

func (f *bundleFixture) service(products []map[string]any) *Service {
	return NewService(func(ctx context.Context, target string, max int64) (Response, error) {
		parsed, _ := url.Parse(target)
		batch := strings.Split(parsed.Query().Get("bigIds"), ",")
		f.mu.Lock()
		f.requests = append(f.requests, batch)
		f.active++
		f.peak = max0(f.peak, f.active)
		f.mu.Unlock()
		time.Sleep(time.Millisecond)
		f.mu.Lock()
		f.active--
		f.mu.Unlock()
		var matched []any
		for _, product := range products {
			if slices.Contains(batch, product["ProductId"].(string)) {
				matched = append(matched, product)
			}
		}
		body, _ := json.Marshal(map[string]any{"Products": append([]any{}, matched...)})
		return Response{200, body}, nil
	})
}

func max0(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func (f *bundleFixture) flat() []string { return slices.Concat(f.requests...) }

func TestEveryLicensedEditionResolvedIndependently(t *testing.T) {
	var roots, children []string
	var products []map[string]any
	for i := range 60 {
		roots = append(roots, bundleID(i))
		children = append(children, bundleID(i+100))
	}
	for i, root := range roots {
		products = append(products, bundleGame(root, children[i]))
	}
	for _, child := range children {
		products = append(products, bundleGame(child))
	}
	f := &bundleFixture{}
	result, err := f.service(products).LicensedProducts(context.Background(), roots, "US")
	var supported []string
	for _, p := range result {
		if p.Installability == "supported" {
			supported = append(supported, p.ProductID)
		}
	}
	slices.Sort(supported)
	sorted := slices.Sorted(slices.Values(roots))
	if err != nil || !slices.Equal(supported, sorted) || len(f.flat()) != 120 || f.peak > 4 ||
		slices.ContainsFunc(f.requests, func(batch []string) bool { return len(batch) > 25 }) {
		t.Errorf("supported %d, requests %d, peak %d, err %v", len(supported), len(f.flat()), f.peak, err)
	}
}

func TestSharedChildrenFetchedOnce(t *testing.T) {
	f := &bundleFixture{}
	result, _ := f.service([]map[string]any{bundleGame(bundleID(1), bundleID(3)), bundleGame(bundleID(2), bundleID(3)), bundleGame(bundleID(3))}).
		LicensedProducts(context.Background(), []string{bundleID(1), bundleID(2)}, "US")
	count := 0
	for _, id := range f.flat() {
		if id == bundleID(3) {
			count++
		}
	}
	if slices.ContainsFunc(result, func(p sharedProduct) bool { return p.Installability != "supported" }) || count != 1 {
		t.Errorf("result %+v, child requests %d", result, count)
	}
}

func TestRootBudgetsAreSeparate(t *testing.T) {
	var many []string
	for i := range 70 {
		many = append(many, bundleID(i+100))
	}
	products := []map[string]any{bundleGame(bundleID(1), many...), bundleGame(bundleID(2), bundleID(3)), bundleGame(bundleID(3))}
	for _, child := range many {
		products = append(products, bundleGame(child))
	}
	f := &bundleFixture{}
	result, _ := f.service(products).LicensedProducts(context.Background(), []string{bundleID(1), bundleID(2)}, "US")
	index := slices.IndexFunc(result, func(p sharedProduct) bool { return p.ProductID == bundleID(2) })
	if index < 0 || result[index].Installability != "supported" || len(unique(f.flat())) > 50 {
		t.Errorf("second root %+v, unique requests %d", result, len(unique(f.flat())))
	}
}

func TestCyclicAndDeepBundles(t *testing.T) {
	var chain []map[string]any
	for i := range 12 {
		chain = append(chain, bundleGame(bundleID(i), bundleID(i+1)))
	}
	f := &bundleFixture{}
	result, _ := f.service(chain).LicensedProducts(context.Background(), []string{bundleID(0)}, "US")
	if result[0].Installability == "supported" || len(f.flat()) != 7 {
		t.Errorf("deep chain: %+v, requests %d", result[0], len(f.flat()))
	}
	cycle := &bundleFixture{}
	result, _ = cycle.service([]map[string]any{bundleGame(bundleID(20), bundleID(21)), bundleGame(bundleID(21), bundleID(20))}).
		LicensedProducts(context.Background(), []string{bundleID(20)}, "US")
	if result[0].Installability == "supported" || len(cycle.flat()) != 2 {
		t.Errorf("cycle: %+v, requests %d", result[0], len(cycle.flat()))
	}
}

func TestDepthNotExtendedByOtherRoots(t *testing.T) {
	var roots []string
	for i := range 9 {
		roots = append(roots, bundleID(i))
	}
	var products []map[string]any
	for i, root := range roots {
		if i+1 < len(roots) {
			products = append(products, bundleGame(root, roots[i+1]))
		} else {
			products = append(products, bundleGame(root))
		}
	}
	f := &bundleFixture{}
	result, _ := f.service(products).LicensedProducts(context.Background(), roots, "US")
	find := func(id string) string {
		return result[slices.IndexFunc(result, func(p sharedProduct) bool { return p.ProductID == id })].Installability
	}
	if find(roots[0]) != "unknown" || find(roots[2]) != "supported" || len(f.flat()) != 9 {
		t.Errorf("root0 %s root2 %s requests %d", find(roots[0]), find(roots[2]), len(f.flat()))
	}
}

type sharedProduct = sharedCatalogProduct
type sharedCatalogProduct = shared.CatalogProduct

func TestSizedImage(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"https://store-images.s-microsoft.com/image/apps.1", "https://store-images.s-microsoft.com/image/apps.1?w=600"},
		{"https://store-images.s-microsoft.com/image/apps.1?w=300", "https://store-images.s-microsoft.com/image/apps.1?w=300"},
		{"https://images-eds-ssl.xboxlive.com/image?url=x", "https://images-eds-ssl.xboxlive.com/image?url=x"},
		{"", ""},
	} {
		if got := sizedImage(tc.in, coverWidth); got != tc.want {
			t.Errorf("sizedImage(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestOriginalImage(t *testing.T) {
	if got := OriginalImage(sizedImage("https://store-images.s-microsoft.com/image/apps.1", coverWidth)); got != "https://store-images.s-microsoft.com/image/apps.1" {
		t.Errorf("OriginalImage = %q", got)
	}
}
