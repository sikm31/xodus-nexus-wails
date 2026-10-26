// Package shortcuts ports game-shortcuts.ts, shortcut-icon.ts,
// shortcut-launch.ts and desktop-open.ts.
package shortcuts

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"syscall"
	"unicode/utf8"

	"golang.org/x/text/unicode/norm"

	"xodus-nexus-wails/internal/install"
	"xodus-nexus-wails/internal/shared"
	"xodus-nexus-wails/internal/steam"
)

var validIDPattern = regexp.MustCompile(`^[a-f0-9]{16}$`)

func validID(id string) bool { return validIDPattern.MatchString(id) }

func shellQuote(value string) string { return "'" + strings.ReplaceAll(value, "'", `'"'"'`) + "'" }

var desktopEscapes = strings.NewReplacer(`\`, `\\`, "\n", `\n`, "\r", `\r`, "\t", `\t`)

func desktopValue(value string) string { return desktopEscapes.Replace(value) }

var execEscapes = regexp.MustCompile("[\"`$\\\\]")

// DesktopArgument quotes value for a desktop entry Exec line. Desktop Exec is
// not a shell command: both the value and Exec layers are escaped.
func DesktopArgument(value string) string {
	return desktopValue(`"` + execEscapes.ReplaceAllString(strings.ReplaceAll(value, "%", "%%"), `\$0`) + `"`)
}

var (
	reservedTitle = regexp.MustCompile(`[<>:"/\\|?*\x00-\x1f\x7f-\x9f]`)
	spaces        = regexp.MustCompile(`\s+`)
	edgeDots      = regexp.MustCompile(`^[. ]+|[. ]+$`)
	plainChar     = regexp.MustCompile(`^[A-Za-z0-9_.]$`)
)

// TitleStem makes a safe, bounded file name stem from a game title.
func TitleStem(title string) string {
	clean := norm.NFC.String(title)
	clean = reservedTitle.ReplaceAllString(clean, " ")
	clean = edgeDots.ReplaceAllString(spaces.ReplaceAllString(clean, " "), "")
	stem, escaped := "", 0
	for _, char := range clean {
		cost := utf8.RuneLen(char) * 4
		if plainChar.MatchString(string(char)) {
			cost = 1
		}
		if escaped+cost > 150 || len(stem)+utf8.RuneLen(char) > 120 {
			break
		}
		stem += string(char)
		escaped += cost
	}
	if stem = strings.TrimRight(stem, " \t"); stem == "" {
		return "Xodus Nexus game"
	}
	return stem
}

type Request struct {
	GameID  string
	Profile string
}

// Arguments parses --launch-game and --nexus-profile from a command line.
func Arguments(argv []string) (Request, error) {
	var result Request
	for i := 0; i < len(argv); i++ {
		flag := argv[i]
		if flag != "--launch-game" && flag != "--nexus-profile" {
			continue
		}
		i++
		value := ""
		if i < len(argv) {
			value = argv[i]
		}
		if flag == "--launch-game" {
			if result.GameID != "" || !validID(value) {
				return Request{}, errors.New("Invalid game shortcut ID.")
			}
			result.GameID = value
		} else {
			if result.Profile != "" || value == "" || !filepath.IsAbs(value) || strings.ContainsAny(value, "\x00\r\n") {
				return Request{}, errors.New("Invalid Nexus profile path.")
			}
			result.Profile = value
		}
	}
	return result, nil
}

// InstanceRequest validates a second-instance command line for this profile.
func InstanceRequest(argv []string, currentProfile string) (Request, error) {
	request, err := Arguments(argv)
	if err != nil {
		return Request{}, err
	}
	if request.Profile != "" && filepath.Clean(request.Profile) != filepath.Clean(currentProfile) {
		return Request{}, errors.New("Shortcut belongs to a different Nexus profile.")
	}
	return request, nil
}

type Locations struct {
	UserData, Desktop, Applications string
	// Launcher is the persistent executable; "" when shortcuts are unavailable.
	Launcher string
	Icon     []byte
}

type Game struct {
	ID, Title, InstallState string
}

type Shortcuts struct {
	locations               Locations
	entry, icon, nexusEntry string
	profileID               string
}

func New(locations Locations) *Shortcuts {
	absolute, _ := filepath.Abs(locations.UserData)
	sum := sha256.Sum256([]byte(absolute))
	return &Shortcuts{locations: locations, profileID: hex.EncodeToString(sum[:])[:12]}
}

func (s *Shortcuts) Available() bool { return s.entry != "" }

func (s *Shortcuts) Reason() string {
	if s.Available() {
		return ""
	}
	return "Shortcuts require an installed Linux launcher or AppImage with a persistent executable path."
}

func (s *Shortcuts) marker() string { return "X-Xodus-Nexus-Profile=" + s.profileID }

func ownedSafe(info fs.FileInfo) bool {
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && int(stat.Uid) == os.Getuid() && info.Mode().Perm()&0o022 == 0
}

func (s *Shortcuts) directory(directory string) error {
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return err
	}
	info, err := os.Lstat(directory)
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode()&fs.ModeSymlink != 0 || !ownedSafe(info) {
		return errors.New("Shortcut directory must be owned by your user and not writable by others.")
	}
	return nil
}

func writeExclusive(file string, data []byte, mode os.FileMode) error {
	handle, err := os.OpenFile(file, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return err
	}
	_, err = handle.Write(data)
	if closeErr := handle.Close(); err == nil {
		err = closeErr
	}
	if err == nil {
		err = os.Chmod(file, mode)
	}
	return err
}

// write replaces file only when it is a safe file carrying every marker.
func (s *Shortcuts) write(file string, data []byte, mode os.FileMode, markers ...string) error {
	info, err := os.Lstat(file)
	if err == nil {
		var lines []string
		if len(markers) > 0 && info.Mode().IsRegular() {
			content, _ := os.ReadFile(file)
			lines = strings.Split(string(content), "\n")
		}
		if !info.Mode().IsRegular() || !ownedSafe(info) || slices.ContainsFunc(markers, func(m string) bool { return !slices.Contains(lines, m) }) {
			return errors.New("Refusing to replace an unrelated or unsafe shortcut file.")
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	temporary := file + "." + install.NewUUID()
	defer os.Remove(temporary)
	if err := writeExclusive(temporary, data, mode); err != nil {
		return err
	}
	return os.Rename(temporary, file)
}

// Initialize writes the stable launch entry points. Shortcuts hold no tokens
// or runtime paths; they are refreshed after moving or updating the app.
func (s *Shortcuts) Initialize() error {
	if s.locations.Launcher == "" {
		return nil
	}
	launcher, err := filepath.EvalSymlinks(s.locations.Launcher)
	if err != nil {
		return err
	}
	info, err := os.Stat(launcher)
	if err != nil || strings.ContainsAny(launcher, "\x00\r\n") || !filepath.IsAbs(launcher) || !info.Mode().IsRegular() || syscall.Access(launcher, 1) != nil {
		return errors.New("Invalid launcher executable.")
	}
	directory := filepath.Join(s.locations.UserData, "shortcuts")
	if err := s.directory(directory); err != nil {
		return err
	}
	body := func(game bool) []byte {
		flag := ""
		if game {
			flag = " --launch-game"
		}
		return []byte(fmt.Sprintf("#!/bin/sh\n# %s\nunset APPIMAGE APPDIR ARGV0 OWD FRONTEND_DEVSERVER_URL XODUS_DEMO XODUS_DEMO_TOUR XODUS_DEMO_CAPTURE_DIR XODUS_SMOKE_SCREENSHOT\nexec %s --nexus-profile %s%s \"$@\"\n",
			s.marker(), shellQuote(launcher), shellQuote(s.locations.UserData), flag))
	}
	entry := filepath.Join(directory, "launch-game")
	if err := s.write(entry, body(true), 0o700, "# "+s.marker()); err != nil {
		return err
	}
	nexusEntry := filepath.Join(directory, "launch-nexus")
	if err := s.write(nexusEntry, body(false), 0o700, "# "+s.marker()); err != nil {
		return err
	}
	icon := filepath.Join(directory, "nexus.png")
	if err := s.write(icon, s.locations.Icon, 0o600); err != nil {
		return err
	}
	s.entry, s.icon, s.nexusEntry = entry, icon, nexusEntry
	return nil
}

func (s *Shortcuts) legacyFilename(id string) string {
	return fmt.Sprintf("xodus-nexus-%s-%s.desktop", s.profileID, id)
}

func gameMarker(id string) string { return "X-Xodus-Nexus-Game=" + id }

func (s *Shortcuts) owns(file, id string) bool {
	info, err := os.Lstat(file)
	if err != nil || !info.Mode().IsRegular() || !ownedSafe(info) || info.Size() > 128*1024 {
		return false
	}
	content, err := os.ReadFile(file)
	if err != nil {
		return false
	}
	lines := strings.Split(string(content), "\n")
	return slices.Contains(lines, s.marker()) && (slices.Contains(lines, gameMarker(id)) || filepath.Base(file) == s.legacyFilename(id))
}

func (s *Shortcuts) ownedFiles(directory, id string) ([]string, error) {
	info, err := os.Lstat(directory)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if !info.IsDir() || info.Mode()&fs.ModeSymlink != 0 || !ownedSafe(info) {
		return nil, errors.New("Unsafe shortcut directory.")
	}
	entries, err := os.ReadDir(directory)
	if err != nil {
		return nil, err
	}
	var files []string
	for _, entry := range entries {
		file := filepath.Join(directory, entry.Name())
		if strings.HasSuffix(entry.Name(), ".desktop") && s.owns(file, id) {
			files = append(files, file)
		}
	}
	return files, nil
}

// writeEntry publishes body atomically without clobbering, even when two
// games share a title.
func (s *Shortcuts) writeEntry(directory, stem, id string, body []byte, mode os.FileMode) (string, error) {
	temporary := filepath.Join(directory, ".nexus-"+install.NewUUID())
	if err := writeExclusive(temporary, body, mode); err != nil {
		return "", err
	}
	defer os.Remove(temporary)
	for index := range 100 {
		suffix := ""
		switch {
		case index == 1:
			suffix = fmt.Sprintf(" (%s)", id[:8])
		case index == 2:
			suffix = fmt.Sprintf(" (%s-%s)", id[:8], s.profileID[:6])
		case index > 2:
			suffix = fmt.Sprintf(" (%s-%s-%d)", id[:8], s.profileID[:6], index)
		}
		target := filepath.Join(directory, stem+suffix+".desktop")
		err := os.Link(temporary, target)
		if err == nil {
			return target, nil
		}
		if !errors.Is(err, fs.ErrExist) {
			return "", err
		}
		if !s.owns(target, id) {
			continue
		}
		if err := s.write(target, body, mode, s.marker(), gameMarker(id)); err != nil {
			return "", err
		}
		return target, nil
	}
	return "", errors.New("No free shortcut filename could be found for this game.")
}

var pngMagic = []byte("\x89PNG\r\n\x1a\n")

func (s *Shortcuts) gameIcon(id string, png []byte) (string, error) {
	directory := filepath.Join(s.locations.UserData, "shortcuts", "icons")
	file := filepath.Join(directory, id+".png")
	if len(png) > 0 && len(png) <= 4*1024*1024 && bytes.HasPrefix(png, pngMagic) {
		if err := s.directory(directory); err != nil {
			return "", err
		}
		if err := s.write(file, png, 0o600); err != nil {
			return "", err
		}
		return file, nil
	}
	// No cached artwork keeps the Nexus fallback.
	if info, err := os.Lstat(file); err == nil && info.Mode().IsRegular() && ownedSafe(info) && info.Size() <= 4*1024*1024 {
		if data, err := os.ReadFile(file); err == nil && bytes.HasPrefix(data, pngMagic) {
			return file, nil
		}
	}
	return s.icon, nil
}

var controlChars = regexp.MustCompile(`[\x00-\x1f\x7f]`)

// SteamLaunch describes the shortcut for game, or for the launcher when game is nil.
func (s *Shortcuts) SteamLaunch(game *Game, png []byte) (steam.Launch, error) {
	if s.entry == "" || s.icon == "" || s.nexusEntry == "" {
		return steam.Launch{}, errors.New(s.Reason())
	}
	if game != nil && (game.InstallState != "ready" || !validID(game.ID)) {
		return steam.Launch{}, errors.New("Only installed, ready games can be added to Steam.")
	}
	entry, title, identity, icon := s.nexusEntry, "Xodus Nexus", s.profileID+":launcher", s.icon
	if game != nil {
		entry, identity = s.entry, s.profileID+":"+game.ID
		if title = shared.TruncateUTF16(strings.TrimSpace(controlChars.ReplaceAllString(game.Title, " ")), 256); title == "" {
			title = "Xodus Nexus game"
		}
		var err error
		if icon, err = s.gameIcon(game.ID, png); err != nil {
			return steam.Launch{}, err
		}
	}
	executable, _ := steam.Argument("/bin/sh")
	options, err := steam.Argument(entry)
	if err != nil {
		return steam.Launch{}, err
	}
	if game != nil {
		quoted, _ := steam.Argument(game.ID)
		options += " " + quoted
	}
	return steam.Launch{Title: title, Executable: executable, Options: options, Directory: filepath.Dir(entry), Identity: identity, Icon: icon}, nil
}

// SteamDesktop writes the private .desktop file used for live Steam imports.
func (s *Shortcuts) SteamDesktop(game *Game, png []byte) (steam.Launch, string, error) {
	launch, err := s.SteamLaunch(game, png)
	if err != nil {
		return steam.Launch{}, "", err
	}
	directory := filepath.Join(s.locations.UserData, "shortcuts", "steam")
	if err := s.directory(directory); err != nil {
		return steam.Launch{}, "", err
	}
	name, entry, arguments := "launcher", s.nexusEntry, ""
	if game != nil {
		name, entry, arguments = game.ID, s.entry, " "+DesktopArgument(game.ID)
	}
	file := filepath.Join(directory, name+".desktop")
	identity := "X-Xodus-Nexus-Steam=" + name
	body := fmt.Sprintf("[Desktop Entry]\nType=Application\nName=%s\nExec=/bin/sh %s%s\nPath=%s\nIcon=%s\nTerminal=false\nCategories=Game;\n%s\n%s\n",
		desktopValue(launch.Title), DesktopArgument(entry), arguments, desktopValue(launch.Directory), desktopValue(launch.Icon), s.marker(), identity)
	if err := s.write(file, []byte(body), 0o600, s.marker(), identity); err != nil {
		return steam.Launch{}, "", err
	}
	return launch, file, nil
}

var titleControls = regexp.MustCompile("[\x00\r\n\t]")

// Create writes a desktop or applications-menu entry for game.
func (s *Shortcuts) Create(game Game, destination string, png []byte) (string, error) {
	if s.entry == "" || s.icon == "" {
		return "", errors.New(s.Reason())
	}
	if destination != "desktop" && destination != "applications" {
		return "", errors.New("Invalid shortcut destination.")
	}
	if game.InstallState != "ready" {
		return "", errors.New("Only installed, ready games can have launch shortcuts.")
	}
	if !validID(game.ID) {
		return "", errors.New("Invalid game shortcut ID.")
	}
	directory, mode := s.locations.Desktop, os.FileMode(0o755)
	if destination == "applications" {
		directory, mode = s.locations.Applications, 0o644
	}
	if err := s.directory(directory); err != nil {
		return "", err
	}
	icon, err := s.gameIcon(game.ID, png)
	if err != nil {
		return "", err
	}
	title := strings.TrimSpace(titleControls.ReplaceAllString(game.Title, " "))
	if title == "" {
		title = "Xodus Nexus game"
	}
	// Keep the executable field constant: desktop loaders may check it before
	// expanding %% in arguments. /bin/sh runs a file, never a shell -c string.
	body := fmt.Sprintf("[Desktop Entry]\nType=Application\nVersion=1.0\nName=%s\nComment=Play with Xodus Nexus\nExec=/bin/sh %s %s\nIcon=%s\nTerminal=false\nCategories=Game;\nStartupNotify=false\n%s\n%s\n",
		desktopValue(title), DesktopArgument(s.entry), DesktopArgument(game.ID), desktopValue(icon), s.marker(), gameMarker(game.ID))
	target, err := s.writeEntry(directory, TitleStem(title), game.ID, []byte(body), mode)
	if err != nil {
		return "", err
	}
	// Migrate only this game's managed entries, after the replacement is safe.
	old, err := s.ownedFiles(directory, game.ID)
	if err != nil {
		return "", err
	}
	for _, file := range old {
		if file != target && s.owns(file, game.ID) {
			os.Remove(file)
		}
	}
	return target, nil
}

func (s *Shortcuts) Remove(id string) error {
	if !validID(id) {
		return errors.New("Invalid game shortcut ID.")
	}
	for _, directory := range []string{s.locations.Desktop, s.locations.Applications} {
		files, err := s.ownedFiles(directory, id)
		if err != nil {
			return err
		}
		for _, file := range files {
			if s.owns(file, id) {
				if err := os.Remove(file); err != nil {
					return err
				}
			}
		}
	}
	return nil
}
