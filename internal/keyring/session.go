package keyring

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"syscall"
	"time"

	"xodus-nexus-wails/internal/install"
	"xodus-nexus-wails/internal/launcherenv"
)

type KDEProvider string

const (
	ProviderKWallet  KDEProvider = "kwallet"
	ProviderKWallet5 KDEProvider = "kwallet5"
	ProviderKSecretD KDEProvider = "ksecretd"
)

var (
	localDisplay  = regexp.MustCompile(`^:\d{1,5}(?:\.\d{1,3})?$`)
	waylandName   = regexp.MustCompile(`^[a-zA-Z0-9_-][a-zA-Z0-9_.-]{0,254}$`)
	controlChars  = regexp.MustCompile(`[\x00-\x1f\x7f]`)
	unsafeRuntime = regexp.MustCompile(`[\x00-\x20\x7f,;%=]`)
)

func ownedBy(info fs.FileInfo, uid int) bool {
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && int(stat.Uid) == uid
}

// SessionEnvironment gives only the provider the current display. It never
// mutates the bus/manager environment, installs persistent units, guesses :0
// or changes wallet settings.
func SessionEnvironment(source launcherenv.Env) (launcherenv.Env, error) {
	if !strings.HasPrefix(source["DBUS_SESSION_BUS_ADDRESS"], "unix:") {
		return nil, errors.New("No local session bus")
	}
	env := launcherenv.External(source)
	// The distro provider must use its own Qt and libraries, not Steam/AppImage.
	for key := range env {
		if strings.HasPrefix(key, "LD_") || strings.HasPrefix(key, "QT_") || strings.HasPrefix(key, "APPIMAGE") || slices.Contains([]string{"APPDIR", "ARGV0", "OWD"}, key) {
			delete(env, key)
		}
	}
	uid := os.Getuid()
	if display := source["DISPLAY"]; display != "" && localDisplay.MatchString(display) {
		number := strings.Split(display[1:], ".")[0]
		socket, err := os.Lstat("/tmp/.X11-unix/X" + number)
		if err != nil || socket.Mode()&fs.ModeSocket == 0 || !ownedBy(socket, uid) {
			return nil, errors.New("Unavailable local display")
		}
		if authority := source["XAUTHORITY"]; authority != "" {
			if !filepath.IsAbs(authority) || controlChars.MatchString(authority) {
				return nil, errors.New("Invalid display authority")
			}
			if syscall.Access(authority, 4) != nil {
				return nil, errors.New("Invalid display authority")
			}
		}
		env["QT_QPA_PLATFORM"] = "xcb"
		delete(env, "WAYLAND_DISPLAY")
		return env, nil
	}
	name, runtime := source["WAYLAND_DISPLAY"], source["XDG_RUNTIME_DIR"]
	if name == "" || !waylandName.MatchString(name) || runtime == "" || !filepath.IsAbs(runtime) {
		return nil, errors.New("Unavailable local display")
	}
	socket, err := os.Lstat(filepath.Join(runtime, name))
	if err != nil || socket.Mode()&fs.ModeSocket == 0 || !ownedBy(socket, uid) {
		return nil, errors.New("Unavailable local display")
	}
	env["QT_QPA_PLATFORM"] = "wayland"
	delete(env, "DISPLAY")
	delete(env, "XAUTHORITY")
	return env, nil
}

// unitWord quotes a literal systemd Exec argument. systemd Exec properties are
// not shell commands; whitespace is preserved and $ / % expansion escaped.
func unitWord(value string) (string, error) {
	if len(value) > 4096 || controlChars.MatchString(value) {
		return "", errors.New("Invalid keyring stop helper")
	}
	value = strings.NewReplacer(`\`, `\\`, `"`, `\"`, "$", "$$", "%", "%%").Replace(value)
	return `"` + value + `"`, nil
}

var sessionKeys = []string{"HOME", "USER", "LOGNAME", "LANG", "LANGUAGE", "LC_ALL", "LC_CTYPE", "LC_MESSAGES",
	"XDG_CONFIG_HOME", "XDG_DATA_HOME", "XDG_CACHE_HOME", "XDG_DATA_DIRS", "XDG_RUNTIME_DIR",
	"XDG_CURRENT_DESKTOP", "DISPLAY", "XAUTHORITY", "WAYLAND_DISPLAY", "DBUS_SESSION_BUS_ADDRESS", "QT_QPA_PLATFORM"}

func rootExecutable(file string) error {
	info, err := os.Stat(file)
	if err != nil || !info.Mode().IsRegular() || !ownedBy(info, 0) || info.Mode().Perm()&0o022 != 0 || syscall.Access(file, 1) != nil {
		return errors.New("Unavailable system keyring provider")
	}
	return nil
}

// StartSessionKeyring starts the KDE provider as a transient user service. A
// provider forked from Steam remains in its reaper even after Nexus exits; a
// transient user service belongs to the graphical session, not the game.
func StartSessionKeyring(provider KDEProvider, source launcherenv.Env, owned func() bool, stopHelper string) error {
	env, err := SessionEnvironment(source)
	if err != nil {
		return err
	}
	binary := map[KDEProvider]string{ProviderKSecretD: "/usr/bin/ksecretd", ProviderKWallet5: "/usr/bin/kwalletd5"}[provider]
	if binary == "" {
		binary = "/usr/bin/kwalletd6"
	}
	for _, executable := range []string{binary, "/usr/bin/systemd-run", "/usr/bin/env"} {
		if err := rootExecutable(executable); err != nil {
			return err
		}
	}
	runtime := source["XDG_RUNTIME_DIR"]
	if runtime == "" || !filepath.IsAbs(runtime) || unsafeRuntime.MatchString(runtime) {
		return errors.New("Unavailable user manager")
	}
	directory, err1 := os.Lstat(runtime)
	managerBus, err2 := os.Lstat(filepath.Join(runtime, "bus"))
	uid := os.Getuid()
	if err1 != nil || err2 != nil || !directory.IsDir() || !ownedBy(directory, uid) || directory.Mode().Perm()&0o077 != 0 ||
		managerBus.Mode()&fs.ModeSocket == 0 || !ownedBy(managerBus, uid) {
		return errors.New("Unavailable user manager")
	}
	// Start with an empty service environment: neither Steam nor stale manager
	// Qt settings, loader paths or unrelated credentials belong in this process.
	var assignments []string
	for _, key := range sessionKeys {
		value, ok := env[key]
		if !ok {
			continue
		}
		if len(value) > 4096 || controlChars.MatchString(value) {
			return errors.New("Invalid keyring session environment")
		}
		assignments = append(assignments, key+"="+value)
	}
	var stopProperties []string
	if stopHelper != "" {
		if !filepath.IsAbs(stopHelper) {
			return errors.New("Invalid keyring stop helper")
		}
		info, err := os.Stat(stopHelper)
		if err != nil || !info.Mode().IsRegular() || !(ownedBy(info, 0) || ownedBy(info, uid)) || info.Mode().Perm()&0o022 != 0 || syscall.Access(stopHelper, 1) != nil {
			return errors.New("Unavailable keyring stop helper")
		}
		var words []string
		for _, word := range []string{"/usr/bin/env", "-i", "PATH=/usr/bin:/bin", "DBUS_SESSION_BUS_ADDRESS=" + env["DBUS_SESSION_BUS_ADDRESS"], stopHelper, "keyring-session-stop", "--provider-pid"} {
			quoted, err := unitWord(word)
			if err != nil {
				return err
			}
			words = append(words, quoted)
		}
		// MAINPID is the systemd-owned provider, never a renderer-supplied PID.
		// The native helper rechecks its UID, executable, Nexus cgroup and unique
		// bus owner, then waits for Qt's normal exit to flush pending wallet writes.
		stopProperties = append(stopProperties, "--property=ExecStop="+strings.Join(words, " ")+" ${MAINPID}", "--property=TimeoutStopSec=12s")
	}
	args := append([]string{"--user", "--collect", "--service-type=exec", "--expand-environment=no", "--quiet",
		"--description=Xodus Nexus session wallet", "--unit=xodus-nexus-keyring-" + install.NewUUID(), "--property=PartOf=graphical-session.target",
		"--property=StandardOutput=null", "--property=StandardError=null"}, stopProperties...)
	args = append(append(append(args, "--", "/usr/bin/env", "-i", "PATH=/usr/bin:/bin"), assignments...), binary)
	if owned() {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "/usr/bin/systemd-run", args...)
	cmd.Env = []string{"PATH=/usr/bin:/bin", "XDG_RUNTIME_DIR=" + runtime, "DBUS_SESSION_BUS_ADDRESS=unix:path=" + filepath.Join(runtime, "bus")}
	if err := cmd.Run(); err != nil {
		return errors.New("Could not start system keyring provider")
	}
	return nil
}
