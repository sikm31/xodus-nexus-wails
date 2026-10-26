// Package library ports account-library.ts: the signed-in account's Store
// entitlements, classified as owned, Game Pass or other access.
package library

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/text/collate"
	"golang.org/x/text/language"

	"xodus-nexus/internal/diagnostics"
	"xodus-nexus/internal/proc"
	"xodus-nexus/internal/shared"
)

type Entitlement struct {
	ProductID            string
	Status               string
	IsTrial              bool
	SharingSource        string
	EndDate              string
	SatisfiedByProductID string
	Special              bool
	IsPositron           *bool
	end                  time.Time
}

// Entries is a Map<string, Entitlement> with JavaScript insertion order.
type Entries struct {
	order []string
	byID  map[string]Entitlement
}

func NewEntries() *Entries { return &Entries{byID: map[string]Entitlement{}} }

func (e *Entries) Get(id string) (Entitlement, bool) { v, ok := e.byID[id]; return v, ok }
func (e *Entries) Len() int                          { return len(e.order) }
func (e *Entries) Keys() []string                    { return slices.Clone(e.order) }

func (e *Entries) Set(id string, value Entitlement) {
	if _, ok := e.byID[id]; !ok {
		e.order = append(e.order, id)
	}
	e.byID[id] = value
}

func (e *Entries) Delete(id string) {
	if _, ok := e.byID[id]; !ok {
		return
	}
	delete(e.byID, id)
	e.order = slices.DeleteFunc(e.order, func(v string) bool { return v == id })
}

type Kind = string // all | owned | gamepass | other

// Reader runs the backend library command and returns its stdout.
type Reader func(ctx context.Context, binary, market string) (string, error)

// Catalog loads public catalog data for licensed product IDs.
type Catalog func(ctx context.Context, ids []string, market string) ([]shared.CatalogProduct, error)

var productIDPattern = regexp.MustCompile(`^[A-Z0-9]{12}$`)
var plans = map[string]bool{"CFQ7TTC0KGQ8": true, "CFQ7TTC0KHS0": true, "CFQ7TTC0P85B": true, "CFQ7TTC0K5DJ": true, "CFQ7TTC0K6L8": true, "CFQ7TTC10QFD": true}

const TTL = 5 * time.Minute

func isID(raw json.RawMessage) (string, bool) {
	var value string
	if raw == nil || json.Unmarshal(raw, &value) != nil || !isJSONString(raw) || !productIDPattern.MatchString(value) {
		return "", false
	}
	return value, true
}

func isJSONString(raw json.RawMessage) bool {
	raw = bytes.TrimSpace(raw)
	return len(raw) > 0 && raw[0] == '"'
}

func jsonString(raw json.RawMessage) (string, bool) {
	var value string
	if !isJSONString(raw) || json.Unmarshal(raw, &value) != nil {
		return "", false
	}
	return value, true
}

func jsonBool(raw json.RawMessage) (bool, bool) {
	switch string(bytes.TrimSpace(raw)) {
	case "true":
		return true, true
	case "false":
		return false, true
	}
	return false, false
}

func isNull(raw json.RawMessage) bool { return string(bytes.TrimSpace(raw)) == "null" }

func jsLen(value string) int {
	n := 0
	for _, r := range value {
		if r > 0xFFFF {
			n += 2
		} else {
			n++
		}
	}
	return n
}

// validRow returns the typed entitlement, or ok=false for a row that must be quarantined.
func validRow(row map[string]json.RawMessage, id string) (Entitlement, bool) {
	e := Entitlement{ProductID: id}
	var ok bool
	if e.Status, ok = jsonString(row["status"]); !ok || jsLen(e.Status) >= 40 {
		return e, false
	}
	if e.IsTrial, ok = jsonBool(row["isTrial"]); !ok {
		return e, false
	}
	if e.Special, ok = jsonBool(row["special"]); !ok {
		return e, false
	}
	if e.SharingSource, ok = jsonString(row["sharingSource"]); !ok || jsLen(e.SharingSource) >= 40 {
		return e, false
	}
	if raw, present := row["isPositron"]; present {
		positron, ok := jsonBool(raw)
		if !ok || (positron && !e.Special) {
			return e, false
		}
		e.IsPositron = &positron
	}
	if e.EndDate, ok = jsonString(row["endDate"]); !ok || jsLen(e.EndDate) >= 50 {
		return e, false
	}
	if e.end, ok = shared.ParseJSDate(e.EndDate); !ok {
		return e, false
	}
	if raw, present := row["satisfiedByProductId"]; present && !isNull(raw) {
		if e.SatisfiedByProductID, ok = isID(raw); !ok {
			return e, false
		}
	}
	return e, true
}

type Parsed struct {
	CheckedAt string
	Entries   *Entries
	Skipped   int
	checked   time.Time
}

var errInvalidResponse = errors.New("Invalid account library response.")

func ParseEntitlements(output, account, market string, now time.Time) (Parsed, error) {
	if len(output) > 8_000_000 {
		return Parsed{}, errors.New("Account library response is too large.")
	}
	lines := strings.Split(output, "\n")
	line := ""
	for i := len(lines) - 1; i >= 0; i-- {
		if strings.HasPrefix(strings.TrimLeft(lines[i], " \t\r\v\f"), "{") {
			line = lines[i]
			break
		}
	}
	var result struct {
		Schema              json.RawMessage `json:"schema"`
		Account             json.RawMessage `json:"account"`
		Market              json.RawMessage `json:"market"`
		CheckedAt           json.RawMessage `json:"checkedAt"`
		Entitlements        json.RawMessage `json:"entitlements"`
		SkippedEntitlements json.RawMessage `json:"skippedEntitlements"`
	}
	if err := json.Unmarshal([]byte(line), &result); err != nil {
		return Parsed{}, errInvalidResponse
	}
	var rows []json.RawMessage
	resultAccount, accountOK := jsonString(result.Account)
	resultMarket, marketOK := jsonString(result.Market)
	checkedAt, checkedOK := jsonString(result.CheckedAt)
	schema, schemaErr := strconv.ParseFloat(string(bytes.TrimSpace(result.Schema)), 64)
	if schemaErr != nil || schema != 1 || !accountOK || resultAccount != account || !marketOK || resultMarket != market ||
		!checkedOK || json.Unmarshal(result.Entitlements, &rows) != nil || bytes.TrimSpace(result.Entitlements)[0] != '[' || len(rows) > 10_000 {
		return Parsed{}, errInvalidResponse
	}
	checked, ok := shared.ParseJSDate(checkedAt)
	age := now.Sub(checked)
	if !ok || age < -time.Minute || age >= TTL {
		return Parsed{}, errors.New("Stale account library response.")
	}
	skippedReported := 0
	if result.SkippedEntitlements != nil {
		value, err := strconv.ParseFloat(string(bytes.TrimSpace(result.SkippedEntitlements)), 64)
		if err != nil || value != float64(int(value)) || value < 0 || value > 200_000 {
			return Parsed{}, errors.New("Invalid skipped entitlement count.")
		}
		skippedReported = int(value)
	}
	entries := NewEntries()
	seen, blocked := map[string]bool{}, map[string]bool{}
	var blockedOrder []string
	block := func(id string) {
		if !blocked[id] {
			blocked[id] = true
			blockedOrder = append(blockedOrder, id)
		}
	}
	for _, raw := range rows {
		var row map[string]json.RawMessage
		if json.Unmarshal(raw, &row) != nil || row == nil {
			continue
		}
		id, ok := isID(row["productId"])
		if !ok {
			continue
		}
		duplicate := seen[id]
		seen[id] = true
		entitlement, valid := validRow(row, id)
		if duplicate || !valid {
			block(id)
			entries.Delete(id)
			continue
		}
		entries.Set(id, entitlement)
	}
	// Quarantine descendants as well, including known subscription IDs. An invalid
	// parent must never turn its child's bundle/subscription reference into access.
	children := map[string][]string{}
	for _, product := range entries.Keys() {
		row, _ := entries.Get(product)
		if row.SatisfiedByProductID != "" {
			children[row.SatisfiedByProductID] = append(children[row.SatisfiedByProductID], product)
		}
	}
	queue := blockedOrder
	for i := 0; i < len(queue); i++ {
		for _, child := range children[queue[i]] {
			if !blocked[child] {
				blocked[child] = true
				queue = append(queue, child)
				entries.Delete(child)
			}
		}
	}
	skipped := min(200_000, skippedReported+len(rows)-entries.Len())
	if skipped != 0 {
		diagnostics.Emit(diagnostics.Record{Operation: "library", Event: "validation", Stage: "partial", Skipped: &skipped})
	}
	return Parsed{CheckedAt: checkedAt, Entries: entries, Skipped: skipped, checked: checked}, nil
}

type resolution struct {
	kind   Kind // empty when there is no access
	reason string
}

// resolveEntitlement: an active effective entitlement can have an absent source
// row. That is not permanent ownership.
func resolveEntitlement(productID string, entries *Entries, now time.Time) resolution {
	visited := map[string]bool{}
	current, subscription, conditional := productID, false, false
	for depth := range 16 {
		if visited[current] {
			return resolution{reason: "access-cycle"}
		}
		visited[current] = true
		subscription = subscription || plans[current]
		item, ok := entries.Get(current)
		if !ok {
			switch {
			case depth == 0:
				return resolution{}
			case conditional:
				return resolution{kind: "owned", reason: "access-conditional"}
			case subscription:
				return resolution{kind: "gamepass"}
			}
			return resolution{kind: "other", reason: "access-unresolved"}
		}
		// Only the backend's explicit boolean provenance permits conditional access.
		// Legacy/unknown special flags remain excluded, never assumed to be owned.
		positron := item.IsPositron != nil && *item.IsPositron
		if item.Special && !positron {
			return resolution{reason: "access-special"}
		}
		if item.Status != "Active" || item.IsTrial || item.SharingSource != "None" || !item.end.After(now) {
			if depth > 0 {
				return resolution{reason: "access-parent-unavailable"}
			}
			return resolution{}
		}
		conditional = conditional || positron
		// Owned is a library category, not a promise that a license cannot expire
		// or be revoked. Explicit disc-derived access belongs with purchased games.
		if item.SatisfiedByProductID == "" {
			switch {
			case current == productID && plans[current]:
				return resolution{}
			case conditional:
				return resolution{kind: "owned", reason: "access-conditional"}
			case subscription:
				return resolution{kind: "gamepass"}
			}
			return resolution{kind: "owned"}
		}
		current = item.SatisfiedByProductID
	}
	return resolution{reason: "access-depth"}
}

// EntitlementKind returns owned, gamepass, other, or "" for no access.
func EntitlementKind(productID string, entries *Entries, now time.Time) Kind {
	return resolveEntitlement(productID, entries, now).kind
}

// Read runs `library --json --market` on the backend binary.
func Read(ctx context.Context, binary, market string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	var diagnosticsText strings.Builder
	var mu sync.Mutex
	tracked, err := proc.Spawn(binary, []string{"library", "--json", "--market", market}, proc.RunOptions{
		MaxOutput: 8_000_000, StdoutOnly: true,
		OnStderr: func(b []byte) {
			mu.Lock()
			diagnosticsText.Write(b)
			if diagnosticsText.Len() > 128_000 {
				text := diagnosticsText.String()
				diagnosticsText.Reset()
				diagnosticsText.WriteString(text[len(text)-64_000:])
			}
			mu.Unlock()
		},
	})
	if err != nil {
		return "", err
	}
	timer := time.AfterFunc(50*time.Second, func() { tracked.Kill() })
	defer timer.Stop()
	select {
	case <-tracked.Done():
	case <-ctx.Done():
		tracked.Kill()
		<-tracked.Done()
		return "", ctx.Err()
	}
	result := tracked.Wait()
	if result.Code != 0 {
		mu.Lock()
		text := diagnosticsText.String()
		mu.Unlock()
		if message := diagnostics.SessionFailureMessage(text); message != "" {
			return "", errors.New(message)
		}
		return "", errors.New("Microsoft account licenses could not be loaded. Try Refresh later.")
	}
	return result.Output, nil
}

type identity struct{ account, binary, market string }

type snapshot struct {
	Parsed
	unclassified, excluded int
	until                  time.Time
	key                    string
}

type job struct {
	done chan struct{}
	err  error
}

var errChanged = errors.New("Account library changed. Refresh this page.")

// AccountLibrary keeps credentials in the CLI. Account-bound memory only, no
// license dumps in UI/disk logs.
type AccountLibrary struct {
	catalog Catalog
	reader  Reader
	// Now is the clock; tests replace it.
	Now func() time.Time

	mu         sync.Mutex
	identity   *identity
	generation int
	cancel     context.CancelFunc
	cached     *snapshot
	job        *job
	pages      map[string]shared.AccountLibraryPage
}

func New(catalog Catalog, reader Reader) *AccountLibrary {
	if reader == nil {
		reader = Read
	}
	return &AccountLibrary{catalog: catalog, reader: reader, Now: time.Now, pages: map[string]shared.AccountLibraryPage{}}
}

func (l *AccountLibrary) Reset() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.resetLocked()
}

func (l *AccountLibrary) resetLocked() {
	l.generation++
	if l.cancel != nil {
		l.cancel()
	}
	l.cancel = nil
	l.cached = nil
	l.job = nil
	clear(l.pages)
}

var marketPattern = regexp.MustCompile(`^[A-Z]{2}$`)

// Select binds the library to an account; empty account or binary means signed out.
func (l *AccountLibrary) Select(account, binary, market string) {
	normalized := strings.ToUpper(market)
	if !marketPattern.MatchString(normalized) {
		normalized = "US"
	}
	var next *identity
	if account != "" && binary != "" {
		next = &identity{account, binary, normalized}
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if (l.identity == nil) != (next == nil) || (next != nil && *l.identity != *next) {
		l.resetLocked()
		l.identity = next
	}
}

func randomUUID() string {
	var b [16]byte
	rand.Read(b[:])
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

func (l *AccountLibrary) load(cursor string, refresh bool) (*snapshot, error) {
	l.mu.Lock()
	id := l.identity
	if id == nil {
		l.mu.Unlock()
		return nil, errors.New("Sign in and update the Xodus backend to load account games.")
	}
	if refresh {
		l.resetLocked()
		cursor = ""
	}
	generation := l.generation
	if l.cached == nil || !l.cached.until.After(l.Now()) {
		if cursor != "" {
			l.mu.Unlock()
			return nil, errors.New("Library results expired. Refresh this page.")
		}
		if l.job == nil {
			ctx, cancel := context.WithCancel(context.Background())
			l.cancel = cancel
			current := &job{done: make(chan struct{})}
			l.job = current
			go l.run(ctx, *id, generation, current)
		}
		current := l.job
		l.mu.Unlock()
		<-current.done
		if current.err != nil {
			return nil, current.err
		}
		l.mu.Lock()
	}
	defer l.mu.Unlock()
	if generation != l.generation {
		return nil, errChanged
	}
	return l.cached, nil
}

func (l *AccountLibrary) run(ctx context.Context, id identity, generation int, current *job) {
	defer func() {
		l.mu.Lock()
		if generation == l.generation {
			l.cancel = nil
			l.job = nil
		}
		l.mu.Unlock()
		close(current.done)
	}()
	output, err := l.reader(ctx, id.binary, id.market)
	if err != nil {
		current.err = err
		return
	}
	data, err := ParseEntitlements(output, id.account, id.market, l.Now())
	if err != nil {
		diagnostics.Emit(diagnostics.Record{Operation: "library", Event: "validation", Stage: "invalid-response"})
		current.err = err
		return
	}
	counts := map[string]int{}
	var reasons []string
	unclassified, excluded := 0, 0
	for _, product := range data.Entries.Keys() {
		resolved := resolveEntitlement(product, data.Entries, data.checked)
		if resolved.kind == "other" {
			unclassified++
		}
		if resolved.reason != "" && resolved.kind == "" {
			excluded++
		}
		if resolved.reason != "" {
			if counts[resolved.reason] == 0 {
				reasons = append(reasons, resolved.reason)
			}
			counts[resolved.reason]++
		}
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if generation != l.generation {
		current.err = errChanged
		return
	}
	for _, stage := range reasons {
		skipped := counts[stage]
		diagnostics.Emit(diagnostics.Record{Operation: "library", Event: "validation", Stage: stage, Skipped: &skipped})
	}
	l.cached = &snapshot{Parsed: data, unclassified: unclassified, excluded: excluded, until: data.checked.Add(TTL), key: randomUUID()}
	clear(l.pages)
}

func accessMap(entries *Entries, now time.Time) map[string]shared.LibraryAccess {
	access := map[string]shared.LibraryAccess{}
	for _, product := range entries.Keys() {
		if kind := EntitlementKind(product, entries, now); kind != "" {
			access[product] = kind
		}
	}
	return access
}

// PeekAccess lets scoped callers read access without fetching unrelated library artwork.
func (l *AccountLibrary) PeekAccess() map[string]shared.LibraryAccess {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.identity == nil || l.cached == nil || !l.cached.until.After(l.Now()) {
		return map[string]shared.LibraryAccess{}
	}
	return accessMap(l.cached.Entries, l.Now())
}

type AccessResult struct {
	CheckedAt         string                          `json:"checkedAt"`
	AccessByProductID map[string]shared.LibraryAccess `json:"accessByProductId"`
}

func (l *AccountLibrary) Access(refresh bool) (AccessResult, error) {
	cached, err := l.load("", refresh)
	if err != nil {
		return AccessResult{}, err
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if cached != l.cached || l.identity == nil {
		return AccessResult{}, errChanged
	}
	return AccessResult{CheckedAt: cached.CheckedAt, AccessByProductID: accessMap(cached.Entries, l.Now())}, nil
}

var cursorPattern = regexp.MustCompile(`^([a-f0-9-]{36}):(all|owned|gamepass|other):(\d{1,5})$`)

func clonePage(page shared.AccountLibraryPage) shared.AccountLibraryPage {
	encoded, _ := json.Marshal(page)
	var out shared.AccountLibraryPage
	json.Unmarshal(encoded, &out)
	return out
}

func intPtr(v int) *int { return &v }

func (l *AccountLibrary) Get(ctx context.Context, kind Kind, cursor string, refresh bool) (shared.AccountLibraryPage, error) {
	if kind != "all" && kind != "owned" && kind != "gamepass" && kind != "other" {
		return shared.AccountLibraryPage{}, errors.New("Invalid library filter.")
	}
	cached, err := l.load(cursor, refresh)
	if err != nil {
		return shared.AccountLibraryPage{}, err
	}
	l.mu.Lock()
	if cached != l.cached || l.identity == nil {
		l.mu.Unlock()
		return shared.AccountLibraryPage{}, errChanged
	}
	if refresh {
		cursor = ""
	}
	market := l.identity.market
	generation := l.generation
	access := accessMap(cached.Entries, l.Now())
	// Stable offsets within one snapshot even when a license expires mid-page.
	snapshotAccess := map[string]Kind{}
	var ids []string
	for _, product := range cached.Entries.Keys() {
		value := EntitlementKind(product, cached.Entries, cached.checked)
		snapshotAccess[product] = value
		if value != "" && (kind == "all" || value == kind) {
			ids = append(ids, product)
		}
	}
	collator := collate.New(language.Und)
	slices.SortStableFunc(ids, func(a, b string) int {
		ownedA, ownedB := snapshotAccess[a] == "owned", snapshotAccess[b] == "owned"
		if ownedA != ownedB {
			if ownedA {
				return -1
			}
			return 1
		}
		return collator.CompareString(a, b)
	})
	offset := 0
	if cursor != "" {
		match := cursorPattern.FindStringSubmatch(cursor)
		if match == nil || match[1] != cached.key || match[2] != kind {
			l.mu.Unlock()
			return shared.AccountLibraryPage{}, errors.New("Invalid or expired library page.")
		}
		offset, _ = strconv.Atoi(match[3])
		if offset%60 != 0 || offset > len(ids) {
			l.mu.Unlock()
			return shared.AccountLibraryPage{}, errors.New("Invalid library page offset.")
		}
	}
	key := fmt.Sprintf("%s:%d", kind, offset)
	if existing, ok := l.pages[key]; ok {
		l.mu.Unlock()
		page := clonePage(existing)
		page.AccessByProductID = access
		page.Products = slices.DeleteFunc(page.Products, func(p shared.LibraryProduct) bool { return access[p.ProductID] == "" })
		return page, nil
	}
	l.mu.Unlock()

	slice := ids[offset:min(len(ids), offset+60)]
	products, err := l.catalog(ctx, slices.Clone(slice), market)
	if err != nil {
		return shared.AccountLibraryPage{}, err
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if generation != l.generation {
		return shared.AccountLibraryPage{}, errChanged
	}
	if l.cached != cached {
		return shared.AccountLibraryPage{}, errors.New("Library results changed. Refresh this page.")
	}
	allowed := map[string]bool{}
	for _, id := range slice {
		allowed[id] = true
	}
	page := shared.AccountLibraryPage{
		Kind: kind, CheckedAt: cached.CheckedAt, AccessByProductID: access, SkippedEntitlements: intPtr(cached.Skipped),
		UnclassifiedEntitlements: intPtr(cached.unclassified), ExcludedEntitlements: intPtr(cached.excluded),
		Products: []shared.LibraryProduct{},
	}
	for _, product := range products {
		if allowed[product.ProductID] && access[product.ProductID] != "" {
			page.Products = append(page.Products, shared.LibraryProduct{CatalogProduct: product, Access: access[product.ProductID]})
		}
	}
	slices.SortStableFunc(page.Products, func(a, b shared.LibraryProduct) int { return collator.CompareString(a.Title, b.Title) })
	if offset+60 < len(ids) {
		page.Cursor = fmt.Sprintf("%s:%s:%d", cached.key, kind, offset+60)
	}
	l.pages[key] = page
	return clonePage(page), nil
}
