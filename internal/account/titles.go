package account

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"xodus-nexus-wails/internal/manifest"
	"xodus-nexus-wails/internal/shared"
)

var (
	titlePattern = regexp.MustCompile(`^[1-9]\d{0,9}$`)
	textControl  = regexp.MustCompile(`[\x00-\x08\x0b\x0c\x0e-\x1f\x7f]`)
	isoPrefix    = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}T`)
	hexTitle     = regexp.MustCompile(`(?i)^[a-f0-9]{1,8}$`)
)

var iconHosts = map[string]bool{"images-eds-ssl.xboxlive.com": true, "images-eds.xboxlive.com": true, "store-images.s-microsoft.com": true}

func validTitleID(titleID string) error {
	if !titlePattern.MatchString(titleID) {
		return errors.New("Invalid Xbox title ID.")
	}
	if value, _ := strconv.ParseUint(titleID, 10, 64); value > 0xffffffff {
		return errors.New("Invalid Xbox title ID.")
	}
	return nil
}

func text(raw json.RawMessage, max int) (string, bool) {
	var value string
	trimmed := strings.TrimSpace(string(raw))
	if !strings.HasPrefix(trimmed, `"`) || json.Unmarshal(raw, &value) != nil {
		return "", false
	}
	return value, shared.UTF16Len(value) <= max && !textControl.MatchString(value)
}

func isNull(raw json.RawMessage) bool { return strings.TrimSpace(string(raw)) == "null" }

func jsonBool(raw json.RawMessage) (bool, bool) {
	switch strings.TrimSpace(string(raw)) {
	case "true":
		return true, true
	case "false":
		return false, true
	}
	return false, false
}

func number(raw json.RawMessage) (float64, bool) {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" || trimmed[0] == '"' {
		return 0, false
	}
	var value float64
	return value, json.Unmarshal(raw, &value) == nil
}

// recentDate validates an ISO timestamp no more than five minutes in the future.
func recentDate(raw json.RawMessage) (string, bool) {
	value, ok := text(raw, 40)
	if !ok || !isoPrefix.MatchString(value) {
		return "", false
	}
	parsed, ok := shared.ParseJSDate(value)
	if !ok || parsed.After(time.Now().Add(5*time.Minute)) {
		return "", false
	}
	return shared.JSISOString(parsed), true
}

func icon(value string) string {
	parsed, err := url.Parse(value)
	if err != nil || parsed.Scheme != "https" || parsed.User != nil || parsed.Port() != "" || !iconHosts[parsed.Hostname()] {
		return ""
	}
	return parsed.String()
}

func ParseGameAchievements(raw, account, titleID string) (shared.GameAchievements, error) {
	if len(raw) > 4_000_000 {
		return shared.GameAchievements{}, errors.New("Achievements too large.")
	}
	var value map[string]json.RawMessage
	if err := json.Unmarshal([]byte(raw), &value); err != nil {
		return shared.GameAchievements{}, err
	}
	invalid := errors.New("Invalid achievements response.")
	var items []map[string]json.RawMessage
	resultAccount, _ := text(value["account"], 1<<20)
	resultTitle, _ := text(value["titleId"], 64)
	partial, partialOK := jsonBool(value["partial"])
	if value == nil || strings.TrimSpace(string(value["schema"])) != "1" || resultAccount != account || resultTitle != titleID || !partialOK ||
		json.Unmarshal(value["items"], &items) != nil || items == nil || len(items) > 500 {
		return shared.GameAchievements{}, invalid
	}
	ids := map[string]bool{}
	out := []shared.GameAchievement{}
	for _, item := range items {
		id, idOK := text(item["id"], 180)
		name, nameOK := text(item["name"], 256)
		description, descriptionOK := text(item["description"], 1500)
		var state string
		json.Unmarshal(item["state"], &state)
		secret, secretOK := jsonBool(item["secret"])
		revoked, revokedOK := jsonBool(item["revoked"])
		if item == nil || !idOK || id == "" || ids[id] || !nameOK || name == "" || !descriptionOK ||
			!slices.Contains([]string{"unlocked", "in-progress", "locked"}, state) || !secretOK || !revokedOK {
			return shared.GameAchievements{}, errors.New("Invalid achievement.")
		}
		ids[id] = true
		if revoked && state == "unlocked" {
			return shared.GameAchievements{}, errors.New("Invalid revoked achievement.")
		}
		achievement := shared.GameAchievement{ID: id, Name: name, Description: description, State: state, Secret: secret, Revoked: revoked}
		if !isNull(item["gamerscore"]) {
			score, ok := number(item["gamerscore"])
			if !ok || score != math.Trunc(score) || score < 0 || score > 10_000 {
				return shared.GameAchievements{}, errors.New("Invalid gamerscore.")
			}
			achievement.Gamerscore = &score
		}
		if !isNull(item["progress"]) {
			progress, ok := number(item["progress"])
			if !ok || progress < 0 || progress > 100 {
				return shared.GameAchievements{}, errors.New("Invalid progress.")
			}
			achievement.Progress = &progress
		}
		if !isNull(item["unlockedAt"]) {
			date, ok := recentDate(item["unlockedAt"])
			if !ok || state != "unlocked" {
				return shared.GameAchievements{}, errors.New("Invalid unlock date.")
			}
			achievement.UnlockedAt = date
		}
		if !isNull(item["iconUrl"]) {
			image, ok := text(item["iconUrl"], 2048)
			if !ok {
				return shared.GameAchievements{}, errors.New("Invalid achievement icon.")
			}
			achievement.IconURL = icon(image)
		}
		out = append(out, achievement)
	}
	status := "unavailable"
	if len(out) > 0 || partial {
		status = "available"
	}
	return shared.GameAchievements{Status: status, Items: out, Partial: partial}, nil
}

func ParseGameActivity(raw, account, titleID string) (shared.GameActivity, error) {
	if len(raw) > 64_000 {
		return shared.GameActivity{}, errors.New("Activity too large.")
	}
	var value map[string]json.RawMessage
	if err := json.Unmarshal([]byte(raw), &value); err != nil {
		return shared.GameActivity{}, err
	}
	resultAccount, _ := text(value["account"], 1<<20)
	resultTitle, _ := text(value["titleId"], 64)
	partial, partialOK := jsonBool(value["partial"])
	if value == nil || strings.TrimSpace(string(value["schema"])) != "1" || resultAccount != account || resultTitle != titleID || !partialOK {
		return shared.GameActivity{}, errors.New("Activity identity mismatch.")
	}
	activity := shared.GameActivity{Partial: partial, Status: "available"}
	if !isNull(value["minutesPlayed"]) {
		minutes, ok := number(value["minutesPlayed"])
		if !ok || minutes < 0 || minutes > 1_000_000_000 {
			return shared.GameActivity{}, errors.New("Invalid time played.")
		}
		activity.MinutesPlayed = &minutes
	}
	if !isNull(value["lastPlayed"]) {
		last, ok := recentDate(value["lastPlayed"])
		if !ok {
			return shared.GameActivity{}, errors.New("Invalid last played.")
		}
		activity.LastPlayed = last
	}
	if activity.MinutesPlayed == nil && activity.LastPlayed == "" {
		activity.Status = "unavailable"
	}
	return activity, nil
}

// InstalledTitleID returns the decimal Xbox title ID of an installed game, or "".
func InstalledTitleID(directory string) string {
	entry, ok := manifest.ResolvePackageFile(directory, "MicrosoftGame.config")
	if !ok {
		return ""
	}
	file := filepath.Join(directory, entry)
	info, err := os.Stat(file)
	if err != nil || !info.Mode().IsRegular() || info.Size() > 1_000_000 {
		return ""
	}
	data, err := os.ReadFile(file)
	if err != nil {
		return ""
	}
	title, _ := manifest.TitleID(string(data))
	if !hexTitle.MatchString(title) {
		return ""
	}
	value, _ := strconv.ParseUint(title, 16, 32)
	if value == 0 {
		return ""
	}
	return strconv.FormatUint(value, 10)
}

// TitleReader runs a per-title CLI command.
type TitleReader func(ctx context.Context, binary, titleID string) (string, error)

func ReadAchievements(ctx context.Context, binary, titleID string) (string, error) {
	return runBounded(ctx, binary, []string{"achievements", titleID, "--json"}, 4_000_000, 35*time.Second, true, "Achievements unavailable.")
}

func ReadActivity(ctx context.Context, binary, titleID string) (string, error) {
	return runBounded(ctx, binary, []string{"game-activity", titleID, "--json"}, 64_000, 30*time.Second, true, "Xbox activity unavailable.")
}

type titleRequest[T any] struct {
	cancel context.CancelFunc
	done   chan struct{}
	value  T
}

type titleCache[T any] struct {
	until time.Time
	value T
}

// TitleData is an account-bound, bounded in-memory cache. No disk cache,
// telemetry, unlock writes or cross-account responses.
type TitleData[T any] struct {
	reader      TitleReader
	parse       func(raw, account, titleID string) (T, error)
	failed      T
	unsupported T
	maxRequests int
	maxCache    int
	ttl         time.Duration

	mu         sync.Mutex
	identity   *[2]string
	generation int
	requests   map[string]*titleRequest[T]
	cache      map[string]titleCache[T]
	order      []string
}

func NewAchievements(reader TitleReader) *TitleData[shared.GameAchievements] {
	if reader == nil {
		reader = ReadAchievements
	}
	return &TitleData[shared.GameAchievements]{reader: reader, parse: ParseGameAchievements, failed: shared.GameAchievements{Status: "error"},
		unsupported: shared.GameAchievements{Status: "unsupported"}, maxRequests: 2, maxCache: 32, ttl: 2 * time.Minute,
		requests: map[string]*titleRequest[shared.GameAchievements]{}, cache: map[string]titleCache[shared.GameAchievements]{}}
}

func NewActivities(reader TitleReader) *TitleData[shared.GameActivity] {
	if reader == nil {
		reader = ReadActivity
	}
	return &TitleData[shared.GameActivity]{reader: reader, parse: ParseGameActivity, failed: shared.GameActivity{Status: "error"},
		unsupported: shared.GameActivity{Status: "unsupported"}, maxRequests: 4, maxCache: 100, ttl: 5 * time.Minute,
		requests: map[string]*titleRequest[shared.GameActivity]{}, cache: map[string]titleCache[shared.GameActivity]{}}
}

func (d *TitleData[T]) Select(account, binary string) {
	var next *[2]string
	if account != "" && binary != "" {
		next = &[2]string{account, binary}
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if (next == nil && d.identity == nil) || (next != nil && d.identity != nil && *next == *d.identity) {
		return
	}
	d.generation++
	for _, request := range d.requests {
		request.cancel()
	}
	clear(d.requests)
	clear(d.cache)
	d.order = nil
	d.identity = next
}

func (d *TitleData[T]) Get(titleID string, refresh bool) (T, error) {
	var zero T
	if err := validTitleID(titleID); err != nil {
		return zero, err
	}
	d.mu.Lock()
	identity, generation := d.identity, d.generation
	if identity == nil {
		d.mu.Unlock()
		return d.unsupported, nil
	}
	if cached, ok := d.cache[titleID]; !refresh && ok && cached.until.After(time.Now()) {
		d.mu.Unlock()
		return shared.Clone(cached.value), nil
	}
	if pending := d.requests[titleID]; pending != nil {
		d.mu.Unlock()
		<-pending.done
		return shared.Clone(pending.value), nil
	}
	if len(d.requests) >= d.maxRequests {
		d.mu.Unlock()
		return d.failed, nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	request := &titleRequest[T]{cancel: cancel, done: make(chan struct{})}
	d.requests[titleID] = request
	d.mu.Unlock()

	request.value = d.failed
	raw, err := d.reader(ctx, identity[1], titleID)
	d.mu.Lock()
	if err == nil && generation == d.generation && ctx.Err() == nil {
		if value, perr := d.parse(raw, identity[0], titleID); perr == nil {
			if _, exists := d.cache[titleID]; !exists {
				if len(d.order) >= d.maxCache {
					delete(d.cache, d.order[0])
					d.order = d.order[1:]
				}
				d.order = append(d.order, titleID)
			}
			d.cache[titleID] = titleCache[T]{time.Now().Add(d.ttl), value}
			request.value = value
		}
	}
	if generation == d.generation {
		delete(d.requests, titleID)
	}
	d.mu.Unlock()
	close(request.done)
	return shared.Clone(request.value), nil
}
