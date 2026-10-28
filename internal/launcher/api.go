package launcher

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"regexp"
	"sync"
	"syscall"
	"time"

	"xodus-nexus-wails/internal/diagnostics"
	"xodus-nexus-wails/internal/eaapp"

	"xodus-nexus-wails/internal/game"
	"xodus-nexus-wails/internal/install"
	"xodus-nexus-wails/internal/launcherenv"
	"xodus-nexus-wails/internal/manifest"
	"xodus-nexus-wails/internal/proc"
	"xodus-nexus-wails/internal/report"
	"xodus-nexus-wails/internal/service"
	"xodus-nexus-wails/internal/shared"
	"xodus-nexus-wails/internal/shortcuts"
	"xodus-nexus-wails/internal/state"
)

// The exported methods below are the renderer's LauncherApi. Optional
// arguments arrive as JSON null.

func (l *Launcher) GetSnapshot() shared.AppSnapshot {
	<-l.ready
	return l.snapshot()
}

func (l *Launcher) IsDemoTour() bool { return l.options.Demo && l.options.DemoTour }

func (l *Launcher) RefreshBackend() shared.BackendStatus {
	if l.options.Demo {
		return l.snapshot().Backend
	}
	value := l.backend.Refresh()
	l.refreshAccount(false, false)
	l.emitSnapshot()
	return value
}

func (l *Launcher) CheckBackendUpdate() shared.BackendStatus {
	if l.options.Demo {
		return l.snapshot().Backend
	}
	value := l.backend.CheckUpdate(context.Background())
	l.emitSnapshot()
	return value
}

func (l *Launcher) UpdateBackend() error {
	if l.options.Demo {
		return nil
	}
	if err := l.backend.Update(context.Background(), l.emitSnapshot); err != nil {
		return err
	}
	l.refreshAccount(true, false)
	return nil
}

func (l *Launcher) RebuildBackend() error {
	if l.options.Demo {
		return nil
	}
	if err := l.backend.Rebuild(l.emitSnapshot); err != nil {
		return err
	}
	l.refreshAccount(true, false)
	return nil
}

func (l *Launcher) RefreshRuntime() shared.RuntimeStatus {
	if l.options.Demo {
		return l.snapshot().Runtime
	}
	value := l.runtime.Refresh()
	l.syncManagedWinePath()
	l.refreshAccount(false, false)
	l.emitSnapshot()
	return value
}

func (l *Launcher) CheckRuntimeUpdate() shared.RuntimeStatus {
	if l.options.Demo {
		return l.snapshot().Runtime
	}
	value := l.runtime.Check()
	l.emitSnapshot()
	return value
}

func (l *Launcher) CheckNexusRuntime() shared.RuntimeStatus {
	if l.options.Demo {
		return l.snapshot().Runtime
	}
	value := l.runtime.CheckNexus(context.Background())
	l.emitSnapshot()
	return value
}

func (l *Launcher) installRuntimeWith(install func() (string, error)) error {
	if l.options.Demo {
		return nil
	}
	if err := l.ensureIdle(""); err != nil {
		return err
	}
	winePath, err := install()
	if err != nil {
		return err
	}
	if _, err := l.store.SetSettings(state.Patch{"winePath": winePath}); err != nil {
		return err
	}
	l.refreshAccount(false, false)
	l.emitSnapshot()
	return nil
}

func (l *Launcher) InstallRuntime() error {
	return l.installRuntimeWith(func() (string, error) { return l.runtime.Install(l.emitSnapshot) })
}

func (l *Launcher) InstallNexusRuntime() error {
	return l.installRuntimeWith(func() (string, error) { return l.runtime.InstallNexus(context.Background(), l.emitSnapshot) })
}

func (l *Launcher) RollbackRuntimePreview() error {
	return l.installRuntimeWith(func() (string, error) { return l.runtime.RollbackCloudSavePreview(l.emitSnapshot) })
}

func (l *Launcher) RefreshAccount() shared.AuthStatus {
	if l.options.Demo {
		return l.snapshot().Auth
	}
	return l.refreshAccount(true, false)
}

func (l *Launcher) CheckKeyring() shared.KeyringStatus {
	if l.options.Demo {
		return shared.KeyringStatus{State: "ready", Message: "Sample system wallet is running and unlocked. No real keyring is accessed."}
	}
	return l.keyring.Check()
}

func (l *Launcher) StartKeyring() (shared.KeyringStatus, error) {
	if l.options.Demo {
		return shared.KeyringStatus{State: "ready", Message: "Demo mode does not start real services."}, nil
	}
	if err := l.ensureIdle(""); err != nil {
		return shared.KeyringStatus{}, err
	}
	result := l.keyring.Start()
	if result.State == "ready" {
		go l.refreshAccount(true, false)
	}
	return result, nil
}

func (l *Launcher) CreateKeyringCollection() (shared.KeyringStatus, error) {
	if l.options.Demo {
		return shared.KeyringStatus{State: "ready", Message: "Demo mode does not create real wallets."}, nil
	}
	if err := l.ensureIdle(""); err != nil {
		return shared.KeyringStatus{}, err
	}
	result := l.keyring.CreateCollection()
	l.host.Emit("keyring-state", result)
	return result, nil
}

func (l *Launcher) CheckLauncherUpdate() *shared.LauncherUpdate {
	if !l.options.Demo {
		l.updater.Check(context.Background())
		l.emitSnapshot()
	}
	return l.snapshot().LauncherUpdate
}

func (l *Launcher) InstallLauncherUpdate() error {
	if l.options.Demo {
		return nil
	}
	if err := l.ensureIdle(""); err != nil {
		return err
	}
	return l.updater.Install()
}

func (l *Launcher) RollbackLauncherUpdate() error {
	if l.options.Demo {
		return nil
	}
	if err := l.ensureIdle(""); err != nil {
		return err
	}
	return l.updater.Rollback()
}

func (l *Launcher) RestartLauncher() error {
	if l.options.Demo {
		return nil
	}
	if err := l.ensureIdle(""); err != nil {
		return err
	}
	target := os.Getenv("APPIMAGE")
	if target == "" {
		target, _ = os.Executable()
	}
	if !l.updater.State().RestartRequired || target == "" {
		return errors.New("No launcher restart is pending.")
	}
	// AppImage sets these again for the new mount. Do not reuse the old mount.
	env := launcherenv.External(nil)
	for _, key := range []string{"APPIMAGE", "APPDIR", "ARGV0", "OWD"} {
		delete(env, key)
	}
	command := exec.Command(target)
	command.Env = env.List()
	command.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	// The new instance waits for this one's single-instance lock to clear.
	go func() {
		<-l.shuttingDown
		if err := command.Start(); err != nil {
			log.Print("Could not restart the launcher: ", err)
		} else {
			command.Process.Release()
		}
	}()
	l.host.Quit()
	return nil
}

func (l *Launcher) CheckGameUpdates(id *string) error {
	if l.options.Demo {
		return nil
	}
	l.mu.Lock()
	current := l.gameUpdateJob
	if current == nil {
		value := ""
		if id != nil {
			value = *id
		}
		current = runJob(func() error {
			defer func() {
				l.mu.Lock()
				l.gameUpdateJob = nil
				l.mu.Unlock()
			}()
			return l.checkGameUpdates(value)
		})
		l.gameUpdateJob = current
	}
	l.mu.Unlock()
	return current.wait()
}

func (l *Launcher) diagnosticLog(id string) (string, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.gameOperation == nil || l.gameOperation.LogPath == "" || l.gameOperation.ID != id {
		return "", errors.New("This diagnostic log is no longer available.")
	}
	return l.gameOperation.LogPath, nil
}

const demoLog = "Demo diagnostic log. No account data."

func (l *Launcher) ReadDiagnosticLog(id string) (shared.DiagnosticLog, error) {
	if l.options.Demo {
		return shared.DiagnosticLog{Name: "demo.log", Content: demoLog}, nil
	}
	path, err := l.diagnosticLog(id)
	if err != nil {
		return shared.DiagnosticLog{}, err
	}
	return diagnostics.ReadDiagnosticTail(path, l.logsRoot(), 0)
}

func (l *Launcher) CopyDiagnosticLog(id string) error {
	if l.options.Demo {
		return l.host.SetClipboard(demoLog)
	}
	path, err := l.diagnosticLog(id)
	if err != nil {
		return err
	}
	return diagnostics.CopyDiagnosticFile(path, l.logsRoot(), l.host.SetClipboard, 0)
}

func (l *Launcher) RevealDiagnosticLog(id string) error {
	if l.options.Demo {
		return nil
	}
	path, err := l.diagnosticLog(id)
	if err != nil {
		return err
	}
	file, err := diagnostics.DiagnosticLogFile(path, l.logsRoot())
	if err != nil {
		return err
	}
	l.host.ShowItemInFolder(file)
	return nil
}

func (l *Launcher) ReadBackendDiagnosticLog() (shared.DiagnosticLog, error) {
	if l.options.Demo {
		return shared.DiagnosticLog{Name: "backend.log", Content: "Sample backend diagnostics. No account data."}, nil
	}
	return l.backendLog.Read()
}

func (l *Launcher) CreateDiagnosticReport(gameID *string) (shared.DiagnosticReport, error) {
	if l.options.Demo {
		return shared.DiagnosticReport{Name: "nexus-diagnostics-demo.txt", Content: "Xodus Nexus diagnostic report\nDemo data only.\nLauncher: " + l.options.Version}, nil
	}
	if gameID != nil {
		if _, ok := l.store.Game(*gameID); !ok {
			return shared.DiagnosticReport{}, errors.New("Game not found for diagnostics.")
		}
	}
	home, _ := os.UserHomeDir()
	private := []string{home}
	if current, err := user.Current(); err == nil {
		private = append(private, current.Username)
	}
	return report.Create(report.Input{Snapshot: l.snapshot(), LauncherVersion: l.options.Version,
		SystemInfo: report.SystemInfo(launcherenv.FromOS(), report.Roots{}), BackendLog: l.backendLog.Read,
		LogsRoot: l.logsRoot(), GameID: gameID, PrivateValues: private})
}

func (l *Launcher) SaveDiagnosticReport(content string) (bool, error) {
	if err := report.ValidateContent(content); err != nil {
		return false, err
	}
	name := fmt.Sprintf("nexus-diagnostics-%d.txt", time.Now().UnixMilli())
	file, err := l.host.SaveFile("Save diagnostic report", filepath.Join(userDir("DOWNLOAD", "Downloads"), name), "Text report", "*.txt")
	if err != nil || file == "" {
		return false, err
	}
	if err := report.Save(file, content); err != nil {
		return false, err
	}
	return true, nil
}

func (l *Launcher) Login() error {
	if l.options.Demo {
		return nil
	}
	return l.requestAuth("login")
}

func (l *Launcher) Logout() error {
	if l.options.Demo {
		return nil
	}
	return l.requestAuth("logout")
}

// InstallEAApp downloads and runs EA's installer in the EA app prefix. It
// returns once the interactive installer has closed.
func (l *Launcher) InstallEAApp() error {
	if l.options.Demo {
		return nil
	}
	return l.eaApp.Install(context.Background())
}

func (l *Launcher) LaunchEAApp() error {
	if l.options.Demo {
		return nil
	}
	return l.eaApp.Launch()
}

// SignOutEAApp closes the EA app and removes its stored sign-in session.
func (l *Launcher) SignOutEAApp() error {
	if l.options.Demo {
		return nil
	}
	return l.eaApp.SignOut()
}

func (l *Launcher) StopEAApp() error {
	if l.options.Demo {
		return nil
	}
	return l.eaApp.Stop()
}

// OpenEAAppFor starts the EA app for an EA Play title and copies the title,
// ready to paste into its search: Store metadata carries no EA offer ID.
func (l *Launcher) OpenEAAppFor(title string) error {
	if l.options.Demo {
		return nil
	}
	status := l.eaApp.Current()
	if !status.Installed {
		return errors.New("Set up the EA app in Settings first, then install EA Play games from it.")
	}
	if !status.Running {
		if err := l.eaApp.Launch(); err != nil {
			return err
		}
	}
	if title != "" {
		if err := l.host.SetClipboard(title); err != nil {
			log.Print("Could not copy the EA Play title: ", err)
		}
	}
	return nil
}

// BuildMaxima builds the pinned Maxima source; it returns when done.
func (l *Launcher) BuildMaxima() error {
	if l.options.Demo {
		return nil
	}
	return l.maxima.Build()
}

// CheckMaximaUpdate refreshes the build state and looks up the newest
// upstream commit.
func (l *Launcher) CheckMaximaUpdate() (shared.MaximaStatus, error) {
	if l.options.Demo {
		return shared.MaximaStatus{}, nil
	}
	l.maxima.Refresh()
	err := l.maxima.CheckUpstream(context.Background())
	return l.maxima.Current(), err
}

func (l *Launcher) SignInMaxima() error {
	if l.options.Demo {
		return nil
	}
	return l.maxima.SignIn(context.Background())
}

func (l *Launcher) SignOutMaxima() error {
	if l.options.Demo {
		return nil
	}
	return l.maxima.SignOut()
}

// GetEALibrary lists the EA account's games through Maxima.
func (l *Launcher) GetEALibrary() ([]shared.EATitle, error) {
	if l.options.Demo {
		return []shared.EATitle{}, nil
	}
	return l.maxima.Library(context.Background())
}

// OpenEAAccountLinking opens the EA page that connects an Xbox account.
func (l *Launcher) OpenEAAccountLinking() error {
	if l.options.Demo {
		return nil
	}
	return l.host.OpenPath(eaapp.LinkAccountsURL)
}

func (l *Launcher) SearchCatalog(query string, cursor *string) (shared.CatalogSearchResult, error) {
	value := ""
	if cursor != nil {
		value = *cursor
	}
	return l.catalog.Search(context.Background(), query, l.catalogMarket(), value)
}

func (l *Launcher) GetFeaturedCatalog() (shared.CatalogSearchResult, error) {
	return l.catalog.Featured(context.Background(), l.catalogMarket())
}

func (l *Launcher) catalogMarket() string {
	if l.options.Demo {
		return l.snapshot().Settings.Market
	}
	return l.currentMarket()
}

func (l *Launcher) GetAccountLibrary(kind string, cursor *string, refresh *bool) (shared.AccountLibraryPage, error) {
	if l.options.Demo {
		l.mu.Lock()
		artwork := shared.Clone(l.demoArtwork)
		l.mu.Unlock()
		return shared.DemoAccountLibrary(kind, artwork), nil
	}
	l.mu.Lock()
	running := l.auth.Running
	l.mu.Unlock()
	user := l.storedAccount()
	if running || user == "" {
		return shared.AccountLibraryPage{}, errors.New("Finish signing in before loading account games.")
	}
	status := l.backend.Current()
	binary := ""
	if status.Capabilities.Library {
		binary = status.BinaryPath
	}
	l.library.Select(user, binary, l.currentMarket())
	value := ""
	if cursor != nil {
		value = *cursor
	}
	page, err := l.library.Get(context.Background(), kind, value, refresh != nil && *refresh)
	if err == nil {
		products := make([]shared.CatalogProduct, len(page.Products))
		for i, product := range page.Products {
			products[i] = product.CatalogProduct
		}
		go l.syncEAGames(products)
	}
	return page, err
}

func (l *Launcher) StartInstall(request shared.InstallRequest) (string, error) {
	if l.options.Demo {
		return "demo-download", nil
	}
	return l.startInstall(request, nil, nil)
}

func (l *Launcher) GetInstallPlan(raw string) (shared.InstallPlan, error) {
	if l.options.Demo {
		return shared.InstallPlan{}, errors.New("Package selection is unavailable in demo mode.")
	}
	binary, err := l.requireBackend("streaming")
	if err != nil {
		return shared.InstallPlan{}, err
	}
	capabilities := l.backend.Current().Capabilities
	if !capabilities.PackageSelection {
		return shared.InstallPlan{}, errors.New("Update the Xodus backend in Settings to choose installation packages.")
	}
	productID, err := shared.SafeProductID(raw)
	if err != nil {
		return shared.InstallPlan{}, err
	}
	market := l.currentMarket()
	plan, err := l.plans.Get(context.Background(), binary, productID, market, capabilities.ContentPackageSelection)
	if err != nil {
		return plan, err
	}
	if binary != l.backend.Current().BinaryPath || market != l.currentMarket() {
		return shared.InstallPlan{}, errors.New("Backend or Store market changed. Try again.")
	}
	return plan, nil
}

func (l *Launcher) GetGameContent(id string) (shared.GameContentPlan, error) {
	if l.options.Demo {
		return shared.GameContentPlan{GameID: id, CheckedAt: now(), Items: []shared.GameContentItem{}}, nil
	}
	plan, _, err := l.gameContent(id)
	return plan, err
}

func (l *Launcher) InstallGameContent(id, productID string) (string, error) {
	if l.options.Demo {
		return "", errors.New("Content installation is unavailable in demo mode.")
	}
	return l.installGameContent(id, productID)
}

func (l *Launcher) UpdateInstalledGame(id string) (string, error) {
	if l.options.Demo {
		return id, nil
	}
	entry, ok := l.store.Game(id)
	if !ok {
		return "", errors.New("Game not found.")
	}
	if entry.StoreID == "" || entry.InstallState != "ready" {
		return "", errors.New("This game has no verified Store ID or complete installation to update.")
	}
	return l.startInstall(shared.InstallRequest{ProductID: entry.StoreID, Title: entry.Title, Destination: entry.Path}, &entry, nil)
}

func (l *Launcher) GetDownloadCancelOptions(id string) (shared.DownloadCancelOptions, error) {
	if l.options.Demo {
		return shared.DownloadCancelOptions{Reason: "File deletion is unavailable in demo mode."}, nil
	}
	task, err := l.cancellableDownload(id)
	if err != nil {
		return shared.DownloadCancelOptions{}, err
	}
	locked, roots := l.cleanupProtection(task)
	return l.cleanup.Options(task, locked, roots), nil
}

func (l *Launcher) CancelDownload(id string, mode *string) error {
	if l.options.Demo {
		return nil
	}
	value := ""
	if mode != nil {
		value = *mode
	}
	return l.cancelDownload(id, value)
}

func (l *Launcher) PauseDownload(id string) error {
	if l.options.Demo {
		return nil
	}
	l.mu.Lock()
	_, cancelling := l.downloadCancellations[id]
	control := l.downloadControls[id]
	l.mu.Unlock()
	if cancelling {
		return errors.New("This download is being cancelled.")
	}
	if control == nil {
		return errors.New("Pause requires a backend with download controls. Update the Nexus backend in Settings.")
	}
	return control(true)
}

func (l *Launcher) ResumeDownload(id string) error {
	if l.options.Demo {
		return nil
	}
	return l.resumeDownload(id)
}

func (l *Launcher) RemoveDownload(id string) error {
	if l.options.Demo {
		return nil
	}
	if l.cancellationsPending() {
		return errors.New("Wait for cancellation cleanup.")
	}
	if err := l.store.RemoveDownload(id); err != nil {
		return err
	}
	l.emitSnapshot()
	return nil
}

func (l *Launcher) ClearFinishedDownloads() error {
	if l.options.Demo {
		return nil
	}
	if l.cancellationsPending() {
		return errors.New("Wait for cancellation cleanup.")
	}
	if err := l.store.ClearFinishedDownloads(); err != nil {
		return err
	}
	l.emitSnapshot()
	return nil
}

func (l *Launcher) ImportGame(path *string) (*shared.Game, error) {
	if l.options.Demo {
		return nil, nil
	}
	value := ""
	if path != nil {
		value = *path
	}
	return l.importGame(value)
}

func (l *Launcher) UpdateGame(id string, patch map[string]json.RawMessage) (shared.Game, error) {
	if l.options.Demo {
		games := shared.DemoGames()
		entry := games[0]
		for _, candidate := range games {
			if candidate.ID == id {
				entry = candidate
			}
		}
		encoded, _ := json.Marshal(patch)
		json.Unmarshal(encoded, &entry)
		return entry, nil
	}
	allowed := state.Patch{}
	for _, key := range []string{"title", "executable", "launchArgs"} {
		var value string
		if raw, ok := patch[key]; ok && json.Unmarshal(raw, &value) == nil {
			allowed[key] = value
		}
	}
	if raw, ok := patch["launchTools"]; ok {
		tools, err := shared.NormalizeLaunchTools(raw)
		if err != nil {
			return shared.Game{}, err
		}
		allowed["launchTools"] = tools
	}
	var locale string
	if raw, ok := patch["locale"]; ok && json.Unmarshal(raw, &locale) == nil {
		if _, ok := l.store.Game(id); !ok {
			return shared.Game{}, errors.New("Game not found.")
		}
		supported, err := manifest.SupportedGameLocale(locale, nil)
		if err != nil {
			return shared.Game{}, err
		}
		allowed["locale"] = supported
	}
	entry, err := l.store.PatchGame(id, allowed)
	if err != nil {
		return entry, err
	}
	l.emitSnapshot()
	return entry, nil
}

func (l *Launcher) GetGameLocales(id string) ([]string, error) {
	if l.options.Demo {
		return shared.DemoLocales, nil
	}
	entry, ok := l.store.Game(id)
	if !ok {
		return nil, errors.New("Game not found.")
	}
	return manifest.InstalledGameLocales(entry.Path)
}

func (l *Launcher) RemoveGame(id, mode string) error {
	if l.options.Demo {
		return nil
	}
	return l.removeGame(id, mode)
}

func (l *Launcher) GetGameLaunchOptions(id string) (shared.GameLaunchOptions, error) {
	if l.options.Demo {
		return shared.DemoLaunchOptions, nil
	}
	return l.gameLaunchOptions(id)
}

func (l *Launcher) GetGameDetails(id string) (*shared.GameStoreDetails, error) {
	var storeID string
	if l.options.Demo {
		for _, entry := range shared.DemoGames() {
			if entry.ID == id {
				storeID = entry.StoreID
			}
		}
		if storeID == "" {
			return nil, nil
		}
		return l.catalog.GameDetails(context.Background(), storeID, l.catalogMarket())
	}
	entry, ok := l.store.Game(id)
	if !ok {
		return nil, errors.New("Game not found.")
	}
	if !storeProductID.MatchString(entry.StoreID) {
		return nil, nil
	}
	market := l.currentMarket()
	details, err := l.catalog.GameDetails(context.Background(), entry.StoreID, market)
	if err == nil && market != l.currentMarket() {
		return nil, errors.New("Store market changed. Try again.")
	}
	return details, err
}

// titleRequest validates an activity/achievements request and returns the
// account, backend and title it is bound to, or a terminal status.
func (l *Launcher) titleRequest(id string, supported func(shared.BackendCapabilities) bool) (user, binary, title, status string, err error) {
	if _, ok := l.store.Game(id); !ok {
		return "", "", "", "", errors.New("Game not found.")
	}
	l.mu.Lock()
	running := l.auth.Running
	l.mu.Unlock()
	user = l.storedAccount()
	if running || user == "" {
		return "", "", "", "signed-out", nil
	}
	backendStatus := l.backend.Current()
	if !supported(backendStatus.Capabilities) || backendStatus.BinaryPath == "" {
		return "", "", "", "unsupported", nil
	}
	entry, _ := l.store.Game(id)
	title, _ = manifest.InstalledGameTitleID(entry.Path)
	if title == "" {
		return "", "", "", "unavailable", nil
	}
	return user, backendStatus.BinaryPath, title, "", nil
}

func (l *Launcher) titleRequestCurrent(user, binary string) bool {
	l.mu.Lock()
	running := l.auth.Running
	l.mu.Unlock()
	return !running && l.storedAccount() == user && l.backend.Current().BinaryPath == binary
}

func (l *Launcher) GetGameActivity(id string, refresh *bool) (shared.GameActivity, error) {
	if l.options.Demo {
		return shared.GameActivity{Status: "unavailable"}, nil
	}
	user, binary, title, status, err := l.titleRequest(id, func(c shared.BackendCapabilities) bool { return c.GameActivity })
	if err != nil || status != "" {
		return shared.GameActivity{Status: status}, err
	}
	l.activities.Select(user, binary)
	if !l.titleRequestCurrent(user, binary) {
		return shared.GameActivity{Status: "error"}, nil
	}
	value, err := l.activities.Get(title, refresh != nil && *refresh)
	if err != nil || !l.titleRequestCurrent(user, binary) {
		return shared.GameActivity{Status: "error"}, nil
	}
	return value, nil
}

func (l *Launcher) GetGameAchievements(id string, refresh *bool) (shared.GameAchievements, error) {
	if l.options.Demo {
		return shared.GameAchievements{Status: "unavailable"}, nil
	}
	user, binary, title, status, err := l.titleRequest(id, func(c shared.BackendCapabilities) bool { return c.Achievements })
	if err != nil || status != "" {
		return shared.GameAchievements{Status: status}, err
	}
	l.achievements.Select(user, binary)
	if !l.titleRequestCurrent(user, binary) {
		return shared.GameAchievements{Status: "error"}, nil
	}
	value, err := l.achievements.Get(title, refresh != nil && *refresh)
	if err != nil || !l.titleRequestCurrent(user, binary) {
		return shared.GameAchievements{Status: "error"}, nil
	}
	return value, nil
}

func (l *Launcher) LaunchGame(id string) error {
	if l.options.Demo {
		return nil
	}
	return l.launchGame(id)
}

func (l *Launcher) CreateGameShortcut(id, destination string) (string, error) {
	if l.options.Demo {
		return "", errors.New("Shortcuts are unavailable in demo mode.")
	}
	return l.createGameShortcut(id, destination)
}

func (l *Launcher) AddToSteam(id *string) (string, error) {
	if l.options.Demo {
		return "", errors.New("Steam integration is unavailable in demo mode.")
	}
	return l.addToSteam(id)
}

func (l *Launcher) InstallGameVcRuntime(id string) error {
	if l.options.Demo {
		return nil
	}
	return l.installGameVCRuntime(id)
}

func (l *Launcher) CancelGame() error {
	if l.options.Demo {
		return nil
	}
	return l.cancelGame()
}

var marketPattern = regexp.MustCompile(`^[A-Za-z-]{2,20}$`)

func (l *Launcher) UpdateSettings(patch map[string]json.RawMessage) (shared.Settings, error) {
	if l.options.Demo {
		settings := l.snapshot().Settings
		encoded, _ := json.Marshal(patch)
		json.Unmarshal(encoded, &settings)
		return settings, nil
	}
	raw := map[string]any{}
	for key, value := range patch {
		var decoded any
		if json.Unmarshal(value, &decoded) == nil && decoded != nil {
			raw[key] = decoded
		}
	}
	if limit, ok := raw["downloadLimitMiB"]; ok {
		if _, err := shared.DownloadLimitKiB(limit); err != nil {
			return shared.Settings{}, err
		}
		if _, err := shared.DownloadLimitArgs(limit, l.backend.Current().Capabilities); err != nil {
			return shared.Settings{}, err
		}
	}
	current := l.store.Settings()
	currentValues := map[string]any{"market": current.Market, "winePath": current.WinePath, "externalXodusRoot": current.ExternalXodusRoot, "runtimeRoot": current.RuntimeRoot}
	for key, value := range currentValues {
		if next, ok := raw[key]; ok && next != value {
			if err := l.ensureIdle(""); err != nil {
				return shared.Settings{}, err
			}
			break
		}
	}
	allowed := state.Patch{}
	if limit, ok := raw["downloadLimitMiB"]; ok {
		allowed["downloadLimitMiB"] = limit
	}
	text := func(key string) (string, bool) { value, ok := raw[key].(string); return value, ok }
	resolve := func(value string) string { absolute, _ := filepath.Abs(value); return absolute }
	if value, ok := text("installRoot"); ok {
		allowed["installRoot"] = resolve(value)
	}
	if value, ok := text("market"); ok && marketPattern.MatchString(value) {
		allowed["market"] = value
	}
	if value, ok := raw["parallelDownloads"].(float64); ok {
		allowed["parallelDownloads"] = max(1, min(16, int(value+0.5)))
	}
	if value, ok := text("winePath"); ok {
		allowed["winePath"] = value
	}
	if value, ok := text("externalXodusRoot"); ok {
		allowed["externalXodusRoot"] = resolve(value)
	}
	if value, ok := text("runtimeRoot"); ok {
		allowed["runtimeRoot"] = resolve(value)
	}
	for _, key := range []string{"verboseLogs", "firstRunCompleted"} {
		if value, ok := raw[key].(bool); ok {
			allowed[key] = value
		}
	}
	settings, err := l.store.SetSettings(allowed)
	if err != nil {
		return settings, err
	}
	if _, ok := raw["downloadLimitMiB"]; ok {
		var limit any = 0.0
		if settings.DownloadLimitMiB != nil {
			limit = *settings.DownloadLimitMiB
		}
		if l.limits.Apply(limit) != nil {
			l.emitSnapshot()
			return settings, errors.New("The speed limit was saved, but could not be sent to the active download. Retry the installation to apply it.")
		}
	}
	// A bandwidth-only change must not restart authentication or refresh devices.
	if install.IsBandwidthOnlyPatch(raw) {
		l.emitSnapshot()
		return settings, nil
	}
	l.backend.SetExternalRoot(settings.ExternalXodusRoot)
	l.runtime.SetRoot(settings.RuntimeRoot)
	l.backend.Refresh()
	l.refreshAccount(false, false)
	l.runtime.Refresh()
	l.emitSnapshot()
	return settings, nil
}

func (l *Launcher) ChooseDirectory(defaultPath *string) (*string, error) {
	if l.options.Demo {
		return nil, nil
	}
	value := ""
	if defaultPath != nil {
		value = *defaultPath
	}
	return optional(l.host.ChooseDirectory(value))
}

func (l *Launcher) ChooseFile(defaultPath *string) (*string, error) {
	if l.options.Demo {
		return nil, nil
	}
	value := ""
	if defaultPath != nil {
		value = *defaultPath
	}
	return optional(l.host.ChooseFile(value))
}

func optional(value string, err error) (*string, error) {
	if err != nil || value == "" {
		return nil, err
	}
	return &value, nil
}

func (l *Launcher) OpenPath(target string) error {
	if l.options.Demo {
		return nil
	}
	if _, err := os.Stat(target); err != nil {
		return err
	}
	if err := shortcuts.OpenDesktopTarget(target); err != nil {
		if fallback := l.host.OpenPath(target); fallback != nil {
			return err
		}
	}
	return nil
}

func (l *Launcher) loadDemoCovers() error {
	var ids []string
	add := func(id string) {
		if id != "" && !contains(ids, id) {
			ids = append(ids, id)
		}
	}
	for _, entry := range shared.DemoGames() {
		add(entry.StoreID)
	}
	for _, task := range shared.DemoDownloads() {
		add(task.ProductID)
	}
	for _, product := range shared.DemoAccountProducts {
		add(product.ProductID)
	}
	artwork := l.catalog.Artwork(context.Background(), ids, l.snapshot().Settings.Market)
	l.mu.Lock()
	for id, images := range artwork {
		l.demoArtwork[id] = shared.DemoArtwork{CoverURL: images.CoverURL, HeroURL: images.HeroURL}
	}
	l.mu.Unlock()
	return nil
}

func contains(values []string, value string) bool {
	for _, entry := range values {
		if entry == value {
			return true
		}
	}
	return false
}

// Shutdown stops operations safely before the application exits. Partial
// downloads are paused at the backend's durable checkpoints.
func Shutdown(l *Launcher) {
	l.unlocker.Cancel()
	l.queue.Stop()
	if l.library != nil {
		l.library.Reset()
	}
	if l.profile != nil {
		l.profile.Cancel()
	}
	l.accountRefresh.Invalidate()
	l.mu.Lock()
	if l.shutdownStarted {
		l.mu.Unlock()
		return
	}
	l.shutdownStarted = true
	cancelUpdates, updates, authJob := l.gameUpdateCancel, l.gameUpdateJob, l.authJob
	l.mu.Unlock()
	defer close(l.shuttingDown)
	defer func() {
		l.display.stop()
		if l.backendLog != nil {
			l.backendLog.Flush()
		}
	}()
	if l.store == nil || l.options.Demo {
		return
	}
	l.unlocker.Stop()
	if cancelUpdates != nil {
		cancelUpdates()
	}
	if updates != nil {
		updates.wait()
	}
	l.updater.Cancel()
	// Let the atomic replacement transaction finish or safely fail before exit.
	for l.updater.State().Updating {
		time.Sleep(100 * time.Millisecond)
	}
	// Persist queue ownership before terminating downloads. Partial files are
	// restored only from the backend's validated durable checkpoints next time.
	l.mu.Lock()
	operations := make(map[string]*procTracked, len(l.operations))
	for id, child := range l.operations {
		operations[id] = child
	}
	games := make([]*gameProcess, 0, len(l.gameProcesses))
	for _, process := range l.gameProcesses {
		games = append(games, process)
	}
	cancellations := make([]chan struct{}, 0, len(l.downloadCancellations))
	for _, done := range l.downloadCancellations {
		cancellations = append(cancellations, done)
	}
	kills := make([]func(), 0, len(l.authKills))
	for _, kill := range l.authKills {
		kills = append(kills, kill)
	}
	l.mu.Unlock()
	if l.backend.Current().Capabilities.StreamingControls {
		for id := range operations {
			l.store.MutateDownload(id, func(task *shared.DownloadTask) {
				task.Status, task.CanPause, task.ControlPending, task.TransferRate = "paused", true, false, ""
				task.Phase = "Paused — resume to restore saved download checkpoints"
			})
		}
	}
	l.store.Save()
	for _, child := range operations {
		if !isGameOperation(games, child) {
			child.Signal(sigint)
		}
	}
	for _, done := range cancellations {
		<-done
	}
	for _, kill := range kills {
		kill()
	}
	if authJob != nil {
		<-authJob
	}
	var wait sync.WaitGroup
	for _, process := range games {
		if err := process.Stop(); err != nil {
			log.Print("Could not stop game on exit: ", err)
		}
		wait.Add(1)
		go func() { defer wait.Done(); process.Wait() }()
	}
	stopped := make(chan struct{})
	go func() { wait.Wait(); close(stopped) }()
	select {
	case <-stopped:
	case <-time.After(8 * time.Second):
	}
	for _, process := range games {
		if err := process.ForceStop(); err != nil {
			log.Print("Could not force-stop game on exit: ", err)
		}
	}
	// Downloads persist their final state when their process exits.
	deadline := time.After(10 * time.Second)
	for id := range operations {
		if isGameOperation(games, operations[id]) {
			continue
		}
		select {
		case <-operations[id].Done():
		case <-deadline:
		}
	}
	l.mu.Lock()
	sessions := make([]*sessionType, 0, len(l.serviceSessions))
	for _, session := range l.serviceSessions {
		sessions = append(sessions, session)
	}
	l.mu.Unlock()
	for _, session := range sessions {
		session.Stop()
	}
	l.store.Save()
}

type (
	procTracked = proc.Tracked
	gameProcess = game.Process
	sessionType = service.Session
)

func isGameOperation(games []*gameProcess, child *procTracked) bool {
	for _, process := range games {
		if process.Tracked == child {
			return true
		}
	}
	return false
}
