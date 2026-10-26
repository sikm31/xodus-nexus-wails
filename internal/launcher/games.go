package launcher

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"

	"xodus-nexus-wails/internal/backend"
	"xodus-nexus-wails/internal/game"
	"xodus-nexus-wails/internal/imageutil"
	"xodus-nexus-wails/internal/install"
	"xodus-nexus-wails/internal/launcherenv"
	"xodus-nexus-wails/internal/manifest"
	"xodus-nexus-wails/internal/proc"
	"xodus-nexus-wails/internal/service"
	"xodus-nexus-wails/internal/shared"
	"xodus-nexus-wails/internal/shortcuts"
	"xodus-nexus-wails/internal/state"
	"xodus-nexus-wails/internal/steam"
)

const sigint = syscall.SIGINT

func executable(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir() && info.Mode()&0o111 != 0 && syscall.Access(path, 1) == nil
}

func readable(path string) bool { return syscall.Access(path, 4) == nil }

func (l *Launcher) importGame(input string) (*shared.Game, error) {
	if l.cancellationsPending() {
		return nil, errors.New("A download is being cancelled. Wait for its cleanup to finish.")
	}
	directory := input
	if directory == "" {
		chosen, err := l.host.ChooseDirectory(l.store.Settings().InstallRoot)
		if err != nil || chosen == "" {
			return nil, err
		}
		directory = chosen
	}
	absolute, err := filepath.Abs(directory)
	if err != nil {
		return nil, err
	}
	entry, err := state.InspectGameDirectory(absolute)
	if err != nil {
		return nil, err
	}
	if entry.StoreID != "" {
		if artwork, ok := l.catalog.Artwork(context.Background(), []string{entry.StoreID}, l.currentMarket())[strings.ToUpper(entry.StoreID)]; ok {
			if artwork.CoverURL != "" {
				entry.CoverURL = artwork.CoverURL
			}
			if artwork.HeroURL != "" {
				entry.HeroURL = artwork.HeroURL
			}
		}
	}
	if l.cancellationsPending() {
		return nil, errors.New("A download is being cancelled. Wait for its cleanup to finish.")
	}
	if _, err := l.store.UpsertGame(entry); err != nil {
		return nil, err
	}
	l.emitSnapshot()
	return &entry, nil
}

func (l *Launcher) cancellationsPending() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.downloadCancellations) > 0
}

func (l *Launcher) gameLaunchOptions(id string) (shared.GameLaunchOptions, error) {
	if _, ok := l.store.Game(id); !ok {
		return shared.GameLaunchOptions{}, errors.New("Game not found.")
	}
	wine := l.selectedWine()
	if wine == "" {
		return shared.GameLaunchOptions{CloudReason: "Select Xodus Runtime in Settings.", VCReason: "Select Xodus Runtime in Settings."}, nil
	}
	if !executable(wine) {
		return shared.GameLaunchOptions{CloudReason: "The selected Wine binary is unavailable.", VCReason: "The selected Wine binary is unavailable."}, nil
	}
	protonRoot := filepath.Clean(filepath.Join(filepath.Dir(wine), "..", ".."))
	protonAvailable := executable(filepath.Join(protonRoot, "proton")) && readable(filepath.Join(protonRoot, "files", "share", "default_pfx"))
	preview := service.CloudSaveRuntime(wine)
	binary := l.backend.Current().BinaryPath
	serviceAvailable := binary != "" && executable(filepath.Join(filepath.Dir(binary), "xodus-service"))
	options := shared.GameLaunchOptions{CloudSaves: preview && serviceAvailable, VCInstall: protonAvailable}
	switch {
	case !preview:
		options.CloudReason = "The selected Wine runtime does not include the cloud-save preview."
	case !serviceAvailable:
		options.CloudReason = "This Xodus backend has no xodus-service."
	}
	if !protonAvailable {
		options.VCReason = "VC++ installation requires a full Proton runtime."
	}
	return options, nil
}

func (l *Launcher) launchGame(id string) error {
	if err := l.ensureIdle("launch"); err != nil {
		return err
	}
	l.mu.Lock()
	l.preparingLaunch = true
	l.mu.Unlock()
	defer func() {
		l.mu.Lock()
		l.preparingLaunch = false
		l.mu.Unlock()
	}()
	return l.prepareGameLaunch(id)
}

// operationLog writes a timestamped private log and mirrors lines into the
// visible game operation while it is current.
type operationLog struct {
	mu   sync.Mutex
	file *os.File
}

func (o *operationLog) write(line string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.file != nil {
		fmt.Fprintf(o.file, "[%s] %s\n", now(), line)
	}
}

func (o *operationLog) close() {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.file != nil {
		o.file.Close()
		o.file = nil
	}
}

func openOperationLog(path string) *operationLog {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		log.Printf("Unable to write log %s: %v", path, err)
	}
	return &operationLog{file: file}
}

func (l *Launcher) appendOperation(operationID, line string) {
	l.mu.Lock()
	if op := l.gameOperation; op != nil && op.ID == operationID {
		op.Log = append(op.Log, line)
		if len(op.Log) > 250 {
			op.Log = op.Log[len(op.Log)-250:]
		}
		op.Phase = line
		if shared.UTF16Len(line) > 180 {
			op.Phase = shared.TruncateUTF16(line, 177) + "…"
		}
	}
	l.mu.Unlock()
	l.scheduleSnapshot()
}

func (l *Launcher) prepareGameLaunch(id string) (err error) {
	entry, ok := l.store.Game(id)
	if !ok {
		return errors.New("Game not found.")
	}
	target, err := install.CanonicalInstallPath(entry.Path)
	if err != nil {
		return err
	}
	for _, task := range l.store.Downloads() {
		if !activeDownload(task.Status) {
			continue
		}
		destination, err := install.CanonicalInstallPath(task.Destination)
		if task.GameID == id || (err == nil && install.PathsOverlap(target, destination)) {
			return errors.New("Finish or cancel this game's installation before playing it.")
		}
	}
	inspected, err := state.InspectGameDirectory(entry.Path)
	if err != nil {
		return err
	}
	l.store.PatchGame(id, inspectedPatch(inspected, entry, false))
	l.emitSnapshot()
	binary, err := l.requireBackend("run")
	if err != nil {
		return err
	}
	if inspected.InstallState != "ready" {
		if inspected.InstallMessage != "" {
			return errors.New(inspected.InstallMessage)
		}
		return errors.New("The installation is not ready for Xodus run.")
	}
	locale, err := manifest.SupportedGameLocale(entry.Locale, nil)
	if err != nil {
		return err
	}
	titleID, _ := manifest.InstalledGameTitleID(entry.Path)
	wine := l.selectedWine()
	if wine == "" {
		return errors.New("Select a Wine / Proton binary in Settings.")
	}
	if !executable(wine) {
		return fmt.Errorf("The selected Wine runtime is unavailable: %s. Select or reinstall Xodus Runtime in Settings.", wine)
	}
	cloudRuntime := service.CloudSaveRuntime(wine)
	if service.UnbridgedCloudSaveRuntime(wine) {
		return errors.New("The active runtime has cloud saves without the required service bridge. Update the active runtime in Settings.")
	}
	settings := l.store.Settings()
	args, err := game.LaunchArguments(inspected.Path, inspected.Executable, wine, settings.Market, l.backend.Current().LinuxRunPatch, backend.LinuxRunPatch)
	if err != nil {
		return err
	}
	operationID := "game-" + id
	dataRoot := filepath.Dir(settings.RuntimeRoot)
	logDirectory := filepath.Join(dataRoot, "logs")
	logPath := filepath.Join(logDirectory, id+"-latest.log")
	if err := os.MkdirAll(logDirectory, 0o755); err != nil {
		return err
	}
	launchLog := openOperationLog(logPath)
	l.mu.Lock()
	l.gameOperation = &shared.GameOperation{ID: operationID, GameID: id, Title: entry.Title, Kind: "launch", Status: "preparing",
		Phase: "Preparing Proton and Xodus…", Log: []string{}, LogPath: logPath, StartedAt: now()}
	l.mu.Unlock()
	appendGameLog := func(line string) {
		launchLog.write(line)
		l.appendOperation(operationID, line)
	}
	verbose := os.Getenv("XODUS_NEXUS_VERBOSE_LOGS") == "1" || settings.VerboseLogs
	gameLog := game.NewLogReducer(appendGameLog, verbose, nil)
	addGameLog := gameLog.Push
	cloudState := "not available in active runtime"
	if cloudRuntime {
		cloudState = "enabled"
	}
	verboseState := "off"
	if verbose {
		verboseState = "on"
	}
	launchLog.write("Game: " + entry.Title)
	launchLog.write(fmt.Sprintf("Mode: package manifest; cloud saves: %s; verbose logs: %s", cloudState, verboseState))
	launchLog.write("Backend: " + binary)
	launchLog.write("Wine: " + wine)
	l.emitSnapshot()
	var session *service.Session
	var wrapper game.Wrapper
	releaseDisplay := func() {}
	defer func() {
		if err == nil {
			return
		}
		releaseDisplay()
		if wrapper.Cleanup != nil {
			wrapper.Cleanup()
		}
		if session != nil {
			session.Stop()
			l.mu.Lock()
			delete(l.serviceSessions, operationID)
			l.mu.Unlock()
		}
		launchLog.write("Preparation error: " + err.Error())
		gameLog.Flush()
		launchLog.close()
		l.mu.Lock()
		if l.gameOperation != nil && l.gameOperation.ID == operationID {
			l.gameOperation = nil
		}
		l.mu.Unlock()
		l.emitSnapshot()
	}()
	if entry.LaunchTools != nil && entry.LaunchTools.DualSenseCompatibility != nil && *entry.LaunchTools.DualSenseCompatibility {
		launchLog.write("DualSense compatibility mode enabled for this game (054c:0ce6).")
	}
	if err = l.prepareSystemWallet(); err != nil {
		return err
	}
	legacyPrefix := filepath.Join(dataRoot, "prefixes", entry.ID)
	compatibilityData := filepath.Join(dataRoot, "compatdata", entry.ID)
	home, _ := os.UserHomeDir()
	steamRoot := filepath.Join(home, ".steam", "root")
	if wrapper, err = game.PrepareWrapper(wine, entry.LaunchTools, filepath.Join(dataRoot, "launch-wrappers"), launcherenv.FromOS(), entry.Path); err != nil {
		return err
	}
	runner, err := game.PrepareProtonRunner(wine, compatibilityData, steamRoot, wrapper.Command)
	if err != nil {
		return err
	}
	wineCommand, runnerCommand, launchCommand := wine, "", ""
	if runner != nil {
		wineCommand, runnerCommand, launchCommand = runner.Command, runner.Command, runner.LaunchCommand
	}
	args[2] = game.PreparedWine(runnerCommand, launchCommand, wrapper.Command)
	if args[2] == "" {
		args[2] = wine
	}
	prefix := legacyPrefix
	if runner != nil {
		prefix = filepath.Join(compatibilityData, "pfx")
	}
	if inspected.VCRedistEncrypted != nil && *inspected.VCRedistEncrypted {
		if runner == nil {
			return errors.New("This encrypted VC++ prerequisite requires a Proton prefix. Select Xodus Runtime in Settings.")
		}
		installed := game.InstalledVCRuntimeVersion(prefix)
		if installed == "" || !game.VersionAtLeast(installed, game.VCRedistVersion) {
			found := installed
			if found == "" {
				found = "not found"
			}
			return fmt.Errorf("VC++ runtime %s is too old for this encrypted installer. Right-click the game and choose Install / update VC++, then press Play.", found)
		}
		addGameLog(fmt.Sprintf("VC++ runtime %s verified; launching the original package executable from a temporary decrypted launch view.", installed))
	}
	base := launcherenv.FromOS()
	if runner == nil {
		base["WINEPREFIX"], base["WINEARCH"] = legacyPrefix, "win64"
	}
	localized, err := manifest.GameLocaleEnvironment(game.WindowEnvironment(game.LoggingEnvironment(base, verbose)), locale)
	if err != nil {
		return err
	}
	environment := game.AntiCheatEnvironment(localized)
	// Never inherit another title's discovery list. Hints are scoped public
	// metadata, not licenses, and cannot make an optional lookup block Play.
	delete(environment, "NEXUS_RELATED_PRODUCT_IDS")
	if service.StoreDiscoveryRuntime(wine) {
		related, err := l.content().RelatedProducts(context.Background(), entry, l.library.PeekAccess(), l.currentMarket())
		if err != nil {
			addGameLog("Additional DLC discovery unavailable. Continuing with package-declared content.")
		} else if len(related) > 0 {
			environment["NEXUS_RELATED_PRODUCT_IDS"] = strings.Join(related, ",")
		}
	}
	roots, rootsErr := l.content().Roots(entry)
	if rootsErr != nil {
		addGameLog("Managed content discovery unavailable: " + rootsErr.Error())
	} else if len(roots) > 0 {
		values := []string{}
		if existing := environment["XODUS_CONTENT_ROOTS"]; existing != "" {
			values = append(values, existing)
		}
		for _, root := range roots {
			values = append(values, "Z:"+strings.ReplaceAll(root, "/", `\`))
		}
		environment["XODUS_CONTENT_ROOTS"] = strings.Join(values, ";")
	}
	if locale != "" {
		addGameLog("Selected game language: " + locale + ".")
	}
	game.SyncPrefixUserLocale(wineCommand, prefix, locale, environment, addGameLog)
	game.SyncUnrealCultureGameIni(prefix, entry.Path, titleID, locale, addGameLog)
	if cloudRuntime {
		if session, err = service.Start(context.Background(), binary, l.userData, addGameLog); err != nil {
			return err
		}
		l.mu.Lock()
		l.serviceSessions[operationID] = session
		l.mu.Unlock()
		environment["XODUS_SOCKET"] = session.SocketPath
		environment["XODUS_BRIDGE_PORT"] = fmt.Sprint(session.BridgePort)
		environment["XODUS_BRIDGE_KEY"] = session.BridgeKey
		environment["XODUS_SAVE_SYNC"] = "auto"
		addGameLog("Xbox cloud save preview is enabled. Saves may download from and upload to the account cloud.")
	}
	var stopPrefix *game.StopPrefix
	if runner != nil {
		addGameLog("Proton prefix: " + runner.CompatibilityDataPath + "/pfx")
		stopPrefix = &runner.StopPrefix
	} else if err = os.MkdirAll(legacyPrefix, 0o755); err != nil {
		return err
	}
	process, err := game.Spawn(binary, args, proc.RunOptions{Dir: entry.Path, Env: environment, OnLine: addGameLog, MaxOutput: 8_000_000}, stopPrefix)
	if err != nil {
		return err
	}
	releaseDisplay = l.display.acquire()
	l.mu.Lock()
	l.operations[operationID] = process.Tracked
	l.gameProcesses[operationID] = process
	if l.gameOperation != nil {
		l.gameOperation.Status, l.gameOperation.Phase = "running", "Xodus is running; waiting for the game window…"
	}
	l.mu.Unlock()
	l.emitSnapshot()
	release := releaseDisplay
	go func() {
		result := process.Wait()
		addGameLog(fmt.Sprintf("The Xodus process exited with code %d.", result.Code))
		l.mu.Lock()
		if op := l.gameOperation; op != nil && op.ID == operationID {
			op.FinishedAt = now()
			if result.Code == 0 || op.Status == "stopping" {
				op.Status = "complete"
				op.Phase = "The game was stopped. Forced shutdown may leave unsynchronized saves."
				if result.Code == 0 {
					op.Phase = "The game has exited."
				}
			} else {
				var lines []string
				for _, line := range strings.Split(result.Output, "\n") {
					if line = strings.TrimSpace(line); line != "" {
						lines = append(lines, line)
					}
				}
				detail := strings.Join(lines[max(0, len(lines)-6):], " | ")
				op.Status = "failed"
				if failure := shared.ContentLicenseFailure(strings.Split(result.Output, "\n")); failure != "" {
					op.Error = failure
				} else if detail != "" {
					op.Error = fmt.Sprintf("Xodus run exited with code %d: %s", result.Code, shared.TruncateUTF16(detail, 900))
				} else {
					op.Error = fmt.Sprintf("Xodus run exited with code %d.", result.Code)
				}
				op.Phase = op.Error
			}
		}
		l.finalizingGame = true
		l.mu.Unlock()
		release()
		l.mu.Lock()
		delete(l.operations, operationID)
		delete(l.gameProcesses, operationID)
		l.mu.Unlock()
		if session != nil {
			session.Stop()
			l.mu.Lock()
			delete(l.serviceSessions, operationID)
			l.mu.Unlock()
		}
		if err := process.Cleanup(); err != nil {
			log.Print("Game service cleanup failed: ", err)
		}
		if wrapper.Cleanup != nil {
			if err := wrapper.Cleanup(); err != nil {
				log.Print("Game wrapper cleanup failed: ", err)
			}
		}
		gameLog.Flush()
		launchLog.close()
		l.emitSnapshot()
		l.mu.Lock()
		failed := l.gameOperation != nil && l.gameOperation.ID == operationID && l.gameOperation.Status == "failed"
		message := ""
		if failed {
			message = l.gameOperation.Error
		}
		shortcutOnly := l.shortcutOnly
		l.mu.Unlock()
		if failed && shortcutOnly {
			if message == "" {
				message = "The game exited with an error."
			}
			l.shortcutError(errors.New(message))
		}
		l.mu.Lock()
		l.finalizingGame = false
		l.mu.Unlock()
		l.finishShortcutSession()
	}()
	return nil
}

func (l *Launcher) installGameVCRuntime(id string) (err error) {
	if err := l.ensureIdle(""); err != nil {
		return err
	}
	entry, ok := l.store.Game(id)
	if !ok {
		return errors.New("Game not found.")
	}
	inspected, err := state.InspectGameDirectory(entry.Path)
	if err != nil {
		return err
	}
	if inspected.InstallState != "ready" {
		if inspected.InstallMessage != "" {
			return errors.New(inspected.InstallMessage)
		}
		return errors.New("The game installation is not ready.")
	}
	wine := l.selectedWine()
	if wine == "" {
		return errors.New("Select Xodus Runtime in Settings first.")
	}
	if !executable(wine) {
		return fmt.Errorf("The selected Wine runtime is unavailable: %s", wine)
	}
	operationID := "vc-" + id
	dataRoot := filepath.Dir(l.store.Settings().RuntimeRoot)
	logDirectory := filepath.Join(dataRoot, "logs")
	if err := os.MkdirAll(logDirectory, 0o755); err != nil {
		return err
	}
	logPath := filepath.Join(logDirectory, id+"-vc-runtime.log")
	vcLog := openOperationLog(logPath)
	l.mu.Lock()
	l.gameOperation = &shared.GameOperation{ID: operationID, GameID: id, Title: entry.Title, Kind: "prerequisite", Status: "preparing",
		Phase: "Preparing the Proton prefix…", Log: []string{}, LogPath: logPath, StartedAt: now()}
	l.mu.Unlock()
	addLine := func(line string) {
		vcLog.write(line)
		l.appendOperation(operationID, line)
	}
	l.emitSnapshot()
	ctx, cancel := context.WithCancel(context.Background())
	l.mu.Lock()
	l.preparations[operationID] = cancel
	l.mu.Unlock()
	finish := func(status, phase, failure string) {
		l.mu.Lock()
		if op := l.gameOperation; op != nil && op.ID == operationID {
			op.Status, op.Phase, op.Error, op.FinishedAt = status, phase, failure, now()
		}
		l.mu.Unlock()
	}
	defer func() {
		if err != nil {
			finish("failed", err.Error(), err.Error())
		}
		cancel()
		l.mu.Lock()
		delete(l.preparations, operationID)
		delete(l.operations, operationID)
		l.mu.Unlock()
		vcLog.close()
		l.emitSnapshot()
	}()
	compatibilityData := filepath.Join(dataRoot, "compatdata", entry.ID)
	home, _ := os.UserHomeDir()
	runner, err := game.PrepareProtonRunner(wine, compatibilityData, filepath.Join(home, ".steam", "root"), "")
	if err != nil {
		return err
	}
	if runner == nil {
		return errors.New("VC++ installation requires a full Proton runtime, not a standalone Wine binary.")
	}
	prefix := filepath.Join(compatibilityData, "pfx")
	previous := game.InstalledVCRuntimeVersion(prefix)
	if previous != "" && game.VersionAtLeast(previous, game.VCRedistVersion) {
		addLine(fmt.Sprintf("VC++ %s is already installed. No changes needed.", previous))
	} else {
		found := previous
		if found == "" {
			found = "not found"
		}
		addLine(fmt.Sprintf("Current VC++ runtime: %s. Downloading Microsoft VC++ %s.", found, game.VCRedistVersion))
		lastProgress := int64(-1)
		installer, err := game.DownloadVerifiedVCRedist(ctx, filepath.Join(dataRoot, "prerequisites"), func(downloaded, total int64) {
			percent := int64(-1)
			if total > 0 {
				percent = downloaded * 100 / total
			}
			if percent == lastProgress || (percent >= 0 && percent < lastProgress+5 && percent != 100) {
				return
			}
			lastProgress = percent
			l.mu.Lock()
			if op := l.gameOperation; op != nil && op.ID == operationID {
				op.Phase = fmt.Sprintf("Downloading Microsoft VC++: %d%%", percent)
				if total <= 0 {
					op.Phase = fmt.Sprintf("Downloading Microsoft VC++: %.1f MiB", float64(downloaded)/1024/1024)
				}
			}
			l.mu.Unlock()
			l.scheduleSnapshot()
		})
		if ctx.Err() != nil {
			return errors.New("VC++ installation was cancelled.")
		}
		if err != nil {
			return err
		}
		l.mu.Lock()
		delete(l.preparations, operationID)
		l.mu.Unlock()
		addLine("Microsoft installer SHA-256 verified. Installing into this game’s Proton prefix…")
		tracked, err := proc.Spawn(runner.Command, []string{installer, "/install", "/quiet", "/norestart"},
			proc.RunOptions{Env: game.LoggingEnvironment(launcherenv.FromOS(), false), OnLine: addLine, MaxOutput: 1_000_000})
		if err != nil {
			return err
		}
		l.mu.Lock()
		l.operations[operationID] = tracked
		if op := l.gameOperation; op != nil && op.ID == operationID {
			op.Status = "running"
		}
		l.mu.Unlock()
		l.emitSnapshot()
		result := tracked.Wait()
		l.mu.Lock()
		delete(l.operations, operationID)
		l.mu.Unlock()
		installed := game.InstalledVCRuntimeVersion(prefix)
		if installed == "" || !game.VersionAtLeast(installed, game.VCRedistVersion) {
			found := installed
			if found == "" {
				found = "none"
			}
			return fmt.Errorf("Microsoft VC++ installer exited with code %d, but required version %s was not registered (found %s).", result.Code, game.VCRedistVersion, found)
		}
		addLine(fmt.Sprintf("Microsoft VC++ %s installed in this game’s Proton prefix.", installed))
	}
	finish("complete", "VC++ is ready. Press Play.", "")
	return nil
}

func (l *Launcher) cancelGame() error {
	l.mu.Lock()
	op := l.gameOperation
	if op == nil || !activeGame(op.Status) {
		l.mu.Unlock()
		return errors.New("No game launch is currently active.")
	}
	child, process, cancel := l.operations[op.ID], l.gameProcesses[op.ID], l.preparations[op.ID]
	if child == nil && cancel == nil {
		l.mu.Unlock()
		return errors.New("Xodus is still preparing; try stopping again in a moment.")
	}
	if child != nil && op.Kind == "prerequisite" {
		l.mu.Unlock()
		return errors.New("VC++ installation is running. Wait for it to finish; interrupting it could damage the Proton prefix.")
	}
	op.Status = "stopping"
	op.Phase = "Cancelling prerequisite download…"
	if child != nil {
		op.Phase = "Stopping Xodus and Wine…"
	}
	operationID := op.ID
	l.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	switch {
	case child != nil && process != nil:
		if err := process.Stop(); err != nil {
			l.mu.Lock()
			if l.gameOperation != nil && l.gameOperation.ID == operationID {
				l.gameOperation.Status = "running"
			}
			l.mu.Unlock()
			l.emitSnapshot()
			return err
		}
		time.AfterFunc(8*time.Second, func() {
			l.mu.Lock()
			current := l.gameProcesses[operationID]
			l.mu.Unlock()
			if current == process {
				if err := process.ForceStop(); err != nil {
					log.Print("Could not force-stop game: ", err)
				}
			}
		})
	case child != nil:
		child.Signal(sigint)
	}
	l.emitSnapshot()
	return nil
}

func (l *Launcher) removeGame(id, mode string) error {
	if mode != "library" && mode != "delete-files" {
		return errors.New("Invalid removal option.")
	}
	if err := l.ensureIdle("remove"); err != nil {
		return err
	}
	entry, ok := l.store.Game(id)
	if !ok {
		return errors.New("Game not found.")
	}
	l.mu.Lock()
	l.gameRemoval = true
	operation := shared.Clone(l.gameOperation)
	l.mu.Unlock()
	defer func() {
		l.mu.Lock()
		l.gameRemoval = false
		l.mu.Unlock()
	}()
	var running *shared.Game
	if operation != nil && activeGame(operation.Status) {
		playing, ok := l.store.Game(operation.GameID)
		if !ok {
			return errors.New("Could not verify the active game. Close it before removing a game.")
		}
		running = &playing
	}
	if err := install.AssertGameRemovalAvailable(entry, l.store.Downloads(), func(task shared.DownloadTask) bool {
		l.mu.Lock()
		_, active := l.operations[task.ID]
		l.mu.Unlock()
		return active || l.queue.Has(task.ID)
	}, running); err != nil {
		return err
	}
	if mode == "delete-files" {
		home, _ := os.UserHomeDir()
		settings := l.store.Settings()
		self, _ := os.Executable()
		protected := []string{}
		for _, path := range []string{"/boot", "/dev", "/etc", "/proc", "/run", "/sys", "/usr", "/var", home, l.userData,
			settings.InstallRoot, settings.RuntimeRoot, settings.ExternalXodusRoot, filepath.Dir(self)} {
			if path != "" && path != "." {
				protected = append(protected, path)
			}
		}
		others := slices.DeleteFunc(l.store.Games(), func(other shared.Game) bool { return other.ID == id })
		target, err := install.ValidateGameDeleteTarget(entry, others, protected)
		if err != nil {
			return err
		}
		if err := install.DeleteGameDirectory(target, nil); err != nil {
			return err
		}
	}
	if err := l.store.RemoveGame(id); err != nil {
		return err
	}
	if err := l.shortcuts.Remove(id); err != nil {
		log.Print("Could not remove game shortcuts: ", err)
	}
	l.emitSnapshot()
	return nil
}

func shortcutGame(entry shared.Game) shortcuts.Game {
	return shortcuts.Game{ID: entry.ID, Title: entry.Title, InstallState: entry.InstallState}
}

func (l *Launcher) addToSteam(id *string) (string, error) {
	var entry *shared.Game
	if id != nil {
		found, ok := l.store.Game(*id)
		if !ok || found.InstallState != "ready" {
			return "", errors.New("Only installed, ready games can be added to Steam.")
		}
		entry = &found
	}
	home, _ := os.UserHomeDir()
	accounts, err := steam.Accounts(home)
	if err != nil {
		return "", err
	}
	if len(accounts) == 0 {
		return "", errors.New("No Steam profile found. Open Steam, sign in, then try again.")
	}
	accounts = slices.DeleteFunc(accounts, func(account string) bool { return strings.Contains(account, "/.var/app/com.valvesoftware.Steam/") })
	if len(accounts) == 0 {
		return "", errors.New("Adding shortcuts currently requires native Linux Steam. Flatpak Steam is not yet supported.")
	}
	normalize := func(data []byte) []byte {
		result := imageutil.Fit(data, 1200)
		if len(result) > 4*1024*1024 {
			return nil
		}
		return result
	}
	var cover []byte
	var launchGame *shortcuts.Game
	if entry != nil {
		cover = shortcuts.LoadIcon("", entry.CoverURL, normalize, nil)
		if cover == nil {
			cover = shortcuts.LoadIcon(entry.CoverDataURL, "", normalize, nil)
		}
		value := shortcutGame(*entry)
		launchGame = &value
	}
	launch, desktop, err := l.shortcuts.SteamDesktop(launchGame, cover)
	if err != nil {
		return "", err
	}
	fallback, err := os.ReadFile(launch.Icon)
	if err != nil {
		return "", err
	}
	var artwork steam.Artwork
	if entry != nil {
		var wide []byte
		if entry.HeroURL != "" {
			wide = shortcuts.LoadIcon("", entry.HeroURL, normalize, nil)
		}
		source := cover
		if source == nil {
			source = fallback
		}
		portrait, err := steam.GamePortrait(source)
		if err != nil {
			portrait = source
		}
		if wide == nil {
			wide = source
		}
		artwork = steam.Artwork{Cover: portrait, Wide: wide}
	} else if nexus, err := steam.NexusArtwork(); err == nil {
		artwork = nexus
	} else {
		artwork = steam.Artwork{Cover: fallback, Wide: fallback}
	}
	if steam.Running() {
		return steam.AddLiveShortcut(accounts, launch, desktop, artwork, nil, 15*time.Second, func(existing []string) (string, error) {
			buttons := make([]string, 0, len(existing)+2)
			for _, account := range existing {
				buttons = append(buttons, "Use Steam profile "+filepath.Base(filepath.Dir(account)))
			}
			buttons = append(buttons, "Add for signed-in account", "Cancel")
			choice := l.host.Ask("Steam shortcut already exists", "Reuse an existing shortcut, or ask Steam to add it for the currently signed-in account?",
				"Choose an existing profile to avoid creating a duplicate in that account.", buttons, len(existing)+1)
			if choice < 0 || choice > len(existing) {
				return "", errors.New("Add to Steam cancelled.")
			}
			if choice == len(existing) {
				return "", nil
			}
			return existing[choice], nil
		})
	}
	selected := 0
	if len(accounts) > 1 {
		buttons := make([]string, 0, len(accounts)+1)
		for _, account := range accounts {
			buttons = append(buttons, "Steam profile "+filepath.Base(filepath.Dir(account)))
		}
		buttons = append(buttons, "Cancel")
		selected = l.host.Ask("Choose Steam profile", "Which Steam profile should receive this shortcut?", "", buttons, len(accounts))
		if selected < 0 || selected >= len(accounts) {
			return "", errors.New("Add to Steam cancelled.")
		}
	}
	return steam.AddShortcut(accounts[selected], launch, artwork, steam.Running)
}

func (l *Launcher) createGameShortcut(id, destination string) (string, error) {
	entry, ok := l.store.Game(id)
	if !ok {
		return "", errors.New("Game not found.")
	}
	if destination != "desktop" && destination != "applications" {
		return "", errors.New("Invalid shortcut destination.")
	}
	png := shortcuts.LoadIcon(entry.CoverDataURL, entry.CoverURL, func(data []byte) []byte { return imageutil.Fit(data, 256) }, nil)
	return l.shortcuts.Create(shortcutGame(entry), destination, png)
}
