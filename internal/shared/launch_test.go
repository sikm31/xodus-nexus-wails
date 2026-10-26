package shared

import (
	"encoding/json"
	"maps"
	"math"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestParseLaunchOptions(t *testing.T) {
	parsed, err := ParseLaunchOptions(`WINEDLLOVERRIDES="dxgi=n,b;winhttp=b" PROTON_ENABLE_WAYLAND=1 gamemoderun %command% --launcher-skip "two words"`)
	if err != nil || !maps.Equal(parsed.Environment, map[string]string{"WINEDLLOVERRIDES": "dxgi=n,b;winhttp=b", "PROTON_ENABLE_WAYLAND": "1"}) ||
		!slices.Equal(parsed.Prefix, []string{"gamemoderun"}) || !slices.Equal(parsed.GameArgs, []string{"--launcher-skip", "two words"}) {
		t.Errorf("parsed = %+v, %v", parsed, err)
	}
	if parsed, _ := ParseLaunchOptions("DXVK_HUD="); parsed.Environment["DXVK_HUD"] != "" || len(parsed.Prefix) != 0 || len(parsed.GameArgs) != 0 {
		t.Errorf("empty assignment = %+v", parsed)
	}
	if words, _ := CommandWords(`"/a path/it's tool" "literal $HOME" ""`); !slices.Equal(words, []string{"/a path/it's tool", "literal $HOME", ""}) {
		t.Errorf("words = %q", words)
	}
	for _, value := range []string{"mangohud %command% %command%", "a%command%", `bad "quote`, `bad\`, "x && y", "$(touch bad)", "x\ny", "x\x00y", strings.Repeat("x", 2049)} {
		if _, err := ParseLaunchOptions(value); err == nil {
			t.Errorf("accepted %.40q", value)
		}
	}
	for _, value := range []string{"WINEPREFIX=/other", "XODUS_SOCKET=/other", "WINE_DLL_FILE_MAP=bad", "STEAM_COMPAT_DATA_PATH=/other"} {
		if _, err := ParseLaunchOptions(value); err == nil || !strings.Contains(err.Error(), "managed by Nexus") {
			t.Errorf("%s: %v", value, err)
		}
	}
}

func TestNormalizeLaunchTools(t *testing.T) {
	if tools, _ := NormalizeLaunchTools(nil); *tools.DualSenseCompatibility || tools.MangoHud || tools.LaunchOptions != "" {
		t.Error("defaults")
	}
	for _, raw := range []string{`{"mangoHud":"yes","gamescope":false,"gamescopeArgs":"","launchOptions":""}`,
		`{"mangoHud":false,"gamescope":false,"gamescopeArgs":"","launchOptions":"","dualSenseCompatibility":"yes"}`,
		`{"mangoHud":false,"gamescope":false,"gamescopeArgs":"-f --","launchOptions":""}`, `null`, `[]`} {
		if _, err := NormalizeLaunchTools(json.RawMessage(raw)); err == nil {
			t.Errorf("accepted %s", raw)
		}
	}
	tools, _ := NormalizeLaunchTools(json.RawMessage(`{"mangoHud":false,"gamescope":false,"gamescopeArgs":"","launchOptions":""}`))
	if *tools.DualSenseCompatibility {
		t.Error("dualSense default")
	}
	tools, _ = NormalizeLaunchTools(json.RawMessage(`{"mangoHud":false,"gamescope":false,"gamescopeArgs":"","launchOptions":" PROTON_ENABLE_WAYLAND=1 %command% ","dualSenseCompatibility":true}`))
	if !*tools.DualSenseCompatibility || tools.LaunchOptions != "PROTON_ENABLE_WAYLAND=1 %command%" {
		t.Errorf("tools = %+v", tools)
	}
}

func TestDownloadLimit(t *testing.T) {
	for value, want := range map[float64]int{0: 0, 0.5: 512, 0.1: 102, 1024: 1048576} {
		if got, err := DownloadLimitKiB(value); err != nil || got != want {
			t.Errorf("DownloadLimitKiB(%v) = %d, %v", value, got, err)
		}
	}
	if got, _ := DownloadLimitKiB(nil); got != 0 {
		t.Error("unset is unlimited")
	}
	for _, value := range []any{math.NaN(), math.Inf(1), -1.0, "10", 1025.0, 0.0001} {
		if _, err := DownloadLimitKiB(value); err == nil {
			t.Errorf("accepted %v", value)
		}
	}
	if args, _ := DownloadLimitArgs(nil, BackendCapabilities{}); len(args) != 0 {
		t.Error("no flag without capability")
	}
	if _, err := DownloadLimitArgs(10.0, BackendCapabilities{}); err == nil || !strings.Contains(err.Error(), "was not started without its limit") {
		t.Error("a requested limit requires the capability")
	}
	capable := BackendCapabilities{StreamingBandwidthLimit: true}
	if args, _ := DownloadLimitArgs(0.0, capable); !slices.Equal(args, []string{"--download-limit-kib", "0"}) {
		t.Errorf("args = %v", args)
	}
	if args, _ := DownloadLimitArgs(10.0, capable); !slices.Equal(args, []string{"--download-limit-kib", "10240"}) {
		t.Errorf("args = %v", args)
	}
	if command, _ := DownloadLimitCommand(1.5); command != `{"schema":1,"command":"set-download-limit","kibPerSecond":1536}`+"\n" {
		t.Errorf("command = %q", command)
	}
}

func subscription(status string, ids []string, checked time.Time) json.RawMessage {
	raw, _ := json.Marshal(map[string]any{"status": status, "productIds": ids, "checkedAt": JSISOString(checked)})
	return raw
}

func TestSubscription(t *testing.T) {
	now := time.Now()
	for ids, want := range map[string]string{"CFQ7TTC0KGQ8": "PC GAME PASS", "CFQ7TTC0K5DJ,CFQ7TTC0KGQ8,CFQ7TTC0KHS0": "ULTIMATE",
		"CFQ7TTC0P85B": "PREMIUM", "CFQ7TTC0K5DJ": "ESSENTIAL"} {
		if got := SubscriptionBadge(subscription("active", strings.Split(ids, ","), now), now); got != want {
			t.Errorf("%s = %q", ids, got)
		}
	}
	pc := []string{"CFQ7TTC0KGQ8"}
	for i, raw := range []json.RawMessage{nil, json.RawMessage(`{}`), subscription("active", []string{"invalid"}, now), subscription("unknown", pc, now),
		json.RawMessage(`{"status":"active","productIds":["CFQ7TTC0KGQ8"],"checkedAt":"bad"}`),
		subscription("active", pc, now.Add(-86_400_001*time.Millisecond)), subscription("active", pc, now.Add(10*time.Minute))} {
		if SubscriptionBadge(raw, now) != "" {
			t.Errorf("case %d treated as active", i)
		}
	}
	if s := ParseSubscription(subscription("none", []string{}, now), now); s == nil || s.Status != "none" {
		t.Error("none is valid")
	}
}

func TestRuntimeDisplay(t *testing.T) {
	runtime := RuntimeStatus{Installed: true, NexusAvailable: true, NexusName: "xodus-nexus-runtime-new-x86_64.tar.xz", Name: "xodus-nexus-runtime-old"}
	if !NexusRuntimeUpdateAvailable(runtime) {
		t.Error("update expected")
	}
	same := runtime
	same.Name = "xodus-nexus-runtime-new"
	if NexusRuntimeUpdateAvailable(same) {
		t.Error("same version is not an update")
	}
	installed := RuntimeStatus{Installed: true, Name: "xodus-nexus-runtime-11.0-20261011-g3fd4ae10", NexusVersion: "runtime-11.0-newer"}
	if got := DescribeRuntime(installed); got != (RuntimeDisplay{"Nexus runtime", "Nexus runtime", "11.0-20261011-g3fd4ae10"}) {
		t.Errorf("installed = %+v", got)
	}
	if got := DescribeRuntime(RuntimeStatus{Installed: true, Name: "nexus-runtime-11.0-experimental-g0d1ed78b"}); got.Version != "11.0-experimental-g0d1ed78b" {
		t.Errorf("experimental = %+v", got)
	}
	for name, source := range map[string]string{"proton-xodus-af521bf3-eac-bridge-v1": "Nexus runtime",
		"proton-xodus-af521bf3-xgamesave-pr28-0123456789-bridge-v1": "Nexus runtime",
		"xodus-proton-af521bf3": "Upstream Xodus Proton snapshot", "my-wine-build": "Custom runtime"} {
		if got := DescribeRuntime(RuntimeStatus{Installed: true, Name: name}).Source; got != source {
			t.Errorf("%s = %q", name, got)
		}
	}
	if DescribeRuntime(RuntimeStatus{}).Version != "Not installed" {
		t.Error("not installed")
	}
}
