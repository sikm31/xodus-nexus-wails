// Package launcherenv ports launcher-environment.ts and launcher-fonts.ts.
package launcherenv

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
)

// Env is a mutable copy of a process environment.
type Env map[string]string

func FromOS() Env {
	env := Env{}
	for _, entry := range os.Environ() {
		if name, value, ok := strings.Cut(entry, "="); ok {
			env[name] = value
		}
	}
	return env
}

func (e Env) Clone() Env {
	out := make(Env, len(e))
	for k, v := range e {
		out[k] = v
	}
	return out
}

// List returns KEY=value entries in a stable order for exec.Cmd.Env.
func (e Env) List() []string {
	out := make([]string, 0, len(e))
	for k, v := range e {
		out = append(out, k+"="+v)
	}
	sort.Strings(out)
	return out
}

type fontState struct {
	file        string
	original    string
	hadOriginal bool
}

var (
	activeMu sync.Mutex
	active   *fontState
)

var xmlEscaper = strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;", "'", "&apos;")

// ExternalFontEnvironment restores the caller's font configuration for subprocesses.
func ExternalFontEnvironment(source Env) Env {
	env := source.Clone()
	activeMu.Lock()
	state := active
	activeMu.Unlock()
	if state != nil && env["FONTCONFIG_FILE"] == state.file {
		if state.hadOriginal {
			env["FONTCONFIG_FILE"] = state.original
		} else {
			delete(env, "FONTCONFIG_FILE")
		}
	}
	return env
}

// ConfigureLauncherFonts must run before GTK/WebKit initialize fontconfig. It
// keeps the host's fonts and adds the bundled fallback font directory.
// It updates the live process environment.
func ConfigureLauncherFonts(fontDirectory, cacheDirectory, temporaryRoot string) (dispose func(), err error) {
	if os.Getenv("FONTCONFIG_SYSROOT") != "" {
		return nil, errors.New("A custom FONTCONFIG_SYSROOT is active; leaving its font configuration unchanged.")
	}
	if _, err := os.Stat(filepath.Join(fontDirectory, "DejaVuSans.ttf")); err != nil {
		return nil, errors.New("The bundled fallback font is missing.")
	}
	if temporaryRoot == "" {
		temporaryRoot = os.TempDir()
	}
	original, hadOriginal := os.LookupEnv("FONTCONFIG_FILE")
	requested := original
	if requested == "" {
		requested = "/etc/fonts/fonts.conf"
	}
	host := requested
	if !filepath.IsAbs(requested) {
		search := os.Getenv("FONTCONFIG_PATH")
		if search == "" {
			search = "/etc/fonts"
		}
		for _, directory := range filepath.SplitList(search) {
			candidate, _ := filepath.Abs(filepath.Join(directory, requested))
			if _, err := os.Stat(candidate); err == nil {
				host = candidate
				break
			}
		}
	}
	if err := os.MkdirAll(cacheDirectory, 0o700); err != nil {
		return nil, err
	}
	directory, err := os.MkdirTemp(temporaryRoot, "nexus-fonts-")
	if err != nil {
		return nil, err
	}
	file := filepath.Join(directory, "fonts.conf")
	// Keep the host's fonts, aliases and preferences. The private cache comes
	// first; no system/user font installation or cache reset is needed.
	content := fmt.Sprintf("<?xml version=\"1.0\"?>\n<!DOCTYPE fontconfig SYSTEM \"urn:fontconfig:fonts.dtd\">\n<fontconfig>\n  <cachedir>%s</cachedir>\n  <include ignore_missing=\"yes\">%s</include>\n  <dir>%s</dir>\n</fontconfig>\n",
		xmlEscaper.Replace(cacheDirectory), xmlEscaper.Replace(host), xmlEscaper.Replace(fontDirectory))
	handle, err := os.OpenFile(file, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err == nil {
		_, err = handle.WriteString(content)
		if closeErr := handle.Close(); err == nil {
			err = closeErr
		}
	}
	if err != nil {
		os.RemoveAll(directory)
		return nil, err
	}
	activeMu.Lock()
	active = &fontState{file: file, original: original, hadOriginal: hadOriginal}
	activeMu.Unlock()
	os.Setenv("FONTCONFIG_FILE", file)
	return func() {
		if os.Getenv("FONTCONFIG_FILE") == file {
			if hadOriginal {
				os.Setenv("FONTCONFIG_FILE", original)
			} else {
				os.Unsetenv("FONTCONFIG_FILE")
			}
		}
		activeMu.Lock()
		if active != nil && active.file == file {
			active = nil
		}
		activeMu.Unlock()
		os.RemoveAll(directory)
	}, nil
}

// SteamImeActive reports the marker set only by the pre-GTK wrapper, not by desktop launch paths.
func SteamImeActive(env Env) bool {
	if env["XODUS_NEXUS_STEAM_IME"] != "1" || env["GTK_IM_MODULE"] != "simple" {
		return false
	}
	for _, desktop := range strings.Split(env["XDG_CURRENT_DESKTOP"], ":") {
		if strings.EqualFold(desktop, "gamescope") {
			return true
		}
	}
	return false
}

// External keeps launcher-only native UI workarounds out of games and external tools.
// A nil source means the current process environment.
func External(source Env) Env {
	if source == nil {
		source = FromOS()
	}
	env := ExternalFontEnvironment(source)
	if SteamImeActive(env) {
		env["GTK_IM_MODULE"] = "Steam"
	}
	delete(env, "XODUS_NEXUS_STEAM_IME")
	return env
}
