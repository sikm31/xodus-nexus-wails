package game

import (
	"context"
	"encoding/binary"
	"fmt"
	"maps"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"xodus-nexus-wails/internal/launcherenv"
	"xodus-nexus-wails/internal/proc"
	"xodus-nexus-wails/internal/shared"
)

func TestLogReducer(t *testing.T) {
	var output []string
	now := time.Unix(0, 0)
	reducer := NewLogReducer(func(line string) { output = append(output, line) }, false, func() time.Time { return now })
	for i := range 1000 {
		reducer.Push(fmt.Sprintf("00e0:fixme:gdkc:x_user_XUserGetState iface 00006FFFFC1D04A0, state %016x stub!", i))
		reducer.Push("65874.496:00dc:01f4:fixme:vkd3d-proton:vk_samples_from_dxgi_sample_desc: Unhandled sample count 0.")
	}
	reducer.Push("00e0:err:module:import_dll Library dxgi.dll not found")
	reducer.Push("xodus-savesync: cloud 1 downloaded 1 uploaded 0")
	now = now.Add(11 * time.Second)
	reducer.Push("00e0:fixme:gdkc:x_user_XUserGetState iface 00006FFFFC1D04A0, state 0000000000C9F6B0 stub!")
	reducer.Flush()
	suppressed := 0
	for _, line := range output {
		if strings.Contains(line, "Suppressed") {
			suppressed++
		}
	}
	joined := strings.Join(output, "\n")
	if suppressed != 2 || len(output) != 8 || !slices.Contains(output, "00e0:err:module:import_dll Library dxgi.dll not found") ||
		!strings.Contains(joined, "Suppressed 999 repeated fixme:gdkc") || !strings.Contains(joined, "Suppressed 998 repeated fixme:vkd3d-proton") {
		t.Errorf("output (%d):\n%s", len(output), joined)
	}
	var verbose []string
	full := NewLogReducer(func(line string) { verbose = append(verbose, line) }, true, nil)
	for range 100 {
		full.Push("00e0:fixme:gdkc:repeated stub")
	}
	full.Flush()
	if len(verbose) != 100 {
		t.Error("verbose mode keeps every line")
	}
}

func TestLoggingAndWindowEnvironment(t *testing.T) {
	if env := LoggingEnvironment(launcherenv.Env{}, false); env["WINEDEBUG"] != "-fixme" || env["VKD3D_DEBUG"] != "warn" || env["VKD3D_SHADER_DEBUG"] != "warn" {
		t.Errorf("defaults = %v", env)
	}
	if env := LoggingEnvironment(launcherenv.Env{"WINEDEBUG": "+relay", "VKD3D_DEBUG": "trace"}, false); env["WINEDEBUG"] != "+relay" || env["VKD3D_DEBUG"] != "trace" {
		t.Error("user choices overridden")
	}
	if env := LoggingEnvironment(launcherenv.Env{"XODUS_NEXUS_VERBOSE_LOGS": "1"}, false); env["WINEDEBUG"] != "" {
		t.Error("verbose env")
	}
	if env := LoggingEnvironment(launcherenv.Env{}, true); !maps.Equal(env, launcherenv.Env{"XODUS_NEXUS_VERBOSE_LOGS": "1"}) {
		t.Errorf("verbose = %v", env)
	}
	steam := launcherenv.Env{"SteamAppId": "3950783330", "SteamGameId": "16968485195965530112", "SDL_GAMECONTROLLERCONFIG": "sample mapping", "PROTON_DISABLE_HIDRAW": "0x054c/0x0ce6"}
	if !maps.Equal(WindowEnvironment(steam), steam) {
		t.Error("Steam Input changed")
	}
	got := WindowEnvironment(launcherenv.Env{"DESKTOP_STARTUP_ID": "launcher", "XDG_ACTIVATION_TOKEN": "t", "XDG_DESKTOP_FILE_NAME": "xodus-nexus",
		"GIO_LAUNCHED_DESKTOP_FILE": "/tmp/x.desktop", "APPDIR": "/tmp/.mount", "APPIMAGE": "/tmp/X.AppImage", "DISPLAY": ":0", "WAYLAND_DISPLAY": "wayland-0", "XODUS_BRIDGE_KEY": "secret"})
	if !maps.Equal(got, launcherenv.Env{"DISPLAY": ":0", "WAYLAND_DISPLAY": "wayland-0", "XODUS_BRIDGE_KEY": "secret"}) {
		t.Errorf("window env = %v", got)
	}
}

type lines struct {
	sync.Mutex
	items []string
}

func (l *lines) add(line string) { l.Lock(); l.items = append(l.items, line); l.Unlock() }
func (l *lines) has(line string) bool {
	l.Lock()
	defer l.Unlock()
	return slices.Contains(l.items, line)
}

func waitFor(t *testing.T, check func() bool) {
	t.Helper()
	for range 300 {
		if check() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("fixture did not become ready")
}

func TestGameProcess(t *testing.T) {
	var output lines
	game, err := Spawn("printf", []string{"game output\n"}, proc.RunOptions{OnLine: output.add}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if result := game.Wait(); result.Code != 0 || !strings.Contains(result.Output, "game output") || !output.has("game output") {
		t.Errorf("result = %+v", result)
	}
	game.Cleanup()

	other, _ := proc.Spawn("sleep", []string{"30"}, proc.RunOptions{})
	defer other.Kill()
	var group lines
	script := `trap 'echo PARENT_STOP; wait; exit 0' INT
env --default-signal=INT sh -c 'trap "echo CHILD_STOP; exit 0" INT; echo CHILD_READY; while :; do sleep 0.05; done' &
wait`
	stopping, err := Spawn("sh", []string{"-c", script}, proc.RunOptions{OnLine: group.add}, nil)
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return group.has("CHILD_READY") })
	stopping.Stop()
	if result := stopping.Wait(); result.Code != 0 || !group.has("CHILD_STOP") || !group.has("PARENT_STOP") || other.Exited() {
		t.Errorf("group stop: %+v %v", result, group.items)
	}
	stopping.ForceStop()

	inherited, _ := Spawn("sh", []string{"-c", "sleep 30 & echo BACKEND_DONE; exit 7"}, proc.RunOptions{DrainAfterExit: true, OutputDrain: 50 * time.Millisecond}, nil)
	if result := inherited.Wait(); result.Code != 7 {
		t.Errorf("exit = %d", result.Code)
	}
	inherited.Cleanup()
	if syscall.Kill(-inherited.Pid(), 0) == nil {
		t.Error("helper still alive")
	}

	directory := t.TempDir()
	marker := filepath.Join(directory, "prefix-control")
	var stubborn lines
	resistant, _ := Spawn("sh", []string{"-c", `trap '' INT TERM; echo READY; while :; do sleep 0.05; done`}, proc.RunOptions{OnLine: stubborn.add},
		&StopPrefix{Command: "sh", Args: []string{"-c", `printf stopped > "$0"`, marker}})
	waitFor(t, func() bool { return stubborn.has("READY") })
	resistant.ForceStop()
	resistant.Wait()
	if data, _ := os.ReadFile(marker); string(data) != "stopped" {
		t.Error("prefix control not run")
	}
	if status, ok := resistant.Cmd.ProcessState.Sys().(syscall.WaitStatus); !ok || !status.Signaled() || status.Signal() != syscall.SIGKILL {
		t.Error("signal-resistant group not killed")
	}
}

type toolFixture struct {
	root, bin, wine string
	env             launcherenv.Env
}

func toolSetup(t *testing.T) toolFixture {
	root := t.TempDir()
	bin := filepath.Join(root, "tool's bin")
	os.Mkdir(bin, 0o755)
	f := toolFixture{root: root, bin: bin}
	f.wine = f.tool(t, "wine", `printf '%s\n' "$DXVK_HUD" "$WINEDLLOVERRIDES" "$XODUS_SOCKET"; printf '<%s>\n' "$@"; exit 7`)
	f.env = launcherenv.FromOS()
	f.env["PATH"] = bin + ":/usr/bin:/bin"
	f.env["XODUS_SOCKET"] = "/private-synthetic-socket"
	return f
}

func (f toolFixture) tool(t *testing.T, name, body string) string {
	file := filepath.Join(f.bin, name)
	if err := os.WriteFile(file, []byte("#!/bin/sh\n"+body+"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	return file
}

func tools(launchOptions string) *shared.GameLaunchTools {
	return &shared.GameLaunchTools{LaunchOptions: launchOptions}
}

func runWith(t *testing.T, command string, env launcherenv.Env, args ...string) proc.Result {
	result, err := proc.Run(command, args, proc.RunOptions{Env: env})
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func TestWrapperDefaultsAndArguments(t *testing.T) {
	f := toolSetup(t)
	dir := filepath.Join(f.root, "wrappers")
	if wrapper, _ := PrepareWrapper(f.wine, nil, dir, f.env, ""); wrapper.Command != f.wine {
		t.Error("default wrapper must be the runner itself")
	}
	if _, err := os.Stat(dir); err == nil {
		t.Error("default launch created files")
	}
	wrapper, err := PrepareWrapper(f.wine, tools(`DXVK_HUD="fps,frametimes" WINEDLLOVERRIDES="dxgi=n,b;winhttp=b" %command% --launcher-skip "two words" "literal $HOME"`), dir, f.env, "")
	if err != nil {
		t.Fatal(err)
	}
	result := runWith(t, wrapper.Command, f.env, "original game.exe")
	if result.Code != 7 || !strings.Contains(result.Output, "fps,frametimes\ndxgi=n,b;winhttp=b\n/private-synthetic-socket") ||
		!strings.Contains(result.Output, "<original game.exe>\n<--launcher-skip>\n<two words>\n<literal $HOME>") {
		t.Errorf("result = %+v", result)
	}
	wrapper.Cleanup()
	if _, err := os.Stat(wrapper.Command); err == nil {
		t.Error("wrapper not cleaned up")
	}
}

func TestDualSense(t *testing.T) {
	f := toolSetup(t)
	os.WriteFile(f.wine, []byte("#!/bin/sh\nprintf \"%s\\n\" \"$PROTON_DISABLE_HIDRAW\" \"$SteamGameId\" \"$SDL_GAMECONTROLLERCONFIG\"\n"), 0o700)
	source := f.env.Clone()
	source["PROTON_DISABLE_HIDRAW"], source["SteamGameId"], source["SDL_GAMECONTROLLERCONFIG"] = "1", "16968485195965530112", "mapping with spaces"
	wrapper, _ := PrepareWrapper(f.wine, tools("PROTON_DISABLE_HIDRAW=0x054c/0x0ce6 %command%"), filepath.Join(f.root, "wrappers"), source, "")
	if out := strings.TrimRight(runWith(t, wrapper.Command, source).Output, "\n"); out != "0x054c/0x0ce6\n16968485195965530112\nmapping with spaces" {
		t.Errorf("explicit = %q", out)
	}
	for _, disabled := range []string{"", "0", "0x1234/0xabcd", "0x054C/0x0CE6", "1"} {
		env := f.env.Clone()
		env["PROTON_DISABLE_HIDRAW"] = disabled
		on := true
		wrapper, _ := PrepareWrapper(f.wine, &shared.GameLaunchTools{DualSenseCompatibility: &on}, filepath.Join(f.root, "w-"+disabled), env, "")
		expected := disabled
		if disabled != "1" && !regexp.MustCompile(`(?i)0x054c/0x0ce6`).MatchString(disabled) {
			if disabled == "" || disabled == "0" {
				expected = "0x054c/0x0ce6"
			} else {
				expected = disabled + ",0x054c/0x0ce6"
			}
		}
		if first := strings.Split(runWith(t, wrapper.Command, env).Output, "\n")[0]; first != expected {
			t.Errorf("%q: %q, want %q", disabled, first, expected)
		}
	}
	f.tool(t, "custom-wrap", `exec "$@"`)
	os.WriteFile(f.wine, []byte("#!/bin/sh\nprintf \"%s\\n\" \"$PROTON_DISABLE_HIDRAW\" \"$@\"\n"), 0o700)
	on := true
	combined := f.env.Clone()
	combined["PROTON_DISABLE_HIDRAW"] = "1"
	wrapper, _ = PrepareWrapper(f.wine, &shared.GameLaunchTools{DualSenseCompatibility: &on, LaunchOptions: `PROTON_DISABLE_HIDRAW=0x1234/0xabcd custom-wrap %command% "two words"`},
		filepath.Join(f.root, "combined"), combined, "")
	if out := runWith(t, wrapper.Command, f.env, "game.exe").Output; out != "0x1234/0xabcd,0x054c/0x0ce6\ngame.exe\ntwo words\n" {
		t.Errorf("combined = %q", out)
	}
}

func TestMangoHudAndGamescope(t *testing.T) {
	f := toolSetup(t)
	env := f.env.Clone()
	env["PATH"] = f.bin
	f.tool(t, "env", `while [ $# -gt 0 ]; do case "$1" in *=*) export "$1"; shift;; *) break;; esac; done; exec "$@"`)
	f.tool(t, "mangohud", `echo MANGO; exec "$@"`)
	f.tool(t, "gamemoderun", `echo GAMEMODE; exec "$@"`)
	f.tool(t, "gamescope", `echo GAMESCOPE; while [ "$1" != -- ]; do printf '%s\n' "$1"; shift; done; shift; exec "$@"`)
	dir := filepath.Join(f.root, "wrappers")
	mango, _ := PrepareWrapper(f.wine, &shared.GameLaunchTools{MangoHud: true}, dir, env, "")
	if !strings.Contains(runWith(t, mango.Command, env, "game.exe").Output, "MANGO") {
		t.Error("MangoHud")
	}
	if _, err := PrepareWrapper(f.wine, &shared.GameLaunchTools{MangoHud: true, Gamescope: true}, dir, env, ""); err == nil || !strings.Contains(err.Error(), "mangoapp") {
		t.Errorf("mangoapp: %v", err)
	}
	f.tool(t, "mangoapp", "exit 99")
	scope, err := PrepareWrapper(f.wine, &shared.GameLaunchTools{MangoHud: true, Gamescope: true, GamescopeArgs: "-W 1920 -f", LaunchOptions: "gamemoderun %command%"}, dir, env, "")
	if err != nil {
		t.Fatal(err)
	}
	result := runWith(t, scope.Command, env, "game.exe")
	if !strings.Contains(result.Output, "GAMEMODE\nGAMESCOPE\n-W\n1920\n-f\n--mangoapp") || strings.Contains(result.Output, "\nMANGO\n") || result.Code != 7 {
		t.Errorf("gamescope = %+v", result)
	}
	if _, err := PrepareWrapper(f.wine, &shared.GameLaunchTools{MangoHud: true}, dir, launcherenv.Env{"PATH": f.root}, ""); err == nil || !strings.Contains(err.Error(), "mangohud") {
		t.Errorf("missing tool: %v", err)
	}
}

func TestLaunchArguments(t *testing.T) {
	if PreparedWine("/compat/pfx-runner", "", "/runtime/wine") != "/compat/pfx-runner" ||
		PreparedWine("/compat/pfx-runner", "/compat/game-runner", "/tools/wrapper") != "/compat/game-runner" || PreparedWine("", "", "/tools/wrapper") != "/tools/wrapper" {
		t.Error("prepared wine")
	}
	for _, executable := range []string{"start_protected_game.exe", "Dungeons.exe", "FSD/Binaries/WinGDK/Shipping.exe"} {
		args, _ := LaunchArguments("/package", executable, "/runtime/wine", "US", "patch", "patch")
		if !slices.Equal(args, []string{"run", "/package", "/runtime/wine", "--exe", executable, "--materialize-chain", "--market", "US"}) {
			t.Errorf("args = %v", args)
		}
	}
	if args, _ := LaunchArguments("/package", "Launcher.exe", "/shared/wine", "", "patch", "patch"); len(args) != 6 {
		t.Error("no market")
	}
	if _, err := LaunchArguments("/package", "Game.exe", "/wine", "", "old", "patch"); err == nil || !strings.Contains(err.Error(), "Update the backend") {
		t.Errorf("old backend: %v", err)
	}
	if _, err := LaunchArguments("/package", "", "/wine", "", "patch", "patch"); err == nil || !strings.Contains(err.Error(), "manifest has no launch executable") {
		t.Errorf("missing exe: %v", err)
	}
}

func updateXML(version string, hr int64, stage string) string {
	return fmt.Sprintf("<Response><HResult>%d</HResult><Checked>%v</Checked><LatestVersion>%s</LatestVersion><ErrorStage>%s</ErrorStage></Response>", hr, hr == 0, version, stage)
}

func TestUpdates(t *testing.T) {
	if !*UpdateResult(updateXML("1.10.0.0", 0x80004001, "version-comparison"), "1.9.0.0").Available ||
		*UpdateResult(updateXML("1.9.0.0", 0, ""), "1.9.0.0").Available || *UpdateResult(updateXML("1.8.0.0", 0, ""), "1.9.0.0").Available {
		t.Error("version comparison")
	}
	for _, xml := range []string{updateXML("1.10.0.0", 0x80070005, "package-query"), updateXML("1.10.0.0", 0x80004001, "package-query"), updateXML("0", 0, ""), "<Response/>"} {
		if result := UpdateResult(xml, "1.9.0.0"); result.Available != nil || result.Error == "" {
			t.Errorf("%s = %+v", xml, result)
		}
	}
	socket := filepath.Join(t.TempDir(), "s")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		conn.Read(make([]byte, 4096))
		xml := []byte(updateXML("1.10.0.0", 0x80004001, "version-comparison"))
		header := make([]byte, 8)
		binary.LittleEndian.PutUint32(header, updateMagic)
		binary.LittleEndian.PutUint16(header[4:], 10)
		binary.LittleEndian.PutUint16(header[6:], uint16(len(xml)))
		conn.Write(header[:3])
		time.Sleep(5 * time.Millisecond)
		conn.Write(append(header[3:], xml...))
	}()
	if result, err := QueryUpdate(context.Background(), socket, "9ABCDEFGHIJK", "1.9.0.0"); err != nil || !*result.Available {
		t.Errorf("query = %+v, %v", result, err)
	}
}

func eacBridge(t *testing.T, library string) string {
	root := filepath.Join(library, "steamapps/common/Proton EasyAntiCheat Runtime")
	os.MkdirAll(filepath.Join(root, "v2/lib64"), 0o755)
	for _, name := range []string{"easyanticheat_x64.dll", "easyanticheat_x64.so"} {
		os.WriteFile(filepath.Join(root, "v2/lib64", name), []byte("test fixture"), 0o644)
	}
	return root
}

func TestAntiCheat(t *testing.T) {
	original := launcherenv.Env{"EOS_USE_ANTICHEATCLIENTNULL": "1", "PROTON_EAC_LAUNCHER_PROCESS": "1", "PROTON_EAC_RUNTIME": "/runtime/eac", "WINEPREFIX": "/prefix"}
	if result := AntiCheatEnvironment(original); !maps.Equal(result, launcherenv.Env{"PROTON_EAC_RUNTIME": "/runtime/eac", "WINEPREFIX": "/prefix"}) || original["EOS_USE_ANTICHEATCLIENTNULL"] != "1" {
		t.Errorf("env = %v", result)
	}
	root := t.TempDir()
	if InstalledEACRuntime(root) != "" {
		t.Error("absent runtime found")
	}
	runtime := eacBridge(t, root)
	if InstalledEACRuntime(root) != runtime {
		t.Error("default library")
	}
	os.Remove(filepath.Join(runtime, "v2/lib64/easyanticheat_x64.so"))
	if InstalledEACRuntime(root) != "" {
		t.Error("incomplete runtime advertised")
	}
	other := t.TempDir()
	secondary := filepath.Join(other, `secondary "library"`)
	runtime = eacBridge(t, secondary)
	os.MkdirAll(filepath.Join(other, "steamapps"), 0o755)
	os.WriteFile(filepath.Join(other, "steamapps/libraryfolders.vdf"), []byte(`"libraryfolders" { "1" { "path" "`+strings.ReplaceAll(secondary, `"`, `\"`)+`" } "2" { "path" "relative-path" } }`), 0o644)
	if InstalledEACRuntime(other) != runtime {
		t.Error("secondary library")
	}
}

func mode(t *testing.T, path string) os.FileMode {
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return info.Mode().Perm()
}

func TestManagedPrefix(t *testing.T) {
	root := t.TempDir()
	compat := filepath.Join(root, "compatdata", "game")
	if err := PrepareManagedPrefix(compat); err != nil {
		t.Fatal(err)
	}
	if mode(t, compat) != 0o700 || mode(t, filepath.Join(compat, "pfx")) != 0o700 {
		t.Error("private directories")
	}
	for _, perm := range []os.FileMode{0o775, 0o755, 0o700} {
		compat := filepath.Join(t.TempDir(), "game")
		prefix := filepath.Join(compat, "pfx")
		nested := filepath.Join(prefix, "drive_c", "users", "test")
		os.MkdirAll(nested, 0o755)
		os.Chmod(compat, perm)
		os.Chmod(prefix, perm)
		os.Chmod(nested, 0o755)
		file := filepath.Join(nested, "unchanged.dat")
		os.WriteFile(file, []byte("existing data"), 0o644)
		PrepareManagedPrefix(compat)
		PrepareManagedPrefix(compat)
		if mode(t, compat) != 0o700 || mode(t, prefix) != 0o700 || mode(t, nested) != 0o755 || mode(t, file) != 0o644 {
			t.Errorf("%o: modes changed beyond the boundaries", perm)
		}
	}
	for _, leaf := range []string{"game", "game/pfx"} {
		root := t.TempDir()
		target := filepath.Join(root, "external")
		os.Mkdir(target, 0o755)
		os.Chmod(target, 0o755)
		link := filepath.Join(root, leaf)
		os.MkdirAll(filepath.Dir(link), 0o755)
		os.Symlink(target, link)
		if err := PrepareManagedPrefix(filepath.Join(root, "game")); err == nil {
			t.Errorf("symlink at %s accepted", leaf)
		}
		if entries, _ := os.ReadDir(target); mode(t, target) != 0o755 || len(entries) != 0 {
			t.Errorf("symlink target at %s modified", leaf)
		}
	}
	file := filepath.Join(t.TempDir(), "file")
	os.WriteFile(file, []byte("unchanged"), 0o644)
	if PrepareManagedPrefix(file) == nil {
		t.Error("file accepted")
	}
	if err := PrepareManagedPrefix("relative"); err == nil || !strings.Contains(err.Error(), "absolute") {
		t.Errorf("relative: %v", err)
	}
}

func TestVCRuntime(t *testing.T) {
	prefix := t.TempDir()
	write := func(lines ...string) {
		os.WriteFile(filepath.Join(prefix, "system.reg"), []byte(strings.Join(lines, "\n")), 0o644)
	}
	if InstalledVCRuntimeVersion(prefix) != "" {
		t.Error("missing registry")
	}
	write("WINE REGISTRY Version 2", `[Software\\Microsoft\\VisualStudio\\14.0\\VC\\Runtimes\\x64] 1786134203`, `"Installed"=dword:00000001`, `"Version"="14.42.34433.0"`, "", `[Software\\Other] 1`)
	if got := InstalledVCRuntimeVersion(prefix); got != "14.42.34433.0" || VersionAtLeast(got, VCRedistVersion) || !VersionAtLeast("14.44.35211.0", VCRedistVersion) {
		t.Errorf("version = %q", got)
	}
	write(`[Software\\Microsoft\\VisualStudio\\14.0\\VC\\Runtimes\\x64] 123`, `"Installed"=dword:00000000`, `"Version"="14.44.35211.0"`)
	if InstalledVCRuntimeVersion(prefix) != "" {
		t.Error("uninstalled accepted")
	}
	write(`[Software\\Microsoft\\VisualStudio\\14.0\\VC\\Runtimes\\x64] 1791621974`, `"Bld"=dword:0000898b`, `"Installed"=dword:00000001`, `"Version"="v14.44.35211.00"`)
	if got := InstalledVCRuntimeVersion(prefix); got != "14.44.35211.00" || !VersionAtLeast(got, VCRedistVersion) {
		t.Errorf("v-prefixed = %q", got)
	}
	if !strings.Contains(VCRedistURL, "CC0FF0EB1DC3F5188AE6300FAEF32BF5BEEBA4BDD6E8E445A9184072096B713B") {
		t.Error("URL must embed the uppercase hash")
	}
}
