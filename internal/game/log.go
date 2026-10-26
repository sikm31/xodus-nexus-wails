// Package game ports the game launch helpers: game-log.ts, game-process.ts,
// game-wrapper.ts, game-launch.ts, game-updates.ts, anti-cheat.ts,
// managed-prefix.ts and vc-runtime.ts.
package game

import (
	"fmt"
	"regexp"
	"strings"
	"sync"
	"time"

	"xodus-nexus-wails/internal/launcherenv"
)

const (
	reportInterval = 10 * time.Second
	maxSignatures  = 512
)

var (
	verboseLine = regexp.MustCompile(`(?i)(?:^|:)(fixme|info|trace):([^:]+):(.+)$`)
	addresses   = regexp.MustCompile(`(?i)\b(?:0x)?[0-9a-f]{8,}\b`)
)

type repeatedLine struct {
	label            string
	seen, suppressed int
	lastReportAt     time.Time
}

func verboseSignature(line string) (key, label string, ok bool) {
	match := verboseLine.FindStringSubmatch(line)
	if match == nil {
		return "", "", false
	}
	detail := addresses.ReplaceAllString(match[3], "<address>")
	key = strings.ToLower(match[1]) + ":" + strings.ToLower(match[2]) + ":" + detail
	runes := []rune(key)
	if len(runes) > 120 {
		runes = runes[:120]
	}
	return key, string(runes), true
}

// LoggingEnvironment reduces default Wine/vkd3d debug output without
// overriding the user's choices.
func LoggingEnvironment(source launcherenv.Env, verbose bool) launcherenv.Env {
	env := source.Clone()
	if verbose {
		env["XODUS_NEXUS_VERBOSE_LOGS"] = "1"
	}
	if env["XODUS_NEXUS_VERBOSE_LOGS"] == "1" {
		return env
	}
	for key, value := range map[string]string{"WINEDEBUG": "-fixme", "VKD3D_DEBUG": "warn", "VKD3D_SHADER_DEBUG": "warn"} {
		if _, set := env[key]; !set {
			env[key] = value
		}
	}
	return env
}

// WindowEnvironment: a game is a separate desktop application. Do not pass the
// launcher's startup notification or desktop-file identity into Wine/Proton.
func WindowEnvironment(source launcherenv.Env) launcherenv.Env {
	env := source.Clone()
	for _, key := range []string{"DESKTOP_STARTUP_ID", "XDG_ACTIVATION_TOKEN", "XDG_DESKTOP_FILE_NAME", "GIO_LAUNCHED_DESKTOP_FILE",
		"GIO_LAUNCHED_DESKTOP_FILE_PID", "BAMF_DESKTOP_FILE_HINT", "CHROME_DESKTOP", "APPDIR", "APPIMAGE"} {
		delete(env, key)
	}
	return env
}

// LogReducer coalesces repeated verbose Wine diagnostic lines.
type LogReducer struct {
	write    func(string)
	verbose  bool
	now      func() time.Time
	mu       sync.Mutex
	repeated map[string]*repeatedLine
	order    []string
}

func NewLogReducer(write func(string), verbose bool, now func() time.Time) *LogReducer {
	if now == nil {
		now = time.Now
	}
	return &LogReducer{write: write, verbose: verbose, now: now, repeated: map[string]*repeatedLine{}}
}

func (r *LogReducer) Push(line string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.verbose {
		r.write(line)
		return
	}
	key, label, ok := verboseSignature(line)
	if !ok {
		r.write(line)
		return
	}
	entry := r.repeated[key]
	if entry == nil {
		if len(r.repeated) >= maxSignatures {
			r.write(line)
			return
		}
		entry = &repeatedLine{label: label, lastReportAt: r.now()}
		r.repeated[key] = entry
		r.order = append(r.order, key)
	}
	entry.seen++
	if entry.seen <= 2 {
		r.write(line)
		return
	}
	entry.suppressed++
	if r.now().Sub(entry.lastReportAt) >= reportInterval {
		r.report(entry)
	}
}

func (r *LogReducer) Flush() {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, key := range r.order {
		r.report(r.repeated[key])
	}
}

func (r *LogReducer) report(entry *repeatedLine) {
	if entry.suppressed == 0 {
		return
	}
	r.write(fmt.Sprintf("Suppressed %d repeated %s diagnostic lines. Enable verbose logs in Settings or set XODUS_NEXUS_VERBOSE_LOGS=1 for full output.", entry.suppressed, entry.label))
	entry.suppressed = 0
	entry.lastReportAt = r.now()
}
