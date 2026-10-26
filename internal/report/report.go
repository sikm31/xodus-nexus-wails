// Package report ports diagnostic-report.ts: a redacted, user-reviewed
// diagnostic report.
package report

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"xodus-nexus-wails/internal/launcherenv"
	"xodus-nexus-wails/internal/shared"
)

const (
	logLimit    = 1024 * 1024
	ReportLimit = 8 * 1024 * 1024
)

var controls = regexp.MustCompile(`[\x00-\x1f\x7f]`)

func clean(value string) string {
	if value == "" {
		return "unavailable"
	}
	return shared.TruncateUTF16(controls.ReplaceAllString(value, " "), 256)
}

func smallFile(file string, limit int64) (string, error) {
	handle, err := os.OpenFile(file, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return "", err
	}
	defer handle.Close()
	if info, err := handle.Stat(); err != nil || !info.Mode().IsRegular() {
		return "", errors.New("Not a file")
	}
	data, err := io.ReadAll(io.LimitReader(handle, limit+1))
	if err != nil {
		return "", err
	}
	if int64(len(data)) > limit {
		return "", errors.New("Too large")
	}
	return string(data), nil
}

var (
	quoted   = regexp.MustCompile(`^(["'])(.*)(["'])$`)
	cardName = regexp.MustCompile(`^card[0-9]+$`)
	pciID    = regexp.MustCompile(`(?i)^0x[0-9a-f]{4}$`)
)

var desktops = []string{"kde", "gnome", "gamescope", "xfce", "cinnamon", "mate", "lxqt", "sway", "hyprland"}

func kernelRelease() string {
	var name syscall.Utsname
	if syscall.Uname(&name) != nil {
		return ""
	}
	var out []byte
	for _, c := range name.Release {
		if c == 0 {
			break
		}
		out = append(out, byte(c))
	}
	return string(out)
}

func cpuInfo() (string, int) {
	data, _ := smallFile("/proc/cpuinfo", 4*1024*1024)
	model, count := "", 0
	for _, line := range strings.Split(data, "\n") {
		key, value, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		switch strings.TrimSpace(key) {
		case "model name":
			if model == "" {
				model = strings.TrimSpace(value)
			}
		case "processor":
			count++
		}
	}
	if count == 0 {
		count = runtime.NumCPU()
	}
	return model, count
}

func totalMemoryMiB() int64 {
	var info syscall.Sysinfo_t
	if syscall.Sysinfo(&info) != nil {
		return 0
	}
	return int64(info.Totalram) * int64(info.Unit) / (1024 * 1024)
}

type Roots struct{ OSRelease, DRM string }

// SystemInfo reports only explicit hardware fields, never hostname, serial
// numbers, the process environment, command output, /proc/cmdline or the
// account database.
func SystemInfo(env launcherenv.Env, roots Roots) string {
	if roots.OSRelease == "" {
		roots = Roots{OSRelease: "/etc/os-release", DRM: "/sys/class/drm"}
	}
	arch := map[string]string{"amd64": "x64", "arm64": "arm64", "386": "ia32"}[runtime.GOARCH]
	if arch == "" {
		arch = runtime.GOARCH
	}
	lines := []string{fmt.Sprintf("OS: %s %s", runtime.GOOS, arch), "Kernel: " + clean(kernelRelease())}
	if text, err := smallFile(roots.OSRelease, 64*1024); err == nil {
		for _, name := range []string{"PRETTY_NAME", "ID", "VERSION_ID", "VARIANT_ID"} {
			for _, line := range strings.Split(text, "\n") {
				if value, ok := strings.CutPrefix(line, name+"="); ok {
					if match := quoted.FindStringSubmatch(value); match != nil && match[1] == match[3] {
						value = match[2]
					}
					if value != "" {
						lines = append(lines, name+": "+clean(value))
					}
					break
				}
			}
		}
	} else {
		lines = append(lines, "Distribution: unavailable")
	}
	var desktop []string
	for _, item := range strings.Split(strings.ToLower(env["XDG_CURRENT_DESKTOP"]), ":") {
		if slices.Contains(desktops, item) {
			desktop = append(desktop, item)
		}
	}
	if len(desktop) == 0 {
		desktop = []string{"unknown"}
	}
	lines = append(lines, "Desktop: "+strings.Join(desktop, ":"))
	session := env["XDG_SESSION_TYPE"]
	if !slices.Contains([]string{"x11", "wayland", "tty"}, session) {
		session = "unknown"
	}
	lines = append(lines, "Session: "+session, fmt.Sprintf("Steam launch: %v", env["SteamGameId"] != "" || env["SteamAppId"] != ""))
	model, count := cpuInfo()
	lines = append(lines, fmt.Sprintf("CPU: %s (%d logical CPUs)", clean(model), count), fmt.Sprintf("RAM: %d MiB", totalMemoryMiB()))
	entries, err := os.ReadDir(roots.DRM)
	found := false
	if err == nil {
		checked := 0
		for _, entry := range entries {
			if !cardName.MatchString(entry.Name()) || checked >= 16 {
				continue
			}
			checked++
			device := filepath.Join(roots.DRM, entry.Name(), "device")
			vendor, err1 := smallFile(filepath.Join(device, "vendor"), 128)
			id, err2 := smallFile(filepath.Join(device, "device"), 128)
			vendor, id = strings.TrimSpace(vendor), strings.TrimSpace(id)
			// Virtual devices need not have PCI IDs.
			if err1 != nil || err2 != nil || !pciID.MatchString(vendor) || !pciID.MatchString(id) {
				continue
			}
			driver := "unknown"
			if link, err := os.Readlink(filepath.Join(device, "driver")); err == nil {
				driver = filepath.Base(link)
			}
			lines = append(lines, fmt.Sprintf("GPU: %s:%s, driver %s", vendor, id, clean(driver)))
			found = true
		}
	}
	if !found {
		lines = append(lines, "GPU: unavailable")
	}
	return strings.Join(lines, "\n")
}

var (
	ansi        = regexp.MustCompile(`\x1b\[[0-?]*[ -/]*[@-~]`)
	badControls = regexp.MustCompile(`[\x00-\x08\x0b-\x1f\x7f]`)
	privateKey  = regexp.MustCompile(`-----BEGIN [^-\r\n]*PRIVATE KEY-----[\s\S]*?(?:-----END [^-\r\n]*PRIVATE KEY-----|$)`)
	sensitive   = regexp.MustCompile(`(?i)authorization|\bbearer\b|\bcookies?\b|password|client.?secret|(?:access|refresh|device|session|id)[ _-]?token|\btoken\s*["':=]|\bxuid\b|\buhs\b|user.?hash|account.?id|user.?id|machine.?id|login.?id|host.?name|tenant.?id|serial.?number|gamertag|\busername\b|\bemail\b|"(?:displayName|unique_name|preferred_username)"|(?:^|[?&])(?:sig|jwt|email_token)=`)
	urls        = regexp.MustCompile(`(?i)(?:https?|wss?|ftp)://[^\s<>"'` + "`" + `]+`)
	tokens      = regexp.MustCompile(`\b[A-Za-z0-9_-]{8,}\.(?:[A-Za-z0-9_-]*\.){1,3}[A-Za-z0-9_-]{8,}\b`)
	longValues  = regexp.MustCompile(`\b[A-Za-z0-9_+/=-]{96,}\b`)
	emails      = regexp.MustCompile("(?i)[A-Z0-9.!#$%&'*+/=?^_`{|}~-]{1,64}@[A-Z0-9.-]{1,253}\\.[A-Z]{2,63}")
	numericIDs  = regexp.MustCompile(`\b\d{16,20}\b`)
	homes       = regexp.MustCompile(`/home/[^/\s"'\\]+`)
	runUser     = regexp.MustCompile(`/run/user/\d+`)
	windowsUser = regexp.MustCompile(`(?i)([A-Z]:\\+users\\+)[^\\\s"']+`)
	lineSplit   = regexp.MustCompile(`\r?\n`)
)

// sensitiveKeywords: every alternative of the sensitive pattern contains one
// of these, so lines without them skip the (slow) case-insensitive regexp.
var sensitiveKeywords = []string{"authorization", "bearer", "cookie", "password", "secret", "token", "xuid", "uhs", "hash", "account",
	"user", "machine", "login", "host", "tenant", "serial", "gamertag", "email", "displayname", "unique_name", "sig=", "jwt="}

func mayBeSensitive(line string) bool {
	lower := strings.ToLower(line)
	for _, keyword := range sensitiveKeywords {
		if strings.Contains(lower, keyword) {
			return true
		}
	}
	return false
}

// Redact omits sensitive records in full rather than guessing token lengths or
// the syntax of every auth provider. Redaction remains best effort, so the
// report is editable and must be reviewed before the user shares it.
func Redact(text string, privateValues []string) string {
	text = badControls.ReplaceAllString(ansi.ReplaceAllString(text, ""), "")
	text = privateKey.ReplaceAllString(text, "[private key removed]")
	lines := lineSplit.Split(text, -1)
	for i, line := range lines {
		if mayBeSensitive(line) && sensitive.MatchString(line) {
			lines[i] = "[sensitive record removed]"
		}
	}
	text = strings.Join(lines, "\n")
	// Remove whole URLs, including notification email_token and signed
	// package query strings, not only the familiar sig= parameter.
	text = urls.ReplaceAllString(text, "[URL removed]")
	text = tokens.ReplaceAllString(text, "[token removed]")
	text = longValues.ReplaceAllString(text, "[long value removed]")
	// Go's regexp has no DFA; bounded email components are only scanned on
	// lines that can contain an address.
	lines = strings.Split(text, "\n")
	for i, line := range lines {
		if strings.Contains(line, "@") {
			lines[i] = emails.ReplaceAllString(line, "[email removed]")
		}
	}
	text = strings.Join(lines, "\n")
	text = numericIDs.ReplaceAllString(text, "[numeric identifier removed]")
	text = runUser.ReplaceAllString(homes.ReplaceAllString(text, "/home/[user]"), "/run/user/[uid]")
	text = windowsUser.ReplaceAllString(text, "${1}[user]")
	// Known display/account names and nonstandard home paths also disappear.
	var values []string
	for _, value := range privateValues {
		if shared.UTF16Len(value) >= 2 && !slices.Contains(values, value) {
			values = append(values, value)
		}
	}
	slices.SortFunc(values, func(a, b string) int { return len(b) - len(a) })
	for _, value := range values {
		text = strings.ReplaceAll(text, value, "[private]")
	}
	return text
}

func reportLog(file, root string) (shared.DiagnosticLog, error) {
	canonicalRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return shared.DiagnosticLog{}, err
	}
	parent, err := filepath.EvalSymlinks(filepath.Dir(file))
	if err != nil || parent != canonicalRoot {
		return shared.DiagnosticLog{}, errors.New("Not a launcher log")
	}
	handle, err := os.OpenFile(filepath.Join(parent, filepath.Base(file)), os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return shared.DiagnosticLog{}, err
	}
	defer handle.Close()
	info, err := handle.Stat()
	if err != nil {
		return shared.DiagnosticLog{}, err
	}
	stat, _ := info.Sys().(*syscall.Stat_t)
	if !info.Mode().IsRegular() || stat == nil || int(stat.Uid) != os.Geteuid() || stat.Nlink != 1 {
		return shared.DiagnosticLog{}, errors.New("Unsafe log")
	}
	length := min(info.Size(), logLimit)
	start := info.Size() - length
	data := make([]byte, length)
	if _, err := handle.ReadAt(data, start); err != nil && !errors.Is(err, io.EOF) {
		return shared.DiagnosticLog{}, errors.New("Log changed")
	}
	content := strings.ToValidUTF8(string(data), "�")
	if start > 0 {
		if index := strings.IndexByte(content, '\n'); index >= 0 {
			content = content[index+1:]
		} else {
			content = ""
		}
	}
	return shared.DiagnosticLog{Name: filepath.Base(file), Content: content, Truncated: start > 0}, nil
}

type Input struct {
	Snapshot        shared.AppSnapshot
	LauncherVersion string
	SystemInfo      string
	BackendLog      func() (shared.DiagnosticLog, error)
	LogsRoot        string
	GameID          *string
	PrivateValues   []string
}

var gameIDPattern = regexp.MustCompile(`^[a-f0-9]{16}$`)

func tail(value string, n int) string {
	if len(value) <= n {
		return value
	}
	return strings.ToValidUTF8(value[len(value)-n:], "")
}

func Create(input Input) (shared.DiagnosticReport, error) {
	data := input.Snapshot
	limit := 0.0
	if data.Settings.DownloadLimitMiB != nil {
		limit = *data.Settings.DownloadLimitMiB
	}
	lines := []string{"Xodus Nexus diagnostic report", "Created: " + shared.JSISOString(time.Now()),
		"Automatic redaction is best effort. Review this report before sharing.",
		"Logs are bounded snapshots, not complete files. Prefixes, game files and saves are not read.",
		"", "## Versions", "Launcher: " + clean(input.LauncherVersion),
		fmt.Sprintf("Backend: %s; available: %v; source: %s", clean(data.Backend.Version), data.Backend.Available, clean(data.Backend.Source)),
		fmt.Sprintf("Backend commit: %s; compatibility: %s", clean(data.Backend.Commit), clean(data.Backend.LinuxRunPatch)),
		fmt.Sprintf("Runtime: %s; installed: %v; commit: %s", clean(data.Runtime.Name), data.Runtime.Installed, clean(data.Runtime.Commit)),
		"", "## System", input.SystemInfo, "", "## Download settings",
		fmt.Sprintf("Parallel downloads: %d; limit: %s MiB/s; market: %s", data.Settings.ParallelDownloads, strconv.FormatFloat(limit, 'f', -1, 64), clean(data.Settings.Market))}
	appendLog := func(label string, read func() (shared.DiagnosticLog, error)) {
		lines = append(lines, "", "## "+label)
		log, err := read()
		if err != nil {
			lines = append(lines, "[Log missing, changed or unavailable for safe reading]")
			return
		}
		marker := "[Complete available snapshot]"
		if log.Truncated || len(log.Content) > logLimit {
			marker = "[TRUNCATED: most recent log tail only]"
		}
		content := tail(log.Content, logLimit)
		if content == "" {
			content = "[Empty log]"
		}
		lines = append(lines, marker, content)
	}
	appendLog("Backend log", input.BackendLog)
	tasks := data.Downloads[:min(5, len(data.Downloads))]
	for index, task := range tasks {
		output := task.Log[max(0, len(task.Log)-200):]
		trimmed := make([]string, len(output))
		for i, line := range output {
			trimmed[i] = tail(line, 8192)
		}
		kind := task.Kind
		if kind == "" {
			kind = "install"
		}
		exit := "unavailable"
		if task.ExitCode != nil {
			exit = strconv.Itoa(*task.ExitCode)
		}
		errorText := "none recorded"
		if task.Error != "" {
			errorText = tail(task.Error, 4096)
		}
		log := tail(strings.Join(trimmed, "\n"), 64*1024)
		if log == "" {
			log = "[No download output]"
		}
		lines = append(lines, "", fmt.Sprintf("## Download %d: %s", index+1, clean(task.Title)),
			fmt.Sprintf("Product: %s; kind: %s; status: %s; exit: %s", clean(task.ProductID), clean(kind), clean(task.Status), exit),
			"Phase: "+clean(task.Phase), "Error: "+errorText, "Only the most recent stored output is available.", log)
	}
	if len(tasks) == 0 {
		lines = append(lines, "", "## Downloads", "[No stored download logs]")
	}
	if input.GameID != nil {
		index := slices.IndexFunc(data.Games, func(game shared.Game) bool { return game.ID == *input.GameID })
		if index < 0 || !gameIDPattern.MatchString(data.Games[index].ID) {
			return shared.DiagnosticReport{}, errors.New("Game not found for diagnostics.")
		}
		game := data.Games[index]
		lines = append(lines, "", "## Selected game", fmt.Sprintf("Title: %s; product: %s; version: %s; state: %s",
			clean(game.Title), clean(game.StoreID), clean(game.InstalledVersion), clean(game.InstallState)))
		for _, item := range [][2]string{{"latest", "Game log"}, {"vc-runtime", "Prerequisite log"}} {
			file := filepath.Join(input.LogsRoot, game.ID+"-"+item[0]+".log")
			appendLog(item[1], func() (shared.DiagnosticLog, error) { return reportLog(file, input.LogsRoot) })
		}
	} else {
		lines = append(lines, "", "## Game logs", "[No game selected]")
	}
	privateValues := append(slices.Clone(input.PrivateValues), data.Auth.Username, data.Auth.Gamertag)
	content := Redact(strings.Join(lines, "\n")+"\n", privateValues)
	if err := ValidateContent(content); err != nil {
		return shared.DiagnosticReport{}, err
	}
	name := "nexus-diagnostics-" + strings.NewReplacer(":", "-", ".", "-").Replace(shared.JSISOString(time.Now())) + ".txt"
	return shared.DiagnosticReport{Name: name, Content: content}, nil
}

func ValidateContent(content string) error {
	if strings.TrimSpace(content) == "" || strings.ContainsRune(content, 0) || len(content) > ReportLimit {
		return errors.New("Invalid or oversized diagnostic report.")
	}
	return nil
}

// Save writes content to a new .txt file. No automatic upload, arbitrary path
// reads, symlink following or overwrites.
func Save(file, content string) error {
	if err := ValidateContent(content); err != nil {
		return err
	}
	if !filepath.IsAbs(file) || !strings.HasSuffix(strings.ToLower(file), ".txt") {
		return errors.New("Choose a new .txt file for the report.")
	}
	handle, err := os.OpenFile(file, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	_, err = handle.WriteString(content)
	if closeErr := handle.Close(); err == nil {
		err = closeErr
	}
	return err
}
