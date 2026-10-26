package library

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"xodus-nexus/internal/shared"
)

const (
	A = "9AAAAAAAAAAA"
	B = "9BBBBBBBBBBB"
	C = "9CCCCCCCCCCC"
	D = "9DDDDDDDDDDD"
)

type row map[string]any

// undefined removes a key, like `{ key: undefined }` in the TypeScript tests.
var undefined = struct{}{}

func mkrow(productID string, extra row) row {
	r := row{"productId": productID, "status": "Active", "isTrial": false, "sharingSource": "None", "endDate": "2099-01-01T00:00:00Z", "special": false}
	for k, v := range extra {
		if v == undefined {
			delete(r, k)
		} else {
			r[k] = v
		}
	}
	return r
}

func envelope(entries []row, account, market string) map[string]any {
	return map[string]any{"schema": 1, "account": account, "market": market, "checkedAt": shared.JSISOString(time.Now()), "entitlements": entries}
}

func output(entries ...row) string {
	if entries == nil {
		entries = []row{mkrow(A, nil)}
	}
	encoded, _ := json.Marshal(envelope(entries, "one@example.test", "US"))
	return string(encoded)
}

func parse(t *testing.T, text string) Parsed {
	t.Helper()
	parsed, err := ParseEntitlements(text, "one@example.test", "US", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	return parsed
}

func products(ctx context.Context, ids []string, market string) ([]shared.CatalogProduct, error) {
	out := make([]shared.CatalogProduct, len(ids))
	for i, id := range ids {
		out[i] = shared.CatalogProduct{ProductID: id, Title: id, Categories: []string{}, Installability: "supported"}
	}
	return out, nil
}

type countingReader struct {
	calls atomic.Int32
	text  func() string
}

func (r *countingReader) read(context.Context, string, string) (string, error) {
	r.calls.Add(1)
	return r.text(), nil
}

func fixed(text string) *countingReader { return &countingReader{text: func() string { return text }} }

func ids(page shared.AccountLibraryPage) []string {
	out := []string{}
	for _, p := range page.Products {
		out = append(out, p.ProductID)
	}
	return out
}

func mustGet(t *testing.T, l *AccountLibrary, kind, cursor string, refresh bool) shared.AccountLibraryPage {
	t.Helper()
	page, err := l.Get(context.Background(), kind, cursor, refresh)
	if err != nil {
		t.Fatal(err)
	}
	return page
}

func positron(extra row) row {
	r := row{"special": true, "isPositron": true}
	maps.Copy(r, extra)
	return r
}

func TestDiscToDigitalAccessIsOwned(t *testing.T) {
	reader := fixed(output(mkrow(A, positron(nil)), mkrow(B, nil)))
	l := New(products, reader.read)
	l.Select("one@example.test", "/not-executed", "US")
	all := mustGet(t, l, "all", "", false)
	if all.Products[slices.IndexFunc(all.Products, func(p shared.LibraryProduct) bool { return p.ProductID == A })].Access != "owned" ||
		*all.ExcludedEntitlements != 0 || *all.UnclassifiedEntitlements != 0 {
		t.Errorf("all = %+v", all)
	}
	if len(mustGet(t, l, "other", "", false).Products) != 0 || len(mustGet(t, l, "gamepass", "", false).Products) != 0 {
		t.Error("conditional access is owned only")
	}
	if got := ids(mustGet(t, l, "owned", "", false)); !slices.Equal(got, []string{A, B}) {
		t.Errorf("owned = %v", got)
	}
	if !maps.Equal(l.PeekAccess(), map[string]string{A: "owned", B: "owned"}) || reader.calls.Load() != 1 {
		t.Errorf("peek %v calls %d", l.PeekAccess(), reader.calls.Load())
	}
}

func kindOf(t *testing.T, rows ...row) string {
	t.Helper()
	return EntitlementKind(A, parse(t, output(rows...)).Entries, time.Now())
}

func TestConditionalAccessThroughValidParents(t *testing.T) {
	cases := [][]row{
		{mkrow(A, nil), mkrow(B, positron(nil))},
		{mkrow(A, row{"satisfiedByProductId": B}), mkrow(B, positron(nil))},
		{mkrow(A, positron(row{"satisfiedByProductId": B})), mkrow(B, nil)},
		{mkrow(A, positron(row{"satisfiedByProductId": "CFQ7TTC0KGQ8"}))},
		{mkrow(A, positron(row{"satisfiedByProductId": B}))},
	}
	for i, rows := range cases {
		if got := kindOf(t, rows...); got != "owned" {
			t.Errorf("case %d = %q", i, got)
		}
	}
	for _, invalid := range []row{{"status": "Revoked"}, {"isTrial": true}, {"sharingSource": "Family"}, {"endDate": "2000-01-01T00:00:00Z"}} {
		withInvalid := func(base row) row { r := maps.Clone(base); maps.Copy(r, invalid); return r }
		for i, rows := range [][]row{
			{mkrow(A, withInvalid(positron(nil)))},
			{mkrow(A, row{"satisfiedByProductId": B}), mkrow(B, withInvalid(positron(nil)))},
			{mkrow(A, positron(row{"satisfiedByProductId": B})), mkrow(B, invalid)},
		} {
			if got := kindOf(t, rows...); got != "" {
				t.Errorf("invalid %v case %d = %q", invalid, i, got)
			}
		}
	}
	if kindOf(t, mkrow(A, positron(row{"satisfiedByProductId": B})), mkrow(B, row{"special": true})) != "" ||
		kindOf(t, mkrow(A, positron(row{"satisfiedByProductId": B})), mkrow(B, row{"satisfiedByProductId": A})) != "" {
		t.Error("special parents and cycles grant no access")
	}
}

func TestLegacySpecialAndMalformedPositron(t *testing.T) {
	for _, extra := range []row{{"special": true}, {"special": true, "isPositron": false}} {
		if kindOf(t, mkrow(A, extra)) != "" {
			t.Errorf("%v must not be owned", extra)
		}
	}
	for _, extra := range []row{{"special": true, "isPositron": "true"}, {"special": true, "isPositron": nil}, {"special": true, "isPositron": 1}, {"special": false, "isPositron": true}} {
		parsed := parse(t, output(mkrow(A, extra), mkrow(B, row{"satisfiedByProductId": A}), mkrow(C, nil)))
		if !slices.Equal(parsed.Entries.Keys(), []string{C}) || parsed.Skipped != 2 {
			t.Errorf("%v: keys %v skipped %d", extra, parsed.Entries.Keys(), parsed.Skipped)
		}
	}
	if kindOf(t, mkrow(A, row{"isPositron": false})) != "owned" {
		t.Error("explicit non-positron ordinary row is owned")
	}
}

func TestBoundedChainsAndQuarantine(t *testing.T) {
	var deep []row
	for i := range 17 {
		extra := row{}
		if i == 0 {
			extra = positron(nil)
		}
		if i < 16 {
			extra["satisfiedByProductId"] = fmt.Sprintf("%012d", i+1)
		}
		deep = append(deep, mkrow(fmt.Sprintf("%012d", i), extra))
	}
	if EntitlementKind(deep[0]["productId"].(string), parse(t, output(deep...)).Entries, time.Now()) != "" {
		t.Error("chains deeper than 16 grant no access")
	}
	bad := parse(t, output(mkrow(A, positron(row{"satisfiedByProductId": B})), mkrow(B, row{"endDate": undefined}), mkrow(C, nil)))
	if !slices.Equal(bad.Entries.Keys(), []string{C}) || bad.Skipped != 2 {
		t.Errorf("keys %v skipped %d", bad.Entries.Keys(), bad.Skipped)
	}
}

func TestRecheckAfterRevocationExpiryAndAccountChange(t *testing.T) {
	var mu sync.Mutex
	status := "Active"
	var rows []row
	for i := range 61 {
		rows = append(rows, mkrow(fmt.Sprintf("%012d", i), positron(nil)))
	}
	reader := &countingReader{text: func() string {
		mu.Lock()
		defer mu.Unlock()
		current := make([]row, len(rows))
		for i, r := range rows {
			current[i] = maps.Clone(r)
			current[i]["status"] = status
		}
		return output(current...)
	}}
	l := New(products, reader.read)
	l.Select("one@example.test", "/not-executed", "US")
	first := mustGet(t, l, "all", "", false)
	if len(first.Products) != 60 || len(mustGet(t, l, "all", first.Cursor, false).Products) != 1 || len(mustGet(t, l, "other", "", false).Products) != 0 {
		t.Fatal("pagination of conditional access")
	}
	owned := mustGet(t, l, "owned", "", false)
	if len(owned.Products) != 60 || len(mustGet(t, l, "owned", owned.Cursor, false).Products) != 1 {
		t.Fatal("owned pagination")
	}
	mu.Lock()
	status = "Revoked"
	mu.Unlock()
	if len(mustGet(t, l, "all", "", true).Products) != 0 || len(l.PeekAccess()) != 0 {
		t.Error("revoked access must disappear on refresh")
	}
	mu.Lock()
	status = "Active"
	mu.Unlock()
	mustGet(t, l, "all", "", true)
	l.Now = func() time.Time { return time.Date(2100, 1, 1, 0, 0, 0, 0, time.UTC) }
	if len(l.PeekAccess()) != 0 {
		t.Error("expired cache must not be used")
	}
	l.Now = time.Now
	l.Select("two@example.test", "/not-executed", "US")
	if _, err := l.Get(context.Background(), "all", "", false); err == nil || !strings.Contains(err.Error(), "Invalid account library response") {
		t.Errorf("foreign account: %v", err)
	}
	if len(l.PeekAccess()) != 0 {
		t.Error("no access after account switch")
	}
}

func TestPeekAccessIsFreshAndAccountBound(t *testing.T) {
	reader := fixed(output(mkrow(A, nil), mkrow(B, row{"satisfiedByProductId": "9ZZZZZZZZZZZ"}), mkrow(C, row{"isTrial": true})))
	l := New(products, reader.read)
	l.Select("one@example.test", "/backend", "US")
	if len(l.PeekAccess()) != 0 || reader.calls.Load() != 0 {
		t.Fatal("peek must not start reads")
	}
	if _, err := l.Access(false); err != nil {
		t.Fatal(err)
	}
	cached := l.PeekAccess()
	if !maps.Equal(cached, map[string]string{A: "owned", B: "other"}) {
		t.Errorf("peek = %v", cached)
	}
	cached[A] = "gamepass"
	if l.PeekAccess()[A] != "owned" || reader.calls.Load() != 1 {
		t.Error("peek must return a copy")
	}
	later := time.Now().Add(300_001 * time.Millisecond)
	l.Now = func() time.Time { return later }
	if len(l.PeekAccess()) != 0 || reader.calls.Load() != 1 {
		t.Error("stale access must not be reused")
	}
	l.Now = time.Now
	l.Select("different@example.test", "/backend", "US")
	if len(l.PeekAccess()) != 0 {
		t.Error("account switch clears access")
	}
}

func TestSessionGuidanceFromRealChildProcess(t *testing.T) {
	directory := t.TempDir()
	binary := filepath.Join(directory, "backend")
	var catalogCalls atomic.Int32
	catalog := func(ctx context.Context, ids []string, market string) ([]shared.CatalogProduct, error) {
		catalogCalls.Add(1)
		return products(ctx, ids, market)
	}
	l := New(catalog, nil)
	l.Select("one@example.test", binary, "US")
	defer l.Reset()
	fixture := func(stdout, stderr string, code int) {
		script := fmt.Sprintf("#!/bin/sh\nprintf '%%s' %s\nprintf '%%s' %s >&2\nexit %d\n", shellQuote(stdout), shellQuote(stderr), code)
		os.WriteFile(binary+".tmp", []byte(script), 0o700)
		os.Rename(binary+".tmp", binary)
	}
	for stage, message := range map[string]string{"session-user-expired": "Sign in again", "ticket-timeout": "timed out",
		"session-device-refresh-failed": "could not be renewed", "ticket-response-parse": "unreadable response"} {
		fixture("", "private@example.test raw-secret\nNEXUS_SESSION_DIAGNOSTIC_V1 "+stage+"\n", 1)
		_, err := l.Get(context.Background(), "all", "", false)
		if err == nil || !strings.Contains(err.Error(), message) || regexp.MustCompile(`example\.test|raw-secret`).MatchString(err.Error()) {
			t.Errorf("%s: %v", stage, err)
		}
	}
	if catalogCalls.Load() != 0 {
		t.Error("catalog must not load after a failed read")
	}
	fixture("", "private-secret\nNEXUS_TICKET_DIAGNOSTIC_V1 ticket-refused 800488AB\n", 1)
	if _, err := l.Get(context.Background(), "all", "", false); err == nil || !strings.Contains(err.Error(), "Microsoft refused the stored sign-in (code 0x800488AB)") {
		t.Errorf("ticket refused: %v", err)
	}
	fixture("NEXUS_SESSION_DIAGNOSTIC_V1 session-user-expired\n", "untrusted error\n", 1)
	if _, err := l.Get(context.Background(), "all", "", false); err == nil || !strings.Contains(err.Error(), "licenses could not be loaded") {
		t.Errorf("stdout must not be trusted: %v", err)
	}
	response := output() + "\n"
	middle := len(response) / 2
	script := fmt.Sprintf("#!/bin/sh\nprintf '%%s' %s\nsleep 0.02\nprintf 'NEXUS_SESSION_DIAGNOSTIC_V1 ticket-response-received\\n' >&2\nsleep 0.02\nprintf '%%s' %s\n",
		shellQuote(response[:middle]), shellQuote(response[middle:]))
	os.WriteFile(binary+".tmp", []byte(script), 0o700)
	os.Rename(binary+".tmp", binary)
	if got := ids(mustGet(t, l, "all", "", true)); !slices.Equal(got, []string{A}) || catalogCalls.Load() != 1 {
		t.Errorf("recovered = %v, catalog calls %d", got, catalogCalls.Load())
	}
}

func shellQuote(value string) string { return "'" + strings.ReplaceAll(value, "'", `'\''`) + "'" }

func TestFreshAccessForContentPlanning(t *testing.T) {
	var catalogCalls atomic.Int32
	catalog := func(ctx context.Context, ids []string, market string) ([]shared.CatalogProduct, error) {
		catalogCalls.Add(1)
		return products(ctx, ids, market)
	}
	reader := fixed(output(mkrow(A, nil), mkrow(B, row{"satisfiedByProductId": "CFQ7TTC0KGQ8"})))
	l := New(catalog, reader.read)
	l.Select("one@example.test", "/backend", "US")
	result, err := l.Access(true)
	if err != nil || !maps.Equal(result.AccessByProductID, map[string]string{A: "owned", B: "gamepass"}) || catalogCalls.Load() != 0 || reader.calls.Load() != 1 {
		t.Fatalf("access = %v, %v", result, err)
	}
	l.Access(true)
	if reader.calls.Load() != 2 {
		t.Error("refresh must reread")
	}
	l.Select("", "", "US")
	if _, err := l.Access(false); err == nil || !strings.Contains(err.Error(), "Sign in") {
		t.Errorf("signed out: %v", err)
	}
}

func TestOwnedBundlesGamePassAndUnknownSources(t *testing.T) {
	entries := parse(t, output(mkrow(A, nil), mkrow(B, row{"satisfiedByProductId": A}), mkrow(C, row{"satisfiedByProductId": "CFQ7TTC0KGQ8"}))).Entries
	now := time.Now()
	if EntitlementKind(A, entries, now) != "owned" || EntitlementKind(B, entries, now) != "owned" || EntitlementKind(C, entries, now) != "gamepass" {
		t.Error("basic classification")
	}
	entries.Delete(A)
	if EntitlementKind(B, entries, now) != "other" {
		t.Error("missing parent is other access")
	}
	parent, _ := entries.Get(B)
	entries.Set(A, Entitlement{ProductID: A, Status: "Active", SharingSource: "None", SatisfiedByProductID: B, end: parent.end})
	if EntitlementKind(B, entries, now) != "" {
		t.Error("cycle grants no access")
	}
}

func TestMissingParentStaysVisibleAsOther(t *testing.T) {
	for _, parent := range []string{"CFQ7TTBZT8LN", "CFQ7TTC0K5DH", "9UNKNOWNROOT"} {
		l := New(products, fixed(output(mkrow(A, row{"satisfiedByProductId": parent}))).read)
		l.Select("one@example.test", "/not-executed", "US")
		all := mustGet(t, l, "all", "", false)
		if len(all.Products) != 1 || all.Products[0].Access != "other" || *all.SkippedEntitlements != 0 || *all.UnclassifiedEntitlements != 1 {
			t.Errorf("%s: %+v", parent, all)
		}
		if len(mustGet(t, l, "owned", "", false).Products) != 0 || len(mustGet(t, l, "gamepass", "", false).Products) != 0 || len(mustGet(t, l, "other", "", false).Products) != 1 {
			t.Errorf("%s: filters", parent)
		}
		if access, _ := l.Access(false); !maps.Equal(access.AccessByProductID, map[string]string{A: "other"}) {
			t.Errorf("%s: access %v", parent, access)
		}
	}
}

func TestUnavailableSubscriptionParentNotBypassed(t *testing.T) {
	for _, extra := range []row{{"isTrial": true}, {"special": true}, {"status": "Revoked"}, {"endDate": "2000-01-01T00:00:00Z"}, {"sharingSource": "Family"}} {
		if kindOf(t, mkrow(A, row{"satisfiedByProductId": "CFQ7TTC0KGQ8"}), mkrow("CFQ7TTC0KGQ8", extra)) != "" {
			t.Errorf("%v parent granted access", extra)
		}
		if kindOf(t, mkrow(A, extra)) != "" {
			t.Errorf("%v labelled as owned", extra)
		}
	}
	entries := NewEntries()
	end, _ := shared.ParseJSDate("2099-01-01T00:00:00Z")
	entries.Set("CFQ7TTC0KGQ8", Entitlement{ProductID: "CFQ7TTC0KGQ8", Status: "Active", SharingSource: "None", end: end})
	if EntitlementKind("CFQ7TTC0KGQ8", entries, time.Now()) != "" {
		t.Error("the plan itself is not a game")
	}
}

func TestPolicyExclusionsReported(t *testing.T) {
	l := New(products, fixed(output(mkrow(A, row{"special": true}), mkrow(B, row{"satisfiedByProductId": C}), mkrow(C, row{"satisfiedByProductId": B}), mkrow(D, nil))).read)
	l.Select("one@example.test", "/not-executed", "US")
	result := mustGet(t, l, "all", "", false)
	if !slices.Equal(ids(result), []string{D}) || *result.ExcludedEntitlements != 3 || *result.UnclassifiedEntitlements != 0 {
		t.Errorf("result = %+v", result)
	}
}

func TestOtherAccessPaginationBinding(t *testing.T) {
	var entries []row
	for i := range 61 {
		entries = append(entries, mkrow(fmt.Sprintf("%012d", i), row{"satisfiedByProductId": "CFQ7TTBZT8LN"}))
	}
	l := New(products, fixed(output(entries...)).read)
	l.Select("one@example.test", "/not-executed", "US")
	first := mustGet(t, l, "other", "", false)
	if len(first.Products) != 60 || len(mustGet(t, l, "other", first.Cursor, false).Products) != 1 {
		t.Fatal("pagination")
	}
	if _, err := l.Get(context.Background(), "all", first.Cursor, false); err == nil {
		t.Error("a cursor is bound to its filter")
	}
	l.Select("", "", "US")
	if _, err := l.Get(context.Background(), "other", first.Cursor, false); err == nil || !strings.Contains(err.Error(), "Sign in") {
		t.Errorf("signed out: %v", err)
	}
	parsed := parse(t, output(entries...))
	if EntitlementKind(entries[0]["productId"].(string), parsed.Entries, time.Date(2100, 1, 1, 0, 0, 0, 0, time.UTC)) != "" {
		t.Error("expired access")
	}
}

func TestRejectsForeignAndMalformedEnvelopes(t *testing.T) {
	foreign, _ := json.Marshal(envelope([]row{mkrow(A, nil)}, "other", "US"))
	market, _ := json.Marshal(envelope([]row{mkrow(A, nil)}, "one@example.test", "CZ"))
	notArray := envelope([]row{mkrow(A, nil)}, "one@example.test", "US")
	notArray["entitlements"] = map[string]any{}
	stale := envelope([]row{mkrow(A, nil)}, "one@example.test", "US")
	stale["checkedAt"] = "2000-01-01T00:00:00Z"
	encodedArray, _ := json.Marshal(notArray)
	encodedStale, _ := json.Marshal(stale)
	for _, text := range []string{string(foreign), string(market), "null", string(encodedArray), string(encodedStale)} {
		if _, err := ParseEntitlements(text, "one@example.test", "US", time.Now()); err == nil {
			t.Errorf("accepted %.80s", text)
		}
	}
}

func TestMalformedAndDuplicateRows(t *testing.T) {
	result := parse(t, output(mkrow(A, nil), mkrow(B, row{"isTrial": undefined}), mkrow(C, row{"satisfiedByProductId": B}), mkrow("invalid", nil)))
	if !slices.Equal(result.Entries.Keys(), []string{A}) || result.Skipped != 3 {
		t.Errorf("keys %v skipped %d", result.Entries.Keys(), result.Skipped)
	}
	for i, rows := range [][]row{{mkrow(A, nil), mkrow(A, nil)}, {mkrow(A, nil), mkrow(A, row{"isTrial": true})}, {mkrow(A, nil), mkrow(A, row{"status": undefined})}} {
		parsed := parse(t, output(append(rows, mkrow(B, row{"satisfiedByProductId": A}), mkrow(C, nil))...))
		if !slices.Equal(parsed.Entries.Keys(), []string{C}) || parsed.Skipped != 3 {
			t.Errorf("duplicate case %d: keys %v skipped %d", i, parsed.Entries.Keys(), parsed.Skipped)
		}
	}
	for _, extra := range []row{{"isTrial": undefined}, {"special": undefined}, {"sharingSource": undefined}, {"endDate": undefined}, {"endDate": "invalid"}, {"satisfiedByProductId": "bad"}} {
		parsed := parse(t, output(mkrow(A, extra), mkrow(B, nil)))
		if !slices.Equal(parsed.Entries.Keys(), []string{B}) || parsed.Skipped != 1 {
			t.Errorf("%v: keys %v skipped %d", extra, parsed.Entries.Keys(), parsed.Skipped)
		}
	}
	parsed := parse(t, output(mkrow("CFQ7TTC0KGQ8", row{"endDate": undefined}), mkrow(A, row{"satisfiedByProductId": "CFQ7TTC0KGQ8"}), mkrow(B, nil)))
	if !slices.Equal(parsed.Entries.Keys(), []string{B}) {
		t.Errorf("invalid subscription parent: %v", parsed.Entries.Keys())
	}
}

func TestPartialResultCount(t *testing.T) {
	value := envelope([]row{mkrow(A, nil), mkrow(B, row{"endDate": undefined})}, "one@example.test", "US")
	value["skippedEntitlements"] = 2
	text, _ := json.Marshal(value)
	l := New(products, fixed(string(text)).read)
	l.Select("one@example.test", "/backend", "US")
	for _, kind := range []string{"all", "owned", "gamepass"} {
		if got := *mustGet(t, l, kind, "", false).SkippedEntitlements; got != 3 {
			t.Errorf("%s skipped = %d", kind, got)
		}
	}
	if access, _ := l.Access(false); !maps.Equal(access.AccessByProductID, map[string]string{A: "owned"}) {
		t.Errorf("access = %v", access)
	}
}

func TestOneAccountReadAndBoundPageCache(t *testing.T) {
	reader := fixed(output(mkrow(A, nil), mkrow(B, row{"satisfiedByProductId": "CFQ7TTC0KGQ8"})))
	var catalogCalls atomic.Int32
	catalog := func(ctx context.Context, ids []string, market string) ([]shared.CatalogProduct, error) {
		catalogCalls.Add(1)
		return products(ctx, ids, market)
	}
	l := New(catalog, reader.read)
	l.Select("one@example.test", "/backend", "neutral")
	all := mustGet(t, l, "all", "", false)
	var access []string
	for _, p := range all.Products {
		access = append(access, p.Access)
	}
	if !slices.Equal(access, []string{"owned", "gamepass"}) || !slices.Equal(ids(mustGet(t, l, "owned", "", false)), []string{A}) ||
		!slices.Equal(ids(mustGet(t, l, "gamepass", "", false)), []string{B}) || reader.calls.Load() != 1 {
		t.Errorf("filters: %v", access)
	}
	mustGet(t, l, "all", "", false)
	if catalogCalls.Load() != 3 {
		t.Errorf("catalog calls = %d", catalogCalls.Load())
	}
	l.Select("", "/backend", "US")
	if _, err := l.Get(context.Background(), "all", "", false); err == nil || !strings.Contains(err.Error(), "Sign in") {
		t.Errorf("signed out: %v", err)
	}
}

func TestLateResponseAfterAccountSwitch(t *testing.T) {
	release := make(chan string)
	l := New(products, func(context.Context, string, string) (string, error) { return <-release, nil })
	l.Select("one@example.test", "/backend", "US")
	errs := make(chan error)
	go func() { _, err := l.Get(context.Background(), "all", "", false); errs <- err }()
	time.Sleep(20 * time.Millisecond)
	l.Select("two@example.test", "/backend", "US")
	release <- output()
	if err := <-errs; err == nil || !strings.Contains(err.Error(), "changed") {
		t.Errorf("late response: %v", err)
	}
}

func TestCursorBoundToSnapshotAndFilter(t *testing.T) {
	var entries []row
	for i := range 61 {
		entries = append(entries, mkrow(fmt.Sprintf("%012d", i), nil))
	}
	l := New(products, fixed(output(entries...)).read)
	l.Select("one@example.test", "/backend", "US")
	first := mustGet(t, l, "all", "", false)
	if len(first.Products) != 60 || first.Cursor == "" {
		t.Fatal("first page")
	}
	if last := mustGet(t, l, "all", first.Cursor, false); len(last.Products) != 1 || last.Cursor != "" {
		t.Error("last page")
	}
	if _, err := l.Get(context.Background(), "owned", first.Cursor, false); err == nil || !strings.Contains(err.Error(), "Invalid") {
		t.Errorf("filter binding: %v", err)
	}
	mustGet(t, l, "all", "", true)
	if _, err := l.Get(context.Background(), "all", first.Cursor, false); err == nil || !strings.Contains(err.Error(), "expired") {
		t.Errorf("snapshot binding: %v", err)
	}
}
