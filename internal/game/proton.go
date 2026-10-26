package game

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"syscall"

	"xodus-nexus-wails/internal/launcherenv"
	"xodus-nexus-wails/internal/manifest"
	"xodus-nexus-wails/internal/proc"
	"xodus-nexus-wails/internal/shared"
)

type Runner struct {
	Command               string
	LaunchCommand         string
	CompatibilityDataPath string
	StopPrefix            StopPrefix
}

var requiredPrefixFiles = []string{
	"drive_c/windows/system32/libvkd3d-utils-1.dll",
	"drive_c/windows/system32/dxgi.dll",
	"drive_c/windows/system32/d3d12.dll",
}

func readable(file string) bool { return syscall.Access(file, 4) == nil }

func missingPrefixFiles(compatibilityDataPath string) []string {
	var missing []string
	for _, relative := range requiredPrefixFiles {
		if !readable(filepath.Join(compatibilityDataPath, "pfx", relative)) {
			missing = append(missing, relative)
		}
	}
	return missing
}

// rebaseRuntimeSymlinks points prefix links into older runtime releases at the current one.
func rebaseRuntimeSymlinks(prefix, protonRoot string) (int, error) {
	releasesRoot := filepath.Dir(protonRoot)
	currentFiles := filepath.Join(protonRoot, "files")
	updated, serial := 0, 0
	var visit func(string) error
	visit = func(directory string) error {
		entries, err := os.ReadDir(directory)
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		for _, entry := range entries {
			entryPath := filepath.Join(directory, entry.Name())
			if entry.IsDir() {
				if err := visit(entryPath); err != nil {
					return err
				}
				continue
			}
			if entry.Type()&fs.ModeSymlink == 0 {
				continue
			}
			target, err := os.Readlink(entryPath)
			if err != nil {
				return err
			}
			if !filepath.IsAbs(target) {
				continue
			}
			relative, err := filepath.Rel(releasesRoot, target)
			if err != nil || relative == "." || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) || filepath.IsAbs(relative) {
				continue
			}
			components := strings.Split(relative, string(filepath.Separator))
			if len(components) < 3 || components[1] != "files" {
				continue
			}
			replacement := filepath.Join(append([]string{currentFiles}, components[2:]...)...)
			if replacement == target || !readable(replacement) {
				continue
			}
			temporary := fmt.Sprintf("%s.xodus-runtime-%d-%d", entryPath, os.Getpid(), serial)
			serial++
			if err := os.Symlink(replacement, temporary); err != nil {
				return err
			}
			if err := os.Rename(temporary, entryPath); err != nil {
				os.Remove(temporary)
				return err
			}
			updated++
		}
		return nil
	}
	return updated, visit(prefix)
}

var (
	regSections   = regexp.MustCompile(`\r?\n\[`)
	international = regexp.MustCompile(`(?m)^\[Control Panel\\\\International\]`)
	localeName    = regexp.MustCompile(`(?m)^"LocaleName"="([^"]*)"`)
)

func latin1(data []byte) string {
	runes := make([]rune, len(data))
	for i, b := range data {
		runes[i] = rune(b)
	}
	return string(runes)
}

// PrefixUserLocale reads the Windows user locale stored in a prefix, or "".
func PrefixUserLocale(prefix string) string {
	data, err := os.ReadFile(filepath.Join(prefix, "user.reg"))
	if err != nil {
		return ""
	}
	text := latin1(data)
	indexes := regSections.FindAllStringIndex(text, -1)
	start := 0
	var sections []string
	for _, index := range indexes {
		sections = append(sections, text[start:index[0]])
		start = index[1] - 1
	}
	sections = append(sections, text[start:])
	for _, section := range sections {
		if international.MatchString(section) {
			if match := localeName.FindStringSubmatch(section); match != nil {
				return match[1]
			}
			return ""
		}
	}
	return ""
}

// SyncPrefixUserLocale never prevents a game from starting: an optional
// language override must not fail the launch when its marker or registry tool
// is unavailable.
func SyncPrefixUserLocale(wineCommand, prefix, locale string, env launcherenv.Env, onLine func(string)) {
	if err := synchronizePrefixUserLocale(wineCommand, prefix, locale, env, onLine); err != nil {
		onLine("The prefix user locale could not be synchronized; the game may use the previous language.")
	}
}

type localeMarker struct {
	Schema  int    `json:"schema"`
	Applied string `json:"applied"`
}

func readLocaleMarker(marker string) (*localeMarker, error) {
	handle, err := os.OpenFile(marker, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	defer handle.Close()
	info, err := handle.Stat()
	if err != nil {
		return nil, err
	}
	stat, _ := info.Sys().(*syscall.Stat_t)
	if !info.Mode().IsRegular() || info.Size() > 512 || stat == nil || stat.Nlink != 1 {
		return nil, errors.New("Invalid locale marker.")
	}
	data, err := io.ReadAll(io.LimitReader(handle, 513))
	if err != nil || len(data) > 512 {
		return nil, errors.New("Invalid locale marker.")
	}
	var value localeMarker
	if json.Unmarshal(data, &value) != nil || value.Schema != 1 || value.Applied == "" {
		return nil, errors.New("Invalid locale marker.")
	}
	if windows, err := shared.WindowsGameLocale(value.Applied); err != nil || windows != value.Applied {
		return nil, errors.New("Invalid locale marker.")
	}
	return &value, nil
}

func synchronizePrefixUserLocale(wineCommand, prefix, locale string, env launcherenv.Env, onLine func(string)) error {
	marker := filepath.Join(prefix, ".nexus-user-locale.json")
	managed, err := readLocaleMarker(marker)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		onLine("The managed game-language record is unavailable; leaving the prefix locale unchanged.")
		return nil
	}
	if locale == "" && managed == nil {
		return nil
	}
	current := PrefixUserLocale(prefix)
	if locale == "" && current != "" && current != managed.Applied {
		// Another tool or the user changed it after Nexus. Do not reset their work.
		return os.Remove(marker)
	}
	var target string
	if locale != "" {
		if target, err = shared.WindowsGameLocale(locale); err != nil {
			return err
		}
	} else {
		target = manifest.SystemGameLocale(env)
	}
	if target == "" {
		onLine("The system language could not be resolved; leaving the prefix locale unchanged.")
		return nil
	}
	if current == target {
		if locale == "" {
			return os.Remove(marker)
		}
		return nil
	}
	from := current
	if from == "" {
		from = "unset"
	}
	onLine(fmt.Sprintf("Updating the Windows user locale of the game prefix from %s to %s.", from, target))
	// Wine pins HKCU\Control Panel\International in the prefix at creation and
	// ignores later LANG changes. Deleting the key makes Wine rebuild it from
	// the launch environment, so titles that read the Windows user locale
	// instead of XPackageGetUserLocale follow the selected game language.
	regEnv := env.Clone()
	regEnv["WINEUSERLOCALE"] = target
	result, err := proc.Run(wineCommand, []string{"reg", "delete", `HKCU\Control Panel\International`, "/f"}, proc.RunOptions{Env: regEnv, MaxOutput: 100_000})
	if err != nil {
		return err
	}
	if result.Code != 0 {
		onLine(fmt.Sprintf("The prefix user locale could not be updated (reg exited with code %d); the game may use the previous language.", result.Code))
		return nil
	}
	if locale == "" {
		return os.Remove(marker)
	}
	// The key is recreated by the next Wine process from this launch's locale.
	if err := os.MkdirAll(prefix, 0o755); err != nil {
		return err
	}
	temporary := fmt.Sprintf("%s.%d.tmp", marker, os.Getpid())
	data, _ := json.Marshal(localeMarker{Schema: 1, Applied: target})
	// Never overwrite or remove a preexisting temporary file.
	handle, err := os.OpenFile(temporary, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	defer os.Remove(temporary)
	_, err = handle.Write(data)
	if closeErr := handle.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	return os.Rename(temporary, marker)
}

// Unreal GDK titles resolve their text culture at engine init, before the
// cloud save (with the in-game Language Select) is available. A read-only
// Game.ini with [Internationalization] Culture pins the language at boot.
// The launcher only manages the two marker-owned lines it wrote itself; a
// manually authored Culture line is never modified, and clearing the game
// language in the launcher removes the pin again.
const managedCultureMarker = "; Xodus Nexus game language"

type UnrealCulturePlan struct {
	Content *string
	Managed bool
	Manual  bool
}

var (
	sectionHeader = regexp.MustCompile(`^\s*\[([^\]]+)\]\s*$`)
	cultureLine   = regexp.MustCompile(`(?i)^(\s*Culture\s*=\s*)(.*?)\s*$`)
	lineBreaks    = regexp.MustCompile(`\r?\n`)
	trailingSpace = regexp.MustCompile(`\s*$`)
)

func isInternationalization(line string) bool {
	match := sectionHeader.FindStringSubmatch(line)
	return match != nil && strings.ToLower(strings.TrimSpace(match[1])) == "internationalization"
}

func withTrailingNewline(lines []string, newline string) *string {
	text := trailingSpace.ReplaceAllString(strings.Join(lines, newline), newline)
	return &text
}

// ManagedUnrealCultureGameIni plans the Game.ini change for locale ("" clears).
func ManagedUnrealCultureGameIni(existing, locale string) UnrealCulturePlan {
	newline := "\n"
	if strings.Contains(existing, "\r\n") || !strings.Contains(existing, "\n") {
		newline = "\r\n"
	}
	lines := lineBreaks.Split(existing, -1)
	isMarker := func(line string) bool { return strings.TrimSpace(line) == managedCultureMarker }

	intlStart := slices.IndexFunc(lines, isInternationalization)
	intlEnd := intlStart
	if intlStart >= 0 {
		for i := intlStart + 1; i < len(lines); i++ {
			if sectionHeader.MatchString(lines[i]) {
				break
			}
			intlEnd = i
		}
	}
	owned, manual := -1, false
	for i := intlStart + 1; i <= intlEnd; i++ {
		if !cultureLine.MatchString(lines[i]) {
			continue
		}
		if i > intlStart && isMarker(lines[i-1]) {
			owned = i
		} else {
			manual = true
		}
	}

	if locale == "" {
		if owned < 0 {
			return UnrealCulturePlan{Manual: manual}
		}
		var remaining []string
		for i, line := range lines {
			if i != owned-1 && i != owned {
				remaining = append(remaining, line)
			}
		}
		// Drop the [Internationalization] header when nothing but blank lines remain in it.
		start, end := -1, -1
		for i, line := range remaining {
			if !sectionHeader.MatchString(line) {
				continue
			}
			if start >= 0 {
				end = i - 1
				break
			}
			if isInternationalization(line) {
				start = i
			}
		}
		if start >= 0 {
			if end < start {
				end = len(remaining) - 1
			}
			if !slices.ContainsFunc(remaining[start+1:end+1], func(line string) bool { return strings.TrimSpace(line) != "" }) {
				remaining = slices.Delete(remaining, start, end+1)
			}
		}
		return UnrealCulturePlan{Content: withTrailingNewline(remaining, newline), Managed: true}
	}
	if manual {
		return UnrealCulturePlan{Manual: true}
	}
	if owned >= 0 {
		match := cultureLine.FindStringSubmatch(lines[owned])
		if match[2] == locale {
			return UnrealCulturePlan{Managed: true}
		}
		lines[owned] = match[1] + locale
		return UnrealCulturePlan{Content: withTrailingNewline(lines, newline), Managed: true}
	}
	if intlStart >= 0 {
		lines = slices.Insert(lines, intlEnd+1, managedCultureMarker, "Culture="+locale)
		return UnrealCulturePlan{Content: withTrailingNewline(lines, newline), Managed: true}
	}
	body := trailingSpace.ReplaceAllString(existing, "")
	separator := ""
	if body != "" {
		separator = newline + newline
	}
	content := body + separator + "[Internationalization]" + newline + managedCultureMarker + newline + "Culture=" + locale + newline
	return UnrealCulturePlan{Content: &content, Managed: true}
}

// SyncUnrealCultureGameIni pins the Unreal text language in each GDK save
// config. Only Unreal installations are touched.
func SyncUnrealCultureGameIni(prefix, installPath, titleID, locale string, onLine func(string)) {
	if titleID == "" {
		return
	}
	if _, err := os.Stat(filepath.Join(installPath, "Engine")); err != nil {
		return
	}
	users, err := os.ReadDir(filepath.Join(prefix, "drive_c", "users"))
	if err != nil {
		return
	}
	for _, user := range users {
		microsoft := filepath.Join(prefix, "drive_c", "users", user.Name(), "AppData", "Local", "Microsoft")
		products, err := os.ReadDir(microsoft)
		if err != nil {
			continue
		}
		for _, product := range products {
			relative := filepath.Join(product.Name(), "WinGDK", titleID, "Saved", "Config", "WinGDK")
			configDirectory := filepath.Join(microsoft, relative)
			if _, err := os.Stat(configDirectory); err != nil {
				continue
			}
			gameIni := filepath.Join(configDirectory, "Game.ini")
			existing, _ := os.ReadFile(gameIni)
			plan := ManagedUnrealCultureGameIni(string(existing), locale)
			if plan.Manual && locale != "" {
				onLine(fmt.Sprintf("Found a manually configured Game.ini culture in %s; the launcher language is not pinned there.", product.Name()))
				continue
			}
			if plan.Content != nil {
				os.Chmod(gameIni, 0o644)
				temporary := fmt.Sprintf("%s.xodus-culture-%d", gameIni, os.Getpid())
				if os.WriteFile(temporary, []byte(*plan.Content), 0o644) == nil && os.Rename(temporary, gameIni) == nil {
					if locale != "" {
						onLine(fmt.Sprintf("Pinned the Unreal text language to %s via %s.", locale, filepath.Join(relative, "Game.ini")))
					} else {
						onLine(fmt.Sprintf("Removed the pinned Unreal text language from %s.", filepath.Join(relative, "Game.ini")))
					}
				}
			}
			if plan.Managed {
				os.Chmod(gameIni, 0o444)
			}
		}
	}
}

func shellQuote(value string) string { return "'" + strings.ReplaceAll(value, "'", `'"'"'`) + "'" }

type runtimeMetadata struct {
	Features   []string          `json:"features"`
	Components map[string]string `json:"components"`
}

var dxvkComponents = []string{"d3d8.dll", "d3d9.dll", "d3d10core.dll", "d3d11.dll", "dxgi.dll"}

// installComponents copies verified runtime DLLs into the prefix. It validates
// every component before changing any prefix entry.
func installComponents(compatibilityDataPath, files string, metadata runtimeMetadata, components []string) error {
	type verified struct {
		name string
		data []byte
	}
	var ready []verified
	for _, name := range components {
		// WinHTTP loads nghttp2 with LOAD_LIBRARY_SEARCH_SYSTEM32. WINEDLLPATH
		// alone is insufficient in existing prefixes. Validate before
		// replacing a directory entry so symlinks cannot overwrite another runtime.
		library := "wine"
		if name == "libvkd3d-shader-1.dll" {
			library = "vkd3d"
		} else if slices.Contains(dxvkComponents, name) {
			library = "wine/dxvk"
		}
		data, err := os.ReadFile(filepath.Join(files, "lib", library, "x86_64-windows", name))
		if err != nil {
			return err
		}
		sum := sha256.Sum256(data)
		if expected := metadata.Components[name]; expected == "" || hex.EncodeToString(sum[:]) != expected {
			return fmt.Errorf("The shared runtime has an invalid %s checksum.", name)
		}
		ready = append(ready, verified{name, data})
	}
	for _, component := range ready {
		target := filepath.Join(compatibilityDataPath, "pfx/drive_c/windows/system32", component.name)
		current, err := os.ReadFile(target)
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		if bytes.Equal(current, component.data) {
			continue
		}
		temporary := fmt.Sprintf("%s.nexus-%d", target, os.Getpid())
		handle, err := os.OpenFile(temporary, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
		if err != nil {
			return err
		}
		_, err = handle.Write(component.data)
		if closeErr := handle.Close(); err == nil {
			err = closeErr
		}
		if err == nil {
			err = os.Rename(temporary, target)
		}
		os.Remove(temporary)
		if err != nil {
			return err
		}
	}
	return nil
}

func writeExecutable(file, content string) error {
	temporary := fmt.Sprintf("%s.tmp-%d", file, os.Getpid())
	if err := os.WriteFile(temporary, []byte(content), 0o700); err != nil {
		return err
	}
	if err := os.Rename(temporary, file); err != nil {
		os.Remove(temporary)
		return err
	}
	return os.Chmod(file, 0o700)
}

// PrepareProtonRunner initializes the managed prefix and writes the launch
// scripts. It returns nil when winePath is not inside a Proton build.
func PrepareProtonRunner(winePath, compatibilityDataPath, steamRoot, gameWineCommand string) (*Runner, error) {
	if gameWineCommand == "" {
		gameWineCommand = winePath
	}
	protonRoot := filepath.Clean(filepath.Join(filepath.Dir(winePath), "..", ".."))
	proton := filepath.Join(protonRoot, "proton")
	if syscall.Access(proton, 1) != nil || !readable(filepath.Join(protonRoot, "files", "share", "default_pfx")) {
		return nil, nil
	}
	if err := PrepareManagedPrefix(compatibilityDataPath); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(steamRoot, 0o755); err != nil {
		return nil, err
	}
	prefix := filepath.Join(compatibilityDataPath, "pfx")
	if _, err := rebaseRuntimeSymlinks(prefix, protonRoot); err != nil {
		return nil, err
	}
	if missing := missingPrefixFiles(compatibilityDataPath); len(missing) > 0 {
		env := launcherenv.FromOS()
		for k, v := range map[string]string{"STEAM_COMPAT_DATA_PATH": compatibilityDataPath, "STEAM_COMPAT_CLIENT_INSTALL_PATH": steamRoot,
			"SteamAppId": "0", "SteamGameId": "0", "UMU_ID": "xodus-nexus", "UMU_USE_STEAM": "0"} {
			env[k] = v
		}
		initialized, err := proc.Run(proton, []string{"run", "cmd", "/c", "exit"}, proc.RunOptions{Env: env, MaxOutput: 1_000_000})
		if err != nil {
			return nil, err
		}
		if missing = missingPrefixFiles(compatibilityDataPath); len(missing) > 0 {
			var tail []string
			for _, line := range strings.Split(initialized.Output, "\n") {
				if line = strings.TrimSpace(line); line != "" {
					tail = append(tail, line)
				}
			}
			if len(tail) > 5 {
				tail = tail[len(tail)-5:]
			}
			detail := fmt.Sprintf(" (code %d)", initialized.Code)
			if len(tail) > 0 {
				detail = ": " + strings.Join(tail, " | ")
			}
			return nil, fmt.Errorf("The Proton prefix was not prepared correctly; missing %s%s", strings.Join(missing, ", "), detail)
		}
	}
	if err := PrepareManagedPrefix(compatibilityDataPath); err != nil {
		return nil, err
	}
	if _, err := rebaseRuntimeSymlinks(prefix, protonRoot); err != nil {
		return nil, err
	}

	files := filepath.Join(protonRoot, "files")
	// A locally built Wine uses the host SDK. Do not combine its GStreamer
	// module with the older libraries/plugins from the base Proton archive.
	// This is a runtime capability, never a per-game exception.
	var metadata runtimeMetadata
	var hostLibraries, http2, dxvk bool
	if data, err := os.ReadFile(filepath.Join(protonRoot, "XODUS_NEXUS_PATCH.json")); err == nil && json.Unmarshal(data, &metadata) == nil {
		has := func(feature string) bool { return slices.Contains(metadata.Features, feature) }
		hostLibraries, http2, dxvk = has("wine-host-libraries-v1"), has("wine-winhttp-http2-v1"), has("dxvk-constant-compare-v1")
		var components []string
		if http2 {
			components = append(components, "libnghttp2.dll")
		}
		if has("winrt-json-v1") {
			components = append(components, "windows.web.dll")
		}
		if has("hlsl-state-keywords-v1") {
			components = append(components, "libvkd3d-shader-1.dll")
		}
		if has("appstate-registration-v1") {
			components = append(components, "twinapi.appcore.dll")
		}
		if dxvk {
			components = append(components, dxvkComponents...)
		}
		// Stock Proton keeps its bundled library environment; a patched
		// runtime with invalid components must not launch.
		if err := installComponents(compatibilityDataPath, files, metadata, components); err != nil {
			return nil, err
		}
	}
	libraryDirectories := []string{filepath.Join(files, "lib", "x86_64-linux-gnu"), filepath.Join(files, "lib", "aarch64-linux-gnu"), filepath.Join(files, "lib", "i386-linux-gnu")}
	// In particular, do not inherit editor/AppImage LD_LIBRARY_PATH entries.
	protonLibraries := libraryDirectories
	gstPlugins := make([]string, len(libraryDirectories))
	for i, directory := range libraryDirectories {
		gstPlugins[i] = filepath.Join(directory, "gstreamer-1.0")
	}
	gstPath := strings.Join(gstPlugins, ":")
	if hostLibraries {
		protonLibraries = append([]string{"/usr/lib"}, libraryDirectories...)
		gstPath = "/usr/lib/gstreamer-1.0"
	}
	wineDLLPath := filepath.Join(files, "lib", "vkd3d") + ":" + filepath.Join(files, "lib", "wine")
	executablePath := filepath.Join(files, "bin")
	if path := os.Getenv("PATH"); path != "" {
		executablePath += ":" + path
	}
	eacRuntime := InstalledEACRuntime(steamRoot)
	wineserver := filepath.Join(files, "bin", "wineserver")
	overrides := "dxgi=n;d3d11=n;d3d10core=n;d3d9=n;d3d12=n;d3d12core=n"
	if http2 {
		overrides += ";winhttp=b;libnghttp2=n"
	}
	if dxvk {
		overrides += ";d3d8=n"
	}
	makeScript := func(launchWine string) string {
		lines := []string{
			"#!/bin/sh",
			"export WINEPREFIX=" + shellQuote(prefix),
			"export WINEARCH='win64'",
			"export WINEDLLPATH=" + shellQuote(wineDLLPath),
			"export LD_LIBRARY_PATH=" + shellQuote(strings.Join(protonLibraries, ":")),
			// Match Proton's environment: Piper initializes eSpeak even before
			// a voice is loaded, and missing data can terminate the game process.
			"export ESPEAK_DATA_PATH=" + shellQuote(filepath.Join(files, "share")),
			"export GST_PLUGIN_SYSTEM_PATH_1_0=" + shellQuote(gstPath),
			"export GST_PLUGIN_PATH_1_0=''",
			"export WINE_GST_REGISTRY_DIR=" + shellQuote(filepath.Join(compatibilityDataPath, "gstreamer-1.0")),
			// The Proton converters may be selected when no native codec is
			// present. Without these fallback assets they fail setup and crash
			// during teardown.
			"export MEDIACONV_BLANK_VIDEO_FILE=" + shellQuote(filepath.Join(files, "share", "media", "blank.mkv")),
			"export MEDIACONV_BLANK_AUDIO_FILE=" + shellQuote(filepath.Join(files, "share", "media", "blank.ptna")),
			"export PATH=" + shellQuote(executablePath),
			"export WINEDLLOVERRIDES='" + overrides + "'",
			"export WINE_LARGE_ADDRESS_AWARE='1'",
		}
		// Use Steam's genuine compatibility bridge when installed. Never force a
		// particular game to load it or replace an explicit user configuration.
		if eacRuntime != "" {
			lines = append(lines, `if [ -z "${PROTON_EAC_RUNTIME:-}" ]; then`, "  export PROTON_EAC_RUNTIME="+shellQuote(eacRuntime), "fi")
		}
		lines = append(lines,
			// Private lifecycle command uses the exact same prefix/library
			// environment as launch. It is only invoked for explicit Stop/shutdown.
			`if [ "${1:-}" = "--nexus-stop-prefix" ]; then`,
			"  exec "+shellQuote(wineserver)+" -k",
			"fi",
			// Launchers (including EAC) may finish before the game they spawned.
			// Keep Xodus and its temporary launch view/account service alive
			// until the isolated game prefix has no remaining Wine processes.
			"if [ ! -x "+shellQuote(wineserver)+" ]; then",
			"  exec "+shellQuote(launchWine)+` "$@"`,
			"fi",
			"launch_status=0",
			shellQuote(launchWine)+` "$@" || launch_status=$?`,
			"wait_status=0",
			shellQuote(wineserver)+" -w || wait_status=$?",
			`if [ "$launch_status" -ne 0 ]; then exit "$launch_status"; fi`,
			`exit "$wait_status"`,
			"",
		)
		return strings.Join(lines, "\n")
	}
	runner := filepath.Join(compatibilityDataPath, "xodus-proton-wine")
	if err := writeExecutable(runner, makeScript(winePath)); err != nil {
		return nil, err
	}
	result := &Runner{Command: runner, CompatibilityDataPath: compatibilityDataPath, StopPrefix: StopPrefix{Command: runner, Args: []string{"--nexus-stop-prefix"}}}
	if gameWineCommand != winePath {
		result.LaunchCommand = filepath.Join(compatibilityDataPath, "xodus-proton-game")
		if err := writeExecutable(result.LaunchCommand, makeScript(gameWineCommand)); err != nil {
			return nil, err
		}
	}
	return result, nil
}
