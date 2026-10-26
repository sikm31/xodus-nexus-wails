package launcher

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"xodus-nexus-wails/internal/backend"
	"xodus-nexus-wails/internal/content"
	"xodus-nexus-wails/internal/install"
	"xodus-nexus-wails/internal/proc"
	"xodus-nexus-wails/internal/service"
	"xodus-nexus-wails/internal/shared"
	"xodus-nexus-wails/internal/state"
)

type contentInstall struct {
	game    shared.Game
	pkg     content.Package
	manager *content.Manager
}

var storeProductID = regexp.MustCompile(`^[A-Z0-9]{12}$`)

func (l *Launcher) startInstall(request shared.InstallRequest, update *shared.Game, resumed *shared.DownloadTask) (string, error) {
	mode := "install"
	if update != nil {
		mode = ""
	}
	if err := l.ensureIdle(mode); err != nil {
		return "", err
	}
	l.mu.Lock()
	l.preparingInstall = true
	l.mu.Unlock()
	defer func() {
		l.mu.Lock()
		l.preparingInstall = false
		l.mu.Unlock()
	}()
	if err := l.prepareSystemWallet(); err != nil {
		return "", err
	}
	return l.prepareInstall(request, update, nil, resumed)
}

func (l *Launcher) content() *content.Manager {
	root := filepath.Dir(l.store.Settings().RuntimeRoot)
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.contentManager == nil || l.contentRoot != root {
		l.contentManager, l.contentRoot = content.NewManager(root, nil), root
	}
	return l.contentManager
}

func (l *Launcher) accountIdentity() string {
	return l.storedAccount() + ":" + l.backend.Current().BinaryPath + ":" + l.currentMarket()
}

func (l *Launcher) gameContent(id string) (shared.GameContentPlan, []content.Package, error) {
	entry, ok := l.store.Game(id)
	if !ok {
		return shared.GameContentPlan{}, nil, errors.New("Game not found.")
	}
	l.mu.Lock()
	running := l.auth.Running
	l.mu.Unlock()
	user := l.storedAccount()
	if running || user == "" {
		return shared.GameContentPlan{}, nil, errors.New("Sign in before checking game content.")
	}
	identity := l.accountIdentity()
	status := l.backend.Current()
	libraryBinary := ""
	if status.Capabilities.Library {
		libraryBinary = status.BinaryPath
	}
	l.library.Select(user, libraryBinary, l.currentMarket())
	plan, packages, err := l.content().Plan(context.Background(), entry, func() (string, content.Access, error) {
		result, err := l.library.Access(true)
		return result.CheckedAt, result.AccessByProductID, err
	}, l.currentMarket())
	if err != nil {
		return plan, nil, err
	}
	wine := l.selectedWine()
	plan.RuntimeSupported = boolPointer(wine != "" && service.StoreContentRuntime(wine))
	l.mu.Lock()
	running = l.auth.Running
	l.mu.Unlock()
	if running || l.storedAccount() == "" || identity != l.accountIdentity() {
		return plan, nil, errors.New("Account changed. Check content again.")
	}
	return plan, packages, nil
}

func (l *Launcher) installGameContent(id, productID string) (string, error) {
	if err := l.ensureIdle(""); err != nil {
		return "", err
	}
	l.mu.Lock()
	l.preparingContent = true
	l.mu.Unlock()
	defer func() {
		l.mu.Lock()
		l.preparingContent, l.preparingInstall = false, false
		l.mu.Unlock()
	}()
	return l.prepareContentInstall(id, productID, "")
}

func (l *Launcher) prepareContentInstall(id, productID, selectedContentID string) (string, error) {
	manager := l.content()
	plan, packages, err := l.gameContent(id)
	if err != nil {
		return "", err
	}
	if l.closing() {
		return "", errors.New("The launcher is closing.")
	}
	if plan.RuntimeSupported == nil || !*plan.RuntimeSupported {
		return "", errors.New("Update the Nexus runtime in Settings to install and use related content.")
	}
	entry, found := l.store.Game(id)
	itemIndex := slices.IndexFunc(plan.Items, func(item shared.GameContentItem) bool { return item.ProductID == productID })
	packageIndex := slices.IndexFunc(packages, func(pkg content.Package) bool { return pkg.ProductID == productID })
	if !found || itemIndex < 0 || packageIndex < 0 {
		return "", errors.New("No supported content package with a verified license or active source offer was found.")
	}
	item, pkg := plan.Items[itemIndex], packages[packageIndex]
	if (item.Access == "" && item.Status != "verify") || !slices.Contains([]string{"available", "installed", "verify"}, item.Status) {
		return "", errors.New("No supported content package with a verified license or active source offer was found.")
	}
	if selectedContentID != "" && pkg.ContentID != selectedContentID {
		return "", errors.New("The add-on package changed. Check content again.")
	}
	// Downloads use the existing genuine license-checked backend, not catalog URLs.
	if _, err := l.requireBackend("streaming"); err != nil {
		return "", err
	}
	if backend.ExternalDownloadRunning() {
		return "", errors.New("Another Xodus download is running.")
	}
	destination, err := manager.Prepare(context.Background(), entry, pkg, l.currentMarket())
	if err != nil {
		return "", err
	}
	l.mu.Lock()
	l.preparingInstall = true
	l.mu.Unlock()
	return l.prepareInstall(shared.InstallRequest{ProductID: productID, Title: entry.Title + ": " + item.Title, Destination: destination},
		nil, &contentInstall{game: entry, pkg: pkg, manager: manager}, nil)
}

func (l *Launcher) prepareInstall(request shared.InstallRequest, update *shared.Game, contentJob *contentInstall, resumed *shared.DownloadTask) (string, error) {
	binary, err := l.requireBackend("streaming")
	if err != nil {
		return "", err
	}
	productID, err := shared.SafeProductID(request.ProductID)
	if err != nil {
		return "", err
	}
	market := l.currentMarket()
	selectedContentID := ""
	if contentJob == nil {
		capabilities := l.backend.Current().Capabilities
		if !capabilities.PackageSelection {
			return "", errors.New("Update the Xodus backend in Settings before installing games.")
		}
		if request.SelectionMarket != "" && request.SelectionMarket != market {
			return "", errors.New("Store market changed. Choose the installation package again.")
		}
		if request.SelectedContentID != "" && request.SelectionMarket != market {
			return "", errors.New("Invalid installation package selection.")
		}
		plan, err := l.plans.Get(context.Background(), binary, productID, market, capabilities.ContentPackageSelection)
		if err != nil {
			return "", err
		}
		var requested *string
		if request.SelectedContentID != "" {
			requested = &request.SelectedContentID
		}
		if selectedContentID, err = install.InstallSelection(plan, requested); err != nil {
			return "", err
		}
		if binary != l.backend.Current().BinaryPath || market != l.currentMarket() {
			return "", errors.New("Backend or Store market changed. Try installing again.")
		}
		choice := plan.Choices[slices.IndexFunc(plan.Choices, func(choice shared.InstallChoice) bool {
			return choice.Supported && choice.ContentID == selectedContentID
		})]
		if update != nil && choice.Kind == "Durable" {
			return "", errors.New("An add-on cannot replace the base game. Use Manage content.")
		}
		parent, err := install.ContentParentForChoice(choice, l.store.Games())
		if err != nil {
			return "", err
		}
		if parent != "" {
			return l.prepareContentInstall(parent, choice.ProductID, selectedContentID)
		}
	}
	var activeTasks []shared.DownloadTask
	for _, task := range l.store.Downloads() {
		if activeDownload(task.Status) {
			activeTasks = append(activeTasks, task)
		}
	}
	if len(activeTasks) >= 100 {
		return "", errors.New("The install queue is full. Cancel an installation before adding another.")
	}
	if slices.ContainsFunc(activeTasks, func(task shared.DownloadTask) bool { return task.ProductID == productID }) {
		return "", errors.New("This game is already in the install queue.")
	}
	otherGames := func() []string {
		var paths []string
		for _, entry := range l.store.Games() {
			if (update == nil || entry.ID != update.ID) && (contentJob == nil || entry.ID != contentJob.game.ID) {
				paths = append(paths, entry.Path)
			}
		}
		return paths
	}
	locked := otherGames()
	for _, task := range activeTasks {
		locked = append(locked, task.Destination)
	}
	locked = append(locked, l.userData, l.store.Settings().RuntimeRoot)
	destination, err := install.AssertInstallDestination(strings.TrimSpace(request.Destination), locked)
	if err != nil {
		return "", err
	}
	if update != nil {
		if err := assertUpdateTarget(destination, *update); err != nil {
			return "", err
		}
	}
	id := install.NewUUID()
	title := strings.TrimSpace(request.Title)
	if title == "" {
		title = productID
	}
	task := shared.DownloadTask{ID: id, Kind: "install", Title: title, ProductID: productID, SelectedContentID: selectedContentID,
		Destination: destination, Status: "queued", Phase: "Waiting in install queue", Log: []string{}, StartedAt: now()}
	switch {
	case contentJob != nil:
		task.Kind, task.GameID = "content", contentJob.game.ID
	case update != nil:
		task.Kind, task.GameID = "update", update.ID
	}
	if selectedContentID != "" {
		task.SelectionMarket = market
	}
	if resumed != nil {
		l.cleanup.InheritResume(*resumed, task)
	}
	if err := l.store.AddDownload(task); err != nil {
		return "", err
	}
	go l.enrichDownloadCovers([]shared.DownloadTask{task}, market)
	identity := l.storedAccount() + ":" + binary + ":" + market
	cancelled := func() bool {
		current, ok := l.store.Download(id)
		return !ok || current.Status == "cancelled"
	}
	err = l.queue.Enqueue(install.Job{ID: id, Run: func() error {
		if l.closing() {
			return errors.New("The launcher is closing.")
		}
		if identity != l.accountIdentity() {
			return errors.New("Account, backend or Store market changed. Retry the installation.")
		}
		if backend.ExternalDownloadRunning() {
			return errors.New("Another Xodus download is running outside this queue.")
		}
		if _, err := install.AssertInstallDestination(destination, otherGames()); err != nil {
			return err
		}
		l.mu.Lock()
		operation := shared.Clone(l.gameOperation)
		l.mu.Unlock()
		if operation != nil && activeGame(operation.Status) {
			if playing, ok := l.store.Game(operation.GameID); ok {
				playingPath, err := install.CanonicalInstallPath(playing.Path)
				if (contentJob != nil && playing.ID == contentJob.game.ID) || (err == nil && install.PathsOverlap(destination, playingPath)) {
					return errors.New("Close this game before changing its installed files.")
				}
			}
		}
		if l.closing() || cancelled() {
			return nil
		}
		current, _ := l.store.Download(id)
		if err := l.cleanup.Prepare(current); err != nil {
			return err
		}
		if l.closing() || cancelled() {
			return nil
		}
		phase := "Starting Xodus streaming…"
		if update != nil {
			phase = "Checking installed files with Xodus…"
		}
		l.store.UpdateDownload(id, state.Patch{"status": "running", "phase": phase})
		if l.closing() || cancelled() {
			return nil
		}
		return l.runDownload(id, binary, market, update, contentJob)
	}, Failed: func(err error) error {
		if cancelled() {
			return nil
		}
		l.store.UpdateDownload(id, state.Patch{"status": "failed", "error": err.Error(), "phase": "Installation could not be started", "finishedAt": now()})
		l.emitSnapshot()
		return nil
	}})
	if err != nil {
		return "", err
	}
	l.emitSnapshot()
	return id, nil
}

var gameProcessPattern = regexp.MustCompile(`xodus-cli\s+run\s`)
var wineProcessPattern = regexp.MustCompile(`wine|proton`)

// assertUpdateTarget refuses in-place updates while the game may be running.
func assertUpdateTarget(destination string, update shared.Game) error {
	actual, err := filepath.EvalSymlinks(destination)
	if err != nil {
		return err
	}
	installed, err := filepath.EvalSymlinks(update.Path)
	if err != nil || actual != installed {
		return errors.New("Update destination differs from the installed game.")
	}
	if backend.ExternalDownloadRunning() {
		return errors.New("Another Xodus download is running. Wait before updating this game.")
	}
	processes, err := proc.Run("ps", []string{"-eo", "args="}, proc.RunOptions{MaxOutput: 2_000_000})
	if err != nil || processes.Code != 0 {
		return errors.New("Could not verify that the game is closed. Update was not started.")
	}
	executable := ""
	if update.Executable != "" {
		executable = strings.ToLower(filepath.Base(update.Executable))
	}
	for _, line := range strings.Split(processes.Output, "\n") {
		command := strings.ToLower(line)
		if gameProcessPattern.MatchString(command) || wineProcessPattern.MatchString(command) &&
			(strings.Contains(command, strings.ToLower(actual)) || executable != "" && strings.Contains(command, executable)) {
			return errors.New("A game process is active. Close the game before updating files in place.")
		}
	}
	return nil
}

type pendingControl struct {
	paused bool
	done   chan error
	timer  *time.Timer
}

func (l *Launcher) runDownload(id, binary, market string, update *shared.Game, contentJob *contentInstall) error {
	task, _ := l.store.Download(id)
	capabilities := l.backend.Current().Capabilities
	structured := capabilities.StreamingProgressJSON
	controllable := structured && capabilities.StreamingControls
	if !structured {
		l.store.MutateDownload(id, func(task *shared.DownloadTask) {
			task.Log = append(task.Log, "Update the Nexus backend in Settings for numeric progress and Pause/Resume. This download can continue without them.")
		})
	}
	l.emitSnapshot()
	productID := task.ProductID
	if contentJob != nil {
		productID = contentJob.pkg.ContentID
	}
	settings := l.store.Settings()
	args := []string{"streaming", productID, task.Destination, "--parallel", strconv.Itoa(settings.ParallelDownloads), "--market", market}
	if task.SelectedContentID != "" {
		args = append(args, "--selected-content-id", task.SelectedContentID)
	}
	if structured {
		args = append(args, "--progress-json")
	}
	if controllable {
		args = append(args, "--control-stdin")
	}
	var limit any
	if settings.DownloadLimitMiB != nil {
		limit = *settings.DownloadLimitMiB
	}
	limitArgs, err := shared.DownloadLimitArgs(limit, capabilities)
	if err != nil {
		return err
	}
	args = append(args, limitArgs...)
	tracker := install.NewProgress()
	lastLogged := -1
	var pending *pendingControl
	if l.closing() {
		return errors.New("The launcher is closing.")
	}
	onLine := func(line string) {
		progress, ok := install.ProgressPatch{}, false
		if structured {
			progress, ok = tracker.Update(line)
		}
		if !ok && strings.HasPrefix(line, install.ProgressPrefix) {
			return // Malformed/future records aren't user logs.
		}
		l.mu.Lock()
		l.store.MutateDownload(id, func(task *shared.DownloadTask) {
			if ok {
				task.Progress, task.TotalBytes, task.TransferRate = progress.Progress, progress.TotalBytes, progress.TransferRate
				downloaded := progress.DownloadedBytes
				task.DownloadedBytes, task.Phase, task.Status = &downloaded, progress.Phase, progress.Status
				task.CanPause, task.ControlPending = controllable && progress.CanPause, false
				if pending != nil {
					task.ControlPending = true
					if (task.Status == "paused") == pending.paused || !task.CanPause {
						pending.timer.Stop()
						pending.done <- nil
						pending = nil
						task.ControlPending = false
					}
				}
				percent := 0
				if task.Progress != nil {
					percent = int(math.Floor(*task.Progress))
				}
				if percent != lastLogged {
					if task.Progress == nil {
						task.Log = append(task.Log, task.Phase)
					} else {
						task.Log = append(task.Log, fmt.Sprintf("%d%% · %s", percent, task.Phase))
					}
					lastLogged = percent
				}
			} else {
				if !structured {
					task.Phase = line
					if shared.UTF16Len(line) > 160 {
						task.Phase = shared.TruncateUTF16(line, 157) + "…"
					}
				}
				task.Log = append(task.Log, line)
			}
			if len(task.Log) > 300 {
				task.Log = task.Log[len(task.Log)-300:]
			}
		})
		l.mu.Unlock()
		l.scheduleSnapshot()
	}
	tracked, spawnErr := proc.Spawn(binary, args, proc.RunOptions{OnLine: onLine, MaxOutput: 8_000_000})
	finishControls := func() {
		l.limits.Detach(id)
		l.mu.Lock()
		delete(l.downloadControls, id)
		if pending != nil {
			pending.timer.Stop()
			pending.done <- errors.New("The download has ended.")
			pending = nil
		}
		l.mu.Unlock()
		l.store.MutateDownload(id, func(task *shared.DownloadTask) { task.CanPause, task.ControlPending = false, false })
	}
	if spawnErr != nil {
		finishControls()
		l.mu.Lock()
		wasCancelled := l.cancelledDownloads[id]
		delete(l.cancelledDownloads, id)
		l.mu.Unlock()
		patch := state.Patch{"status": "failed", "error": spawnErr.Error(), "phase": "The process could not be started", "finishedAt": now()}
		if wasCancelled {
			patch = state.Patch{"status": "cancelled", "error": nil, "phase": "Cancelled by user", "finishedAt": now()}
		}
		l.store.UpdateDownload(id, patch)
		l.emitSnapshot()
		l.finishShortcutSession()
		return nil
	}
	l.mu.Lock()
	l.operations[id] = tracked
	l.mu.Unlock()
	if capabilities.StreamingBandwidthLimit {
		l.limits.Attach(id, tracked.Stdin)
	}
	if controllable {
		l.mu.Lock()
		l.downloadControls[id] = func(paused bool) error {
			l.mu.Lock()
			current, _ := l.store.Download(id)
			if pending != nil || !current.CanPause || (current.Status != "running" && current.Status != "paused") {
				l.mu.Unlock()
				return errors.New("This download cannot be paused or resumed right now.")
			}
			if (current.Status == "paused") == paused {
				l.mu.Unlock()
				return nil
			}
			l.store.MutateDownload(id, func(task *shared.DownloadTask) { task.ControlPending = true })
			control := &pendingControl{paused: paused, done: make(chan error, 1)}
			control.timer = time.AfterFunc(10*time.Second, func() {
				l.mu.Lock()
				if pending == control {
					pending = nil
					l.store.MutateDownload(id, func(task *shared.DownloadTask) { task.ControlPending = false })
					control.done <- errors.New("The backend did not acknowledge the download control.")
				}
				l.mu.Unlock()
				l.emitSnapshot()
			})
			pending = control
			command := "resume"
			if paused {
				command = "pause"
			}
			message, _ := json.Marshal(map[string]any{"schema": 1, "command": command})
			_, writeErr := tracked.Stdin.Write(append(message, '\n'))
			if writeErr != nil {
				// Broken stdin is an operation error, not a launcher crash.
				control.timer.Stop()
				pending = nil
				l.store.MutateDownload(id, func(task *shared.DownloadTask) { task.ControlPending = false })
				l.mu.Unlock()
				l.scheduleSnapshot()
				return writeErr
			}
			l.mu.Unlock()
			l.emitSnapshot()
			return <-control.done
		}
		l.mu.Unlock()
	}
	result := tracked.Wait()
	finishControls()
	l.mu.Lock()
	wasCancelled := l.cancelledDownloads[id]
	delete(l.cancelledDownloads, id)
	shutdown := l.shutdownStarted
	l.mu.Unlock()
	task, _ = l.store.MutateDownload(id, func(task *shared.DownloadTask) {
		code := result.Code
		task.ExitCode, task.FinishedAt = &code, now()
		switch {
		case wasCancelled || code == 130:
			task.Status = "cancelled"
		case code == 0 && (!structured || tracker.Terminal == "complete"):
			task.Status = "complete"
		default:
			task.Status = "failed"
		}
		if task.Status == "cancelled" {
			task.Error = ""
		}
		if shutdown && controllable && !wasCancelled && task.Status != "complete" {
			task.Status = "paused"
		}
		switch {
		case task.Status == "complete":
			hundred := 100.0
			task.Progress = &hundred
			task.Phase = "Installation complete"
			if update != nil {
				task.Phase = "Update complete"
			}
		case task.Status == "cancelled":
			task.Phase = "Cancelled by user"
		case structured && code == 0:
			task.Phase = "The backend did not confirm a completed installation."
		default:
			task.Phase = fmt.Sprintf("Xodus exited with code %d", code)
		}
		if task.Status == "failed" {
			task.Error = shared.DownloadFailure(task.Log, code)
		}
		if task.Status == "paused" {
			task.Phase = "Paused — resume to restore saved download checkpoints"
			task.CanPause, task.Error, task.FinishedAt = true, "", ""
		}
	})
	if task.Status == "complete" && contentJob != nil {
		if err := contentJob.manager.Complete(contentJob.game, contentJob.pkg); err != nil {
			task, _ = l.store.MutateDownload(id, func(task *shared.DownloadTask) {
				task.Status, task.Error, task.Phase = "failed", err.Error(), "Content metadata validation failed. Files were kept for repair."
			})
		}
	}
	l.store.Save()
	if task.Status == "complete" && contentJob == nil {
		if inspected, err := state.InspectGameDirectory(task.Destination); err == nil {
			if update != nil {
				patch := state.Fields(inspected, "installState", "installMessage", "antiCheat", "vcRedistEncrypted", "coverDataUrl", "installedVersion", "packageName")
				patch["storeId"] = update.StoreID
				if inspected.StoreID != "" {
					patch["storeId"] = inspected.StoreID
				}
				l.store.PatchGame(update.ID, patch)
				l.mu.Lock()
				delete(l.gameUpdates, update.ID)
				l.mu.Unlock()
			} else {
				if storeProductID.MatchString(task.ProductID) {
					inspected.OfferProductID = task.ProductID
				}
				l.store.UpsertGame(inspected)
			}
		}
	}
	if task.Status == "complete" {
		l.cleanup.Complete(task)
	}
	l.mu.Lock()
	delete(l.operations, id)
	l.mu.Unlock()
	l.emitSnapshot()
	l.notifications.Complete(task)
	if task.Status == "complete" && contentJob == nil {
		go func() {
			if l.enrichLibraryCovers() == nil {
				l.emitSnapshot()
			}
		}()
	}
	l.finishShortcutSession()
	return nil
}

func (l *Launcher) cleanupProtection(task shared.DownloadTask) (locked, roots []string) {
	for _, entry := range l.store.Games() {
		locked = append(locked, entry.Path)
	}
	for _, other := range l.store.Downloads() {
		if other.ID != task.ID && (activeDownload(other.Status) || other.Status == "complete") {
			locked = append(locked, other.Destination)
		}
	}
	settings := l.store.Settings()
	for _, path := range []string{"/boot", "/dev", "/etc", "/proc", "/run", "/sys", "/usr", "/var", l.userData, settings.RuntimeRoot, settings.ExternalXodusRoot} {
		if path != "" {
			locked = append(locked, path)
		}
	}
	home, _ := os.UserHomeDir()
	for _, path := range []string{"/", "/tmp", home, settings.InstallRoot} {
		if path != "" {
			roots = append(roots, path)
		}
	}
	return locked, roots
}

func (l *Launcher) cancellableDownload(id string) (shared.DownloadTask, error) {
	task, ok := l.store.Download(id)
	if !ok || !activeDownload(task.Status) {
		return task, errors.New("The download is no longer running.")
	}
	return task, nil
}

func (l *Launcher) cancelDownload(id, mode string) error {
	if mode == "" {
		mode = "keep"
	}
	if mode != "keep" && mode != "delete-files" {
		return errors.New("Invalid cancellation option.")
	}
	l.mu.Lock()
	if _, ok := l.downloadCancellations[id]; ok {
		l.mu.Unlock()
		return errors.New("This download is already being cancelled.")
	}
	if l.shutdownStarted || l.preparingInstall || l.preparingContent || l.gameRemoval {
		l.mu.Unlock()
		return errors.New("Wait for the current operation to finish.")
	}
	l.mu.Unlock()
	task, err := l.cancellableDownload(id)
	if err != nil {
		return err
	}
	release := l.queue.Hold()
	done := make(chan struct{})
	l.mu.Lock()
	l.downloadCancellations[id] = done
	l.mu.Unlock()
	l.emitSnapshot()
	defer func() {
		l.mu.Lock()
		delete(l.downloadCancellations, id)
		l.mu.Unlock()
		close(done)
		release()
		l.emitSnapshot()
	}()
	stopped := false
	err = func() error {
		if mode == "delete-files" {
			locked, roots := l.cleanupProtection(task)
			if options := l.cleanup.Options(task, locked, roots); !options.DeleteFilesAllowed {
				return errors.New(options.Reason)
			}
		}
		// Completion may have happened while checking the filesystem.
		if _, err := l.cancellableDownload(id); err != nil {
			return err
		}
		l.mu.Lock()
		child := l.operations[id]
		if child != nil {
			l.cancelledDownloads[id] = true
		}
		l.mu.Unlock()
		if child != nil {
			if err := child.Signal(sigint); err != nil {
				l.mu.Lock()
				delete(l.cancelledDownloads, id)
				l.mu.Unlock()
				return errors.New("The download could not be stopped. Files were not deleted.")
			}
			stopped = true
		} else {
			l.queue.Cancel(id)
			stopped = true
			l.store.UpdateDownload(id, state.Patch{"status": "cancelled", "canPause": false, "finishedAt": now()})
		}
		// Includes process exit, metadata persistence and the queue's cleanup.
		if err := l.queue.Settled(id, 0); err != nil {
			return err
		}
		l.mu.Lock()
		_, running := l.operations[id]
		l.mu.Unlock()
		if running || l.queue.Has(id) {
			return errors.New("The download is still stopping. Files were not deleted.")
		}
		if current, _ := l.store.Download(id); current.Status != "cancelled" {
			return errors.New("The installation state changed. Files were not deleted.")
		}
		if mode == "delete-files" {
			if backend.ExternalDownloadRunning() {
				return errors.New("Another Xodus download is running. Files were not deleted.")
			}
			current, _ := l.store.Download(id)
			locked, roots := l.cleanupProtection(current)
			if err := l.cleanup.Remove(current, locked, roots); err != nil {
				return err
			}
		}
		phase := "Cancelled; downloaded files and checkpoints were kept."
		if mode == "delete-files" {
			phase = "Cancelled; downloaded files were deleted."
		}
		return l.store.UpdateDownload(id, state.Patch{"error": nil, "phase": phase})
	}()
	if err != nil && stopped {
		l.store.UpdateDownload(id, state.Patch{"error": err.Error()})
	}
	return err
}

func (l *Launcher) resumeDownload(id string) error {
	l.mu.Lock()
	_, cancelling := l.downloadCancellations[id]
	control := l.downloadControls[id]
	_, running := l.operations[id]
	l.mu.Unlock()
	if cancelling {
		return errors.New("This download is being cancelled.")
	}
	if control != nil {
		return control(false)
	}
	if !l.backend.Current().Capabilities.StreamingControls {
		return errors.New("Update the Nexus backend in Settings before resuming a saved download.")
	}
	task, ok := l.store.Download(id)
	if !ok || task.Status != "paused" || running {
		return errors.New("This download cannot be resumed.")
	}
	// Remove only the stale queue row, never files/checkpoints. The new backend
	// process validates package identity and resumes durable byte checkpoints.
	resumed := shared.Clone(task)
	l.store.MutateDownload(id, func(task *shared.DownloadTask) { task.Status = "cancelled" })
	defer l.emitSnapshot()
	err := func() error {
		var update *shared.Game
		if task.Kind == "update" && task.GameID != "" {
			if entry, ok := l.store.Game(task.GameID); ok {
				update = &entry
			}
		}
		if task.Kind == "update" && update == nil {
			return errors.New("The installed game for this update is no longer in the library.")
		}
		if task.Kind == "content" {
			if task.GameID == "" {
				return errors.New("Content download has no parent game.")
			}
			if _, err := l.installGameContent(task.GameID, task.ProductID); err != nil {
				return err
			}
		} else if _, err := l.startInstall(shared.InstallRequest{ProductID: task.ProductID, Title: task.Title, Destination: task.Destination,
			SelectedContentID: task.SelectedContentID, SelectionMarket: task.SelectionMarket}, update, &resumed); err != nil {
			return err
		}
		return l.store.RemoveDownload(id)
	}()
	if err != nil {
		l.store.MutateDownload(id, func(task *shared.DownloadTask) { task.Status = "paused" })
	}
	return err
}

// enrichDownloadCovers fetches real covers even when an installation has a small logo.
func (l *Launcher) enrichDownloadCovers(tasks []shared.DownloadTask, market string) {
	var ids []string
	for _, task := range tasks {
		if task.CoverURL == "" && activeDownload(task.Status) && !slices.Contains(ids, task.ProductID) {
			ids = append(ids, task.ProductID)
		}
	}
	if len(ids) == 0 {
		return
	}
	posters := l.catalog.Posters(context.Background(), ids, market)
	changed := false
	for _, task := range tasks {
		current, ok := l.store.Download(task.ID)
		cover := posters[task.ProductID]
		if !ok || current.ProductID != task.ProductID || current.CoverURL != "" || cover == "" || task.CoverURL != "" || !activeDownload(task.Status) {
			continue
		}
		l.store.UpdateDownload(task.ID, state.Patch{"coverUrl": cover})
		changed = true
	}
	if changed {
		l.emitSnapshot()
	}
}

func (l *Launcher) enrichLibraryCovers() error {
	var missing []shared.Game
	var ids []string
	for _, entry := range l.store.Games() {
		if entry.StoreID != "" && (entry.CoverURL == "" || entry.HeroURL == "") {
			missing = append(missing, entry)
			ids = append(ids, entry.StoreID)
		}
	}
	if len(missing) == 0 {
		return nil
	}
	artwork := l.catalog.Artwork(context.Background(), ids, l.currentMarket())
	for _, entry := range missing {
		images, ok := artwork[strings.ToUpper(entry.StoreID)]
		if !ok {
			continue
		}
		patch := state.Patch{}
		if images.CoverURL != "" {
			patch["coverUrl"] = images.CoverURL
		}
		if images.HeroURL != "" {
			patch["heroUrl"] = images.HeroURL
		}
		if len(patch) > 0 {
			l.store.PatchGame(entry.ID, patch)
		}
	}
	return nil
}
