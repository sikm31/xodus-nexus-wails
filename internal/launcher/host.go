// Package launcher is the launcher's main process: it owns state, backend
// operations and the API the renderer calls through Wails bindings.
package launcher

import (
	"log"
	"sync"

	"github.com/godbus/dbus/v5"
)

// Host is everything the launcher needs from the desktop shell. The Wails
// application implements it; tests use a fake.
type Host interface {
	// Emit sends an event to the renderer.
	Emit(event string, data any)
	// ShowWindow creates the main window if needed, then shows and focuses it.
	ShowWindow()
	HideWindow()
	WindowVisible() bool
	// WindowHandle is the X11 window id for parented native dialogs, or "".
	WindowHandle() string
	ChooseDirectory(defaultPath string) (string, error)
	ChooseFile(defaultPath string) (string, error)
	SaveFile(title, defaultPath, filterName, pattern string) (string, error)
	// Ask shows a question and returns the chosen button index, or -1.
	Ask(title, message, detail string, buttons []string, cancel int) int
	ShowError(title, message string)
	SetClipboard(text string) error
	Notify(title, body string) error
	NotificationsSupported() bool
	ShowItemInFolder(path string)
	// OpenPath opens a file, directory or URL with the desktop default handler.
	OpenPath(target string) error
	// Quit asks the application to exit; Shutdown runs before it does.
	Quit()
}

// displayBlocker holds org.freedesktop.ScreenSaver inhibitors only while a
// game runs. No fake input or persistent power-policy changes.
type displayBlocker struct {
	mu       sync.Mutex
	releases map[*func()]struct{}
	inhibit  func() (release func(), err error)
}

func newDisplayBlocker() *displayBlocker {
	return &displayBlocker{releases: map[*func()]struct{}{}, inhibit: screenSaverInhibit}
}

func (d *displayBlocker) acquire() func() {
	stop, err := d.inhibit()
	if err != nil {
		log.Print("Could not inhibit display sleep for the running game.")
		return func() {}
	}
	var once sync.Once
	release := func() {}
	release = func() {
		once.Do(func() {
			d.mu.Lock()
			delete(d.releases, &release)
			d.mu.Unlock()
			stop()
		})
	}
	d.mu.Lock()
	d.releases[&release] = struct{}{}
	d.mu.Unlock()
	return release
}

func (d *displayBlocker) stop() {
	d.mu.Lock()
	releases := make([]func(), 0, len(d.releases))
	for release := range d.releases {
		releases = append(releases, *release)
	}
	d.mu.Unlock()
	for _, release := range releases {
		release()
	}
}

// screenSaverInhibit keeps a private session-bus connection open: the
// desktop drops the inhibitor when its owning connection closes.
func screenSaverInhibit() (func(), error) {
	conn, err := dbus.ConnectSessionBus()
	if err != nil {
		return nil, err
	}
	object := conn.Object("org.freedesktop.ScreenSaver", "/org/freedesktop/ScreenSaver")
	var cookie uint32
	if err := object.Call("org.freedesktop.ScreenSaver.Inhibit", 0, "Xodus Nexus", "A game is running").Store(&cookie); err != nil {
		conn.Close()
		return nil, err
	}
	return func() {
		if call := object.Call("org.freedesktop.ScreenSaver.UnInhibit", 0, cookie); call.Err != nil {
			log.Print("Could not release the game display-sleep inhibitor.")
		}
		conn.Close()
	}, nil
}
