package install

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"xodus-nexus-wails/internal/shared"
)

const (
	rootID    = "9AAAAAAAAAAA"
	contentID = "11111111-1111-4111-8111-111111111111"
	otherID   = "22222222-2222-4222-8222-222222222222"
)

func choiceJSON(extra map[string]any) map[string]any {
	choice := map[string]any{"productId": rootID, "contentId": contentID, "title": "Test game", "platform": "Windows.Desktop",
		"packageFormat": "MSIXVC", "kind": "Game", "supported": true}
	for k, v := range extra {
		if v == nil {
			delete(choice, k)
		} else {
			choice[k] = v
		}
	}
	return choice
}

func planJSON(status string, choices ...map[string]any) map[string]any {
	if choices == nil {
		choices = []map[string]any{choiceJSON(nil)}
	}
	return map[string]any{"schema": 1, "rootProductId": rootID, "market": "US", "status": status, "choices": choices}
}

func parsePlan(value map[string]any) (shared.InstallPlan, error) {
	encoded, _ := json.Marshal(value)
	return ParseInstallPlan(string(encoded), rootID, "US")
}

func str(s string) *string { return &s }

func TestInstallSelection(t *testing.T) {
	plan, err := parsePlan(planJSON("ready"))
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := InstallSelection(plan, nil); got != contentID {
		t.Errorf("ready selection = %q", got)
	}
	if _, err := InstallSelection(plan, str(otherID)); err == nil {
		t.Error("foreign content ID accepted")
	}
	multi, _ := parsePlan(planJSON("choice", choiceJSON(nil), choiceJSON(map[string]any{"contentId": otherID})))
	if _, err := InstallSelection(multi, nil); err == nil {
		t.Error("multiple games need an explicit choice")
	}
	if got, _ := InstallSelection(multi, str(contentID)); got != contentID {
		t.Error("explicit choice")
	}
	addOn, _ := parsePlan(planJSON("choice", choiceJSON(map[string]any{"kind": "GameAddOn"})))
	if _, err := InstallSelection(addOn, nil); err == nil {
		t.Error("add-on selected automatically")
	}
	durable := map[string]any{"kind": "Durable", "parentProductId": "9BBBBBBBBBBB", "mainPackageFamilyName": "Example.Base_hash"}
	selected, err := parsePlan(planJSON("choice", choiceJSON(durable)))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := InstallSelection(selected, nil); err == nil {
		t.Error("Durable selected automatically")
	}
	if got, _ := InstallSelection(selected, str(contentID)); got != contentID {
		t.Error("explicit Durable")
	}
	for _, change := range []map[string]any{{"parentProductId": nil}, {"parentProductId": "bad"}, {"mainPackageFamilyName": nil}, {"mainPackageFamilyName": "../outside"}} {
		fields := map[string]any{}
		for k, v := range durable {
			fields[k] = v
		}
		for k, v := range change {
			fields[k] = v
		}
		if _, err := parsePlan(planJSON("choice", choiceJSON(fields))); err == nil {
			t.Errorf("Durable %v accepted", change)
		}
	}
	unsupported, _ := parsePlan(planJSON("unavailable", choiceJSON(map[string]any{"supported": false, "platform": "Xbox.One"})))
	if _, err := InstallSelection(unsupported, str(contentID)); err == nil {
		t.Error("console package selected")
	}
}

func TestParseInstallPlanRejects(t *testing.T) {
	cases := []map[string]any{planJSON("ready"), planJSON("ready"), planJSON("ready"), planJSON("choice"),
		planJSON("ready", choiceJSON(nil), choiceJSON(nil)), planJSON("ready", choiceJSON(map[string]any{"platform": "Xbox.One"})),
		planJSON("ready", choiceJSON(map[string]any{"title": "Bad\nlabel"})), planJSON("ready", choiceJSON(map[string]any{"packageFormat": "APPX"}))}
	cases[0]["rootProductId"] = "9BBBBBBBBBBB"
	cases[1]["market"] = "CZ"
	cases[2]["schema"] = 2
	for i, value := range cases {
		if _, err := parsePlan(value); err == nil {
			t.Errorf("case %d accepted", i)
		}
	}
}

func executable(t *testing.T, script string) string {
	file := filepath.Join(t.TempDir(), "cli")
	os.WriteFile(file, []byte("#!/bin/sh\n"+script), 0o700)
	return file
}

func TestReadInstallPlan(t *testing.T) {
	value := planJSON("ready", choiceJSON(map[string]any{"title": "Příliš žluťoučký"}))
	encoded, _ := json.Marshal(value)
	cli := executable(t, `[ "$*" = "resolve-package `+rootID+` --json --market US" ] || exit 2
cat >/dev/null
printf '%s' '`+string(encoded)+`'
`)
	plan, err := ReadInstallPlan(context.Background(), cli, rootID, "US", false)
	if err != nil || plan.Choices[0].Title != "Příliš žluťoučký" {
		t.Errorf("plan = %+v, %v", plan, err)
	}
	dlc := executable(t, `case "$*" in *--include-dlc*) ;; *) exit 2;; esac
printf '%s' '`+string(encoded)+`'
`)
	if plan, err := ReadInstallPlan(context.Background(), dlc, rootID, "US", true); err != nil || plan.Status != "ready" {
		t.Errorf("dlc = %v", err)
	}
	if _, err := ReadInstallPlan(context.Background(), dlc, rootID, "US", false); err == nil {
		t.Error("DLC flag must be explicit")
	}
	huge := executable(t, `head -c 300000 /dev/zero | tr '\0' x`)
	if _, err := ReadInstallPlan(context.Background(), huge, rootID, "US", false); err == nil || !strings.Contains(err.Error(), "too large") {
		t.Errorf("huge: %v", err)
	}
	failed := executable(t, "echo 'Store unavailable' >&2; exit 1")
	if _, err := ReadInstallPlan(context.Background(), failed, rootID, "US", false); err == nil || !strings.Contains(err.Error(), "Store unavailable") {
		t.Errorf("failed: %v", err)
	}
	if _, err := ReadInstallPlan(context.Background(), "/must-not-run", "--help", "US", false); err == nil || !strings.Contains(err.Error(), "Product ID") {
		t.Errorf("invalid input: %v", err)
	}
}

func TestContentParentForChoice(t *testing.T) {
	durable := shared.InstallChoice{ProductID: rootID, ContentID: contentID, Kind: "Durable", Supported: true, Platform: "Windows.Desktop",
		PackageFormat: "MSIXVC", ParentProductID: "9BBBBBBBBBBB", MainPackageFamilyName: "Example.Base_hash"}
	game := shared.Game{ID: "parent", StoreID: "9BBBBBBBBBBB", PackageName: "Example.Base", InstallState: "ready"}
	if id, err := ContentParentForChoice(durable, []shared.Game{game}); err != nil || id != "parent" {
		t.Errorf("parent = %q, %v", id, err)
	}
	if id, err := ContentParentForChoice(shared.InstallChoice{Kind: "Game"}, nil); err != nil || id != "" {
		t.Error("games have no parent")
	}
	copyGame, wrongStore, foreign, incomplete := game, game, game, game
	copyGame.ID, wrongStore.StoreID, foreign.PackageName, incomplete.InstallState = "copy", rootID, "Foreign.Base", "incomplete"
	for i, games := range [][]shared.Game{{}, {game, copyGame}, {wrongStore}, {foreign}, {incomplete}} {
		if _, err := ContentParentForChoice(durable, games); err == nil || !strings.Contains(err.Error(), "matching base") {
			t.Errorf("case %d: %v", i, err)
		}
	}
}

func TestPlansCache(t *testing.T) {
	now := time.Unix(0, 0)
	var calls atomic.Int32
	cache := NewPlans(func(context.Context, string, string, string, bool) (shared.InstallPlan, error) {
		calls.Add(1)
		time.Sleep(5 * time.Millisecond)
		return parsePlan(planJSON("ready"))
	}, func() time.Time { return now })
	results := make(chan shared.InstallPlan, 2)
	for range 2 {
		go func() { plan, _ := cache.Get(context.Background(), "/cli", rootID, "US", false); results <- plan }()
	}
	one, two := <-results, <-results
	if calls.Load() != 1 {
		t.Errorf("deduplicated calls = %d", calls.Load())
	}
	one.Choices[0].Title = "mutated"
	if two.Choices[0].Title != "Test game" {
		t.Error("results must be cloned")
	}
	cache.Get(context.Background(), "/other-cli", rootID, "US", false)
	cache.Get(context.Background(), "/cli", rootID, "DE", false)
	if calls.Load() != 3 {
		t.Errorf("scoped calls = %d", calls.Load())
	}
	now = now.Add(time.Minute)
	cache.Get(context.Background(), "/cli", rootID, "US", false)
	cache.Get(context.Background(), "/cli", rootID, "US", true)
	if calls.Load() != 5 {
		t.Errorf("TTL calls = %d", calls.Load())
	}

	fail := true
	var failures atomic.Int32
	failing := NewPlans(func(context.Context, string, string, string, bool) (shared.InstallPlan, error) {
		failures.Add(1)
		if fail {
			return shared.InstallPlan{}, errors.New("failure")
		}
		return parsePlan(planJSON("ready"))
	}, nil)
	if _, err := failing.Get(context.Background(), "/cli", rootID, "US", false); err == nil {
		t.Error("failure expected")
	}
	fail = false
	if plan, err := failing.Get(context.Background(), "/cli", rootID, "US", false); err != nil || plan.Status != "ready" || failures.Load() != 2 {
		t.Error("failures must not be cached")
	}
	if _, err := failing.Get(context.Background(), "/cli", "../outside", "US", false); err == nil || !strings.Contains(err.Error(), "scope") || failures.Load() != 2 {
		t.Error("invalid scope executed")
	}
}
