package game

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"xodus-nexus-wails/internal/launcherenv"
	"xodus-nexus-wails/internal/shared"
)

func quote(value string) string { return "'" + strings.ReplaceAll(value, "'", `'"'"'`) + "'" }

func executable(command string, env launcherenv.Env, cwd string) (string, error) {
	var candidates []string
	if strings.Contains(command, "/") {
		candidates = []string{resolve(cwd, command)}
	} else {
		for _, directory := range filepath.SplitList(env["PATH"]) {
			if directory != "" {
				candidates = append(candidates, resolve(cwd, filepath.Join(directory, command)))
			}
		}
	}
	for _, file := range candidates {
		if info, err := os.Stat(file); err == nil && info.Mode().IsRegular() && info.Mode()&0o111 != 0 {
			return file, nil
		}
	}
	return "", fmt.Errorf(`Launch tool "%s" was not found or is not executable. Install it or disable it in this game's settings.`, command)
}

func resolve(cwd, path string) string {
	if filepath.IsAbs(path) {
		return filepath.Clean(path)
	}
	return filepath.Join(cwd, path)
}

type Wrapper struct {
	Command string
	Cleanup func() error
}

var dualSense = regexp.MustCompile(`(?i)0x054c/0x0ce6`)

// PrepareWrapper wraps only the game's Wine entry point. Auth, service, prefix
// setup and Stop stay outside. tools is raw JSON from the saved game (nil for defaults).
func PrepareWrapper(wine string, tools *shared.GameLaunchTools, root string, env launcherenv.Env, cwd string) (Wrapper, error) {
	none := Wrapper{Command: wine, Cleanup: func() error { return nil }}
	var raw json.RawMessage
	if tools != nil {
		raw, _ = json.Marshal(tools)
	}
	normalized, err := shared.NormalizeLaunchTools(raw)
	if err != nil {
		return Wrapper{}, err
	}
	if env == nil {
		env = launcherenv.FromOS()
	}
	if cwd == "" {
		cwd, _ = os.Getwd()
	}
	options, err := shared.ParseLaunchOptions(normalized.LaunchOptions)
	if err != nil {
		return Wrapper{}, err
	}
	environment := options.Environment
	if normalized.DualSenseCompatibility != nil && *normalized.DualSenseCompatibility {
		disabled, ok := environment["PROTON_DISABLE_HIDRAW"]
		if !ok {
			disabled = env["PROTON_DISABLE_HIDRAW"]
		}
		// Keep other device overrides and a deliberate all-device opt-out intact.
		if disabled != "1" && !dualSense.MatchString(disabled) {
			if disabled == "0" || disabled == "" {
				disabled = "0x054c/0x0ce6"
			} else {
				disabled += ",0x054c/0x0ce6"
			}
		}
		environment["PROTON_DISABLE_HIDRAW"] = disabled
	}
	// An explicit WINE_CPU_TOPOLOGY in the launch options wins.
	if topology := shared.CPUTopology(normalized.CPULimit); topology != "" && environment["WINE_CPU_TOPOLOGY"] == "" {
		environment["WINE_CPU_TOPOLOGY"] = topology
	}
	if !normalized.MangoHud && !normalized.Gamescope && len(options.Prefix) == 0 && len(environment) == 0 && len(options.GameArgs) == 0 {
		return none, nil
	}
	var words []string
	if len(environment) > 0 {
		tool, err := executable("env", env, cwd)
		if err != nil {
			return Wrapper{}, err
		}
		words = append(words, tool)
		keys := make([]string, 0, len(environment))
		for key := range environment {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			words = append(words, key+"="+environment[key])
		}
	}
	if len(options.Prefix) > 0 {
		tool, err := executable(options.Prefix[0], env, cwd)
		if err != nil {
			return Wrapper{}, err
		}
		words = append(append(words, tool), options.Prefix[1:]...)
	}
	if normalized.Gamescope {
		tool, err := executable("gamescope", env, cwd)
		if err != nil {
			return Wrapper{}, err
		}
		args, err := shared.CommandWords(normalized.GamescopeArgs)
		if err != nil {
			return Wrapper{}, err
		}
		words = append(append(words, tool), args...)
		if normalized.MangoHud {
			if _, err := executable("mangoapp", env, cwd); err != nil {
				return Wrapper{}, err
			}
			words = append(words, "--mangoapp")
		}
		words = append(words, "--")
	} else if normalized.MangoHud {
		tool, err := executable("mangohud", env, cwd)
		if err != nil {
			return Wrapper{}, err
		}
		words = append(words, tool)
	}
	words = append(words, wine)
	if err := os.MkdirAll(root, 0o700); err != nil {
		return Wrapper{}, err
	}
	directory, err := os.MkdirTemp(root, "game-tools-")
	if err != nil {
		return Wrapper{}, err
	}
	cleanup := func() error { return os.RemoveAll(directory) }
	command := filepath.Join(directory, "wine-wrapper")
	quoted := make([]string, len(words))
	for i, word := range words {
		quoted[i] = quote(word)
	}
	gameArgs := make([]string, len(options.GameArgs))
	for i, arg := range options.GameArgs {
		gameArgs[i] = quote(arg)
	}
	script := fmt.Sprintf("#!/bin/sh\nexec %s \"$@\" %s\n", strings.Join(quoted, " "), strings.Join(gameArgs, " "))
	handle, err := os.OpenFile(command, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o700)
	if err == nil {
		_, err = handle.WriteString(script)
		if closeErr := handle.Close(); err == nil {
			err = closeErr
		}
	}
	if err != nil {
		cleanup()
		return Wrapper{}, err
	}
	return Wrapper{Command: command, Cleanup: cleanup}, nil
}

// PreparedWine: the prepared runner owns WINEPREFIX and runtime libraries, even without tools.
func PreparedWine(command, launchCommand, fallback string) string {
	if launchCommand != "" {
		return launchCommand
	}
	if command != "" {
		return command
	}
	return fallback
}

// LaunchArguments is the one package launch policy. Runtime/backend, never
// title names, own compatibility.
func LaunchArguments(path, executable, wine, market, backendPatch, requiredPatch string) ([]string, error) {
	if backendPatch != requiredPatch {
		return nil, errors.New("Play requires the current managed Xodus backend. Update the backend in Settings.")
	}
	if strings.TrimSpace(executable) == "" {
		return nil, errors.New("The package manifest has no launch executable.")
	}
	args := []string{"run", path, wine, "--exe", executable, "--materialize-chain"}
	if market != "" {
		args = append(args, "--market", market)
	}
	return args, nil
}
