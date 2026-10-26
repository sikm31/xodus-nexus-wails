package shared

import (
	"errors"
	"fmt"
	"math"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

var (
	commandLine      = regexp.MustCompile(`(?m)^[ \t]{2}([a-z][a-z0-9-]+)[ \t]{2,}`)
	cliVersion       = regexp.MustCompile(`(?i)xodus-cli\s+([\w.+-]+)`)
	externalAuth     = regexp.MustCompile(`(?m)(?:^|\s)--external-webview-stdio(?:\s|$)`)
	ansiOSC          = regexp.MustCompile(`\x1b\][^\x07]*(?:\x07|\x1b\\)`)
	ansiCSI          = regexp.MustCompile(`\x1b\[[0-?]*[ -/]*[@-~]`)
	explicitPct      = regexp.MustCompile(`(?:^|\s)(100|\d{1,2}(?:\.\d+)?)%`)
	byteCounters     = regexp.MustCompile(`(?i)([\d.]+)\s*(B|KiB|MiB|GiB|TiB|KB|MB|GB|TB)\s*/\s*([\d.]+)\s*(B|KiB|MiB|GiB|TiB|KB|MB|GB|TB)`)
	percentPrefix    = regexp.MustCompile(`^\d+% ·`)
	rustBacktrace    = regexp.MustCompile(`(?i)^note: run with\b.*RUST_BACKTRACE=`)
	sessionEnded     = regexp.MustCompile(`(?i)^session terminated`)
	scriptDone       = regexp.MustCompile(`(?i)^script done`)
	productID        = regexp.MustCompile(`^[A-Z0-9-]{6,40}$`)
	rate             = regexp.MustCompile(`(?i)^([\d.]+)\s*(KiB|MiB|GiB|TiB|KB|MB|GB|TB|B)/s$`)
	leadingInt       = regexp.MustCompile(`^[+-]?\d+`)
	versionSeparator = regexp.MustCompile(`[.-]`)
)

var units = map[string]float64{
	"b": 1, "kib": 1024, "mib": 1 << 20, "gib": 1 << 30, "tib": 1 << 40,
	"kb": 1e3, "mb": 1e6, "gb": 1e9, "tb": 1e12,
}

// IsNewerVersion is a loose semver comparison ("v1.2.3-rc" style tags included):
// true when the candidate version is strictly newer than the current one.
func IsNewerVersion(candidate, current string) bool {
	if candidate == "" {
		return false
	}
	parse := func(value string) []int {
		value = strings.TrimSpace(value)
		if len(value) > 0 && (value[0] == 'v' || value[0] == 'V') {
			value = value[1:]
		}
		parts := versionSeparator.Split(value, -1)
		out := make([]int, len(parts))
		for i, part := range parts {
			out[i], _ = strconv.Atoi(leadingInt.FindString(strings.TrimLeft(part, " \t")))
		}
		return out
	}
	left, right := parse(candidate), parse(current)
	for i := range max(len(left), len(right)) {
		var a, b int
		if i < len(left) {
			a = left[i]
		}
		if i < len(right) {
			b = right[i]
		}
		if a != b {
			return a > b
		}
	}
	return false
}

func ParseBackendHelp(help string) BackendCapabilities {
	commands := []string{}
	for _, match := range commandLine.FindAllStringSubmatch(help, -1) {
		commands = append(commands, match[1])
	}
	has := func(name string) bool { return slices.Contains(commands, name) }
	var version string
	if match := cliVersion.FindStringSubmatch(help); match != nil {
		version = match[1]
	}
	return BackendCapabilities{
		Commands:     commands,
		Account:      has("account"),
		Profile:      has("profile"),
		GameActivity: has("game-activity"),
		Achievements: has("achievements"),
		Library:      has("library"),
		Login:        has("login"),
		ExternalAuth: externalAuth.MatchString(help),
		Logout:       has("logout"),
		Download:     has("download"),
		Streaming:    has("streaming"),
		Run:          has("run"),
		Version:      version,
	}
}

// StripANSI removes ANSI CSI/OSC sequences emitted by indicatif and tracing.
func StripANSI(value string) string {
	value = ansiOSC.ReplaceAllString(value, "")
	value = ansiCSI.ReplaceAllString(value, "")
	return strings.ReplaceAll(value, "\r", "\n")
}

// ProgressFromLine returns a percentage, or ok=false when the line has none.
func ProgressFromLine(line string) (float64, bool) {
	if match := explicitPct.FindStringSubmatch(line); match != nil {
		value, _ := strconv.ParseFloat(match[1], 64)
		return math.Min(100, value), true
	}
	if current, total, ok := ProgressBytesFromLine(line); ok {
		return math.Min(100, current/total*100), true
	}
	return 0, false
}

func ProgressBytesFromLine(line string) (current, total float64, ok bool) {
	match := byteCounters.FindStringSubmatch(line)
	if match == nil {
		return 0, 0, false
	}
	current = jsNumber(match[1]) * units[strings.ToLower(match[2])]
	total = jsNumber(match[3]) * units[strings.ToLower(match[4])]
	if math.IsNaN(current) || math.IsInf(current, 0) || math.IsNaN(total) || math.IsInf(total, 0) || total <= 0 {
		return 0, 0, false
	}
	return current, total, true
}

// jsNumber follows JavaScript Number(): strings like "1.2.3" become NaN.
func jsNumber(value string) float64 {
	number, err := strconv.ParseFloat(value, 64)
	if err != nil {
		return math.NaN()
	}
	return number
}

func DownloadFailure(log []string, exitCode int) string {
	if transfer := TransferFailure(log); transfer != "" {
		return fmt.Sprintf("Xodus exited with code %d: %s", exitCode, transfer)
	}
	if licensing := ContentLicenseFailure(log); licensing != "" {
		return fmt.Sprintf("Xodus exited with code %d: %s", exitCode, licensing)
	}
	for i := len(log) - 1; i >= 0; i-- {
		line := log[i]
		if strings.TrimSpace(line) == "" || percentPrefix.MatchString(line) || rustBacktrace.MatchString(line) ||
			sessionEnded.MatchString(line) || scriptDone.MatchString(line) {
			continue
		}
		return fmt.Sprintf("Xodus exited with code %d: %s", exitCode, truncateUTF16(line, 300))
	}
	return fmt.Sprintf("Xodus exited with code %d", exitCode)
}

// truncateUTF16 mirrors String.prototype.slice(0, n) closely enough for UI text
// without splitting a UTF-8 sequence.
func truncateUTF16(value string, limit int) string {
	units := 0
	for index, r := range value {
		width := 1
		if r > 0xFFFF {
			width = 2
		}
		if units+width > limit {
			return value[:index]
		}
		units += width
	}
	return value
}

func SafeProductID(value string) (string, error) {
	normalized := strings.ToUpper(strings.TrimSpace(value))
	if !productID.MatchString(normalized) {
		return "", errors.New("Enter a valid Store/Product ID (6–40 characters: A–Z, 0–9, or a hyphen).")
	}
	return normalized, nil
}

// EstimateTimeRemaining derives a download ETA from the transfer rate Xodus
// reports (for example "86.4 MiB/s") and the byte counters.
func EstimateTimeRemaining(transferRate string, downloaded, total *float64) string {
	if transferRate == "" || downloaded == nil || total == nil {
		return ""
	}
	remaining := *total - *downloaded
	if remaining <= 0 {
		return ""
	}
	match := rate.FindStringSubmatch(strings.TrimSpace(transferRate))
	if match == nil {
		return ""
	}
	bytesPerSecond := jsNumber(match[1]) * units[strings.ToLower(match[2])]
	if math.IsNaN(bytesPerSecond) || math.IsInf(bytesPerSecond, 0) || bytesPerSecond <= 0 {
		return ""
	}
	seconds := remaining / bytesPerSecond
	if math.IsInf(seconds, 0) || math.IsNaN(seconds) {
		return ""
	}
	rounded := int64(jsRound(seconds))
	if rounded < 60 {
		return fmt.Sprintf("~%ds left", rounded)
	}
	minutes := int64(jsRound(float64(rounded) / 60))
	if minutes < 60 {
		return fmt.Sprintf("~%dm left", minutes)
	}
	hours := minutes / 60
	if hours < 24 {
		return fmt.Sprintf("~%dh %dm left", hours, minutes%60)
	}
	return fmt.Sprintf("~%dd %dh left", hours/24, hours%24)
}

// jsRound matches Math.round: halves round toward +Infinity.
func jsRound(value float64) float64 { return math.Floor(value + 0.5) }
