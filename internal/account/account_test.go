package account

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"xodus-nexus-wails/internal/diagnostics"
	"xodus-nexus-wails/internal/shared"
)

const avatar = "data:image/png;base64,iVBORw0KGgo="

func profileResult(account string) string {
	return fmt.Sprintf(`{"schema":1,"account":%q,"gamertag":"ExampleGamer","gamerpicDataUrl":%q}`, account, avatar)
}

func identity(s string) string { return s }

func fixed(text string, err error) ProfileReader {
	return func(context.Context, string) (string, error) { return text, err }
}

func TestProfileProcessRunsOnceForRepeatedRefresh(t *testing.T) {
	root := t.TempDir()
	binary := filepath.Join(root, "xodus-cli")
	log := diagnostics.NewBackend(root, nil, 0)
	diagnostics.SetBackend(log)
	defer diagnostics.SetBackend(nil)
	os.WriteFile(binary, []byte("#!/bin/sh\nsleep 0.1\nprintf '%s\\n' '"+profileResult("one@example.test")+"'\n"), 0o700)
	profile := NewProfiles(root, func() {}, identity, nil)
	for range 5 {
		profile.Select("one@example.test", binary, true)
	}
	profile.Settled()
	if value := profile.Value(); value == nil || value.Gamertag != "ExampleGamer" {
		t.Fatalf("profile = %+v", value)
	}
	content, _ := log.Read()
	exits := strings.Count(content.Content, `"operation":"profile","event":"exit"`)
	if exits != 1 || !strings.Contains(content.Content, `"code":0`) || strings.Contains(content.Content, "SIGKILL") || strings.Contains(content.Content, "one@example.test") {
		t.Errorf("log = %s", content.Content)
	}
}

type blockingReader struct {
	mu    sync.Mutex
	jobs  []blockingJob
	reads atomic.Int32
}

type blockingJob struct {
	binary string
	ctx    context.Context
	finish chan string
}

func (b *blockingReader) read(ctx context.Context, binary string) (string, error) {
	b.reads.Add(1)
	job := blockingJob{binary, ctx, make(chan string, 1)}
	b.mu.Lock()
	b.jobs = append(b.jobs, job)
	b.mu.Unlock()
	select {
	case text := <-job.finish:
		return text, nil
	case <-ctx.Done():
		return <-job.finish, nil
	}
}

func (b *blockingReader) job(i int) blockingJob {
	for {
		b.mu.Lock()
		if len(b.jobs) > i {
			job := b.jobs[i]
			b.mu.Unlock()
			return job
		}
		b.mu.Unlock()
		time.Sleep(time.Millisecond)
	}
}

func TestProfileCoalescesAndCancelsObsoleteBackend(t *testing.T) {
	reader := &blockingReader{}
	profile := NewProfiles(t.TempDir(), func() {}, identity, reader.read)
	for range 5 {
		profile.Select("one@example.test", "/backend", true)
	}
	first := reader.job(0)
	if reader.reads.Load() != 1 || first.ctx.Err() != nil {
		t.Fatal("refreshes must share one request")
	}
	first.finish <- profileResult("one@example.test")
	profile.Settled()
	if profile.Value().Gamertag != "ExampleGamer" {
		t.Error("profile not published")
	}

	switching := &blockingReader{}
	profile = NewProfiles(t.TempDir(), func() {}, identity, switching.read)
	profile.Select("one@example.test", "/old", false)
	old := switching.job(0)
	profile.Select("one@example.test", "/new", false)
	next := switching.job(1)
	if old.ctx.Err() == nil {
		t.Error("obsolete backend request must be cancelled")
	}
	old.finish <- profileResult("one@example.test")
	time.Sleep(10 * time.Millisecond)
	if profile.Value() != nil {
		t.Error("late result published")
	}
	next.finish <- profileResult("one@example.test")
	profile.Settled()
	if profile.Value() == nil {
		t.Error("current backend result missing")
	}
}

func TestProfileCacheBoundToAccount(t *testing.T) {
	root := t.TempDir()
	profile := NewProfiles(root, func() {}, identity, fixed(profileResult("one@example.test"), nil))
	profile.Select("one@example.test", "/fake/backend", false)
	profile.Settled()
	if value := profile.Value(); value == nil || *value != (Profile{Gamertag: "ExampleGamer", AvatarDataURL: avatar}) {
		t.Fatalf("value = %+v", value)
	}
	file := filepath.Join(root, "xbox-profile.json")
	if info, _ := os.Stat(file); info.Mode().Perm() != 0o600 {
		t.Error("cache must be private")
	}
	if data, _ := os.ReadFile(file); strings.Contains(string(data), "one@example.test") {
		t.Error("cache contains the account")
	}
	offline := NewProfiles(root, func() {}, identity, fixed("", errors.New("offline")))
	offline.Select("one@example.test", "/fake/backend", false)
	offline.Settled()
	if offline.Value() == nil || offline.Value().Gamertag != "ExampleGamer" {
		t.Error("offline cache not reused")
	}
	offline.Select("", "", false)
	if offline.Value() != nil {
		t.Error("logout keeps profile")
	}
	if _, err := os.Stat(file); err == nil {
		t.Error("logout keeps cache")
	}
}

func TestProfileLateResultAfterAccountChange(t *testing.T) {
	root := t.TempDir()
	reader := &blockingReader{}
	profile := NewProfiles(root, func() {}, identity, reader.read)
	profile.Select("one@example.test", "/fake/backend", false)
	job := reader.job(0)
	profile.Select("two@example.test", "", false)
	job.finish <- profileResult("one@example.test")
	time.Sleep(10 * time.Millisecond)
	if profile.Value() != nil {
		t.Error("late result published for another account")
	}
	wrong := NewProfiles(root, func() {}, identity, fixed(profileResult("one@example.test"), nil))
	wrong.Select("two@example.test", "/fake/backend", false)
	wrong.Settled()
	if wrong.Value() != nil {
		t.Error("mismatched account accepted")
	}
}

func TestProfileFallbacks(t *testing.T) {
	root := t.TempDir()
	var called atomic.Bool
	old := NewProfiles(root, func() {}, identity, func(context.Context, string) (string, error) { called.Store(true); return "", nil })
	old.Select("one@example.test", "", false)
	if called.Load() {
		t.Error("no backend, no read")
	}
	for _, output := range []string{"bad json", `{"schema":1,"account":"one@example.test","gamertag":"\nbad"}`} {
		profile := NewProfiles(root, func() {}, identity, fixed(output, nil))
		profile.Select("one@example.test", "/fake/backend", false)
		profile.Settled()
		if profile.Value() != nil {
			t.Errorf("%q accepted", output)
		}
	}
}

func TestProfileSubscription(t *testing.T) {
	root := t.TempDir()
	checked := shared.JSISOString(time.Now())
	subscription := fmt.Sprintf(`{"status":"active","productIds":["CFQ7TTC0KGQ8"],"checkedAt":%q}`, checked)
	output := strings.TrimSuffix(profileResult("one@example.test"), "}") + `,"subscription":` + subscription + "}"
	profile := NewProfiles(root, func() {}, identity, fixed(output, nil))
	profile.Select("one@example.test", "/fake/backend", false)
	profile.Settled()
	want := shared.GamePassSubscription{Status: "active", ProductIDs: []string{"CFQ7TTC0KGQ8"}, CheckedAt: checked}
	if value := profile.Value(); value == nil || value.Subscription == nil || fmt.Sprint(*value.Subscription) != fmt.Sprint(want) {
		t.Fatalf("subscription = %+v", value)
	}
	offline := NewProfiles(root, func() {}, identity, nil)
	offline.Select("one@example.test", "", false)
	if value := offline.Value(); value == nil || value.Subscription == nil {
		t.Error("cached subscription lost")
	}
	offline.Select("two@example.test", "", false)
	if offline.Value() != nil {
		t.Error("another account's subscription shown")
	}
}

func TestRefreshCoalescesAndDiscardsObsolete(t *testing.T) {
	var gate Refresh
	finish := make(chan string)
	var reads, publishes atomic.Int32
	var published atomic.Value
	var group sync.WaitGroup
	for range 5 {
		group.Add(1)
		go func() {
			defer group.Done()
			Run(&gate, "same", func(context.Context) (string, error) { reads.Add(1); return <-finish, nil },
				func(v string) error { publishes.Add(1); published.Store(v); return nil }, func() bool { return true })
		}()
	}
	time.Sleep(20 * time.Millisecond)
	finish <- "current"
	group.Wait()
	if reads.Load() != 1 || publishes.Load() != 1 || published.Load() != "current" {
		t.Errorf("reads %d publishes %d", reads.Load(), publishes.Load())
	}

	var stored atomic.Bool
	release := make(chan bool)
	done := make(chan error)
	go func() {
		done <- Run(&gate, "backend", func(context.Context) (bool, error) { return <-release, nil }, func(v bool) error { stored.Store(v); return nil }, func() bool { return true })
	}()
	time.Sleep(10 * time.Millisecond)
	gate.Invalidate()
	Run(&gate, "backend", func(context.Context) (bool, error) { return true, nil }, func(v bool) error { stored.Store(v); return nil }, func() bool { return true })
	release <- false
	<-done
	if !stored.Load() {
		t.Error("pre-login response overwrote the newer account")
	}

	var count atomic.Int32
	publish := func(int) error { count.Add(1); return nil }
	Run(&gate, "old", func(context.Context) (int, error) { return 1, nil }, publish, func() bool { return false })
	if err := Run(&gate, "new", func(context.Context) (int, error) { return 0, errors.New("failed") }, publish, func() bool { return true }); err == nil {
		t.Error("live failure must be reported")
	}
	Run(&gate, "new", func(context.Context) (int, error) { return 2, nil }, publish, func() bool { return true })
	if count.Load() != 1 {
		t.Errorf("publishes = %d", count.Load())
	}

	cancelled := make(chan error)
	var observed atomic.Bool
	go func() {
		cancelled <- Run(&gate, "older", func(ctx context.Context) (string, error) {
			<-ctx.Done()
			observed.Store(true)
			return "", errors.New("aborted")
		}, func(string) error { t.Error("obsolete read published"); return nil }, func() bool { return true })
	}()
	time.Sleep(10 * time.Millisecond)
	Run(&gate, "newer", func(context.Context) (string, error) { return "current", nil }, func(string) error { return nil }, func() bool { return true })
	if err := <-cancelled; err != nil || !observed.Load() {
		t.Errorf("obsolete rejection: %v", err)
	}
}

func TestReadAccount(t *testing.T) {
	binary := filepath.Join(t.TempDir(), "cli")
	os.WriteFile(binary, []byte(`#!/bin/sh
printf '{"stored":true,"username":"sample@example.test"}'
printf '{"stored":false}' >&2
`), 0o700)
	result, err := ReadAccount(context.Background(), binary)
	var parsed map[string]any
	if err != nil || json.Unmarshal([]byte(result.Output), &parsed) != nil || parsed["stored"] != true {
		t.Errorf("result = %+v, %v", result, err)
	}
	os.WriteFile(binary, []byte("#!/bin/sh\nexec sleep 30\n"), 0o700)
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(20 * time.Millisecond); cancel() }()
	if result, _ := ReadAccount(ctx, binary); result.Code == 0 {
		t.Error("cancelled read reported success")
	}
}

func achievementItem(patch map[string]any) map[string]any {
	item := map[string]any{"id": "service:1", "name": "First step", "description": "Finish a level", "state": "unlocked", "secret": false, "revoked": false,
		"gamerscore": 10, "unlockedAt": "2026-01-01T12:00:00Z", "iconUrl": "https://images-eds-ssl.xboxlive.com/image", "progress": nil}
	for k, v := range patch {
		item[k] = v
	}
	return item
}

func achievementPayload(patch map[string]any) string {
	value := map[string]any{"schema": 1, "account": "example@test", "titleId": "42", "partial": false, "items": []any{achievementItem(nil)}}
	for k, v := range patch {
		value[k] = v
	}
	encoded, _ := json.Marshal(value)
	return string(encoded)
}

func TestParseGameAchievements(t *testing.T) {
	parse := func(patch map[string]any) (shared.GameAchievements, error) {
		return ParseGameAchievements(achievementPayload(patch), "example@test", "42")
	}
	zero, _ := parse(map[string]any{"items": []any{achievementItem(map[string]any{"gamerscore": 0, "iconUrl": nil})}})
	if *zero.Items[0].Gamerscore != 0 {
		t.Error("zero gamerscore")
	}
	missing, _ := parse(map[string]any{"items": []any{achievementItem(map[string]any{"gamerscore": nil, "iconUrl": nil, "unlockedAt": nil})}, "partial": true})
	if !missing.Partial || missing.Items[0].Gamerscore != nil {
		t.Error("missing values")
	}
	if empty, _ := parse(map[string]any{"items": []any{}}); empty.Status != "unavailable" {
		t.Error("empty is unavailable")
	}
	if partial, _ := parse(map[string]any{"items": []any{}, "partial": true}); partial.Status != "available" {
		t.Error("partial empty is available")
	}
	many := []any{}
	for i := range 501 {
		many = append(many, achievementItem(map[string]any{"id": fmt.Sprint(i)}))
	}
	for i, patch := range []map[string]any{{"account": "other"}, {"titleId": "43"}, {"schema": 2}, {"partial": "true"}, {"items": []any{nil}},
		{"items": []any{achievementItem(nil), achievementItem(nil)}}, {"items": many}} {
		if _, err := parse(patch); err == nil {
			t.Errorf("envelope %d accepted", i)
		}
	}
	for _, patch := range []map[string]any{{"state": "guess"}, {"secret": "false"}, {"name": ""}, {"gamerscore": -1}, {"gamerscore": 1.5}, {"gamerscore": "10"},
		{"progress": 101}, {"unlockedAt": "tomorrow"}, {"unlockedAt": "9999-01-01T00:00:00Z"}, {"revoked": true}, {"state": "locked"}} {
		if _, err := parse(map[string]any{"items": []any{achievementItem(patch)}}); err == nil {
			t.Errorf("item %v accepted", patch)
		}
	}
	for _, iconURL := range []string{"file:///etc/passwd", "https://evil.test/icon", "https://user:secret@images-eds-ssl.xboxlive.com/icon", "http://images-eds-ssl.xboxlive.com/icon"} {
		if value, _ := parse(map[string]any{"items": []any{achievementItem(map[string]any{"iconUrl": iconURL})}}); value.Items[0].IconURL != "" {
			t.Errorf("unsafe icon %s exposed", iconURL)
		}
	}
}

func TestAchievementsCache(t *testing.T) {
	var reads atomic.Int32
	data := NewAchievements(func(context.Context, string, string) (string, error) {
		reads.Add(1)
		time.Sleep(5 * time.Millisecond)
		return achievementPayload(nil), nil
	})
	data.Select("example@test", "/example/cli")
	results := make(chan shared.GameAchievements, 2)
	for range 2 {
		go func() { value, _ := data.Get("42", false); results <- value }()
	}
	a, b := <-results, <-results
	if reads.Load() != 1 {
		t.Errorf("reads = %d", reads.Load())
	}
	a.Items[0].Name = "changed"
	if b.Items[0].Name != "First step" {
		t.Error("results must be independent clones")
	}
	if cached, _ := data.Get("42", false); cached.Items[0].Name != "First step" {
		t.Error("cache mutated")
	}
	data.Get("42", true)
	if reads.Load() != 2 {
		t.Error("refresh must reread")
	}
	data.Select("other", "/example/cli")
	if value, _ := data.Get("42", false); value.Status != "error" {
		t.Error("other account must not accept the response")
	}
	data.Select("", "")
	if value, _ := data.Get("42", false); value.Status != "unsupported" {
		t.Error("signed out")
	}
}

func TestAchievementsBoundsAndStaleReads(t *testing.T) {
	var mu sync.Mutex
	var pending []chan string
	var contexts []context.Context
	data := NewAchievements(func(ctx context.Context, _, _ string) (string, error) {
		finish := make(chan string, 1)
		mu.Lock()
		pending = append(pending, finish)
		contexts = append(contexts, ctx)
		mu.Unlock()
		return <-finish, nil
	})
	data.Select("example@test", "/example/cli")
	a, b := make(chan string), make(chan string)
	go func() { v, _ := data.Get("42", false); a <- v.Status }()
	go func() { v, _ := data.Get("43", false); b <- v.Status }()
	for {
		mu.Lock()
		n := len(pending)
		mu.Unlock()
		if n == 2 {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if value, _ := data.Get("44", false); value.Status != "error" {
		t.Error("concurrent requests must be bounded")
	}
	for _, id := range []string{"0", "42;id", "../42", "4294967296"} {
		if _, err := data.Get(id, false); err == nil {
			t.Errorf("title %q accepted", id)
		}
	}
	data.Select("other", "/example/cli")
	for _, ctx := range contexts {
		if ctx.Err() == nil {
			t.Error("stale request not aborted")
		}
	}
	for _, finish := range pending {
		finish <- achievementPayload(nil)
	}
	if <-a != "error" || <-b != "error" {
		t.Error("stale results accepted")
	}

	var failures atomic.Int32
	failing := NewAchievements(func(context.Context, string, string) (string, error) {
		failures.Add(1)
		return "", errors.New("synthetic failure")
	})
	failing.Select("example@test", "/example/cli")
	for range 4 {
		if value, _ := failing.Get("42", false); value.Status != "error" {
			t.Error("failure status")
		}
	}
	if failures.Load() != 4 {
		t.Error("failed readers must not stay pending")
	}
}

func activityPayload(patch map[string]any) string {
	value := map[string]any{"schema": 1, "account": "example@test", "titleId": "42", "minutesPlayed": 2052, "lastPlayed": "2026-01-01T12:00:00Z", "partial": false}
	for k, v := range patch {
		value[k] = v
	}
	encoded, _ := json.Marshal(value)
	return string(encoded)
}

func TestParseGameActivity(t *testing.T) {
	parse := func(raw string) (shared.GameActivity, error) { return ParseGameActivity(raw, "example@test", "42") }
	if value, _ := parse(activityPayload(nil)); value.Status != "available" || *value.MinutesPlayed != 2052 {
		t.Errorf("available = %+v", value)
	}
	if value, _ := parse(activityPayload(map[string]any{"minutesPlayed": 0})); *value.MinutesPlayed != 0 {
		t.Error("genuine zero")
	}
	if value, _ := parse(activityPayload(map[string]any{"minutesPlayed": nil, "lastPlayed": nil})); value.Status != "unavailable" || value.MinutesPlayed != nil {
		t.Error("unavailable")
	}
	if value, _ := parse(activityPayload(map[string]any{"minutesPlayed": nil})); value.Status != "available" || value.LastPlayed != "2026-01-01T12:00:00.000Z" {
		t.Errorf("last played = %+v", value)
	}
	for _, raw := range []string{activityPayload(map[string]any{"account": "other@test"}), activityPayload(map[string]any{"titleId": "43"}),
		activityPayload(map[string]any{"schema": 2}), activityPayload(map[string]any{"minutesPlayed": -1}), activityPayload(map[string]any{"minutesPlayed": "42"}),
		strings.Replace(activityPayload(nil), "2052", "1e999", 1), activityPayload(map[string]any{"minutesPlayed": 1e12}),
		activityPayload(map[string]any{"lastPlayed": "tomorrow"}), activityPayload(map[string]any{"lastPlayed": "9999-01-01T00:00:00Z"}),
		activityPayload(map[string]any{"partial": "yes"})} {
		if _, err := parse(raw); err == nil {
			t.Errorf("accepted %s", raw)
		}
	}
}

func TestActivitiesCache(t *testing.T) {
	var reads atomic.Int32
	activities := NewActivities(func(context.Context, string, string) (string, error) { reads.Add(1); return activityPayload(nil), nil })
	for _, id := range []string{"0", "-1", "0x2a", "42;id", "4294967296", "../42"} {
		if _, err := activities.Get(id, false); err == nil {
			t.Errorf("title %q accepted", id)
		}
	}
	if reads.Load() != 0 {
		t.Error("invalid title started a read")
	}
	activities.Select("example@test", "/example/cli")
	a, _ := activities.Get("42", false)
	*a.MinutesPlayed = 0
	if b, _ := activities.Get("42", false); *b.MinutesPlayed != 2052 || reads.Load() != 1 {
		t.Error("cache")
	}
	activities.Get("42", true)
	if reads.Load() != 2 {
		t.Error("refresh")
	}
	activities.Select("", "")
	if value, _ := activities.Get("42", false); value.Status != "unsupported" {
		t.Error("signed out")
	}
	activities.Select("other@test", "/example/cli")
	if value, _ := activities.Get("42", false); value.Status != "error" {
		t.Error("other account")
	}
}

func TestInstalledTitleID(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "MicrosoftGame.Config")
	os.WriteFile(file, []byte("<Game><!-- <TitleId>DEAD</TitleId> --><TitleId>0000002A</TitleId></Game>"), 0o644)
	if got := InstalledTitleID(dir); got != "42" {
		t.Errorf("title = %q", got)
	}
	os.WriteFile(file, []byte("<Game><TitleId>FFFFFFFFF</TitleId></Game>"), 0o644)
	if InstalledTitleID(dir) != "" {
		t.Error("9-digit hex accepted")
	}
	os.WriteFile(file, []byte(strings.Repeat("x", 1_000_001)), 0o644)
	if InstalledTitleID(dir) != "" {
		t.Error("oversized manifest accepted")
	}
}
