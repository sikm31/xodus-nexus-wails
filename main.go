// Xodus Nexus: a Microsoft Store game launcher for Linux built with Wails.
package main

import (
	"crypto/sha256"
	"embed"
	"fmt"
	"io/fs"
	"log"
	"os"
	"path/filepath"

	"github.com/wailsapp/wails/v3/pkg/application"
	"github.com/wailsapp/wails/v3/pkg/events"

	"xodus-nexus-wails/assets"
	"xodus-nexus-wails/internal/launcher"
	"xodus-nexus-wails/internal/launcherenv"
	"xodus-nexus-wails/internal/shortcuts"
	"xodus-nexus-wails/internal/updater"
)

// Version is set at build time with -ldflags "-X main.Version=…".
var Version = "0.3.13"

//go:embed all:frontend/dist
var frontend embed.FS

func main() {
	startup, startupErr := shortcuts.Arguments(os.Args[1:])
	userData := defaultUserData()
	if startup.Profile != "" {
		userData = startup.Profile
	}
	if err := os.MkdirAll(userData, 0o700); err != nil {
		log.Fatal(err)
	}
	if dispose := configureFonts(userData); dispose != nil {
		defer dispose()
	}
	demo := os.Getenv("XODUS_DEMO") == "1"
	executable, _ := os.Executable()
	if image := os.Getenv("APPIMAGE"); image != "" {
		executable = image
	}
	host := &wailsHost{userData: userData}
	core := launcher.New(host, launcher.Options{
		Version: Version, UserData: userData, Icon: assets.Icon, Executable: executable, Startup: startup,
		Demo: demo, DemoTour: os.Getenv("XODUS_DEMO_TOUR") == "1",
		// In-app updates stay off until this build publishes its own releases.
		Updater: updater.Config{},
	})
	dist, err := fs.Sub(frontend, "frontend/dist")
	if err != nil {
		log.Fatal(err)
	}
	options := application.Options{
		Name:        "Xodus Nexus",
		Description: "Xbox-inspired Microsoft Store game launcher for Linux",
		Icon:        assets.Icon,
		Services:    []application.Service{application.NewService(core)},
		Assets:      application.AssetOptions{Handler: application.AssetFileServerFS(dist)},
		Linux:       application.LinuxOptions{ProgramName: "xodus-nexus-wails"},
		OnShutdown:  func() { launcher.Shutdown(core) },
	}
	// The lock is scoped to the profile, so independent profiles stay isolated.
	if !demo {
		options.SingleInstance = &application.SingleInstanceOptions{
			UniqueID:      "org.xodusgaming.nexus.wails." + profileKey(userData),
			EncryptionKey: sha256.Sum256([]byte("xodus-nexus-wails:" + userData)),
			OnSecondInstanceLaunch: func(data application.SecondInstanceData) {
				launcher.SecondInstance(core, data.Args)
			},
		}
	}
	app := application.New(options)
	host.app = app
	host.window = newMainWindow(app, userData, func() bool { return launcher.ShortcutOnly(core) })
	app.Event.OnApplicationEvent(events.Common.ApplicationStarted, func(*application.ApplicationEvent) {
		go func() {
			if startupErr != nil {
				host.ShowError("Invalid Nexus shortcut", startupErr.Error())
				app.Quit()
				return
			}
			if err := launcher.Startup(core); err != nil {
				log.Print(err)
				if startup.GameID != "" {
					host.ShowError("Unable to start Nexus", err.Error())
				}
				app.Quit()
			}
		}()
	})
	if err := app.Run(); err != nil {
		log.Fatal(err)
	}
}

func defaultUserData() string {
	config := os.Getenv("XDG_CONFIG_HOME")
	if !filepath.IsAbs(config) {
		home, _ := os.UserHomeDir()
		config = filepath.Join(home, ".config")
	}
	return filepath.Join(config, "xodus-nexus-wails")
}

// profileKey is used as a D-Bus name element, which must not start with a
// digit, so the hex digest gets a letter prefix.
func profileKey(userData string) string {
	sum := sha256.Sum256([]byte(userData))
	return fmt.Sprintf("p%x", sum[:8])
}

// configureFonts adds the bundled fallback font before GTK and WebKit
// initialize fontconfig (for example on minimal gamescope sessions).
func configureFonts(userData string) func() {
	root := fontRoot(userData)
	directory := filepath.Join(root, "fonts")
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return nil
	}
	data, err := assets.Fonts.ReadFile("fonts/DejaVuSans.ttf")
	if err != nil {
		return nil
	}
	target := filepath.Join(directory, "DejaVuSans.ttf")
	if existing, err := os.ReadFile(target); err != nil || len(existing) != len(data) {
		if err := os.WriteFile(target, data, 0o600); err != nil {
			return nil
		}
	}
	dispose, err := launcherenv.ConfigureLauncherFonts(directory, filepath.Join(root, "cache"), root)
	if err != nil {
		log.Print("Launcher fallback font unavailable: ", err)
		return nil
	}
	return dispose
}

// fontRoot keeps the font, its cache and the generated fonts.conf under
// $XDG_CACHE_HOME/fontconfig: the WebKit web process sandbox binds that
// directory, but has a private /tmp and cannot see the user data directory.
func fontRoot(userData string) string {
	if cache, err := os.UserCacheDir(); err == nil {
		return filepath.Join(cache, "fontconfig", "xodus-nexus-wails")
	}
	return userData
}
