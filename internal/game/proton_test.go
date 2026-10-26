package game

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"xodus-nexus-wails/internal/launcherenv"
	"xodus-nexus-wails/internal/shared"
)

func writeFile(t *testing.T, path, content string, perm os.FileMode) {
	t.Helper()
	os.MkdirAll(filepath.Dir(path), 0o755)
	if err := os.WriteFile(path, []byte(content), perm); err != nil {
		t.Fatal(err)
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func sum(content string) string { s := sha256.Sum256([]byte(content)); return hex.EncodeToString(s[:]) }

func execOutput(t *testing.T, command string, env []string, args ...string) (string, int) {
	t.Helper()
	cmd := exec.Command(command, args...)
	if env != nil {
		cmd.Env = env
	}
	out, err := cmd.Output()
	code := 0
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		code = exit.ExitCode()
	} else if err != nil {
		t.Fatal(err)
	}
	return string(out), code
}

// runtimeFixture builds a fake Proton tree with an initialized prefix.
type runtimeFixture struct {
	root, runtime, files, wine, compat, system32 string
}

func newRuntime(t *testing.T, runtimeName, wineScript string) runtimeFixture {
	root := t.TempDir()
	f := runtimeFixture{root: root, runtime: filepath.Join(root, runtimeName)}
	f.files = filepath.Join(f.runtime, "files")
	f.wine = filepath.Join(f.files, "bin", "wine")
	f.compat = filepath.Join(root, "compat")
	f.system32 = filepath.Join(f.compat, "pfx/drive_c/windows/system32")
	os.MkdirAll(filepath.Join(f.files, "share", "default_pfx"), 0o755)
	writeFile(t, f.wine, "#!/bin/sh\n"+wineScript+"\n", 0o700)
	writeFile(t, filepath.Join(f.runtime, "proton"), "#!/bin/sh\nexit 0\n", 0o700)
	for _, dll := range []string{"libvkd3d-utils-1.dll", "dxgi.dll", "d3d12.dll"} {
		writeFile(t, filepath.Join(f.system32, dll), "ready", 0o644)
	}
	return f
}

func (f runtimeFixture) prepare(t *testing.T, gameWine string) *Runner {
	t.Helper()
	runner, err := PrepareProtonRunner(f.wine, f.compat, filepath.Join(f.root, "steam"), gameWine)
	if err != nil {
		t.Fatal(err)
	}
	return runner
}

func metadata(t *testing.T, f runtimeFixture, features []string, components map[string]string) {
	data, _ := json.Marshal(map[string]any{"features": features, "components": components})
	writeFile(t, filepath.Join(f.runtime, "XODUS_NEXUS_PATCH.json"), string(data), 0o644)
}

func TestDXVKComponents(t *testing.T) {
	f := newRuntime(t, "runtime", `printf "%s" "$WINEDLLOVERRIDES"`)
	modules := []string{"d3d8.dll", "d3d9.dll", "d3d10core.dll", "d3d11.dll", "dxgi.dll"}
	source := filepath.Join(f.files, "lib/wine/dxvk/x86_64-windows")
	previous := filepath.Join(f.root, "previous-dxvk.dll")
	writeFile(t, previous, "unchanged", 0o644)
	components := map[string]string{}
	for _, name := range modules {
		writeFile(t, filepath.Join(source, name), "new "+name, 0o644)
		os.Remove(filepath.Join(f.system32, name))
		os.Symlink(previous, filepath.Join(f.system32, name))
		components[name] = sum("new " + name)
	}
	metadata(t, f, []string{"dxvk-constant-compare-v1"}, components)
	runner := f.prepare(t, "")
	for _, name := range modules {
		if readFile(t, filepath.Join(f.system32, name)) != "new "+name {
			t.Errorf("%s not provisioned", name)
		}
	}
	if readFile(t, previous) != "unchanged" {
		t.Error("previous runtime file modified through a symlink")
	}
	if out, _ := execOutput(t, runner.Command, nil); !strings.Contains(out, "d3d8=n") {
		t.Errorf("overrides = %q", out)
	}
	for _, name := range modules {
		writeFile(t, filepath.Join(f.system32, name), "previous "+name, 0o644)
	}
	writeFile(t, filepath.Join(source, "dxgi.dll"), "corrupt", 0o644)
	if _, err := PrepareProtonRunner(f.wine, f.compat, filepath.Join(f.root, "steam"), ""); err == nil || !strings.Contains(err.Error(), "dxgi.dll checksum") {
		t.Errorf("corrupt component: %v", err)
	}
	for _, name := range modules {
		if readFile(t, filepath.Join(f.system32, name)) != "previous "+name {
			t.Errorf("%s changed despite a failed verification", name)
		}
	}
}

func TestRunnerWithGameTools(t *testing.T) {
	for _, withTools := range []bool{false, true} {
		f := newRuntime(t, "runtime's directory", `printf "%s\n" "$WINEDLLOVERRIDES" "$DXVK_HUD" "$WINEPREFIX" "$ESPEAK_DATA_PATH"; printf "<%s>\n" "$@"; printf "<controller:%s:%s>\n" "$PROTON_DISABLE_HIDRAW" "$SteamGameId"`)
		writeFile(t, filepath.Join(f.system32, "wined3d.dll"), "unmanaged shader module", 0o644)
		writeFile(t, filepath.Join(f.files, "bin", "wineserver"), "#!/bin/sh\nexit 0\n", 0o700)
		var tools *GameTools
		if withTools {
			tools = &GameTools{LaunchOptions: `WINEDLLOVERRIDES="dxgi=n,b;winhttp=b" DXVK_HUD=fps %command% --launcher-skip`}
		}
		wrapper, err := PrepareWrapper(f.wine, tools, filepath.Join(f.root, "wrappers"), nil, "")
		if err != nil {
			t.Fatal(err)
		}
		runner := f.prepare(t, wrapper.Command)
		if readFile(t, filepath.Join(f.system32, "wined3d.dll")) != "unmanaged shader module" {
			t.Error("unmanaged module changed")
		}
		setup, _ := execOutput(t, runner.Command, nil, "setup")
		if strings.Contains(setup, "fps") || strings.Contains(setup, "--launcher-skip") || !slices.Contains(strings.Split(setup, "\n"), filepath.Join(f.files, "share")) {
			t.Errorf("setup = %q", setup)
		}
		env := launcherenv.FromOS()
		env["WINEPREFIX"], env["ESPEAK_DATA_PATH"], env["PROTON_DISABLE_HIDRAW"], env["SteamGameId"] = "", "/wrong/host/data", "0x054c/0x0ce6", "16968485195965530112"
		game, _ := execOutput(t, PreparedWine(runner.Command, runner.LaunchCommand, wrapper.Command), env.List(), "actual game.exe")
		pfx := filepath.Join(f.compat, "pfx")
		if !strings.Contains(game, pfx) || strings.Contains(game, "/wrong/host/data") || !strings.Contains(game, "<controller:0x054c/0x0ce6:16968485195965530112>") {
			t.Errorf("game = %q", game)
		}
		if withTools {
			if !strings.Contains(game, "dxgi=n,b;winhttp=b\nfps\n"+pfx) || !strings.Contains(game, "<actual game.exe>\n<--launcher-skip>") {
				t.Errorf("tools game = %q", game)
			}
		} else if runner.LaunchCommand != "" || strings.Contains(game, "fps") {
			t.Errorf("plain game = %q", game)
		}
		if runner.StopPrefix.Command != runner.Command {
			t.Error("stop control must use the same runner")
		}
		execOutput(t, runner.StopPrefix.Command, nil, runner.StopPrefix.Args...)
	}
}

// GameTools is an alias that keeps the test readable.
type GameTools = shared.GameLaunchTools

func TestSharedRuntimeComponents(t *testing.T) {
	for _, oldSymlink := range []bool{false, true} {
		f := newRuntime(t, "runtime", `printf '%s\n' "$WINEDLLOVERRIDES"`)
		dlls := map[string]string{"libnghttp2.dll": "wine", "windows.web.dll": "wine", "libvkd3d-shader-1.dll": "vkd3d", "twinapi.appcore.dll": "wine"}
		components := map[string]string{}
		for name, library := range dlls {
			writeFile(t, filepath.Join(f.files, "lib", library, "x86_64-windows", name), "new "+name, 0o644)
			components[name] = sum("new " + name)
		}
		features := []string{"wine-winhttp-http2-v1", "winrt-json-v1", "hlsl-state-keywords-v1", "appstate-registration-v1"}
		metadata(t, f, features, components)
		original := filepath.Join(f.root, "previous.dll")
		if oldSymlink {
			writeFile(t, original, "old runtime", 0o644)
			for name := range dlls {
				os.Symlink(original, filepath.Join(f.system32, name))
			}
		}
		runner := f.prepare(t, "")
		for name := range dlls {
			if readFile(t, filepath.Join(f.system32, name)) != "new "+name {
				t.Errorf("%s not provisioned", name)
			}
		}
		if oldSymlink && readFile(t, original) != "old runtime" {
			t.Error("old runtime modified")
		}
		if out, _ := execOutput(t, runner.Command, nil); !strings.Contains(out, "winhttp=b;libnghttp2=n") {
			t.Errorf("overrides = %q", out)
		}
		for name, library := range dlls {
			source := filepath.Join(f.files, "lib", library, "x86_64-windows", name)
			writeFile(t, source, "corrupt", 0o644)
			if _, err := PrepareProtonRunner(f.wine, f.compat, filepath.Join(f.root, "steam"), ""); err == nil || !strings.Contains(err.Error(), name+" checksum") {
				t.Errorf("%s corrupt: %v", name, err)
			}
			if readFile(t, filepath.Join(f.system32, name)) != "new "+name {
				t.Errorf("%s replaced despite failed verification", name)
			}
			writeFile(t, source, "new "+name, 0o644)
		}
		writeFile(t, filepath.Join(f.files, "lib/wine/x86_64-windows/twinapi.appcore.dll"), "corrupt", 0o644)
		metadata(t, f, []string{"appstate-registration-v1"}, map[string]string{"twinapi.appcore.dll": sum("new twinapi.appcore.dll")})
		if _, err := PrepareProtonRunner(f.wine, f.compat, filepath.Join(f.root, "steam"), ""); err == nil {
			t.Error("a single capability must fail closed")
		}
	}
}

func TestProtonInitialization(t *testing.T) {
	root := t.TempDir()
	wine := filepath.Join(root, "runtime", "files", "bin", "wine")
	os.MkdirAll(filepath.Join(root, "runtime", "files", "share", "default_pfx"), 0o755)
	writeFile(t, wine, "#!/bin/sh\nprintf '%s\\n' \"$WINEPREFIX\" \"$WINEDLLPATH\" \"$WINEDLLOVERRIDES\" \"$1\"\n", 0o700)
	writeFile(t, filepath.Join(root, "runtime", "proton"), "#!/bin/sh\nroot=\"$STEAM_COMPAT_DATA_PATH/pfx/drive_c/windows/system32\"\nmkdir -p \"$root\"\ntouch \"$root/libvkd3d-utils-1.dll\" \"$root/dxgi.dll\" \"$root/d3d12.dll\"\n", 0o700)
	compat := filepath.Join(root, "compat game's data")
	runner, err := PrepareProtonRunner(wine, compat, filepath.Join(root, "steam user's root"), "")
	if err != nil || runner.Command != filepath.Join(compat, "xodus-proton-wine") {
		t.Fatalf("runner = %+v, %v", runner, err)
	}
	out, _ := execOutput(t, runner.Command, nil, "target path")
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if lines[0] != filepath.Join(compat, "pfx") || !strings.Contains(lines[1], "/files/lib/vkd3d:") || !strings.Contains(lines[2], "dxgi=n") || lines[3] != "target path" {
		t.Errorf("lines = %q", lines)
	}
	if runner, err := PrepareProtonRunner(filepath.Join(root, "bin", "wine"), filepath.Join(root, "c"), filepath.Join(root, "s"), ""); runner != nil || err != nil {
		t.Error("plain Wine is not a Proton build")
	}
}

func TestEACBridgeInRunner(t *testing.T) {
	for _, explicit := range []string{"", "/explicit/eac"} {
		f := newRuntime(t, "runtime", `printf '%s\n' "$PROTON_EAC_RUNTIME"`)
		steam := filepath.Join(f.root, "Steam's library")
		eac := eacBridge(t, steam)
		runner, err := PrepareProtonRunner(f.wine, f.compat, steam, "")
		if err != nil {
			t.Fatal(err)
		}
		env := launcherenv.FromOS()
		env["PROTON_EAC_RUNTIME"] = explicit
		want := explicit
		if want == "" {
			want = eac
		}
		if out, _ := execOutput(t, runner.Command, env.List()); strings.TrimSpace(out) != want {
			t.Errorf("EAC = %q, want %q", out, want)
		}
	}
}

func TestRunnerWaitsForDescendants(t *testing.T) {
	for _, exit := range []int{0, 23} {
		f := newRuntime(t, "runtime's directory", "printf 'launcher exited\\n' >> \"$RECORD\"\nexit "+strconv.Itoa(exit))
		writeFile(t, filepath.Join(f.files, "bin", "wineserver"), "#!/bin/sh\n[ \"$1\" = \"-w\" ] || exit 91\n[ \"$WINEPREFIX\" = \"$EXPECTED_PREFIX\" ] || exit 92\nprintf 'descendant still running\\n' >> \"$RECORD\"\nsleep 0.1\nprintf 'descendant finished\\n' >> \"$RECORD\"\n", 0o700)
		record := filepath.Join(f.root, "lifecycle")
		runner := f.prepare(t, "")
		env := launcherenv.FromOS()
		env["RECORD"], env["EXPECTED_PREFIX"] = record, filepath.Join(f.compat, "pfx")
		if _, code := execOutput(t, runner.Command, env.List(), "launcher.exe"); code != exit {
			t.Errorf("exit = %d, want %d", code, exit)
		}
		if got := strings.Split(strings.TrimSpace(readFile(t, record)), "\n"); !slices.Equal(got, []string{"launcher exited", "descendant still running", "descendant finished"}) {
			t.Errorf("lifecycle = %q", got)
		}
	}
}

func TestMediaLibraries(t *testing.T) {
	for _, host := range []bool{false, true} {
		f := newRuntime(t, "runtime's files", `printf '%s\n' "$LD_LIBRARY_PATH" "$GST_PLUGIN_SYSTEM_PATH_1_0" "$GST_PLUGIN_PATH_1_0" "$WINE_GST_REGISTRY_DIR" "$MEDIACONV_BLANK_VIDEO_FILE" "$MEDIACONV_BLANK_AUDIO_FILE"`)
		if host {
			metadata(t, f, []string{"wine-host-libraries-v1"}, nil)
		}
		runner := f.prepare(t, "")
		env := launcherenv.FromOS()
		env["LD_LIBRARY_PATH"], env["GST_PLUGIN_PATH_1_0"] = "/editor/foreign-libraries", "/editor/foreign-plugins"
		out, _ := execOutput(t, runner.Command, env.List())
		values := strings.Split(out, "\n")
		gst := "/usr/lib/gstreamer-1.0"
		if !host {
			var paths []string
			for _, arch := range []string{"x86_64-linux-gnu", "aarch64-linux-gnu", "i386-linux-gnu"} {
				paths = append(paths, filepath.Join(f.files, "lib", arch, "gstreamer-1.0"))
			}
			gst = strings.Join(paths, ":")
		}
		if strings.Contains(values[0], "/editor") || strings.HasPrefix(values[0], "/usr/lib:") != host || values[1] != gst || values[2] != "" ||
			values[3] != filepath.Join(f.compat, "gstreamer-1.0") || values[4] != filepath.Join(f.files, "share", "media", "blank.mkv") ||
			values[5] != filepath.Join(f.files, "share", "media", "blank.ptna") {
			t.Errorf("host=%v values = %q", host, values)
		}
	}
}

func TestRebaseRuntimeSymlinks(t *testing.T) {
	root := t.TempDir()
	previous := filepath.Join(root, "releases", "previous")
	current := filepath.Join(root, "releases", "current")
	wine := filepath.Join(current, "files", "bin", "wine")
	previousDLL := filepath.Join(previous, "files", "lib", "wine", "x86_64-windows", "xgameruntime.dll")
	currentDLL := filepath.Join(current, "files", "lib", "wine", "x86_64-windows", "xgameruntime.dll")
	compat := filepath.Join(root, "compat")
	system32 := filepath.Join(compat, "pfx", "drive_c", "windows", "system32")
	os.MkdirAll(filepath.Join(current, "files", "share", "default_pfx"), 0o755)
	writeFile(t, wine, "#!/bin/sh\nexit 0\n", 0o700)
	writeFile(t, filepath.Join(current, "proton"), "#!/bin/sh\nexit 0\n", 0o700)
	writeFile(t, previousDLL, "old", 0o644)
	writeFile(t, currentDLL, "new", 0o644)
	for _, dll := range []string{"libvkd3d-utils-1.dll", "dxgi.dll", "d3d12.dll"} {
		writeFile(t, filepath.Join(system32, dll), "ready", 0o644)
	}
	os.Symlink(previousDLL, filepath.Join(system32, "xgameruntime.dll"))
	if _, err := PrepareProtonRunner(wine, compat, filepath.Join(root, "steam"), ""); err != nil {
		t.Fatal(err)
	}
	if target, _ := os.Readlink(filepath.Join(system32, "xgameruntime.dll")); target != currentDLL {
		t.Errorf("link = %s", target)
	}
}

func registry(locale string) string {
	return "WINE REGISTRY Version 2\n\n[Unrelated] 1\n\"LocaleName\"=\"ignore-me\"\n\n[Control Panel\\\\International] 1\n\"LocaleName\"=\"" + locale + "\"\n"
}

func TestPrefixUserLocale(t *testing.T) {
	root := t.TempDir()
	prefix := filepath.Join(root, "pfx")
	writeFile(t, filepath.Join(prefix, "user.reg"), registry("en-US"), 0o644)
	wine := filepath.Join(root, "wine")
	record := filepath.Join(root, "reg-calls")
	writeFile(t, wine, "#!/bin/sh\nprintf '%s\\n' \"$@\" >> \"$RECORD\"\nexit 0\n", 0o700)
	if PrefixUserLocale(prefix) != "en-US" {
		t.Fatal("locale not read")
	}
	var lines []string
	SyncPrefixUserLocale(wine, prefix, "cs-CZ", launcherenv.Env{"RECORD": record}, func(line string) { lines = append(lines, line) })
	if calls := strings.Split(strings.TrimSpace(readFile(t, record)), "\n"); !slices.Equal(calls, []string{"reg", "delete", `HKCU\Control Panel\International`, "/f"}) ||
		len(lines) != 1 || !strings.Contains(lines[0], "en-US") || !strings.Contains(lines[0], "cs-CZ") {
		t.Errorf("calls %q lines %q", calls, lines)
	}

	same := t.TempDir()
	writeFile(t, filepath.Join(same, "user.reg"), registry("cs-CZ"), 0o644)
	lines = nil
	SyncPrefixUserLocale(wine, same, "cs-CZ", launcherenv.Env{"RECORD": filepath.Join(same, "calls")}, func(line string) { lines = append(lines, line) })
	SyncPrefixUserLocale(wine, same, "cs", launcherenv.Env{"RECORD": filepath.Join(same, "calls")}, func(line string) { lines = append(lines, line) })
	if _, err := os.Stat(filepath.Join(same, "calls")); err == nil || len(lines) != 0 {
		t.Error("matching locale must not run reg")
	}
}

func TestPrefixLocaleOwnership(t *testing.T) {
	root := t.TempDir()
	prefix := filepath.Join(root, "pfx")
	reg := filepath.Join(prefix, "user.reg")
	writeFile(t, reg, registry("en-US"), 0o644)
	wine := filepath.Join(root, "wine")
	record := filepath.Join(root, "calls")
	writeFile(t, wine, "#!/bin/sh\nprintf \"%s\\n\" \"$WINEUSERLOCALE\" >> \"$RECORD\"\n", 0o700)
	env := launcherenv.Env{"RECORD": record, "LANG": "en_US.UTF-8"}
	marker := filepath.Join(prefix, ".nexus-user-locale.json")
	SyncPrefixUserLocale(wine, prefix, "de", env, func(string) {})
	if readFile(t, record) != "de-DE\n" || readFile(t, marker) != `{"schema":1,"applied":"de-DE"}` {
		t.Errorf("apply: %q %q", readFile(t, record), readFile(t, marker))
	}
	writeFile(t, reg, registry("de-DE"), 0o644)
	SyncPrefixUserLocale(wine, prefix, "", env, func(string) {})
	if readFile(t, record) != "de-DE\nen-US\n" {
		t.Errorf("reset: %q", readFile(t, record))
	}
	if _, err := os.Stat(marker); err == nil {
		t.Error("marker kept after reset")
	}
	SyncPrefixUserLocale(wine, prefix, "", env, func(string) {})
	if readFile(t, record) != "de-DE\nen-US\n" {
		t.Error("unmanaged prefix changed")
	}

	manual := t.TempDir()
	writeFile(t, filepath.Join(manual, "user.reg"), registry("fr-FR"), 0o644)
	never := filepath.Join(manual, "must-never-run")
	SyncPrefixUserLocale(never, manual, "", launcherenv.Env{"LANG": "en_US.UTF-8"}, func(string) {})
	manualMarker := filepath.Join(manual, ".nexus-user-locale.json")
	writeFile(t, manualMarker, `{"schema":1,"applied":"de-DE"}`, 0o644)
	SyncPrefixUserLocale(never, manual, "", launcherenv.Env{"LANG": "en_US.UTF-8"}, func(string) {})
	if _, err := os.Stat(manualMarker); err == nil {
		t.Error("ownership of a manually changed prefix kept")
	}
	external := filepath.Join(manual, "external")
	writeFile(t, external, "unchanged", 0o644)
	os.Symlink(external, manualMarker)
	var lines []string
	SyncPrefixUserLocale(never, manual, "de", launcherenv.Env{}, func(line string) { lines = append(lines, line) })
	if !strings.Contains(strings.Join(lines, ","), "unchanged") || readFile(t, external) != "unchanged" || PrefixUserLocale(manual) != "fr-FR" {
		t.Errorf("linked marker: %q", lines)
	}
}

func TestPrefixLocaleFailures(t *testing.T) {
	retry := t.TempDir()
	writeFile(t, filepath.Join(retry, "user.reg"), registry("de-DE"), 0o644)
	marker := filepath.Join(retry, ".nexus-user-locale.json")
	writeFile(t, marker, `{"schema":1,"applied":"de-DE"}`, 0o644)
	failing := filepath.Join(retry, "wine")
	writeFile(t, failing, "#!/bin/sh\nexit 1\n", 0o700)
	var lines []string
	SyncPrefixUserLocale(failing, retry, "", launcherenv.Env{"LANG": "en_US.UTF-8"}, func(line string) { lines = append(lines, line) })
	if !strings.Contains(strings.Join(lines, ","), "could not be updated") || readFile(t, marker) != `{"schema":1,"applied":"de-DE"}` {
		t.Error("ownership must survive a failed reg call")
	}

	missing := t.TempDir()
	lines = nil
	SyncPrefixUserLocale(filepath.Join(missing, "missing-wine"), missing, "pl", launcherenv.Env{}, func(line string) { lines = append(lines, line) })
	joined := strings.Join(lines, ",")
	if !strings.Contains(joined, "could not be synchronized") || strings.Contains(joined, missing) {
		t.Errorf("missing tool: %q", joined)
	}

	collision := t.TempDir()
	temporary := filepath.Join(collision, ".nexus-user-locale.json."+strconv.Itoa(os.Getpid())+".tmp")
	writeFile(t, temporary, "preexisting user data", 0o644)
	ok := filepath.Join(collision, "wine")
	writeFile(t, ok, "#!/bin/sh\nexit 0\n", 0o700)
	lines = nil
	SyncPrefixUserLocale(ok, collision, "pl", launcherenv.Env{}, func(line string) { lines = append(lines, line) })
	if readFile(t, temporary) != "preexisting user data" || !strings.Contains(strings.Join(lines, ","), "could not be synchronized") {
		t.Error("preexisting temporary file touched")
	}
}

func unrealPrefix(t *testing.T, unreal bool) (prefix, config, install string) {
	root := t.TempDir()
	config = filepath.Join(root, "pfx", "drive_c", "users", "steamuser", "AppData", "Local", "Microsoft", "Gears of War E-Day", "WinGDK", "6BE41AB5", "Saved", "Config", "WinGDK")
	os.MkdirAll(config, 0o755)
	install = filepath.Join(root, "install")
	os.MkdirAll(install, 0o755)
	if unreal {
		os.Mkdir(filepath.Join(install, "Engine"), 0o755)
	}
	return filepath.Join(root, "pfx"), config, install
}

func TestUnrealCulture(t *testing.T) {
	prefix, config, install := unrealPrefix(t, true)
	gameIni := filepath.Join(config, "Game.ini")
	writeFile(t, gameIni, "[/Script/AkAudio.AkSettings]\r\nUnrealCultureToWwiseCulture=((\"en\",\"English(US)\"))\r\n", 0o644)
	var lines []string
	log := func(line string) { lines = append(lines, line) }
	SyncUnrealCultureGameIni(prefix, install, "6BE41AB5", "cs-CZ", log)
	content := readFile(t, gameIni)
	info, _ := os.Stat(gameIni)
	if !strings.Contains(content, "Culture=cs-CZ") || !strings.Contains(content, "[Internationalization]") || !strings.Contains(content, "UnrealCultureToWwiseCulture") ||
		info.Mode().Perm()&0o222 != 0 || !strings.Contains(strings.Join(lines, "\n"), "Pinned the Unreal text language") {
		t.Errorf("pinned: %q %q", content, lines)
	}
	pinned := content
	lines = nil
	SyncUnrealCultureGameIni(prefix, install, "6BE41AB5", "cs-CZ", log)
	if len(lines) != 0 || readFile(t, gameIni) != pinned {
		t.Error("unchanged pin rewritten")
	}
	SyncUnrealCultureGameIni(prefix, install, "6BE41AB5", "de-DE", log)
	if replaced := readFile(t, gameIni); !strings.Contains(replaced, "Culture=de-DE") || strings.Contains(replaced, "cs-CZ") {
		t.Errorf("replaced = %q", replaced)
	}
	lines = nil
	SyncUnrealCultureGameIni(prefix, install, "6BE41AB5", "", log)
	if removed := readFile(t, gameIni); strings.Contains(removed, "\nCulture=") || !strings.Contains(removed, "UnrealCultureToWwiseCulture") || strings.Contains(removed, "[Internationalization]") ||
		!strings.Contains(strings.Join(lines, "\n"), "Removed the pinned Unreal text language") {
		t.Errorf("removed = %q", removed)
	}

	prefix, config, install = unrealPrefix(t, true)
	gameIni = filepath.Join(config, "Game.ini")
	manual := "[Internationalization]\r\nCulture=fr-FR\r\n"
	writeFile(t, gameIni, manual, 0o644)
	lines = nil
	SyncUnrealCultureGameIni(prefix, install, "6BE41AB5", "cs-CZ", log)
	info, _ = os.Stat(gameIni)
	if readFile(t, gameIni) != manual || info.Mode().Perm()&0o222 == 0 || !strings.Contains(strings.Join(lines, "\n"), "manually configured") {
		t.Error("manual culture modified")
	}

	prefix, _, install = unrealPrefix(t, false)
	lines = nil
	SyncUnrealCultureGameIni(prefix, install, "", "cs-CZ", log)
	SyncUnrealCultureGameIni(prefix, install, "6BE41AB5", "cs-CZ", log)
	SyncUnrealCultureGameIni(prefix, filepath.Join(prefix, "missing"), "6BE41AB5", "cs-CZ", log)
	if len(lines) != 0 {
		t.Errorf("non-Unreal touched: %q", lines)
	}

	created := ManagedUnrealCultureGameIni("", "cs-CZ")
	if *created.Content != "[Internationalization]\r\n; Xodus Nexus game language\r\nCulture=cs-CZ\r\n" || !created.Managed {
		t.Errorf("created = %q", *created.Content)
	}
	if removed := ManagedUnrealCultureGameIni(*created.Content, ""); removed.Content == nil || strings.Contains(*removed.Content, "Culture") || strings.Contains(*removed.Content, "Xodus Nexus") {
		t.Error("owned lines not removed")
	}
	if untouched := ManagedUnrealCultureGameIni("[Internationalization]\r\nCulture=en\r\n", "cs-CZ"); untouched.Content != nil || !untouched.Manual {
		t.Error("manual line must be untouched")
	}
}
