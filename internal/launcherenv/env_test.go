package launcherenv

import (
	"maps"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func session() Env {
	return Env{"XDG_CURRENT_DESKTOP": "gamescope", "GTK_IM_MODULE": "simple",
		"XODUS_NEXUS_STEAM_IME": "1", "QT_IM_MODULE": "steam", "SteamGameId": "123", "DISPLAY": ":1"}
}

func TestSteamImeRestoredWithoutMutatingCaller(t *testing.T) {
	env := session()
	if !SteamImeActive(env) {
		t.Fatal("marker should be active")
	}
	want := Env{"XDG_CURRENT_DESKTOP": "gamescope", "GTK_IM_MODULE": "Steam", "QT_IM_MODULE": "steam", "SteamGameId": "123", "DISPLAY": ":1"}
	if got := External(env); !maps.Equal(got, want) {
		t.Errorf("External = %v", got)
	}
	if !maps.Equal(env, session()) {
		t.Error("caller environment was mutated")
	}
}

func TestExplicitImeOverridesPreserved(t *testing.T) {
	for _, ime := range []string{"ibus", "fcitx", "Steam", ""} {
		env := session()
		env["GTK_IM_MODULE"] = ime
		if got := External(env)["GTK_IM_MODULE"]; got != ime {
			t.Errorf("IME %q became %q", ime, got)
		}
	}
	env := session()
	delete(env, "GTK_IM_MODULE")
	if _, ok := External(env)["GTK_IM_MODULE"]; ok {
		t.Error("an unset IME must stay unset")
	}
}

func TestUnrelatedSimpleImeKept(t *testing.T) {
	variants := []func(Env){
		func(e Env) { delete(e, "XODUS_NEXUS_STEAM_IME") },
		func(e Env) { e["XODUS_NEXUS_STEAM_IME"] = "invalid" },
		func(e Env) { e["XDG_CURRENT_DESKTOP"] = "KDE" },
		func(e Env) { e["XDG_CURRENT_DESKTOP"] = "gamescope-nested" },
	}
	for i, change := range variants {
		env := session()
		change(env)
		external := External(env)
		if SteamImeActive(env) || external["GTK_IM_MODULE"] != "simple" {
			t.Errorf("variant %d restored the Steam IME", i)
		}
		if _, ok := external["XODUS_NEXUS_STEAM_IME"]; ok {
			t.Errorf("variant %d leaked the private marker", i)
		}
	}
}

func TestControllerAndSessionSettingsUnchanged(t *testing.T) {
	env := session()
	extra := Env{"PROTON_DISABLE_HIDRAW": "0x054c/0x0ce6", "SDL_GAMECONTROLLERCONFIG": "03000000,Custom Xbox,a:b0,b:b1,",
		"WINEPREFIX": "/tmp/prefix with spaces", "WAYLAND_DISPLAY": "gamescope-0", "DBUS_SESSION_BUS_ADDRESS": "unix:path=/run/user/1000/bus"}
	maps.Copy(env, extra)
	want := env.Clone()
	delete(want, "XODUS_NEXUS_STEAM_IME")
	want["GTK_IM_MODULE"] = "Steam"
	if got := External(env); !maps.Equal(got, want) {
		t.Errorf("External = %v", got)
	}
}

func TestFontsComposeAndAreIdempotent(t *testing.T) {
	root := t.TempDir()
	fonts := filepath.Join(root, "fonts")
	os.Mkdir(fonts, 0o755)
	os.WriteFile(filepath.Join(fonts, "DejaVuSans.ttf"), []byte("font"), 0o644)
	t.Setenv("FONTCONFIG_FILE", "/host/fonts.conf")
	dispose, err := ConfigureLauncherFonts(fonts, filepath.Join(root, "cache"), root)
	if err != nil {
		t.Fatal(err)
	}
	config := os.Getenv("FONTCONFIG_FILE")
	data, _ := os.ReadFile(config)
	if config == "/host/fonts.conf" || !strings.Contains(string(data), "<include ignore_missing=\"yes\">/host/fonts.conf</include>") {
		t.Errorf("config %s = %s", config, data)
	}
	env := session()
	env["FONTCONFIG_FILE"] = config
	external := External(env)
	if external["FONTCONFIG_FILE"] != "/host/fonts.conf" || external["GTK_IM_MODULE"] != "Steam" || !maps.Equal(External(external), external) {
		t.Errorf("external = %v", external)
	}
	dispose()
	if os.Getenv("FONTCONFIG_FILE") != "/host/fonts.conf" {
		t.Error("dispose must restore the original configuration")
	}
	if _, err := os.Stat(filepath.Dir(config)); !os.IsNotExist(err) {
		t.Error("dispose must remove the private configuration")
	}
}
