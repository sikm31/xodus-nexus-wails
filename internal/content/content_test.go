package content

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"xodus-nexus-wails/internal/catalog"
	"xodus-nexus-wails/internal/shared"
)

const (
	BASE    = "9AAAAAAAAAAA"
	EDITION = "9BBBBBBBBBBB"
	PASS    = "9CCCCCCCCCCC"
	DLC     = "9DDDDDDDDDDD"
	FOREIGN = "9EEEEEEEEEEE"
)

func merge(base map[string]any, extra map[string]any) map[string]any {
	out := map[string]any{}
	for k, v := range base {
		out[k] = v
	}
	for k, v := range extra {
		out[k] = v
	}
	return out
}

func pkgRecord(extra map[string]any) map[string]any {
	return merge(map[string]any{"ContentId": "01234567-89ab-cdef-0123-456789abcdef", "PackageFormat": "MSIXVC", "Architectures": []any{"x64"},
		"PackageFamilyName": "Example.Base_hash", "PlatformDependencies": []any{map[string]any{"PlatformName": "Windows.Desktop"}}}, extra)
}

func skuRecord(children []string, packages []any) map[string]any {
	bundled := []any{}
	for _, child := range children {
		bundled = append(bundled, map[string]any{"BigId": child})
	}
	if packages == nil {
		packages = []any{}
	}
	return map[string]any{"Sku": map[string]any{"Properties": map[string]any{"IsTrial": false, "IsPreOrder": false, "Packages": packages, "BundledSkus": bundled}}}
}

func productRecord(id, kind string, children []string, packages ...any) map[string]any {
	return map[string]any{"ProductId": id, "ProductType": kind, "LocalizedProperties": []any{map[string]any{"ProductTitle": id}},
		"DisplaySkuAvailabilities": []any{skuRecord(children, packages)}}
}

func related(kind, id string) map[string]any {
	return map[string]any{"MarketProperties": []any{map[string]any{"RelatedProducts": []any{map[string]any{"RelationshipType": kind, "RelatedProductId": id}}}}}
}

type recordingFetcher struct {
	mu   sync.Mutex
	urls []string
	body func(ids []string) []byte
}

func (r *recordingFetcher) fetch(ctx context.Context, target string, max int64) (catalog.Response, error) {
	r.mu.Lock()
	r.urls = append(r.urls, target)
	r.mu.Unlock()
	parsed, _ := url.Parse(target)
	return catalog.Response{Status: 200, Body: r.body(strings.Split(parsed.Query().Get("bigIds"), ","))}, nil
}

func (r *recordingFetcher) calls() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.urls)
}

func serving(records ...map[string]any) *recordingFetcher {
	return &recordingFetcher{body: func(ids []string) []byte {
		var matched []any
		for _, record := range records {
			if slices.Contains(ids, record["ProductId"].(string)) {
				matched = append(matched, record)
			}
		}
		body, _ := json.Marshal(map[string]any{"Products": append([]any{}, matched...)})
		return body
	}}
}

func defaultRecords() []map[string]any {
	return []map[string]any{
		merge(productRecord(BASE, "Game", nil, pkgRecord(nil)), related("Bundle", EDITION)),
		productRecord(EDITION, "Game", []string{BASE, PASS}),
		merge(productRecord(PASS, "Durable", []string{DLC}), related("addOnParent", BASE)),
		productRecord(DLC, "Durable", nil, pkgRecord(map[string]any{"PackageFamilyName": "Example.Dlc_hash", "MainPackageFamilyNameForDlc": "Example.Base_hash"})),
		productRecord(FOREIGN, "Durable", nil, pkgRecord(map[string]any{"MainPackageFamilyNameForDlc": "Foreign.Base_hash"})),
	}
}

var base = Base{StoreID: BASE, Name: "Example.Base", Related: []string{}}

func sorted(values []string) []string { return slices.Sorted(slices.Values(values)) }

func TestDiscoverScopedSeasonPass(t *testing.T) {
	fetcher := serving(defaultRecords()...)
	result, err := NewCatalog(fetcher.fetch).Discover(context.Background(), base, Access{PASS: "owned", FOREIGN: "other"}, "DE", "")
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(sorted(result.Related), sorted([]string{PASS, DLC})) {
		t.Errorf("related = %v", result.Related)
	}
	if slices.ContainsFunc(result.Products, func(p Product) bool { return p["ProductId"] == FOREIGN }) {
		t.Error("foreign product returned")
	}
	if len(fetcher.calls()) != 2 {
		t.Errorf("requests = %d", len(fetcher.calls()))
	}
	for _, call := range fetcher.calls() {
		parsed, _ := url.Parse(call)
		if parsed.Query().Get("market") != "DE" {
			t.Error("market not used")
		}
	}
}

func TestDiscoverVerifyFromBundle(t *testing.T) {
	c := NewCatalog(serving(defaultRecords()...).fetch)
	result, _ := c.Discover(context.Background(), base, Access{EDITION: "owned"}, "US", "")
	if !slices.Equal(sorted(result.Verify), sorted([]string{PASS, DLC})) {
		t.Errorf("verify = %v", result.Verify)
	}
	if result, _ := c.Discover(context.Background(), base, Access{}, "US", ""); len(result.Verify) != 0 {
		t.Error("verify without a license")
	}
}

func TestDiscoverOwnedDurableByParent(t *testing.T) {
	owned := merge(productRecord(DLC, "Durable", nil), related("addOnParent", BASE))
	foreign := productRecord(FOREIGN, "Durable", nil)
	foreign["LocalizedProperties"] = []any{map[string]any{"ProductTitle": "Example.Base DLC"}}
	result, _ := NewCatalog(serving(productRecord(BASE, "Game", nil, pkgRecord(nil)), owned, foreign).fetch).
		Discover(context.Background(), base, Access{DLC: "owned", FOREIGN: "owned"}, "US", "")
	if !slices.Equal(result.Related, []string{DLC}) {
		t.Errorf("related = %v", result.Related)
	}
	edition, _ := NewCatalog(serving(productRecord(BASE, "Game", nil, pkgRecord(nil)), productRecord(EDITION, "Game", []string{DLC, FOREIGN}),
		productRecord(DLC, "Durable", nil, pkgRecord(map[string]any{"MainPackageFamilyNameForDlc": "Example.Base_hash"})), productRecord(FOREIGN, "Durable", nil)).fetch).
		Discover(context.Background(), base, Access{}, "US", EDITION)
	if !slices.Equal(edition.Related, []string{DLC}) || !slices.ContainsFunc(edition.Products, func(p Product) bool { return p["ProductId"] == DLC }) {
		t.Errorf("edition hint = %v", edition.Related)
	}
}

func TestDiscoverManyBatches(t *testing.T) {
	records := []map[string]any{productRecord(BASE, "Game", nil, pkgRecord(nil))}
	access := Access{DLC: "owned"}
	for i := range 125 {
		id := fmt.Sprintf("%012d", i)
		records = append(records, productRecord(id, "Game", nil))
		access[id] = "gamepass"
	}
	records = append(records, productRecord(DLC, "Durable", nil, pkgRecord(map[string]any{"MainPackageFamilyNameForDlc": "Example.Base_hash"})))
	result, err := NewCatalog(serving(records...).fetch).Discover(context.Background(), base, access, "US", "")
	if err != nil || !slices.Equal(result.Related, []string{DLC}) {
		t.Errorf("related = %v, %v", result.Related, err)
	}
}

func TestDiscoverCacheAndFailures(t *testing.T) {
	var healthy atomic.Bool
	var calls atomic.Int32
	c := NewCatalog(func(context.Context, string, int64) (catalog.Response, error) {
		calls.Add(1)
		if !healthy.Load() {
			return catalog.Response{}, errors.New("network")
		}
		body, _ := json.Marshal(map[string]any{"Products": []any{productRecord(BASE, "Game", nil)}})
		return catalog.Response{Status: 200, Body: body}, nil
	})
	if _, err := c.Discover(context.Background(), base, Access{}, "US", ""); err == nil {
		t.Error("failure must surface")
	}
	healthy.Store(true)
	c.Discover(context.Background(), base, Access{}, "US", "")
	c.Discover(context.Background(), base, Access{}, "US", "")
	if calls.Load() != 2 {
		t.Errorf("calls = %d", calls.Load())
	}
	c.Discover(context.Background(), base, Access{}, "DE", "")
	if calls.Load() != 3 {
		t.Error("cache must be scoped to market")
	}
	duplicate, _ := json.Marshal(map[string]any{"Products": []any{productRecord(BASE, "Game", nil), productRecord(BASE, "Game", nil)}})
	unrequested, _ := json.Marshal(map[string]any{"Products": []any{productRecord(FOREIGN, "Durable", nil)}})
	for i, response := range []catalog.Response{{Status: 200, Body: unrequested}, {Status: 200, Body: duplicate},
		{Status: 200, Body: []byte(strings.Repeat("x", 8_000_001))}, {Status: 200, Body: []byte{0xff}}, {Status: 503}} {
		fixed := func(context.Context, string, int64) (catalog.Response, error) { return response, nil }
		if _, err := NewCatalog(fixed).Discover(context.Background(), base, Access{}, "US", ""); err == nil {
			t.Errorf("response %d accepted", i)
		}
	}
}

func TestDiscoverBounds(t *testing.T) {
	fetcher := serving(defaultRecords()...)
	if _, err := NewCatalog(fetcher.fetch).Discover(context.Background(), base, Access{"../../private": "owned"}, "US", ""); err == nil || !strings.Contains(err.Error(), "scope") {
		t.Errorf("scope: %v", err)
	}
	if len(fetcher.calls()) != 0 {
		t.Error("invalid scope fetched")
	}
	var nodes []string
	for i := range 257 {
		nodes = append(nodes, fmt.Sprintf("9%011d", i))
	}
	if _, err := NewCatalog(serving(productRecord(BASE, "Game", nodes)).fetch).Discover(context.Background(), base, Access{}, "US", ""); err == nil || !strings.Contains(err.Error(), "large") {
		t.Errorf("graph: %v", err)
	}
	cached := NewCatalog(fetcher.fetch)
	cached.Discover(context.Background(), base, Access{}, "US", "")
	before := len(fetcher.calls())
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := cached.Discover(ctx, base, Access{}, "US", ""); err == nil || len(fetcher.calls()) != before {
		t.Error("cancelled discovery must not use the cache or network")
	}
	var pending atomic.Int32
	hanging := func(ctx context.Context, _ string, _ int64) (catalog.Response, error) {
		pending.Add(1)
		<-ctx.Done()
		return catalog.Response{}, ctx.Err()
	}
	timeout, stop := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer stop()
	if _, err := NewCatalog(hanging).Discover(timeout, base, Access{}, "US", ""); err == nil || pending.Load() != 1 {
		t.Errorf("deadline: %v", err)
	}
}

// --- game content ---

const (
	OWN  = "9BBBBBBBBBBB"
	GP   = "9CCCCCCCCCCC"
	PAID = "9DDDDDDDDDDD"
	GUID = "01234567-89ab-cdef-0123-456789abcdef"
)

func identityXML(name, version, publisher string) string {
	return `<Identity Name="` + name + `" Version="` + version + `" Publisher="` + publisher + `"/>`
}

func config(id, extra string) string {
	return "<Game>" + identityXML("Example.Base", "1.0.0.0", "CN=Example") + "<StoreId>" + id + "</StoreId>" + extra + "</Game>"
}

var baseXML = config(BASE, "<RelatedProducts><RelatedProduct>"+OWN+"</RelatedProduct><RelatedProduct>"+GP+"</RelatedProduct><RelatedProduct>"+PAID+"</RelatedProduct></RelatedProducts>")

func childXML(id string) string {
	return "<Game>" + identityXML("Example.Content", "1.0.0.0", "CN=Example") + "<StoreId>" + id + "</StoreId><AllowedProducts><AllowedProduct>" + BASE +
		`</AllowedProduct></AllowedProducts><DesktopRegistration><MainPackageDependency Name="Example.Base"/></DesktopRegistration></Game>`
}

var appxXML = `<Package xmlns:uap4="example">` + identityXML("Example.Content", "1.0.0.0", "CN=Example") + `<Dependencies><uap4:MainPackageDependency Name="Example.Base"/></Dependencies></Package>`

const layoutXML = `<Package><Chunk><FileGroup SourcePath="." Include="MicrosoftGame.config"/><FileGroup SourcePath="." Include="*.png"/></Chunk></Package>`

func contentPkg(family string) map[string]any {
	return map[string]any{"ContentId": GUID, "PackageFamilyName": family, "MainPackageFamilyNameForDlc": "Example.Base_hash", "PackageFormat": "MSIXVC",
		"Architectures": []any{"x64"}, "PlatformDependencies": []any{map[string]any{"PlatformName": "Windows.Desktop"}}, "MaxDownloadSizeInBytes": 1024.0}
}

func contentProduct(id, kind string) map[string]any {
	return map[string]any{"ProductId": id, "ProductType": kind, "LocalizedProperties": []any{map[string]any{"ProductTitle": "Sample " + id}},
		"DisplaySkuAvailabilities": []any{map[string]any{"Sku": map[string]any{"Properties": map[string]any{"IsTrial": false, "Packages": []any{contentPkg("Example.Content_hash")}}}}}}
}

func catalogData() map[string]any {
	basePkg := contentPkg("Example.Base_hash")
	basePkg["MainPackageFamilyNameForDlc"] = ""
	baseProduct := contentProduct(BASE, "Game")
	baseProduct["DisplaySkuAvailabilities"] = []any{map[string]any{"Sku": map[string]any{"Properties": map[string]any{"IsTrial": false, "Packages": []any{basePkg}}}}}
	return map[string]any{"Products": []any{baseProduct, contentProduct(OWN, "Durable"), contentProduct(GP, "Durable"), contentProduct(PAID, "Durable")}}
}

func properties(data map[string]any, index int) map[string]any {
	product := data["Products"].([]any)[index].(map[string]any)
	return product["DisplaySkuAvailabilities"].([]any)[0].(map[string]any)["Sku"].(map[string]any)["Properties"].(map[string]any)
}

func readAccess() (string, Access, error) {
	return shared.JSISOString(time.Now()), Access{OWN: "owned", GP: "gamepass"}, nil
}

type gameFixture struct {
	root, base, data string
	game             shared.Game
	fetcher          *recordingFetcher
	manager          *Manager
}

func newGameFixture(t *testing.T) *gameFixture {
	root, _ := filepath.EvalSymlinks(t.TempDir())
	f := &gameFixture{root: root, base: filepath.Join(root, "base"), data: filepath.Join(root, "data")}
	os.Mkdir(f.base, 0o755)
	os.Mkdir(f.data, 0o755)
	os.WriteFile(filepath.Join(f.base, "MicrosoftGame.Config"), []byte(baseXML), 0o644)
	f.game = shared.Game{ID: "arbitrary-game", Title: "Sample", Path: f.base, StoreID: BASE, InstallState: "ready"}
	f.fetcher = &recordingFetcher{body: func(ids []string) []byte {
		var matched []any
		for _, product := range catalogData()["Products"].([]any) {
			if slices.Contains(ids, product.(map[string]any)["ProductId"].(string)) {
				matched = append(matched, product)
			}
		}
		body, _ := json.Marshal(map[string]any{"Products": append([]any{}, matched...)})
		return body
	}}
	f.manager = NewManager(f.data, f.fetcher.fetch)
	return f
}

func (f *gameFixture) install(t *testing.T) (Package, string) {
	_, packages, err := f.manager.Plan(context.Background(), f.game, readAccess, "CZ")
	if err != nil {
		t.Fatal(err)
	}
	index := slices.IndexFunc(packages, func(p Package) bool { return p.ProductID == OWN })
	selected := packages[index]
	directory, err := f.manager.Prepare(context.Background(), f.game, selected, "US")
	if err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(directory, "MicrosoftGame.Config"), []byte(childXML(OWN)), 0o644)
	os.WriteFile(filepath.Join(directory, "appxmanifest.xml"), []byte(appxXML), 0o644)
	os.WriteFile(filepath.Join(directory, "layout_"+GUID+".xml"), []byte(layoutXML), 0o644)
	os.WriteFile(filepath.Join(directory, "logo.PNG"), []byte("synthetic payload"), 0o644)
	return selected, directory
}

func mustManifest(t *testing.T, xml string) Manifest {
	m, err := ParseManifest(xml)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func TestRelatedProducts(t *testing.T) {
	f := newGameFixture(t)
	ids, err := f.manager.RelatedProducts(context.Background(), f.game, Access{}, "CZ")
	if err != nil || !slices.Equal(sorted(ids), sorted([]string{OWN, GP, PAID})) {
		t.Errorf("related = %v, %v", ids, err)
	}
	if data, _ := os.ReadFile(filepath.Join(f.base, "MicrosoftGame.Config")); string(data) != baseXML {
		t.Error("base manifest changed")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := f.manager.RelatedProducts(ctx, f.game, Access{}, "CZ"); err == nil {
		t.Error("cancelled discovery")
	}
}

func TestContentOffers(t *testing.T) {
	m := mustManifest(t, baseXML)
	items, packages, _ := ContentOffers(m, catalogData(), Access{OWN: "owned", GP: "gamepass"}, nil)
	var got []string
	for _, item := range items {
		got = append(got, item.ProductID+"/"+item.Access+"/"+item.Status)
	}
	if !slices.Equal(got, []string{OWN + "/owned/available", GP + "/gamepass/available", PAID + "//unlicensed"}) || len(packages) != 2 {
		t.Errorf("items = %v packages = %d", got, len(packages))
	}
	if _, packages, _ := ContentOffers(m, catalogData(), Access{}, nil); len(packages) != 0 {
		t.Error("no license, no package")
	}
	items, packages, _ = ContentOffers(m, catalogData(), Access{}, []string{OWN})
	if items[0].Status != "verify" || items[0].Access != "" || len(packages) != 1 || items[1].Status != "unlicensed" {
		t.Errorf("verify = %+v", items)
	}
	for _, change := range []map[string]any{{"PackageFormat": "APPX"}, {"Architectures": []any{"arm64"}}, {"PlatformDependencies": []any{}},
		{"MainPackageFamilyNameForDlc": "Other.Base_hash"}, {"ContentId": "https://evil.invalid/download"}} {
		data := catalogData()
		pkg := properties(data, 1)["Packages"].([]any)[0].(map[string]any)
		for k, v := range change {
			pkg[k] = v
		}
		if items, _, _ := ContentOffers(m, data, Access{OWN: "owned"}, nil); items[0].Status != "unsupported" {
			t.Errorf("%v: %s", change, items[0].Status)
		}
	}
	data := catalogData()
	properties(data, 1)["IsTrial"] = true
	if _, packages, _ := ContentOffers(m, data, Access{OWN: "owned"}, nil); len(packages) != 0 {
		t.Error("trial SKU")
	}
	properties(data, 1)["IsTrial"] = false
	extra := contentPkg("Example.Content_hash")
	extra["ContentId"] = "aaaaaaaa-89ab-cdef-0123-456789abcdef"
	properties(data, 1)["Packages"] = append(properties(data, 1)["Packages"].([]any), extra)
	if _, packages, _ := ContentOffers(m, data, Access{OWN: "owned"}, nil); len(packages) != 0 {
		t.Error("ambiguous packages")
	}
	preorder := catalogData()
	properties(preorder, 1)["IsPreOrder"] = true
	if _, packages, _ := ContentOffers(m, preorder, Access{OWN: "owned"}, nil); len(packages) != 0 {
		t.Error("preorder SKU")
	}

	licenseOnly := catalogData()
	properties(licenseOnly, 1)["Packages"] = []any{}
	if items, _, _ := ContentOffers(m, licenseOnly, Access{OWN: "other"}, nil); items[0].Status != "license-only" || items[0].Access != "other" {
		t.Errorf("license-only = %+v", items[0])
	}
	properties(licenseOnly, 1)["BundledSkus"] = []any{map[string]any{"BigId": GP}}
	if items, _, _ := ContentOffers(m, licenseOnly, Access{OWN: "owned"}, nil); items[0].Status != "bundle" {
		t.Error("bundle")
	}
	consoleOnly := catalogData()
	properties(consoleOnly, 1)["Packages"].([]any)[0].(map[string]any)["PlatformDependencies"] = []any{}
	if items, _, _ := ContentOffers(m, consoleOnly, Access{OWN: "owned"}, nil); items[0].Status != "unsupported" {
		t.Error("console only")
	}
}

func TestManifestValidation(t *testing.T) {
	for _, xml := range []string{config(BASE, "<RelatedProducts><RelatedProduct>../../escape</RelatedProduct></RelatedProducts>"),
		strings.Replace(baseXML, "</Game>", "<StoreId>"+GP+"</StoreId></Game>", 1), strings.Replace(baseXML, "1.0.0.0", "70000.0.0.0", 1)} {
		if _, err := ParseManifest(xml); err == nil {
			t.Errorf("accepted %s", xml)
		}
	}
	f := newGameFixture(t)
	other := f.game
	other.StoreID = PAID
	if _, _, err := f.manager.Plan(context.Background(), other, readAccess, "US"); err == nil || !strings.Contains(err.Error(), "Store ID changed") {
		t.Errorf("store change: %v", err)
	}
}

func TestPlanQueriesOnlyDeclaredProducts(t *testing.T) {
	f := newGameFixture(t)
	plan, _, err := f.manager.Plan(context.Background(), f.game, readAccess, "CZ")
	if err != nil || len(plan.Items) != 3 {
		t.Fatalf("plan = %+v, %v", plan, err)
	}
	parsed, _ := url.Parse(f.fetcher.calls()[0])
	if parsed.Hostname() != "displaycatalog.mp.microsoft.com" || parsed.Query().Get("market") != "CZ" ||
		parsed.Query().Get("bigIds") != strings.Join([]string{BASE, OWN, GP, PAID}, ",") {
		t.Errorf("url = %s", parsed)
	}
	for i, response := range []catalog.Response{{Status: 503}, {Status: 200, Body: []byte(strings.Repeat("x", 8_000_001))},
		{Status: 200, Body: func() []byte {
			b, _ := json.Marshal(map[string]any{"Products": []any{contentProduct("9XXXXXXXXXXX", "Durable")}})
			return b
		}()}} {
		manager := NewManager(f.data, func(context.Context, string, int64) (catalog.Response, error) { return response, nil })
		if _, _, err := manager.Plan(context.Background(), f.game, readAccess, "US"); err == nil {
			t.Errorf("response %d accepted", i)
		}
	}
	os.WriteFile(filepath.Join(f.base, "MicrosoftGame.Config"), []byte(config(BASE, "")), 0o644)
	plain := newGameFixture(t)
	os.WriteFile(filepath.Join(plain.base, "MicrosoftGame.Config"), []byte(config(BASE, "")), 0o644)
	plan, _, _ = plain.manager.Plan(context.Background(), plain.game, readAccess, "US")
	if len(plan.Items) != 2 || len(plain.fetcher.calls()) != 1 {
		t.Errorf("owned discovery without manifest list = %d items, %d calls", len(plan.Items), len(plain.fetcher.calls()))
	}
}

func TestCompletedContentRoots(t *testing.T) {
	f := newGameFixture(t)
	selected, directory := f.install(t)
	if strings.HasPrefix(directory, f.base) {
		t.Error("content inside the base game")
	}
	if roots, _ := f.manager.Roots(f.game); len(roots) != 0 {
		t.Error("partial content registered")
	}
	if err := f.manager.Complete(f.game, selected); err != nil {
		t.Fatal(err)
	}
	if roots, _ := f.manager.Roots(f.game); !slices.Equal(roots, []string{directory}) {
		t.Errorf("roots = %v", roots)
	}
	plan, _, _ := f.manager.Plan(context.Background(), f.game, readAccess, "US")
	if plan.Items[0].Status != "installed" {
		t.Error("installed status")
	}
	unlicensed, _, _ := f.manager.Plan(context.Background(), f.game, func() (string, Access, error) { return "", Access{}, nil }, "US")
	if unlicensed.Items[0].Status != "installed" || unlicensed.Items[0].Access != "" {
		t.Errorf("unlicensed installed = %+v", unlicensed.Items[0])
	}
	offline := NewManager(f.data, func(context.Context, string, int64) (catalog.Response, error) {
		return catalog.Response{}, errors.New("No launch-time network")
	})
	if roots, _ := offline.Roots(f.game); !slices.Equal(roots, []string{directory}) {
		t.Error("roots must not need the network")
	}
	os.WriteFile(filepath.Join(directory, "appxmanifest.xml"), []byte(strings.Replace(appxXML, "1.0.0.0", "2.0.0.0", 1)), 0o644)
	if roots, _ := f.manager.Roots(f.game); len(roots) != 0 {
		t.Error("changed content kept")
	}
	os.WriteFile(filepath.Join(directory, "appxmanifest.xml"), []byte(appxXML), 0o644)
	f.manager.Prepare(context.Background(), f.game, selected, "US")
	if roots, _ := f.manager.Roots(f.game); len(roots) != 0 {
		t.Error("prepare must reset completion")
	}
}

func TestCompleteRejectsMismatches(t *testing.T) {
	for _, failure := range []string{"publisher", "parent", "store", "appx", "payload", "layout"} {
		f := newGameFixture(t)
		selected, directory := f.install(t)
		file := func(name string) string { return filepath.Join(directory, name) }
		switch failure {
		case "publisher":
			os.WriteFile(file("MicrosoftGame.Config"), []byte(strings.Replace(childXML(OWN), "CN=Example", "CN=Other", 1)), 0o644)
		case "parent":
			os.WriteFile(file("MicrosoftGame.Config"), []byte(strings.Replace(childXML(OWN), "<AllowedProduct>"+BASE, "<AllowedProduct>"+PAID, 1)), 0o644)
		case "store":
			os.WriteFile(file("MicrosoftGame.Config"), []byte(childXML(PAID)), 0o644)
		case "appx":
			os.WriteFile(file("appxmanifest.xml"), []byte(strings.Replace(appxXML, "1.0.0.0", "2.0.0.0", 1)), 0o644)
		case "payload":
			os.Remove(file("logo.PNG"))
		case "layout":
			os.WriteFile(file("layout_"+GUID+".xml"), []byte(strings.Replace(layoutXML, `SourcePath="."`, `SourcePath="../outside"`, 1)), 0o644)
		}
		if err := f.manager.Complete(f.game, selected); err == nil {
			t.Errorf("%s accepted", failure)
		}
		if roots, _ := f.manager.Roots(f.game); len(roots) != 0 {
			t.Errorf("%s registered", failure)
		}
	}
}

func TestPrepareRejections(t *testing.T) {
	f := newGameFixture(t)
	_, packages, _ := f.manager.Plan(context.Background(), f.game, readAccess, "US")
	selected := packages[0]
	for _, change := range []func(*Package){func(p *Package) { p.ContentID = "aaaaaaaa-89ab-cdef-0123-456789abcdef" }, func(p *Package) { p.Family = "Foreign.Content_hash" },
		func(p *Package) { p.ProductID = "9XXXXXXXXXXX" }} {
		forged := selected
		change(&forged)
		if _, err := f.manager.Prepare(context.Background(), f.game, forged, "US"); err == nil || !strings.Contains(err.Error(), "Invalid content") {
			t.Errorf("forged: %v", err)
		}
	}
	inside := filepath.Join(f.base, "data")
	os.Mkdir(inside, 0o755)
	if _, err := NewManager(inside, f.fetcher.fetch).Prepare(context.Background(), f.game, selected, "US"); err == nil || !strings.Contains(err.Error(), "overlaps") {
		t.Errorf("inside base: %v", err)
	}
	if entries, _ := os.ReadDir(inside); len(entries) != 0 {
		t.Error("created content inside the base game")
	}
	linked := newGameFixture(t)
	os.Symlink(linked.base, filepath.Join(linked.data, "content"))
	if _, err := linked.manager.Prepare(context.Background(), linked.game, Package{ProductID: OWN, ContentID: GUID, Family: "Example.Content_hash"}, "US"); err == nil || !strings.Contains(err.Error(), "real directory") {
		t.Errorf("symlinked storage: %v", err)
	}
}
