package main

import (
	"os"
	"os/exec"
	"path/filepath"

	"github.com/godbus/dbus/v5"
	"github.com/wailsapp/wails/v3/pkg/application"
	"github.com/wailsapp/wails/v3/pkg/events"

	"xodus-nexus-wails/assets"
)

// wailsHost implements launcher.Host with the Wails application.
type wailsHost struct {
	app      *application.App
	window   *application.WebviewWindow
	userData string
}

func newMainWindow(app *application.App, userData string, keepRunning func() bool) *application.WebviewWindow {
	window := app.Window.NewWithOptions(application.WebviewWindowOptions{
		Name: "main", Title: "Xodus Nexus", Width: 1440, Height: 900, MinWidth: 800, MinHeight: 600,
		BackgroundColour: application.NewRGB(8, 10, 12), URL: "/", Hidden: true,
	})
	// A game started from a shortcut keeps running without the window.
	window.RegisterHook(events.Common.WindowClosing, func(event *application.WindowEvent) {
		if keepRunning() {
			event.Cancel()
			window.Hide()
		}
	})
	return window
}

func (h *wailsHost) Emit(event string, data any) { h.app.Event.Emit(event, data) }
func (h *wailsHost) ShowWindow()                 { h.window.Show(); h.window.Focus() }
func (h *wailsHost) HideWindow()                 { h.window.Hide() }
func (h *wailsHost) WindowVisible() bool         { return h.window.IsVisible() }
func (h *wailsHost) WindowHandle() string        { return "" }

func (h *wailsHost) ChooseDirectory(defaultPath string) (string, error) {
	return h.app.Dialog.OpenFile().CanChooseDirectories(true).CanChooseFiles(false).CanCreateDirectories(true).
		SetDirectory(defaultPath).AttachToWindow(h.window).PromptForSingleSelection()
}

func (h *wailsHost) ChooseFile(defaultPath string) (string, error) {
	return h.app.Dialog.OpenFile().CanChooseFiles(true).SetDirectory(defaultPath).AttachToWindow(h.window).PromptForSingleSelection()
}

func (h *wailsHost) SaveFile(title, defaultPath, filterName, pattern string) (string, error) {
	return h.app.Dialog.SaveFile().SetMessage(title).SetDirectory(filepath.Dir(defaultPath)).SetFilename(filepath.Base(defaultPath)).
		AddFilter(filterName, pattern).AttachToWindow(h.window).PromptForSingleSelection()
}

func (h *wailsHost) Ask(title, message, detail string, buttons []string, cancel int) int {
	choice := make(chan int, 1)
	dialog := h.app.Dialog.Question().SetTitle(title).SetMessage(message + "\n\n" + detail).AttachToWindow(h.window)
	for index, label := range buttons {
		button := dialog.AddButton(label).OnClick(func() { choice <- index })
		if index == cancel {
			dialog.SetCancelButton(button)
			dialog.SetDefaultButton(button)
		}
	}
	dialog.Show()
	return <-choice
}

func (h *wailsHost) ShowError(title, message string) {
	h.app.Dialog.Error().SetTitle(title).SetMessage(message).Show()
}

func (h *wailsHost) SetClipboard(text string) error {
	h.app.Clipboard.SetText(text)
	return nil
}

func (h *wailsHost) NotificationsSupported() bool {
	conn, err := dbus.SessionBus()
	if err != nil {
		return false
	}
	var owned bool
	return conn.BusObject().Call("org.freedesktop.DBus.NameHasOwner", 0, "org.freedesktop.Notifications").Store(&owned) == nil && owned
}

func (h *wailsHost) Notify(title, body string) error {
	conn, err := dbus.SessionBus()
	if err != nil {
		return err
	}
	icon := filepath.Join(h.userData, "icon.png")
	if _, err := os.Stat(icon); err != nil {
		os.WriteFile(icon, assets.Icon, 0o600)
	}
	return conn.Object("org.freedesktop.Notifications", "/org/freedesktop/Notifications").Call("org.freedesktop.Notifications.Notify", 0,
		"Xodus Nexus", uint32(0), icon, title, body, []string{}, map[string]dbus.Variant{"urgency": dbus.MakeVariant(byte(1))}, int32(-1)).Err
}

func (h *wailsHost) ShowItemInFolder(path string) {
	if conn, err := dbus.SessionBus(); err == nil {
		call := conn.Object("org.freedesktop.FileManager1", "/org/freedesktop/FileManager1").
			Call("org.freedesktop.FileManager1.ShowItems", 0, []string{"file://" + path}, "")
		if call.Err == nil {
			return
		}
	}
	exec.Command("xdg-open", filepath.Dir(path)).Start()
}

func (h *wailsHost) OpenPath(target string) error { return h.app.Browser.OpenFile(target) }
func (h *wailsHost) Quit()                        { h.app.Quit() }
