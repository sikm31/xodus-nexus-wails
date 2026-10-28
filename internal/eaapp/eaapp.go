// Package eaapp runs the official EA app in its own Proton prefix. EA Play
// titles ("Rosebud" Store products) have no Microsoft Store package: Windows
// hands the install to the EA app, which also checks the license at launch.
// Sign-in, Xbox account linking and downloads all happen inside the EA app;
// Nexus never sees EA credentials.
package eaapp

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"sync"
	"syscall"
	"time"

	"xodus-nexus-wails/internal/game"
	"xodus-nexus-wails/internal/launcherenv"
	"xodus-nexus-wails/internal/proc"
	"xodus-nexus-wails/internal/shared"
)

// InstallerURL is EA's official bootstrapper; it downloads the current EA app.
const InstallerURL = "https://origin-a.akamaihd.net/EA-Desktop-Client-Download/installer-releases/EAappInstaller.exe"

// LinkAccountsURL is the EA account page where an Xbox account is connected.
// The link is what makes an Ultimate membership grant EA Play in the EA app.
const LinkAccountsURL = "https://myaccount.ea.com/cp-ui/connectaccounts/index"

const maxInstallerBytes = 64 << 20

// Proton is a Proton build usable through its `proton run` entry point.
type Proton struct {
	Name, Root string
}

func (p Proton) script() string     { return filepath.Join(p.Root, "proton") }
func (p Proton) wineserver() string { return filepath.Join(p.Root, "files", "bin", "wineserver") }

func usableProton(root string) bool {
	return syscall.Access(filepath.Join(root, "proton"), 1) == nil &&
		syscall.Access(filepath.Join(root, "files", "share", "default_pfx"), 4) == nil
}

var (
	versionNumbers = regexp.MustCompile(`\d+`)
	geProton       = regexp.MustCompile(`^GE-Proton\d`)
	steamProton    = regexp.MustCompile(`^Proton \d+\.\d+$`)
)

// newerName orders build names such as GE-Proton10-25 and GE-Proton11-3 by
// their numbers rather than lexically.
func newerName(a, b string) int {
	left, right := versionNumbers.FindAllString(a, -1), versionNumbers.FindAllString(b, -1)
	for i := 0; i < min(len(left), len(right)); i++ {
		l, _ := strconv.Atoi(left[i])
		r, _ := strconv.Atoi(right[i])
		if l != r {
			return r - l
		}
	}
	return len(right) - len(left)
}

// FindProton prefers a stock Steam-side Proton over the GDK-patched Xodus
// runtime: GE-Proton ships protonfixes for the EA app, then Proton
// Experimental, then numbered Proton releases. xodusWine is the last resort.
func FindProton(steamRoot, xodusWine string) (Proton, bool) {
	var ge, numbered []Proton
	var experimental *Proton
	tools := filepath.Join(steamRoot, "compatibilitytools.d")
	if entries, err := os.ReadDir(tools); err == nil {
		for _, entry := range entries {
			if root := filepath.Join(tools, entry.Name()); geProton.MatchString(entry.Name()) && usableProton(root) {
				ge = append(ge, Proton{entry.Name(), root})
			}
		}
	}
	for _, library := range game.SteamLibraries(steamRoot) {
		common := filepath.Join(library, "steamapps", "common")
		entries, err := os.ReadDir(common)
		if err != nil {
			continue
		}
		for _, entry := range entries {
			root := filepath.Join(common, entry.Name())
			switch name := entry.Name(); {
			case !usableProton(root):
			case name == "Proton - Experimental" && experimental == nil:
				experimental = &Proton{name, root}
			case steamProton.MatchString(name):
				numbered = append(numbered, Proton{name, root})
			}
		}
	}
	byVersion := func(a, b Proton) int { return newerName(a.Name, b.Name) }
	slices.SortFunc(ge, byVersion)
	slices.SortFunc(numbered, byVersion)
	switch {
	case len(ge) > 0:
		return ge[0], true
	case experimental != nil:
		return *experimental, true
	case len(numbered) > 0:
		return numbered[0], true
	}
	if xodusWine != "" {
		if root := filepath.Clean(filepath.Join(filepath.Dir(xodusWine), "..", "..")); usableProton(root) {
			return Proton{"Xodus runtime " + filepath.Base(root), root}, true
		}
	}
	return Proton{}, false
}

// Fetch performs an HTTP GET.
type Fetch func(ctx context.Context, url string) (*http.Response, error)

func httpFetch(ctx context.Context, url string) (*http.Response, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	client := &http.Client{CheckRedirect: func(next *http.Request, _ []*http.Request) error {
		if next.URL.Scheme != "https" {
			return errors.New("redirect away from HTTPS")
		}
		return nil
	}}
	return client.Do(request)
}

// Paths locate the EA app's data; root is normally <data>/ea-app.
type Paths struct {
	Root, SteamRoot string
}

func (p Paths) compatData() string { return filepath.Join(p.Root, "compatdata") }
func (p Paths) Prefix() string     { return filepath.Join(p.compatData(), "pfx") }
func (p Paths) installer() string  { return filepath.Join(p.Root, "EAappInstaller.exe") }
func (p Paths) LogPath() string    { return filepath.Join(p.Root, "ea-app.log") }

// Executable returns the installed EADesktop.exe, or "". The EA app installs
// into a directory per version and updates itself, so the newest one wins.
func (p Paths) Executable() string {
	root := filepath.Join(p.Prefix(), "drive_c", "Program Files", "Electronic Arts", "EA Desktop")
	matches, _ := filepath.Glob(filepath.Join(root, "*", "EA Desktop", "EADesktop.exe"))
	slices.SortFunc(matches, newerName)
	for _, candidate := range append(matches, filepath.Join(root, "EA Desktop", "EADesktop.exe")) {
		if info, err := os.Stat(candidate); err == nil && info.Mode().IsRegular() {
			return candidate
		}
	}
	return ""
}

// sessions are the EA app's per-user data: sign-in cookies, tokens and
// user_<id>.ini files. Games and the EA app itself live elsewhere.
func (p Paths) sessions() []string {
	matches, _ := filepath.Glob(filepath.Join(p.Prefix(), "drive_c", "users", "*", "AppData", "Local", "Electronic Arts", "EA Desktop"))
	return matches
}

// signedIn reports whether the EA app has a stored user session.
func (p Paths) signedIn() bool {
	for _, session := range p.sessions() {
		if users, _ := filepath.Glob(filepath.Join(session, "user_*.ini")); len(users) > 0 {
			return true
		}
	}
	return false
}

// running reports whether a wineserver serves the prefix. Wine names its
// socket directory after the prefix's device and inode numbers.
func (p Paths) running() bool {
	var info syscall.Stat_t
	if syscall.Stat(p.Prefix(), &info) != nil {
		return false
	}
	socket := fmt.Sprintf("/tmp/.wine-%d/server-%x-%x/socket", os.Getuid(), uint64(info.Dev), info.Ino)
	_, err := os.Stat(socket)
	return err == nil
}

// Manager owns the EA app prefix. Locations are read on every call because
// the data root follows the runtime directory setting.
type Manager struct {
	paths     func() Paths
	xodusWine func() string
	onChange  func()
	fetch     Fetch

	mu    sync.Mutex
	busy  bool
	phase string
	err   string
}

func NewManager(paths func() Paths, xodusWine func() string, onChange func(), fetch Fetch) *Manager {
	if fetch == nil {
		fetch = httpFetch
	}
	return &Manager{paths: paths, xodusWine: xodusWine, onChange: onChange, fetch: fetch}
}

func (m *Manager) Current() shared.EAAppStatus {
	paths := m.paths()
	status := shared.EAAppStatus{Installed: paths.Executable() != "", Running: paths.running(), SignedIn: paths.signedIn()}
	if proton, ok := FindProton(paths.SteamRoot, m.xodusWine()); ok {
		status.Proton = proton.Name
	}
	if _, err := os.Stat(paths.LogPath()); err == nil {
		status.LogPath = paths.LogPath()
	}
	m.mu.Lock()
	status.Busy, status.Phase, status.Error = m.busy, m.phase, m.err
	m.mu.Unlock()
	return status
}

func (m *Manager) set(phase, err string) {
	m.mu.Lock()
	m.phase, m.err = phase, err
	m.mu.Unlock()
	m.onChange()
}

// begin claims the manager for one operation.
func (m *Manager) begin(phase string) error {
	m.mu.Lock()
	if m.busy {
		m.mu.Unlock()
		return errors.New("The EA app is already being prepared.")
	}
	m.busy, m.phase, m.err = true, phase, ""
	m.mu.Unlock()
	m.onChange()
	return nil
}

func (m *Manager) finish(err error) {
	m.mu.Lock()
	m.busy = false
	if err != nil {
		m.phase, m.err = "", err.Error()
	}
	m.mu.Unlock()
	m.onChange()
}

func (m *Manager) proton() (Proton, error) {
	proton, ok := FindProton(m.paths().SteamRoot, m.xodusWine())
	if !ok {
		return Proton{}, errors.New("No Proton build was found. Install GE-Proton or Proton Experimental through Steam, or install the Xodus runtime in Settings.")
	}
	return proton, nil
}

func (m *Manager) environment(proton Proton) launcherenv.Env {
	paths := m.paths()
	env := launcherenv.FromOS()
	for k, v := range map[string]string{"STEAM_COMPAT_DATA_PATH": paths.compatData(), "STEAM_COMPAT_CLIENT_INSTALL_PATH": paths.SteamRoot,
		"SteamAppId": "0", "SteamGameId": "0", "UMU_ID": "xodus-nexus-ea-app", "UMU_USE_STEAM": "0"} {
		env[k] = v
	}
	return env
}

// spawn runs a Proton verb in the EA prefix, appending all output to the log.
// waitforexitandrun is the verb GE-Proton applies protonfixes to.
func (m *Manager) spawn(proton Proton, verb string, args ...string) (*proc.Tracked, error) {
	paths := m.paths()
	if err := os.MkdirAll(paths.compatData(), 0o700); err != nil {
		return nil, err
	}
	logFile, err := os.OpenFile(paths.LogPath(), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, err
	}
	fmt.Fprintf(logFile, "\n== %s · %s · %s %v\n", time.Now().Format(time.RFC3339), proton.Name, verb, args)
	var write sync.Mutex
	tracked, err := proc.Spawn(proton.script(), append([]string{verb}, args...), proc.RunOptions{
		Dir: paths.Root, Env: m.environment(proton), Detached: true, MaxOutput: 64_000,
		OnLine: func(line string) { write.Lock(); fmt.Fprintln(logFile, line); write.Unlock() },
	})
	if err != nil {
		logFile.Close()
		return nil, err
	}
	go func() { <-tracked.Done(); write.Lock(); logFile.Close(); write.Unlock() }()
	return tracked, nil
}

// watch refreshes the status once the prefix's wineserver has gone away.
func (m *Manager) watch(tracked *proc.Tracked) {
	<-tracked.Done()
	m.onChange()
	for m.paths().running() {
		time.Sleep(3 * time.Second)
	}
	m.onChange()
}

func (m *Manager) download(ctx context.Context, target string) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	response, err := m.fetch(ctx, InstallerURL)
	if err != nil {
		return fmt.Errorf("The EA app installer could not be downloaded: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("The EA app installer download returned HTTP %d.", response.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, maxInstallerBytes+1))
	if err != nil {
		return fmt.Errorf("The EA app installer could not be downloaded: %w", err)
	}
	if len(body) > maxInstallerBytes || !bytes.HasPrefix(body, []byte("MZ")) {
		return errors.New("The EA app installer download is not a Windows executable.")
	}
	temporary := fmt.Sprintf("%s.tmp-%d", target, os.Getpid())
	if err := os.WriteFile(temporary, body, 0o600); err != nil {
		return err
	}
	if err := os.Rename(temporary, target); err != nil {
		os.Remove(temporary)
		return err
	}
	return nil
}

// registerDotNet records where .NET lives. Proton serves Wine Mono from its
// own tree and leaves InstallRoot unset, so the EA installer's managed custom
// action (JunoInitializeSession) cannot load and the MSI fails with 1603.
func (m *Manager) registerDotNet(proton Proton) error {
	for _, view := range []struct{ flag, root string }{{"/reg:32", `C:\windows\Microsoft.NET\Framework\`}, {"/reg:64", `C:\windows\Microsoft.NET\Framework64\`}} {
		tracked, err := m.spawn(proton, "run", "reg", "add", `HKLM\Software\Microsoft\.NETFramework`, "/v", "InstallRoot", "/t", "REG_SZ", "/d", view.root, "/f", view.flag)
		if err != nil {
			return err
		}
		if result := tracked.Wait(); result.Code != 0 {
			return fmt.Errorf("The EA app prefix could not be prepared for .NET (reg exited with code %d). See %s.", result.Code, m.paths().LogPath())
		}
	}
	return nil
}

// Install downloads EA's installer and runs it. The installer is interactive;
// it finishes when the user completes or closes it.
func (m *Manager) Install(ctx context.Context) (err error) {
	if err = m.begin("Downloading the EA app installer…"); err != nil {
		return err
	}
	defer func() { m.finish(err) }()
	proton, err := m.proton()
	if err != nil {
		return err
	}
	paths := m.paths()
	if err = os.MkdirAll(paths.Root, 0o700); err != nil {
		return err
	}
	if err = m.download(ctx, paths.installer()); err != nil {
		return err
	}
	m.set("Preparing the Proton prefix. The first start can take a minute…", "")
	if err = m.registerDotNet(proton); err != nil {
		return err
	}
	m.set("Complete the EA app installer window…", "")
	tracked, err := m.spawn(proton, "waitforexitandrun", paths.installer())
	if err != nil {
		return err
	}
	result := tracked.Wait()
	// The installer starts the EA app itself; keep its status current.
	go m.watch(tracked)
	if paths.Executable() == "" {
		return fmt.Errorf("The EA app installer exited with code %d before the EA app was installed. See %s.", result.Code, paths.LogPath())
	}
	m.set("The EA app is installed. Sign in with your Xbox account in its window.", "")
	return nil
}

// Launch starts the installed EA app.
func (m *Manager) Launch() (err error) {
	paths := m.paths()
	executable := paths.Executable()
	if executable == "" {
		return errors.New("Install the EA app first.")
	}
	if paths.running() {
		return errors.New("The EA app is already running.")
	}
	if err = m.begin("Starting the EA app…"); err != nil {
		return err
	}
	defer func() { m.finish(err) }()
	proton, err := m.proton()
	if err != nil {
		return err
	}
	tracked, err := m.spawn(proton, "waitforexitandrun", executable)
	if err != nil {
		return err
	}
	go m.watch(tracked)
	m.set("", "")
	return nil
}

// SignOut stops the EA app and removes its stored session, so the next
// start asks for an account again. Installed games are kept.
func (m *Manager) SignOut() error {
	paths := m.paths()
	if paths.running() {
		if err := m.Stop(); err != nil {
			return err
		}
		for deadline := time.Now().Add(15 * time.Second); paths.running(); time.Sleep(300 * time.Millisecond) {
			if time.Now().After(deadline) {
				return errors.New("The EA app did not close. Stop it and sign out again.")
			}
		}
	}
	for _, session := range paths.sessions() {
		if err := os.RemoveAll(session); err != nil {
			return err
		}
	}
	m.onChange()
	return nil
}

// Stop ends every Wine process in the EA prefix, including running EA games.
func (m *Manager) Stop() error {
	proton, err := m.proton()
	if err != nil {
		return err
	}
	env := m.environment(proton)
	env["WINEPREFIX"] = m.paths().Prefix()
	_, err = proc.Run(proton.wineserver(), []string{"-k"}, proc.RunOptions{Env: env, MaxOutput: 64_000})
	m.onChange()
	return err
}
