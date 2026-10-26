package library

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync/atomic"
	"testing"

	"xodus-nexus-wails/internal/catalog"
)

func storeRecord(productID, kind, format string) map[string]any {
	return map[string]any{"ProductId": productID, "ProductType": kind, "LocalizedProperties": []any{map[string]any{"ProductTitle": productID}},
		"DisplaySkuAvailabilities": []any{map[string]any{"Sku": map[string]any{"Properties": map[string]any{
			"Packages": []any{map[string]any{"PackageFormat": format, "PlatformDependencies": []any{map[string]any{"PlatformName": "Windows.Desktop"}}}}}}}}}
}

func serve(records ...map[string]any) catalog.Fetcher {
	body, _ := json.Marshal(map[string]any{"Products": records})
	return func(context.Context, string, int64) (catalog.Response, error) {
		return catalog.Response{Status: 200, Body: body}, nil
	}
}

func withService(fetch catalog.Fetcher) Catalog {
	return catalog.NewService(fetch).LicensedProducts
}

func TestConditionalGamesThroughRealCatalog(t *testing.T) {
	l := New(withService(serve(storeRecord(A, "Game", "MSIXVC"), storeRecord(B, "Game", "EAppxBundle"), storeRecord(C, "Durable", "MSIXVC"))),
		fixed(output(mkrow(A, positron(nil)), mkrow(B, positron(nil)), mkrow(C, positron(nil)))).read)
	l.Select("one@example.test", "/not-executed", "US")
	all := mustGet(t, l, "all", "", false)
	byID := map[string]string{}
	for _, p := range all.Products {
		byID[p.ProductID] = p.Access + "/" + p.Installability
	}
	if len(all.Products) != 2 || byID[A] != "owned/supported" || byID[B] != "owned/unsupported" {
		t.Errorf("all = %v", byID)
	}
	if len(mustGet(t, l, "owned", "", false).Products) != 2 || len(mustGet(t, l, "other", "", false).Products) != 0 {
		t.Error("filters")
	}
}

func TestUnsupportedAndUnknownOwnedGames(t *testing.T) {
	E, F := "9EEEEEEEEEEE", "9FFFFFFFFFFF"
	incomplete := map[string]any{"ProductId": E, "ProductType": "Game", "LocalizedProperties": []any{map[string]any{"ProductTitle": "Incomplete edition"}},
		"DisplaySkuAvailabilities": []any{map[string]any{"Sku": map[string]any{"Properties": map[string]any{"BundledSkus": []any{map[string]any{"BigId": F}}}}}}}
	l := New(withService(serve(storeRecord(A, "Game", "EAppxBundle"), storeRecord(B, "Game", "MSIXVC"), storeRecord(C, "Game", "APPX"),
		storeRecord(D, "Durable", "MSIXVC"), incomplete, storeRecord("9UNLICENSED0", "Game", "MSIXVC"))),
		fixed(output(mkrow(A, nil), mkrow(B, nil), mkrow(C, row{"satisfiedByProductId": "CFQ7TTC0KGQ8"}), mkrow(D, nil), mkrow(E, nil))).read)
	l.Select("one@example.test", "/not-executed", "US")
	owned := map[string]string{}
	for _, p := range mustGet(t, l, "owned", "", false).Products {
		owned[p.ProductID] = p.Installability
	}
	if len(owned) != 3 || owned[A] != "unsupported" || owned[B] != "supported" || owned[E] != "unknown" {
		t.Errorf("owned = %v", owned)
	}
	gamepass := mustGet(t, l, "gamepass", "", false).Products
	if len(gamepass) != 1 || gamepass[0].ProductID != C || gamepass[0].Installability != "unsupported" || len(mustGet(t, l, "all", "", false).Products) != 4 {
		t.Errorf("gamepass = %+v", gamepass)
	}
}

func TestCatalogFailureIsNotCachedAsEmpty(t *testing.T) {
	for _, failure := range []string{"network", "http", "json", "shape"} {
		var healthy atomic.Bool
		var calls atomic.Int32
		healthyBody, _ := json.Marshal(map[string]any{"Products": []any{storeRecord(A, "Game", "MSIXVC")}})
		fetch := func(context.Context, string, int64) (catalog.Response, error) {
			calls.Add(1)
			if !healthy.Load() {
				switch failure {
				case "network":
					return catalog.Response{}, errors.New("private signed URL must not appear")
				case "http":
					return catalog.Response{Status: 503}, nil
				case "json":
					return catalog.Response{Status: 200, Body: []byte("{")}, nil
				}
				return catalog.Response{Status: 200, Body: []byte("{}")}, nil
			}
			return catalog.Response{Status: 200, Body: healthyBody}, nil
		}
		l := New(withService(fetch), fixed(output()).read)
		l.Select("one@example.test", "/backend", "US")
		if _, err := l.Get(context.Background(), "all", "", false); err == nil || !strings.Contains(err.Error(), "game details could not be loaded") {
			t.Errorf("%s: %v", failure, err)
		}
		if access, _ := l.Access(false); access.AccessByProductID[A] != "owned" {
			t.Errorf("%s: access survives catalog failure", failure)
		}
		healthy.Store(true)
		if got := ids(mustGet(t, l, "all", "", false)); len(got) != 1 || got[0] != A || calls.Load() != 2 {
			t.Errorf("%s: recovered %v after %d calls", failure, got, calls.Load())
		}
	}
}
