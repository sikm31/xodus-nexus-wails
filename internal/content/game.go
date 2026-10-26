package content

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"xodus-nexus-wails/internal/catalog"
	"xodus-nexus-wails/internal/install"
	"xodus-nexus-wails/internal/manifest"
	"xodus-nexus-wails/internal/shared"
)

var (
	contentIDPattern = regexp.MustCompile(`(?i)^[0-9a-f]{8}(?:-[0-9a-f]{4}){3}-[0-9a-f]{12}$`)
	versionPattern   = regexp.MustCompile(`^\d+\.\d+\.\d+\.\d+$`)
	familyChars      = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)
	layoutName       = regexp.MustCompile(`(?i)^layout_[0-9a-f]{8}(?:-[0-9a-f]{4}){3}-[0-9a-f]{12}\.xml$`)
	unsafeLayoutPath = regexp.MustCompile(`[:\x00-\x1f]`)
	pathSplit        = regexp.MustCompile(`[/\\]`)
	unsafeRootPath   = regexp.MustCompile(`[;\r\n]`)
)

const markerName = ".nexus-content-complete.json"

type Manifest struct {
	StoreID, Name, Publisher, Version string
	Related, Allowed                  []string
	Dependency                        string
}

// Package is a verified content package selected for installation.
type Package struct {
	ProductID     string   `json:"productId"`
	ContentID     string   `json:"contentId"`
	Family        string   `json:"family"`
	DownloadBytes *float64 `json:"downloadBytes,omitempty"`
}

type Access = map[string]shared.LibraryAccess

func boundedFile(file string) (string, error) {
	handle, err := os.OpenFile(file, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return "", err
	}
	defer handle.Close()
	info, err := handle.Stat()
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() || info.Size() > 1_048_576 {
		return "", errors.New("Package metadata is too large or not a file.")
	}
	data, err := io.ReadAll(io.LimitReader(handle, info.Size()+1))
	if err != nil {
		return "", err
	}
	if int64(len(data)) > info.Size() {
		return "", errors.New("Package metadata changed while reading.")
	}
	return strings.ToValidUTF8(string(data), "�"), nil
}

func manifestFile(directory, filename string) (string, error) {
	entries, err := os.ReadDir(directory)
	if err != nil {
		return "", err
	}
	var matches []string
	for _, entry := range entries {
		if strings.EqualFold(entry.Name(), filename) {
			matches = append(matches, entry.Name())
		}
	}
	if len(matches) != 1 {
		return "", fmt.Errorf("Missing or ambiguous %s.", filename)
	}
	return boundedFile(filepath.Join(directory, matches[0]))
}

func ParseManifest(xml string) (Manifest, error) {
	root, err := manifest.Parse(xml)
	if err != nil {
		return Manifest{}, err
	}
	if root.LocalName() != "Game" {
		return Manifest{}, errors.New("Expected a Microsoft Game manifest.")
	}
	identity, err := manifest.Single(root, "Identity")
	if err != nil {
		return Manifest{}, err
	}
	store, err := manifest.Single(root, "StoreId")
	if err != nil {
		return Manifest{}, err
	}
	id := strings.TrimSpace(store.Text)
	version := identity.Attr("Version")
	invalidVersion := !versionPattern.MatchString(version) || slices.ContainsFunc(strings.Split(version, "."), func(part string) bool {
		value, _ := strconv.ParseFloat(part, 64)
		return value > 65535
	})
	if !validIDPattern.MatchString(id) || identity.Attr("Name") == "" || identity.Attr("Publisher") == "" || invalidVersion {
		return Manifest{}, errors.New("Invalid package identity.")
	}
	ids := func(section, tag string) ([]string, error) {
		sections := manifest.ChildrenNamed(root, section)
		if len(sections) > 1 {
			return nil, errors.New("Ambiguous product relationships.")
		}
		var result []string
		for _, node := range sections {
			for _, row := range manifest.ChildrenNamed(node, tag) {
				result = append(result, strings.TrimSpace(row.Text))
			}
		}
		if len(result) > 128 || slices.ContainsFunc(result, func(id string) bool { return !validIDPattern.MatchString(id) }) {
			return nil, errors.New("Invalid related product list.")
		}
		unique := uniqueStrings(result)
		if unique == nil {
			unique = []string{}
		}
		return unique, nil
	}
	registration := manifest.ChildrenNamed(root, "DesktopRegistration")
	if len(registration) > 1 {
		return Manifest{}, errors.New("Ambiguous desktop registration.")
	}
	var dependencies []*manifest.Node
	for _, node := range registration {
		dependencies = append(dependencies, manifest.ChildrenNamed(node, "MainPackageDependency")...)
	}
	if len(dependencies) > 1 {
		return Manifest{}, errors.New("Ambiguous main package dependency.")
	}
	related, err := ids("RelatedProducts", "RelatedProduct")
	if err != nil {
		return Manifest{}, err
	}
	allowed, err := ids("AllowedProducts", "AllowedProduct")
	if err != nil {
		return Manifest{}, err
	}
	result := Manifest{StoreID: id, Name: identity.Attr("Name"), Publisher: identity.Attr("Publisher"), Version: version, Related: related, Allowed: allowed}
	if len(dependencies) == 1 {
		result.Dependency = dependencies[0].Attr("Name")
	}
	return result, nil
}

func baseManifest(game shared.Game) (Manifest, error) {
	xml, err := manifestFile(game.Path, "MicrosoftGame.config")
	if err != nil {
		return Manifest{}, err
	}
	base, err := ParseManifest(xml)
	if err != nil {
		return Manifest{}, err
	}
	if base.StoreID != game.StoreID {
		return Manifest{}, errors.New("The installed game Store ID changed. Reimport the game.")
	}
	return base, nil
}

func packageChoice(base Manifest, baseProduct, product Product) *Package {
	families := map[string]bool{}
	if baseProduct != nil {
		for _, pkg := range DesktopContentPackages(baseProduct) {
			if family, ok := pkg["PackageFamilyName"].(string); ok && strings.HasPrefix(family, base.Name+"_") {
				families[family] = true
			}
		}
	}
	kind, _ := product["ProductType"].(string)
	if product == nil || (kind != "Durable" && kind != "GameAddOn") {
		return nil
	}
	var candidates []map[string]any
	for _, pkg := range DesktopContentPackages(product) {
		main, _ := pkg["MainPackageFamilyNameForDlc"].(string)
		contentID, _ := pkg["ContentId"].(string)
		family, ok := pkg["PackageFamilyName"].(string)
		if families[main] && contentIDPattern.MatchString(contentID) && ok && familyChars.MatchString(family) {
			candidates = append(candidates, pkg)
		}
	}
	unique := map[string]map[string]any{}
	familySet := map[string]bool{}
	var last map[string]any
	for _, pkg := range candidates {
		unique[strings.ToLower(pkg["ContentId"].(string))] = pkg
		familySet[pkg["PackageFamilyName"].(string)] = true
	}
	if len(unique) != 1 || len(familySet) != 1 {
		return nil
	}
	for _, pkg := range unique {
		last = pkg
	}
	result := &Package{ProductID: product["ProductId"].(string), ContentID: strings.ToLower(last["ContentId"].(string)), Family: last["PackageFamilyName"].(string)}
	if size, ok := last["MaxDownloadSizeInBytes"].(float64); ok && size > 0 && size == math.Trunc(size) && size <= 1<<53-1 {
		result.DownloadBytes = &size
	}
	return result
}

func productTitle(product Product, id string) string {
	for _, row := range list(product["LocalizedProperties"]) {
		if title, ok := object(row)["ProductTitle"].(string); ok {
			if title = shared.TruncateUTF16(title, 300); title != "" {
				return title
			}
			return id
		}
	}
	return id
}

// ContentOffers classifies related content without inferring licenses from
// catalog membership, a bundle title or disk.
func ContentOffers(base Manifest, catalogData map[string]any, access Access, verify []string) ([]shared.GameContentItem, []Package, error) {
	products, ok := catalogData["Products"].([]any)
	if !ok || len(products) > 256 {
		return nil, nil, errors.New("Invalid content catalog.")
	}
	byID := map[string]Product{}
	for _, entry := range products {
		product := object(entry)
		id, _ := product["ProductId"].(string)
		if product == nil || !validIDPattern.MatchString(id) || byID[id] != nil {
			return nil, nil, errors.New("Ambiguous content catalog.")
		}
		byID[id] = product
	}
	baseProduct := byID[base.StoreID]
	selected := []Package{}
	items := []shared.GameContentItem{}
	for _, id := range base.Related {
		if id == base.StoreID {
			continue
		}
		product := byID[id]
		title := productTitle(product, id)
		licensed := access[id]
		if licensed == "" {
			if slices.Contains(verify, id) {
				if pkg := packageChoice(base, baseProduct, product); pkg != nil {
					selected = append(selected, *pkg)
					items = append(items, shared.GameContentItem{ProductID: id, Title: title, Status: "verify", DownloadBytes: pkg.DownloadBytes,
						Note: "Included in an active offer. Xodus must verify this add-on license before downloading."})
					continue
				}
			}
			items = append(items, shared.GameContentItem{ProductID: id, Title: title, Status: "unlicensed", Note: "No active account license was verified."})
			continue
		}
		skus := ContentSkus(product)
		durable := product["ProductType"] == "Durable"
		if durable && slices.ContainsFunc(skus, func(props map[string]any) bool { return len(list(props["BundledSkus"])) > 0 }) {
			items = append(items, shared.GameContentItem{ProductID: id, Title: title, Access: licensed, Status: "bundle", Note: "Content bundle. Its related add-ons are listed separately."})
			continue
		}
		licenseOnly := len(skus) > 0 && !slices.ContainsFunc(skus, func(props map[string]any) bool { return !declaresNoPackages(props) })
		if durable && licenseOnly {
			items = append(items, shared.GameContentItem{ProductID: id, Title: title, Access: licensed, Status: "license-only",
				Note: "No download required. The game must verify this license through the runtime."})
			continue
		}
		pkg := packageChoice(base, baseProduct, product)
		if pkg == nil {
			items = append(items, shared.GameContentItem{ProductID: id, Title: title, Access: licensed, Status: "unsupported", Note: "No unambiguous related PC MSIXVC content package was verified."})
			continue
		}
		selected = append(selected, *pkg)
		items = append(items, shared.GameContentItem{ProductID: id, Title: title, Access: licensed, Status: "available", DownloadBytes: pkg.DownloadBytes,
			Note: "Install-time licensing is checked again by Xodus."})
	}
	return items, selected, nil
}

// declaresNoPackages reports a SKU that explicitly declares no download.
func declaresNoPackages(props map[string]any) bool {
	declared, ok := props["PackagesDeclared"].(bool)
	if !ok {
		_, declared = props["Packages"]
	}
	packages := props["Packages"]
	items, isList := packages.([]any)
	return declared && (packages == nil || (isList && len(items) == 0))
}

type Manager struct {
	dataRoot string
	catalog  *Catalog
}

func NewManager(dataRoot string, fetch catalog.Fetcher) *Manager {
	return &Manager{dataRoot: dataRoot, catalog: NewCatalog(fetch)}
}

func toBase(m Manifest) Base { return Base{StoreID: m.StoreID, Name: m.Name, Related: m.Related} }

// RelatedProducts returns public discovery hints only. It never reads
// credentials or grants runtime rights.
func (m *Manager) RelatedProducts(ctx context.Context, game shared.Game, access Access, market string) ([]string, error) {
	base, err := baseManifest(game)
	if err != nil {
		return nil, err
	}
	discovery, err := m.catalog.Discover(ctx, toBase(base), access, market, game.OfferProductID)
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	installed, err := m.installedIDs(game)
	if err != nil {
		return nil, err
	}
	related := slices.DeleteFunc(uniqueStrings(append(discovery.Related, installed...)), func(id string) bool { return id == base.StoreID })
	if len(related) > 256 || slices.ContainsFunc(related, func(id string) bool { return !validIDPattern.MatchString(id) }) {
		return nil, errors.New("Content discovery scope is too large.")
	}
	if related == nil {
		related = []string{}
	}
	return related, nil
}

func (m *Manager) directory(game shared.Game, id string) (string, error) {
	if id != "" && !validIDPattern.MatchString(id) {
		return "", errors.New("Invalid content product ID.")
	}
	absolute, _ := filepath.Abs(game.Path)
	sum := sha256.Sum256([]byte(game.ID + "\x00" + absolute))
	parts := []string{m.dataRoot, "content", hex.EncodeToString(sum[:])[:32]}
	if id != "" {
		parts = append(parts, id)
	}
	return filepath.Join(parts...), nil
}

// safeDirectory never lets managed content write into the base game. It
// rejects symlink redirection at every managed component, including persisted
// queue destinations.
func (m *Manager) safeDirectory(game shared.Game, id string, create bool) (string, error) {
	target, err := m.directory(game, id)
	if err != nil {
		return "", err
	}
	relative, err := filepath.Rel(m.dataRoot, target)
	if err != nil {
		return "", err
	}
	current, err := filepath.EvalSymlinks(m.dataRoot)
	if err != nil {
		return "", err
	}
	for _, name := range strings.Split(relative, string(filepath.Separator)) {
		current = filepath.Join(current, name)
		if create {
			if err := os.Mkdir(current, 0o700); err != nil && !errors.Is(err, fs.ErrExist) {
				return "", err
			}
		}
		info, err := os.Lstat(current)
		if err != nil {
			return "", err
		}
		if !info.IsDir() || info.Mode()&fs.ModeSymlink != 0 {
			return "", errors.New("Managed content path is not a real directory.")
		}
	}
	if unsafeRootPath.MatchString(current) {
		return "", errors.New("Content paths cannot contain semicolons or line breaks.")
	}
	return current, nil
}

func validLayoutPath(value string) bool {
	return value != "" && !strings.HasPrefix(value, "/") && !strings.HasPrefix(value, `\`) && !unsafeLayoutPath.MatchString(value) &&
		!slices.Contains(pathSplit.Split(value, -1), "..")
}

func (m *Manager) validate(game shared.Game, id, directory, family string) (Manifest, error) {
	base, err := baseManifest(game)
	if err != nil {
		return Manifest{}, err
	}
	contentXML, err := manifestFile(directory, "MicrosoftGame.config")
	if err != nil {
		return Manifest{}, err
	}
	content, err := ParseManifest(contentXML)
	if err != nil {
		return Manifest{}, err
	}
	appxXML, err := manifestFile(directory, "appxmanifest.xml")
	if err != nil {
		return Manifest{}, err
	}
	appx, err := manifest.Parse(appxXML)
	if err != nil {
		return Manifest{}, err
	}
	identity, err := manifest.Single(appx, "Identity")
	if err != nil {
		return Manifest{}, err
	}
	dependencies, err := manifest.Single(appx, "Dependencies")
	if err != nil {
		return Manifest{}, err
	}
	dependency, err := manifest.Single(dependencies, "MainPackageDependency")
	if err != nil {
		return Manifest{}, err
	}
	if appx.LocalName() != "Package" || content.StoreID != id || content.Publisher != base.Publisher || content.Dependency != base.Name ||
		!slices.Contains(content.Allowed, base.StoreID) || identity.Attr("Name") != content.Name || identity.Attr("Publisher") != content.Publisher ||
		identity.Attr("Version") != content.Version || dependency.Attr("Name") != base.Name || (family != "" && !strings.HasPrefix(family, content.Name+"_")) {
		return Manifest{}, errors.New("Downloaded content does not match the game/package identity.")
	}
	entries, err := os.ReadDir(directory)
	if err != nil {
		return Manifest{}, err
	}
	var layouts []string
	for _, entry := range entries {
		if layoutName.MatchString(entry.Name()) {
			layouts = append(layouts, entry.Name())
		}
	}
	if len(layouts) != 1 {
		return Manifest{}, errors.New("Missing or ambiguous content layout.")
	}
	layoutXML, err := boundedFile(filepath.Join(directory, layouts[0]))
	if err != nil {
		return Manifest{}, err
	}
	layout, err := manifest.Parse(layoutXML)
	if err != nil {
		return Manifest{}, err
	}
	if layout.Name != "Package" {
		return Manifest{}, errors.New("Unsupported content layout.")
	}
	var groups []*manifest.Node
	var visit func(*manifest.Node, int) error
	visit = func(node *manifest.Node, depth int) error {
		if depth > 16 || len(groups) > 256 {
			return errors.New("Content layout limit exceeded.")
		}
		if node.Name == "FileGroup" {
			groups = append(groups, node)
		}
		for _, child := range node.Children {
			if err := visit(child, depth+1); err != nil {
				return err
			}
		}
		return nil
	}
	if err := visit(layout, 0); err != nil {
		return Manifest{}, err
	}
	if len(groups) == 0 || len(groups) > 256 {
		return Manifest{}, errors.New("Content layout contains no supported file groups.")
	}
	for _, group := range groups {
		source, include := group.Attr("SourcePath"), group.Attr("Include")
		if !validLayoutPath(source) || !validLayoutPath(include) || strings.ContainsAny(source, "*?") {
			return Manifest{}, errors.New("Unsupported content layout path.")
		}
		var parts []string
		for _, part := range append(pathSplit.Split(source, -1), pathSplit.Split(include, -1)...) {
			if part != "" && part != "." {
				parts = append(parts, part)
			}
		}
		if len(parts) == 0 {
			return Manifest{}, errors.New("Unsupported content layout wildcard.")
		}
		pattern := parts[len(parts)-1]
		parts = parts[:len(parts)-1]
		if slices.ContainsFunc(parts, func(part string) bool { return strings.ContainsAny(part, "*?") }) {
			return Manifest{}, errors.New("Unsupported content layout wildcard.")
		}
		folder := directory
		for _, part := range parts {
			entries, err := os.ReadDir(folder)
			if err != nil {
				return Manifest{}, err
			}
			var matches []string
			for _, entry := range entries {
				if strings.EqualFold(entry.Name(), part) {
					matches = append(matches, entry.Name())
				}
			}
			if len(matches) != 1 {
				return Manifest{}, errors.New("Missing or ambiguous content payload directory.")
			}
			folder = filepath.Join(folder, matches[0])
			if info, err := os.Lstat(folder); err != nil || !info.IsDir() {
				return Manifest{}, errors.New("Invalid content payload directory.")
			}
		}
		expression := "(?i)^" + strings.NewReplacer(`\*`, ".*", `\?`, ".").Replace(regexp.QuoteMeta(pattern)) + "$"
		match, err := regexp.Compile(expression)
		if err != nil {
			return Manifest{}, err
		}
		names, err := os.ReadDir(folder)
		if err != nil {
			return Manifest{}, err
		}
		if len(names) > 4096 {
			return Manifest{}, errors.New("Content payload directory limit exceeded.")
		}
		found := slices.ContainsFunc(names, func(entry os.DirEntry) bool {
			if !match.MatchString(entry.Name()) {
				return false
			}
			info, err := os.Lstat(filepath.Join(folder, entry.Name()))
			return err == nil && info.Mode().IsRegular()
		})
		if !found {
			return Manifest{}, errors.New("A file group declared by the content layout is missing.")
		}
	}
	return content, nil
}

type AccessReader func() (checkedAt string, access Access, err error)

func (m *Manager) Plan(ctx context.Context, game shared.Game, readAccess AccessReader, market string) (shared.GameContentPlan, []Package, error) {
	base, err := baseManifest(game)
	if err != nil {
		return shared.GameContentPlan{}, nil, err
	}
	checkedAt, access, err := readAccess()
	if err != nil {
		return shared.GameContentPlan{}, nil, err
	}
	discovery, err := m.catalog.Discover(ctx, toBase(base), access, market, game.OfferProductID)
	if err != nil {
		return shared.GameContentPlan{}, nil, err
	}
	scoped := base
	scoped.Related = discovery.Related
	products := make([]any, len(discovery.Products))
	for i, product := range discovery.Products {
		products[i] = product
	}
	items, packages, err := ContentOffers(scoped, map[string]any{"Products": products}, access, discovery.Verify)
	if err != nil {
		return shared.GameContentPlan{}, nil, err
	}
	installed, err := m.installedIDs(game)
	if err != nil {
		return shared.GameContentPlan{}, nil, err
	}
	for i := range items {
		if !slices.Contains(installed, items[i].ProductID) {
			continue
		}
		if items[i].Status == "verify" {
			items[i].Note = "Installed files retained. Xodus must verify this add-on license before repair."
			continue
		}
		items[i].Status = "installed"
		if items[i].Access != "" {
			items[i].Note = "Installed. The runtime verifies the license when requested."
		} else {
			items[i].Note = "Installed files retained, but no active account license was verified."
		}
	}
	return shared.GameContentPlan{GameID: game.ID, CheckedAt: checkedAt, Items: items}, packages, nil
}

// Prepare verifies pkg against public metadata and returns its empty-marker content directory.
func (m *Manager) Prepare(ctx context.Context, game shared.Game, pkg Package, market string) (string, error) {
	base, err := baseManifest(game)
	if err != nil {
		return "", err
	}
	invalid := errors.New("Invalid content installation.")
	if !validIDPattern.MatchString(pkg.ProductID) || !contentIDPattern.MatchString(pkg.ContentID) {
		return "", invalid
	}
	discovery, err := m.catalog.Discover(ctx, toBase(base), Access{}, market, pkg.ProductID)
	if err != nil {
		return "", err
	}
	var expected *Package
	if slices.Contains(discovery.Related, pkg.ProductID) {
		var baseProduct, product Product
		for _, candidate := range discovery.Products {
			switch candidate["ProductId"] {
			case base.StoreID:
				baseProduct = candidate
			case pkg.ProductID:
				product = candidate
			}
		}
		expected = packageChoice(base, baseProduct, product)
	}
	if expected == nil || expected.ContentID != pkg.ContentID || expected.Family != pkg.Family {
		return "", invalid
	}
	root, err := filepath.EvalSymlinks(m.dataRoot)
	if err != nil {
		return "", err
	}
	gameRoot, err := filepath.EvalSymlinks(game.Path)
	if err != nil {
		return "", err
	}
	directory, _ := m.directory(game, pkg.ProductID)
	relative, _ := filepath.Rel(m.dataRoot, directory)
	target := filepath.Join(root, relative)
	separator := string(filepath.Separator)
	if target == gameRoot || strings.HasPrefix(target, gameRoot+separator) || strings.HasPrefix(gameRoot, target+separator) {
		return "", errors.New("Content storage overlaps the base game. Choose a separate runtime storage folder.")
	}
	safe, err := m.safeDirectory(game, pkg.ProductID, true)
	if err != nil {
		return "", err
	}
	if err := os.Remove(filepath.Join(safe, markerName)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return "", err
	}
	return safe, nil
}

type marker struct {
	Schema    int    `json:"schema"`
	ProductID string `json:"productId"`
	ContentID string `json:"contentId"`
	Parent    string `json:"parent"`
	Name      string `json:"name"`
	Version   string `json:"version"`
}

// Complete validates downloaded content and marks it installed.
func (m *Manager) Complete(game shared.Game, pkg Package) error {
	directory, err := m.safeDirectory(game, pkg.ProductID, false)
	if err != nil {
		return err
	}
	content, err := m.validate(game, pkg.ProductID, directory, pkg.Family)
	if err != nil {
		return err
	}
	data, _ := json.Marshal(marker{1, pkg.ProductID, pkg.ContentID, game.StoreID, content.Name, content.Version})
	temporary := filepath.Join(directory, markerName+"."+install.NewUUID()+".tmp")
	handle, err := os.OpenFile(temporary, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	_, err = handle.Write(data)
	if closeErr := handle.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		os.Remove(temporary)
		return err
	}
	return os.Rename(temporary, filepath.Join(directory, markerName))
}

func (m *Manager) installedIDs(game shared.Game) ([]string, error) {
	base, err := baseManifest(game)
	if err != nil {
		return nil, err
	}
	root, err := m.safeDirectory(game, "", false)
	if errors.Is(err, fs.ErrNotExist) {
		return []string{}, nil
	}
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, err
	}
	if len(entries) > 256 {
		return nil, errors.New("Too many managed content directories.")
	}
	installed := []string{}
	for _, entry := range entries {
		id := entry.Name()
		if !validIDPattern.MatchString(id) {
			continue
		}
		// Missing/incomplete content is never promoted to installed.
		directory, err := m.safeDirectory(game, id, false)
		if err != nil {
			continue
		}
		raw, err := boundedFile(filepath.Join(directory, markerName))
		if err != nil {
			continue
		}
		var value marker
		if json.Unmarshal([]byte(raw), &value) != nil || value.Schema != 1 || value.Parent != base.StoreID || value.ProductID != id || !contentIDPattern.MatchString(value.ContentID) {
			continue
		}
		content, err := m.validate(game, id, directory, "")
		if err == nil && value.Name == content.Name && value.Version == content.Version {
			installed = append(installed, id)
		}
	}
	return installed, nil
}

// Roots returns validated content directories for the runtime. Discovery is
// bounded and uses MAX_PATH: fail explicitly, never by silently omitting
// installed content or splitting a path into extra roots.
func (m *Manager) Roots(game shared.Game) ([]string, error) {
	ids, err := m.installedIDs(game)
	if err != nil {
		return nil, err
	}
	roots := []string{}
	for _, id := range ids {
		directory, err := m.safeDirectory(game, id, false)
		if err != nil {
			return nil, err
		}
		roots = append(roots, directory)
	}
	if slices.ContainsFunc(roots, func(root string) bool { return shared.UTF16Len(root)+2 >= 260 }) || shared.UTF16Len(strings.Join(roots, ";")) > 16000 {
		return nil, errors.New("Managed content paths exceed runtime discovery limits.")
	}
	return roots, nil
}

// RelatedProductsTimeout bounds discovery used during launch.
const RelatedProductsTimeout = 5 * time.Second
