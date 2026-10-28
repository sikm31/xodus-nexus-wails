package launcher

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"xodus-nexus-wails/internal/install"
	"xodus-nexus-wails/internal/maxima"
	"xodus-nexus-wails/internal/proc"
	"xodus-nexus-wails/internal/shared"
	"xodus-nexus-wails/internal/state"
)

// EA games (EA Play titles without a Microsoft Store package) are installed
// and launched by Maxima. They share the download queue and the library with
// Xodus games but never run through the Xodus backend.

var titleNoise = regexp.MustCompile(`[^\p{L}\p{N}]+`)

// eaTitleKey folds Store and EA spellings of a title together:
// "Dead Space™" and "Dead Space" match, "Dead Space 2" does not.
func eaTitleKey(title string) string {
	return strings.Trim(titleNoise.ReplaceAllString(strings.ToLower(strings.NewReplacer("™", "", "®", "", "©", "").Replace(title)), " "), " ")
}

// editionSuffix is a trailing edition name on a title key: the Store sells
// "Titanfall® 2: Ultimate Edition" while the EA library lists "Titanfall 2".
var editionSuffix = regexp.MustCompile(`\s+(?:game of the year|goty|ultimate|deluxe|digital deluxe|complete|definitive|standard|gold|premium|legendary|enhanced|special|anniversary|remastered|champions)\s+edition$`)

// matchEATitle prefers an exact title, then the Store title without its
// edition suffix.
func matchEATitle(library []shared.EATitle, title string) (shared.EATitle, bool) {
	key := eaTitleKey(title)
	for _, candidate := range []string{key, editionSuffix.ReplaceAllString(key, "")} {
		if index := slices.IndexFunc(library, func(entry shared.EATitle) bool { return eaTitleKey(entry.Name) == candidate }); index >= 0 {
			return library[index], true
		}
	}
	return shared.EATitle{}, false
}

func (l *Launcher) maximaReady() error {
	status := l.maxima.Refresh()
	switch {
	case !status.Available || status.BuildRequired:
		return errors.New("Build Maxima in Settings → Maxima to install EA games.")
	case !status.SignedIn:
		return errors.New("Sign in to EA in Settings → Maxima to install EA games.")
	}
	return nil
}

// StartEAInstall queues an EA game for Maxima. The Store title is matched to
// the EA library, which already contains EA Play titles added to the account.
func (l *Launcher) StartEAInstall(request shared.InstallRequest) (string, error) {
	if l.options.Demo {
		return "", nil
	}
	if err := l.maximaReady(); err != nil {
		return "", err
	}
	library, err := l.maxima.Library(context.Background())
	if err != nil {
		return "", err
	}
	title, ok := matchEATitle(library, request.Title)
	if !ok {
		return "", fmt.Errorf("%s is not in your EA library yet. EA Play games appear there after they are added once to the account (Download in the EA app); then install it here.", request.Title)
	}
	productID, err := shared.SafeProductID(request.ProductID)
	if err != nil {
		return "", err
	}
	if title.Installed {
		// Installed by Maxima elsewhere (or before Nexus tracked it): add the
		// existing installation to the library instead of downloading again.
		if title.ExecutePath == "" {
			return "", fmt.Errorf("%s is already installed for this EA account, but Maxima did not report where.", title.Name)
		}
		l.addEAGame(title, productID, filepath.Dir(title.ExecutePath), "")
		l.emitSnapshot()
		return "", nil
	}
	if slices.ContainsFunc(l.store.Downloads(), func(task shared.DownloadTask) bool {
		return activeDownload(task.Status) && (task.ProductID == productID || task.OfferID == title.OfferID)
	}) {
		return "", errors.New("This game is already in the install queue.")
	}
	var locked []string
	for _, entry := range l.store.Games() {
		locked = append(locked, entry.Path)
	}
	for _, task := range l.store.Downloads() {
		if activeDownload(task.Status) {
			locked = append(locked, task.Destination)
		}
	}
	locked = append(locked, l.userData, l.store.Settings().RuntimeRoot)
	destination, err := install.AssertInstallDestination(strings.TrimSpace(request.Destination), locked)
	if err != nil {
		return "", err
	}
	id := install.NewUUID()
	task := shared.DownloadTask{ID: id, Kind: "install", Title: title.Name, ProductID: productID, Destination: destination,
		Provider: "maxima", OfferID: title.OfferID, Status: "queued", Phase: "Waiting in install queue", Log: []string{}, StartedAt: now()}
	if err := l.store.AddDownload(task); err != nil {
		return "", err
	}
	go l.enrichDownloadCovers([]shared.DownloadTask{task}, l.currentMarket())
	cancelled := func() bool {
		current, ok := l.store.Download(id)
		return !ok || current.Status == "cancelled"
	}
	err = l.queue.Enqueue(install.Job{ID: id, Run: func() error {
		if l.closing() || cancelled() {
			return nil
		}
		return l.runEAInstall(id, title, productID, destination)
	}, Failed: func(err error) error {
		if !cancelled() {
			l.store.UpdateDownload(id, state.Patch{"status": "failed", "error": err.Error(), "phase": "Installation could not be started", "finishedAt": now()})
			l.emitSnapshot()
		}
		return nil
	}})
	if err != nil {
		return "", err
	}
	l.emitSnapshot()
	return id, nil
}

// eaInstalled returns the EA library's installed titles, at most a minute
// old: the library page asks on every load and Maxima takes seconds.
func (l *Launcher) eaInstalled() ([]shared.EATitle, error) {
	l.mu.Lock()
	cached, at := l.eaLibraryCache, l.eaLibraryAt
	l.mu.Unlock()
	if cached != nil && time.Since(at) < time.Minute {
		return cached, nil
	}
	library, err := l.maxima.Library(context.Background())
	if err != nil {
		return nil, err
	}
	installed := []shared.EATitle{}
	for _, title := range library {
		if title.Installed && title.ExecutePath != "" {
			installed = append(installed, title)
		}
	}
	l.mu.Lock()
	l.eaLibraryCache, l.eaLibraryAt = installed, time.Now()
	l.mu.Unlock()
	return installed, nil
}

func (l *Launcher) hasEAGame(title shared.EATitle) bool {
	dir := filepath.Dir(title.ExecutePath)
	return slices.ContainsFunc(l.store.Games(), func(entry shared.Game) bool { return entry.OfferID == title.OfferID || entry.Path == dir })
}

// syncEAGames adds EA games that Maxima reports installed (for example
// installed before Nexus tracked them) to the library, matched to the
// account's Store products by title.
func (l *Launcher) syncEAGames(products []shared.CatalogProduct) {
	if l.maximaReady() != nil || !slices.ContainsFunc(products, func(p shared.CatalogProduct) bool { return p.Distributor == "ea-app" }) {
		return
	}
	installed, err := l.eaInstalled()
	if err != nil {
		return
	}
	added := false
	for _, product := range products {
		if product.Distributor != "ea-app" {
			continue
		}
		if title, ok := matchEATitle(installed, product.Title); ok && !l.hasEAGame(title) {
			l.addEAGame(title, product.ProductID, filepath.Dir(title.ExecutePath), product.PosterURL)
			added = true
		}
	}
	if added {
		l.emitSnapshot()
	}
}

// importEAGame adds an EA game folder (one with __Installer/installerdata.xml)
// that Maxima knows as installed at that path.
func (l *Launcher) importEAGame(directory string) (*shared.Game, error) {
	if err := l.maximaReady(); err != nil {
		return nil, fmt.Errorf("This is an EA game folder. %s", err.Error())
	}
	l.mu.Lock()
	l.eaLibraryCache = nil
	l.mu.Unlock()
	installed, err := l.eaInstalled()
	if err != nil {
		return nil, err
	}
	index := slices.IndexFunc(installed, func(title shared.EATitle) bool { return filepath.Dir(title.ExecutePath) == directory })
	if index < 0 {
		return nil, errors.New("This EA game folder is not an installation known to your EA account in Maxima. Install the game from the library instead.")
	}
	title := installed[index]
	if !l.hasEAGame(title) {
		l.addEAGame(title, "", directory, "")
		l.emitSnapshot()
	}
	for _, entry := range l.store.Games() {
		if entry.OfferID == title.OfferID {
			return &entry, nil
		}
	}
	return nil, errors.New("The EA game could not be added to the library.")
}

func (l *Launcher) addEAGame(title shared.EATitle, productID, path, cover string) {
	tools := shared.DefaultLaunchTools()
	if legacyEAOffer(title.OfferID) {
		tools.CPULimit = legacyCPULimit
	}
	_, err := l.store.UpsertGame(shared.Game{ID: install.NewUUID(), Title: title.Name, Path: path, StoreID: productID,
		Publisher: "Electronic Arts", Provider: "maxima", OfferID: title.OfferID, EASlug: title.Slug,
		CoverURL: cover, InstallState: "ready", LaunchTools: &tools, AddedAt: now()})
	if err == nil && cover == "" {
		go func() {
			if l.enrichLibraryCovers() == nil {
				l.emitSnapshot()
			}
		}()
	}
}

// Origin-era offers (OFB-…, as opposed to Origin.OFR.…) are older games, some
// of which crash on hosts with many cores; they start with a CPU limit that
// the game's launch tools can change.
const legacyCPULimit = 8

func legacyEAOffer(offerID string) bool { return strings.HasPrefix(offerID, "OFB-") }

// eaLaunchEnvironment applies the game's launch tools that work without the
// Xodus wrapper: the CPU limit and NAME=value launch options.
func eaLaunchEnvironment(entry shared.Game) (map[string]string, error) {
	var raw json.RawMessage
	if entry.LaunchTools != nil {
		raw, _ = json.Marshal(entry.LaunchTools)
	}
	tools, err := shared.NormalizeLaunchTools(raw)
	if err != nil {
		return nil, err
	}
	options, err := shared.ParseLaunchOptions(tools.LaunchOptions)
	if err != nil {
		return nil, err
	}
	env := options.Environment
	if topology := shared.CPUTopology(tools.CPULimit); topology != "" && env["WINE_CPU_TOPOLOGY"] == "" {
		env["WINE_CPU_TOPOLOGY"] = topology
	}
	return env, nil
}

func (l *Launcher) runEAInstall(id string, title shared.EATitle, productID, destination string) error {
	if err := os.MkdirAll(destination, 0o755); err != nil {
		return err
	}
	l.store.UpdateDownload(id, state.Patch{"status": "running", "phase": "Starting Maxima…"})
	l.emitSnapshot()
	var lastBytes int64
	lastAt := time.Now()
	err := l.maxima.Install(context.Background(), title.OfferID, destination, func(tracked *proc.Tracked) {
		l.mu.Lock()
		l.operations[id] = tracked
		l.mu.Unlock()
	}, func(progress maxima.Progress) {
		if progress.Event != "progress" {
			return
		}
		patch := state.Patch{"phase": "Downloading with Maxima…", "progress": progress.Percent,
			"downloadedBytes": float64(progress.Downloaded), "totalBytes": float64(progress.Total)}
		if elapsed := time.Since(lastAt).Seconds(); elapsed >= 1 && progress.Downloaded >= lastBytes {
			patch["transferRate"] = fmt.Sprintf("%.1f MB/s", float64(progress.Downloaded-lastBytes)/elapsed/1e6)
			lastBytes, lastAt = progress.Downloaded, time.Now()
		}
		l.store.UpdateDownload(id, patch)
		l.scheduleSnapshot()
	})
	l.mu.Lock()
	delete(l.operations, id)
	wasCancelled := l.cancelledDownloads[id]
	delete(l.cancelledDownloads, id)
	l.mu.Unlock()
	switch {
	case wasCancelled:
		l.store.UpdateDownload(id, state.Patch{"status": "cancelled", "error": nil, "phase": "Cancelled by user", "finishedAt": now()})
	case err != nil:
		l.store.UpdateDownload(id, state.Patch{"status": "failed", "error": err.Error(), "phase": "Maxima could not install the game", "finishedAt": now()})
	default:
		l.store.UpdateDownload(id, state.Patch{"status": "complete", "progress": 100.0, "phase": "Installation complete", "finishedAt": now()})
		task, _ := l.store.Download(id)
		l.addEAGame(title, productID, destination, task.CoverURL)
	}
	l.emitSnapshot()
	l.finishShortcutSession()
	return nil
}

// launchEAGame runs an installed EA game through Maxima, which acts as its EA
// client until the game exits. Stop sends Maxima an interrupt.
func (l *Launcher) launchEAGame(entry shared.Game) error {
	if err := l.maximaReady(); err != nil {
		return err
	}
	if entry.EASlug == "" {
		return errors.New("This EA game has no EA identifier. Reinstall it through Maxima.")
	}
	env, err := eaLaunchEnvironment(entry)
	if err != nil {
		return err
	}
	operationID := "game-" + entry.ID
	logPath := filepath.Join(filepath.Dir(l.store.Settings().RuntimeRoot), "logs", entry.ID+"-latest.log")
	if err := os.MkdirAll(filepath.Dir(logPath), 0o755); err != nil {
		return err
	}
	launchLog := openOperationLog(logPath)
	launchLog.write("Game: " + entry.Title + " (EA, through Maxima)")
	l.mu.Lock()
	l.gameOperation = &shared.GameOperation{ID: operationID, GameID: entry.ID, Title: entry.Title, Kind: "launch", Status: "preparing",
		Phase: "Starting Maxima… The first launch also installs its Proton runtime.", Log: []string{}, LogPath: logPath, StartedAt: now()}
	l.mu.Unlock()
	l.emitSnapshot()
	appendLog := func(line string) {
		launchLog.write(line)
		l.appendOperation(operationID, line)
	}
	if topology := env["WINE_CPU_TOPOLOGY"]; topology != "" {
		appendLog("CPU limit: " + topology)
	}
	tracked, err := l.maxima.Launch(context.Background(), entry.EASlug, env, appendLog)
	if err != nil {
		launchLog.write("Launch error: " + err.Error())
		launchLog.close()
		l.mu.Lock()
		l.gameOperation = nil
		l.mu.Unlock()
		l.emitSnapshot()
		return err
	}
	release := l.display.acquire()
	l.mu.Lock()
	l.operations[operationID] = tracked
	l.gameOperation.Status, l.gameOperation.Phase = "running", "Maxima is running the game…"
	l.mu.Unlock()
	l.emitSnapshot()
	go func() {
		result := tracked.Wait()
		release()
		appendLog(fmt.Sprintf("Maxima exited with code %d.", result.Code))
		launchLog.close()
		l.mu.Lock()
		delete(l.operations, operationID)
		if op := l.gameOperation; op != nil && op.ID == operationID {
			op.FinishedAt = now()
			if result.Code == 0 || op.Status == "stopping" {
				op.Status, op.Phase = "complete", "The game has exited."
			} else {
				op.Status = "failed"
				op.Error = fmt.Sprintf("Maxima exited with code %d. See the game log.", result.Code)
				op.Phase = op.Error
			}
		}
		l.mu.Unlock()
		l.emitSnapshot()
		l.finishShortcutSession()
	}()
	return nil
}
