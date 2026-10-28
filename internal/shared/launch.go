package shared

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"
)

// DownloadLimitKiB converts the UI's MiB/s to the backend's integer KiB/s.
// value is nil when unset. Zero is unlimited.
func DownloadLimitKiB(value any) (int, error) {
	if value == nil {
		return 0, nil
	}
	var number float64
	switch v := value.(type) {
	case float64:
		number = v
	case *float64:
		if v == nil {
			return 0, nil
		}
		number = *v
	case int:
		number = float64(v)
	default:
		return 0, errors.New("Download speed limit must be 0 (unlimited) or between 0.001 and 1024 MiB/s.")
	}
	if math.IsNaN(number) || math.IsInf(number, 0) || number < 0 || number > 1024 || (number > 0 && number < 1.0/1024) {
		return 0, errors.New("Download speed limit must be 0 (unlimited) or between 0.001 and 1024 MiB/s.")
	}
	if number == 0 {
		return 0, nil
	}
	return max(1, int(math.Floor(number*1024))), nil
}

func DownloadLimitArgs(value any, capabilities BackendCapabilities) ([]string, error) {
	kib, err := DownloadLimitKiB(value)
	if err != nil {
		return nil, err
	}
	if !capabilities.StreamingBandwidthLimit {
		if kib != 0 {
			return nil, errors.New("Update the Nexus backend in Settings to use the configured download speed limit. The download was not started without its limit.")
		}
		return []string{}, nil
	}
	return []string{"--download-limit-kib", strconv.Itoa(kib)}, nil
}

func DownloadLimitCommand(value any) (string, error) {
	kib, err := DownloadLimitKiB(value)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf(`{"schema":1,"command":"set-download-limit","kibPerSecond":%d}`+"\n", kib), nil
}

func DefaultLaunchTools() GameLaunchTools {
	off := false
	return GameLaunchTools{DualSenseCompatibility: &off}
}

var shellOperator = regexp.MustCompile("[|&;<>()`$]")

// CommandWords splits command words only, never a shell program. Quotes group literal arguments.
func CommandWords(input string) ([]string, error) {
	if UTF16Len(input) > 2048 || strings.ContainsAny(input, "\r\n\x00") {
		return nil, errors.New("Launch options must be a single line of at most 2048 characters.")
	}
	runes := []rune(input)
	words := []string{}
	var word strings.Builder
	var quote rune
	started := false
	for i := 0; i < len(runes); i++ {
		c := runes[i]
		switch {
		case c == '\\' && quote != '\'':
			i++
			if i == len(runes) {
				return nil, errors.New("Launch options end with an incomplete escape.")
			}
			word.WriteRune(runes[i])
			started = true
		case quote != 0:
			if c == quote {
				quote = 0
			} else {
				word.WriteRune(c)
			}
		case c == '"' || c == '\'':
			quote = c
			started = true
		case unicode.IsSpace(c):
			if started {
				words = append(words, word.String())
				word.Reset()
				started = false
			}
		default:
			if shellOperator.MatchString(string(c)) {
				return nil, errors.New("Shell operators and substitutions are not supported in launch options.")
			}
			word.WriteRune(c)
			started = true
		}
	}
	if quote != 0 {
		return nil, errors.New("Launch options contain an unclosed quote.")
	}
	if started {
		words = append(words, word.String())
	}
	return words, nil
}

type LaunchOptions struct {
	Environment map[string]string
	Prefix      []string
	GameArgs    []string
}

var (
	assignment = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*=`)
	managedVar = regexp.MustCompile(`^(?:WINEPREFIX|WINEARCH|WINE_DLL_FILE_MAP|XODUS_.*|STEAM_COMPAT_.*)$`)
)

func ParseLaunchOptions(input string) (LaunchOptions, error) {
	words, err := CommandWords(input)
	if err != nil {
		return LaunchOptions{}, err
	}
	marker := slices.Index(words, "%command%")
	for index, word := range words {
		if strings.Contains(word, "%command%") && index != marker {
			return LaunchOptions{}, errors.New("Use %command% only once, as a separate word.")
		}
	}
	prefix := words
	gameArgs := []string{}
	if marker >= 0 {
		prefix, gameArgs = words[:marker], words[marker+1:]
	}
	environment := map[string]string{}
	for len(prefix) > 0 && assignment.MatchString(prefix[0]) {
		name, value, _ := strings.Cut(prefix[0], "=")
		if managedVar.MatchString(name) {
			return LaunchOptions{}, fmt.Errorf("%s is managed by Nexus and cannot be overridden in launch options.", name)
		}
		environment[name] = value
		prefix = prefix[1:]
	}
	return LaunchOptions{Environment: environment, Prefix: slices.Clone(prefix), GameArgs: slices.Clone(gameArgs)}, nil
}

const MaxCPULimit = 64

// CPUTopology is the WINE_CPU_TOPOLOGY value exposing the first limit host
// CPUs, or "" for no limit.
func CPUTopology(limit int) string {
	if limit <= 0 {
		return ""
	}
	cpus := make([]string, limit)
	for i := range cpus {
		cpus[i] = strconv.Itoa(i)
	}
	return fmt.Sprintf("%d:%s", limit, strings.Join(cpus, ","))
}

// NormalizeLaunchTools validates raw JSON from the renderer. nil means defaults.
func NormalizeLaunchTools(raw json.RawMessage) (GameLaunchTools, error) {
	if raw == nil {
		return DefaultLaunchTools(), nil
	}
	var tools map[string]json.RawMessage
	if json.Unmarshal(raw, &tools) != nil || tools == nil {
		return GameLaunchTools{}, errors.New("Invalid game launch tools.")
	}
	var result GameLaunchTools
	var ok [4]bool
	result.MangoHud, ok[0] = jsonBool(tools["mangoHud"])
	result.Gamescope, ok[1] = jsonBool(tools["gamescope"])
	result.GamescopeArgs, ok[2] = jsonString(tools["gamescopeArgs"])
	result.LaunchOptions, ok[3] = jsonString(tools["launchOptions"])
	if slices.Contains(ok[:], false) {
		return GameLaunchTools{}, errors.New("Invalid game launch tools.")
	}
	dualSense := false
	if value, present := tools["dualSenseCompatibility"]; present {
		var valid bool
		if dualSense, valid = jsonBool(value); !valid {
			return GameLaunchTools{}, errors.New("Invalid DualSense compatibility setting.")
		}
	}
	result.DualSenseCompatibility = &dualSense
	if value, present := tools["cpuLimit"]; present && string(value) != "null" {
		if json.Unmarshal(value, &result.CPULimit) != nil || result.CPULimit < 0 || result.CPULimit > MaxCPULimit {
			return GameLaunchTools{}, fmt.Errorf("The CPU core limit must be between 1 and %d.", MaxCPULimit)
		}
	}
	result.GamescopeArgs = strings.TrimSpace(result.GamescopeArgs)
	result.LaunchOptions = strings.TrimSpace(result.LaunchOptions)
	args, err := CommandWords(result.GamescopeArgs)
	if err != nil {
		return GameLaunchTools{}, err
	}
	if slices.Contains(args, "--") || slices.ContainsFunc(args, func(arg string) bool { return strings.Contains(arg, "%command%") }) {
		return GameLaunchTools{}, errors.New("Gamescope options must not include -- or %command%; Nexus adds the game command.")
	}
	if _, err := ParseLaunchOptions(result.LaunchOptions); err != nil {
		return GameLaunchTools{}, err
	}
	return result, nil
}

func jsonBool(raw json.RawMessage) (bool, bool) {
	switch strings.TrimSpace(string(raw)) {
	case "true":
		return true, true
	case "false":
		return false, true
	}
	return false, false
}

func jsonString(raw json.RawMessage) (string, bool) {
	trimmed := strings.TrimSpace(string(raw))
	var value string
	if !strings.HasPrefix(trimmed, `"`) || json.Unmarshal(raw, &value) != nil {
		return "", false
	}
	return value, true
}

var subscriptionLabels = []struct{ id, label string }{
	{"CFQ7TTC0KHS0", "ULTIMATE"}, {"CFQ7TTC0KGQ8", "PC GAME PASS"},
	{"CFQ7TTC0P85B", "PREMIUM"}, {"CFQ7TTC0K5DJ", "ESSENTIAL"}, {"CFQ7TTC0K6L8", "GAME PASS CONSOLE"},
}

func knownPlan(id string) bool {
	return slices.ContainsFunc(subscriptionLabels, func(entry struct{ id, label string }) bool { return entry.id == id })
}

// ParseSubscription validates a backend subscription record; nil when invalid or stale.
func ParseSubscription(raw json.RawMessage, now time.Time) *GamePassSubscription {
	var data struct {
		Status     json.RawMessage   `json:"status"`
		CheckedAt  json.RawMessage   `json:"checkedAt"`
		ProductIDs []json.RawMessage `json:"productIds"`
	}
	if raw == nil || json.Unmarshal(raw, &data) != nil || data.ProductIDs == nil || len(data.ProductIDs) > 5 {
		return nil
	}
	status, ok := jsonString(data.Status)
	checkedAt, checkedOK := jsonString(data.CheckedAt)
	if !ok || !checkedOK || (status != "active" && status != "none" && status != "unknown") {
		return nil
	}
	var ids []string
	for _, entry := range data.ProductIDs {
		id, ok := jsonString(entry)
		if !ok || !knownPlan(id) {
			return nil
		}
		if !slices.Contains(ids, id) {
			ids = append(ids, id)
		}
	}
	checked, ok := ParseJSDate(checkedAt)
	age := now.Sub(checked)
	if !ok || age < -5*time.Minute || age >= 24*time.Hour {
		return nil
	}
	if (status == "active") != (len(data.ProductIDs) > 0) {
		return nil
	}
	if ids == nil {
		ids = []string{}
	}
	return &GamePassSubscription{Status: status, ProductIDs: ids, CheckedAt: checkedAt}
}

func SubscriptionBadge(raw json.RawMessage, now time.Time) string {
	subscription := ParseSubscription(raw, now)
	if subscription == nil || subscription.Status != "active" {
		return ""
	}
	for _, entry := range subscriptionLabels {
		if slices.Contains(subscription.ProductIDs, entry.id) {
			return entry.label
		}
	}
	return ""
}

var (
	nexusRuntimePrefix = regexp.MustCompile(`^(?:xodus-)?nexus-runtime-`)
	legacyPatched      = regexp.MustCompile(`^(?:proton-xodus-|xodus-proton-)`)
	legacyPatch        = regexp.MustCompile(`-(?:xgamesave-pr28-|eac-bridge-v\d)`)
	upstreamShort      = regexp.MustCompile(`^xodus-proton-[a-f0-9]{8}$`)
	upstreamLong       = regexp.MustCompile(`^proton-xodus-[a-f0-9]{8,40}$`)
)

func NexusRuntimeUpdateAvailable(runtime RuntimeStatus) bool {
	return runtime.NexusAvailable && runtime.NexusName != "" && runtime.Name != strings.TrimSuffix(runtime.NexusName, "-x86_64.tar.xz")
}

type RuntimeDisplay struct{ Title, Source, Version string }

// DescribeRuntime describes the active runtime, not the available update.
func DescribeRuntime(runtime RuntimeStatus) RuntimeDisplay {
	if !runtime.Installed {
		return RuntimeDisplay{"Nexus runtime", "Not installed", "Not installed"}
	}
	name := runtime.Name
	prefix := nexusRuntimePrefix.FindString(name)
	nexus := prefix != "" || (legacyPatched.MatchString(name) && legacyPatch.MatchString(name))
	upstream := !nexus && (upstreamShort.MatchString(name) || upstreamLong.MatchString(name))
	display := RuntimeDisplay{"Custom runtime", "Custom runtime", ""}
	if nexus {
		display.Title, display.Source = "Nexus runtime", "Nexus runtime"
	} else if upstream {
		display.Title, display.Source = "Xodus Proton runtime", "Upstream Xodus Proton snapshot"
	}
	switch {
	case prefix != "":
		display.Version = name[len(prefix):]
	case runtime.Commit != "":
		display.Version = "Build " + runtime.Commit[:min(12, len(runtime.Commit))]
	case name != "":
		display.Version = name
	default:
		display.Version = "Version unavailable"
	}
	return display
}
