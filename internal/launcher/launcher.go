package launcher

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"xodus-nexus-wails/internal/account"
	"xodus-nexus-wails/internal/backend"
	"xodus-nexus-wails/internal/catalog"
	"xodus-nexus-wails/internal/content"
	"xodus-nexus-wails/internal/diagnostics"
	"xodus-nexus-wails/internal/eaapp"
	"xodus-nexus-wails/internal/game"
	"xodus-nexus-wails/internal/imageutil"
	"xodus-nexus-wails/internal/install"
	"xodus-nexus-wails/internal/keyring"
	"xodus-nexus-wails/internal/launcherenv"
	"xodus-nexus-wails/internal/library"
	"xodus-nexus-wails/internal/maxima"
	"xodus-nexus-wails/internal/proc"
	"xodus-nexus-wails/internal/protonruntime"
	"xodus-nexus-wails/internal/service"
	"xodus-nexus-wails/internal/shared"
	"xodus-nexus-wails/internal/shortcuts"
	"xodus-nexus-wails/internal/state"
	"xodus-nexus-wails/internal/updater"
)

// Options configure a launcher instance.
type Options struct {
	Version  string
	UserData string
	// Icon is the launcher icon PNG used for shortcuts and notifications.
	Icon []byte
	// Executable is the persistent launcher used by desktop and Steam
	// shortcuts; "" disables shortcut creation.
	Executable string
	// Startup is the request parsed from the command line.
	Startup shortcuts.Request
	Demo    bool
	// DemoTour enables the renderer's guided demo tour.
	DemoTour bool
	Updater  updater.Config
}

// Launcher holds the main-process state. Exported methods form the API the
// renderer calls; everything else is internal.
type Launcher struct {
	host     Host
	options  Options
	userData string

	mu                    sync.Mutex
	store                 *state.Store
	backend               *backend.Manager
	backendLog            *diagnostics.Backend
	runtime               *protonruntime.Manager
	eaApp                 *eaapp.Manager
	maxima                *maxima.Manager
	eaLibraryCache        []shared.EATitle
	eaLibraryAt           time.Time
	operations            map[string]*proc.Tracked
	gameProcesses         map[string]*game.Process
	cancelledDownloads    map[string]bool
	downloadCancellations map[string]chan struct{}
	cleanup               *install.Cleanup
	downloadControls      map[string]func(paused bool) error
	limits                *install.BandwidthControls
	queue                 *install.Queue
	serviceSessions       map[string]*service.Session
	preparations          map[string]context.CancelFunc
	gameOperation         *shared.GameOperation
	gameRemoval           bool
	shutdownStarted       bool
	auth                  shared.AuthStatus
	authJob               chan struct{}
	authKills             map[string]func()
	// authVersion changes whenever the account state is replaced.
	authVersion         int
	walletAttempted     bool
	catalog             *catalog.Service
	activities          *account.TitleData[shared.GameActivity]
	achievements        *account.TitleData[shared.GameAchievements]
	library             *library.AccountLibrary
	plans               *install.Plans
	unlocker            *keyring.Unlocker
	keyring             *keyring.Support
	demoArtwork         map[string]shared.DemoArtwork
	updater             *updater.Updater
	gameUpdates         map[string]shared.GameUpdate
	checkingGameUpdates bool
	gameUpdateCancel    context.CancelFunc
	gameUpdateJob       *job
	snapshotTimer       *time.Timer
	shortcuts           *shortcuts.Shortcuts
	profile             *account.Profiles
	preparingLaunch     bool
	preparingInstall    bool
	preparingContent    bool
	finalizingGame      bool
	shortcutOnly        bool
	launchReady         bool
	pendingRequests     []string
	pendingErrors       []error
	accountRefresh      account.Refresh
	contentManager      *content.Manager
	contentRoot         string
	shortcutLaunch      *shortcuts.Launch
	notifications       *install.CompletionNotifications
	display             *displayBlocker
	shuttingDown        chan struct{}
	// ready closes once every component exists and snapshots can be built.
	ready     chan struct{}
	readyOnce sync.Once
}

// job is a shared in-flight operation whose result every caller receives.
type job struct {
	done chan struct{}
	err  error
}

func runJob(work func() error) *job {
	j := &job{done: make(chan struct{})}
	go func() { defer close(j.done); j.err = work() }()
	return j
}

func (j *job) wait() error { <-j.done; return j.err }

// New creates a launcher. Call Startup once the application is running.
func New(host Host, options Options) *Launcher {
	userData := options.UserData
	if options.Startup.Profile != "" {
		userData = options.Startup.Profile
	}
	l := &Launcher{
		host: host, options: options, userData: userData,
		operations: map[string]*proc.Tracked{}, gameProcesses: map[string]*game.Process{},
		cancelledDownloads: map[string]bool{}, downloadCancellations: map[string]chan struct{}{},
		downloadControls: map[string]func(bool) error{}, limits: install.NewBandwidthControls(),
		serviceSessions: map[string]*service.Session{}, authKills: map[string]func(){}, preparations: map[string]context.CancelFunc{},
		auth:         shared.AuthStatus{Note: "Checking the Xodus system keychain for a stored Microsoft account…"},
		catalog:      catalog.NewService(nil),
		activities:   account.NewActivities(nil),
		achievements: account.NewAchievements(nil),
		plans:        install.NewPlans(install.ReadInstallPlan, nil),
		unlocker:     &keyring.Unlocker{},
		demoArtwork:  map[string]shared.DemoArtwork{},
		gameUpdates:  map[string]shared.GameUpdate{},
		display:      newDisplayBlocker(),
		shuttingDown: make(chan struct{}),
		ready:        make(chan struct{}),
	}
	l.queue = install.NewQueue(l.finishShortcutSession)
	l.library = library.New(func(ctx context.Context, ids []string, market string) ([]shared.CatalogProduct, error) {
		return l.catalog.LicensedProducts(ctx, ids, market)
	}, nil)
	l.notifications = install.NewCompletionNotifications(notificationPort{host})
	l.keyring = keyring.NewSupport(nil, nil, nil, keyring.Actions{
		CanCreate: func() bool { return l.backendCommand("keyring-create") },
		Create: func() string {
			if l.closing() || !l.host.WindowVisible() {
				return "cancelled"
			}
			if !l.backendCommand("keyring-create") {
				return "unavailable"
			}
			binary, err := l.requireBackend("commands")
			if err != nil {
				return "unavailable"
			}
			return l.unlocker.Create(binary, l.host.WindowHandle())
		},
		StartProvider: func(provider keyring.KDEProvider, env launcherenv.Env, owned func() bool) error {
			stop := ""
			if l.backendCommand("keyring-session-stop") {
				stop = l.backend.Current().BinaryPath
			}
			return keyring.StartSessionKeyring(provider, env, owned, stop)
		},
		CanUnlock: func() bool { return l.backendCommand("keyring-unlock") },
		Unlock: func() string {
			if l.closing() || !l.host.WindowVisible() {
				return "cancelled"
			}
			binary, err := l.requireBackend("commands")
			if err != nil {
				return "unavailable"
			}
			return l.unlocker.Unlock(binary, l.host.WindowHandle())
		},
	})
	l.shortcutLaunch = shortcuts.NewLaunch(func() string {
		l.mu.Lock()
		defer l.mu.Unlock()
		if op := l.gameOperation; op != nil && op.Kind == "launch" && activeGame(op.Status) {
			return op.GameID
		}
		return ""
	}, l.launchGame)
	return l
}

type notificationPort struct{ host Host }

func (n notificationPort) Supported() bool               { return n.host.NotificationsSupported() }
func (n notificationPort) Show(title, body string) error { return n.host.Notify(title, body) }

func activeGame(status string) bool {
	return status == "preparing" || status == "running" || status == "stopping"
}

func activeDownload(status string) bool {
	return status == "running" || status == "queued" || status == "paused"
}

func now() string { return shared.JSISOString(time.Now()) }

func (l *Launcher) closing() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.shutdownStarted
}

func (l *Launcher) backendCommand(command string) bool {
	if l.backend == nil {
		return false
	}
	return slices.Contains(l.backend.Current().Capabilities.Commands, command)
}

// UserData is the profile directory in use.
func (l *Launcher) UserDataPath() string { return l.userData }

// Startup loads state, refreshes components and opens the window. It
// mirrors the original app.whenReady sequence.
func Startup(l *Launcher) error {
	if !l.options.Demo {
		l.backendLog = diagnostics.NewBackend(l.userData, func() diagnostics.Context {
			context := diagnostics.Context{Launcher: l.options.Version}
			if l.backend != nil {
				status := l.backend.Current()
				context.Backend, context.Source = status.Version, status.Source
			}
			return context
		}, 0)
		diagnostics.SetBackend(l.backendLog)
	}
	l.mu.Lock()
	l.shortcutOnly = l.options.Startup.GameID != "" && !l.options.Demo
	l.mu.Unlock()
	l.store = state.New(l.userData)
	l.store.Load()
	l.cleanup = install.NewCleanup(l.userData, nil, nil)
	l.profile = account.NewProfiles(l.userData, l.scheduleSnapshot, func(dataURL string) string {
		return imageutil.FitDataURL(dataURL, 256)
	}, nil)
	l.updater = updater.New(l.options.Updater, l.options.Version, os.Getenv("APPIMAGE"), l.userData, l.scheduleSnapshot, nil)
	l.updater.Initialize()
	home, _ := os.UserHomeDir()
	dataHome := os.Getenv("XDG_DATA_HOME")
	if !filepath.IsAbs(dataHome) {
		dataHome = filepath.Join(home, ".local", "share")
	}
	l.shortcuts = shortcuts.New(shortcuts.Locations{UserData: l.userData, Desktop: userDir("DESKTOP", "Desktop"),
		Applications: filepath.Join(dataHome, "applications"), Launcher: l.options.Executable, Icon: l.options.Icon})
	if l.options.Demo {
		l.markReady()
		go func() {
			if l.loadDemoCovers() == nil {
				l.emitSnapshot()
			}
		}()
		l.host.ShowWindow()
		return nil
	}
	if err := l.shortcuts.Initialize(); err != nil {
		log.Print("Game shortcut integration unavailable: ", err)
	}
	for _, entry := range l.store.Games() {
		if inspected, err := state.InspectGameDirectory(entry.Path); err == nil {
			l.store.PatchGame(entry.ID, inspectedPatch(inspected, entry, true))
		}
	}
	l.runtime = protonruntime.NewManager(l.store.Settings().RuntimeRoot, l.emitLog, func() bool {
		l.mu.Lock()
		busy := len(l.downloadCancellations) > 0 || len(l.operations) > 0 || l.preparingInstall
		l.mu.Unlock()
		return busy || l.queue.Size() > 0 || (l.backend != nil && l.backend.Current().Updating)
	}, nil)
	l.backend = backend.NewManager(l.userData, l.store.Settings().ExternalXodusRoot, l.emitLog, func() bool {
		l.mu.Lock()
		busy := len(l.downloadCancellations) > 0 || len(l.operations) > 0 || l.preparingInstall
		l.mu.Unlock()
		return busy || l.keyring.Busy() || l.queue.Size() > 0 || l.runtime.Current().Updating
	}, nil)
	l.eaApp = eaapp.NewManager(func() eaapp.Paths {
		return eaapp.Paths{Root: filepath.Join(filepath.Dir(l.store.Settings().RuntimeRoot), "ea-app"), SteamRoot: filepath.Join(home, ".steam", "root")}
	}, l.selectedWine, l.scheduleSnapshot, nil)
	l.maxima = maxima.NewManager(filepath.Join(l.userData, "maxima-builds"), l.emitLog, l.scheduleSnapshot)
	l.backend.Refresh()
	l.runtime.Refresh()
	l.markReady()
	go func() {
		l.backend.CheckUpdate(context.Background())
		// First run: install the prebuilt backend automatically so the setup
		// wizard's sign-in step works without a detour into Settings.
		status := l.backend.Current()
		if !l.store.Settings().FirstRunCompleted && !status.Available && status.ReleaseAvailable && status.UpdateAvailable {
			if l.backend.Update(context.Background(), l.emitSnapshot) == nil && l.host.WindowVisible() {
				go l.refreshInitialAccount()
			}
		}
		l.emitSnapshot()
	}()
	go func() { l.runtime.CheckNexus(context.Background()); l.emitSnapshot() }()
	l.syncManagedWinePath()
	go func() {
		if l.enrichLibraryCovers() == nil {
			l.emitSnapshot()
		}
	}()
	go l.enrichDownloadCovers(l.store.Downloads(), l.currentMarket())
	go func() { l.updater.Check(context.Background()); l.emitSnapshot() }()
	l.mu.Lock()
	l.launchReady = true
	l.mu.Unlock()
	if id := l.options.Startup.GameID; id != "" {
		if err := l.prepareSystemWallet(); err != nil {
			return err
		}
		l.refreshAccount(false, false)
		l.receiveLaunchRequest(id)
	} else {
		l.showLauncher()
		// Deliberately not awaited. A native wallet prompt must not hide the launcher.
		go func() {
			if err := l.refreshInitialAccount(); err != nil {
				l.mu.Lock()
				if !l.shutdownStarted && !l.auth.Running && l.auth.LastResult != "success" {
					l.auth.Note = "Account status is unavailable. Check the system keyring in Settings, then refresh."
				}
				l.mu.Unlock()
				l.emitSnapshot()
			}
		}()
	}
	l.mu.Lock()
	requests, failures := l.pendingRequests, l.pendingErrors
	l.pendingRequests, l.pendingErrors = nil, nil
	l.mu.Unlock()
	for _, id := range requests {
		l.receiveLaunchRequest(id)
	}
	for _, err := range failures {
		go l.shortcutError(err)
	}
	return nil
}

// SecondInstance handles a launch from another process with the same profile.
func SecondInstance(l *Launcher, args []string) {
	if l.options.Demo {
		return
	}
	request, err := shortcuts.InstanceRequest(args, l.userData)
	l.mu.Lock()
	ready := l.launchReady
	if err != nil && !ready {
		l.pendingErrors = append(l.pendingErrors, err)
	}
	l.mu.Unlock()
	if err != nil {
		if ready {
			go l.shortcutError(err)
		}
		return
	}
	l.receiveLaunchRequest(request.GameID)
}

// ShortcutOnly reports whether closing the window should keep the launcher
// running for a game started from a shortcut.
func ShortcutOnly(l *Launcher) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.shortcutOnly
}

func (l *Launcher) showLauncher() {
	l.mu.Lock()
	l.shortcutOnly = false
	l.mu.Unlock()
	l.host.ShowWindow()
}

func (l *Launcher) shortcutError(err error) {
	l.showLauncher()
	message := "The game could not be launched."
	if err != nil {
		message = err.Error()
	}
	l.host.ShowError("Unable to launch game", message)
}

func (l *Launcher) finishShortcutSession() {
	// Account service and game-process cleanup must finish before normal quit.
	l.mu.Lock()
	idle := l.shortcutOnly && !l.preparingLaunch && !l.finalizingGame && len(l.downloadCancellations) == 0 &&
		len(l.operations) == 0 && len(l.serviceSessions) == 0
	l.mu.Unlock()
	if idle && !l.shortcutLaunch.Preparing() && l.queue.Size() == 0 {
		l.host.Quit()
	}
}

func (l *Launcher) receiveLaunchRequest(id string) {
	l.mu.Lock()
	if !l.launchReady {
		l.pendingRequests = append(l.pendingRequests, id)
		l.mu.Unlock()
		return
	}
	l.mu.Unlock()
	if id == "" {
		go l.showLauncher()
		return
	}
	go func() {
		if err := l.shortcutLaunch.Request(id); err != nil {
			l.shortcutError(err)
		}
		l.finishShortcutSession()
	}()
}

func (l *Launcher) snapshot() shared.AppSnapshot {
	if l.options.Demo {
		lock := backend.SourceLock
		l.mu.Lock()
		artwork := shared.Clone(l.demoArtwork)
		l.mu.Unlock()
		return shared.DemoSnapshot(shared.DemoVersions{Launcher: l.options.Version, Backend: lock.Version,
			BackendCommit: lock.Commit, BackendCompatibility: lock.Compatibility}, artwork)
	}
	// Components report changes through scheduleSnapshot, so read them
	// before taking the launcher lock.
	status, runtime := l.backend.Current(), l.runtime.Current()
	games, downloads, settings := l.store.Games(), l.store.Downloads(), l.store.Settings()
	profile, update := l.profile.Value(), l.updater.State()
	var eaApp *shared.EAAppStatus
	if l.eaApp != nil {
		value := l.eaApp.Current()
		eaApp = &value
	}
	var maximaStatus *shared.MaximaStatus
	if l.maxima != nil {
		value := l.maxima.Current()
		maximaStatus = &value
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	for index, task := range downloads {
		if _, ok := l.downloadCancellations[task.ID]; ok {
			downloads[index].Cancelling, downloads[index].CanPause = true, false
			downloads[index].Phase = "Cancelling download and checking files…"
		}
	}
	auth := shared.Clone(l.auth)
	if profile != nil {
		auth.Gamertag, auth.AvatarDataURL, auth.Subscription = profile.Gamertag, profile.AvatarDataURL, profile.Subscription
	}
	return shared.AppSnapshot{
		Backend: status, Runtime: runtime, Games: games, Downloads: downloads,
		Settings: settings, Auth: auth, LauncherUpdate: &update, GameUpdates: shared.Clone(l.gameUpdates),
		DownloadProgressSupported: status.Capabilities.StreamingProgressJSON, GameOperation: shared.Clone(l.gameOperation), EAApp: eaApp, Maxima: maximaStatus,
	}
}

func (l *Launcher) emitSnapshot() {
	l.mu.Lock()
	if l.snapshotTimer != nil {
		l.snapshotTimer.Stop()
		l.snapshotTimer = nil
	}
	l.mu.Unlock()
	select {
	case <-l.ready:
	default:
		return
	}
	l.host.Emit("snapshot", l.snapshot())
}

func (l *Launcher) markReady() { l.readyOnce.Do(func() { close(l.ready) }) }

func (l *Launcher) scheduleSnapshot() {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.snapshotTimer == nil {
		l.snapshotTimer = time.AfterFunc(250*time.Millisecond, l.emitSnapshot)
	}
}

func (l *Launcher) emitLog(scope, line string) {
	l.host.Emit("operation-log", map[string]string{"scope": scope, "line": line})
}

// requireBackend returns the backend binary when the named capability is
// available. "commands" only checks that a backend exists.
func (l *Launcher) requireBackend(command string) (string, error) {
	status := l.backend.Current()
	if !status.Available || status.BinaryPath == "" {
		return "", errors.New("The Xodus backend is unavailable.")
	}
	capabilities := map[string]bool{
		"account": status.Capabilities.Account, "login": status.Capabilities.Login, "logout": status.Capabilities.Logout,
		"download": status.Capabilities.Download, "streaming": status.Capabilities.Streaming, "run": status.Capabilities.Run,
	}
	if supported, ok := capabilities[command]; ok && !supported {
		return "", fmt.Errorf("The active Xodus backend does not support the %s command.", command)
	}
	return status.BinaryPath, nil
}

func (l *Launcher) ensureIdle(mode string) error {
	if l.keyring.Busy() {
		return errors.New("Wait for the wallet operation to finish.")
	}
	l.mu.Lock()
	var err error
	switch {
	case l.auth.Running:
		err = errors.New("A sign-in operation is already in progress.")
	case l.shutdownStarted:
		err = errors.New("The launcher is closing.")
	case len(l.downloadCancellations) > 0:
		err = errors.New("A download is being cancelled. Wait for its cleanup to finish.")
	case l.preparingInstall || l.preparingContent:
		err = errors.New("An installation is being prepared.")
	case l.preparingLaunch:
		err = errors.New("A game launch is already being prepared.")
	case l.finalizingGame:
		err = errors.New("The previous game is still finishing cleanup.")
	case l.checkingGameUpdates:
		err = errors.New("A game update check is in progress.")
	case l.gameRemoval:
		err = errors.New("A game is being removed. Wait for it to finish.")
	}
	operations := make([]string, 0, len(l.operations))
	for id := range l.operations {
		operations = append(operations, id)
	}
	operation := shared.Clone(l.gameOperation)
	l.mu.Unlock()
	if err != nil {
		return err
	}
	if l.updater.State().Updating {
		return errors.New("The launcher is being updated.")
	}
	if err := install.AssertActivityCompatible(operations, l.store.Downloads(), operation, l.queue.Size(), mode); err != nil {
		return err
	}
	if l.backend.Current().Updating {
		return errors.New("The backend is being updated.")
	}
	if l.runtime.Current().Updating {
		return errors.New("Xodus Runtime is being installed.")
	}
	return nil
}

func (l *Launcher) selectedWine() string {
	settings := l.store.Settings()
	return service.SelectedWinePath(settings.WinePath, settings.RuntimeRoot, l.runtime.Current().WinePath)
}

func (l *Launcher) syncManagedWinePath() {
	if selected := l.selectedWine(); selected != "" && selected != l.store.Settings().WinePath {
		l.store.SetSettings(state.Patch{"winePath": selected})
	}
}

func (l *Launcher) currentMarket() string {
	configured := l.store.Settings().Market
	if strings.EqualFold(configured, "neutral") {
		return localeCountry()
	}
	return configured
}

func (l *Launcher) logsRoot() string {
	return filepath.Join(filepath.Dir(l.store.Settings().RuntimeRoot), "logs")
}

// localeCountry mirrors app.getLocaleCountryCode from the POSIX locale.
func localeCountry() string {
	for _, key := range []string{"LC_ALL", "LC_MESSAGES", "LANG"} {
		value := os.Getenv(key)
		if value == "" {
			continue
		}
		value, _, _ = strings.Cut(value, ".")
		value, _, _ = strings.Cut(value, "@")
		if _, region, ok := strings.Cut(value, "_"); ok && len(region) == 2 {
			return strings.ToUpper(region)
		}
	}
	return "US"
}

// userDir resolves an XDG user directory such as DESKTOP or DOWNLOAD.
func userDir(name, fallback string) string {
	home, _ := os.UserHomeDir()
	configHome := os.Getenv("XDG_CONFIG_HOME")
	if !filepath.IsAbs(configHome) {
		configHome = filepath.Join(home, ".config")
	}
	if file, err := os.Open(filepath.Join(configHome, "user-dirs.dirs")); err == nil {
		defer file.Close()
		scanner := bufio.NewScanner(file)
		for scanner.Scan() {
			key, value, ok := strings.Cut(strings.TrimSpace(scanner.Text()), "=")
			if !ok || key != "XDG_"+name+"_DIR" {
				continue
			}
			value = strings.Trim(value, `"`)
			value = strings.Replace(value, "$HOME", home, 1)
			if filepath.IsAbs(value) {
				return value
			}
		}
	}
	return filepath.Join(home, fallback)
}

// inspectedPatch copies refreshed installation metadata onto a game entry.
func inspectedPatch(inspected, previous shared.Game, includeVersion bool) state.Patch {
	keys := []string{"installState", "installMessage", "executable", "antiCheat", "vcRedistEncrypted", "coverDataUrl"}
	if includeVersion {
		keys = append(keys, "installedVersion", "packageName")
	}
	patch := state.Fields(inspected, keys...)
	storeID := inspected.StoreID
	if storeID == "" {
		storeID = previous.StoreID
	}
	patch["storeId"] = storeID
	if storeID == "" {
		patch["storeId"] = nil
	}
	return patch
}
