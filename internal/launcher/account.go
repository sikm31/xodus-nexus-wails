package launcher

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"time"

	"xodus-nexus-wails/internal/account"
	"xodus-nexus-wails/internal/auth"
	"xodus-nexus-wails/internal/game"
	"xodus-nexus-wails/internal/install"
	"xodus-nexus-wails/internal/launcherenv"
	"xodus-nexus-wails/internal/proc"
	"xodus-nexus-wails/internal/service"
	"xodus-nexus-wails/internal/shared"
	"xodus-nexus-wails/internal/state"
)

type accountState struct {
	stored   *bool
	username string
	note     string
}

func boolPointer(value bool) *bool { return &value }

func (l *Launcher) accountRefreshKey() string {
	status := l.backend.Current()
	key, _ := json.Marshal([]any{status.Available, status.BinaryPath, status.Source, status.Capabilities, l.currentMarket()})
	return string(key)
}

func (l *Launcher) storedAccount() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.auth.Stored != nil && *l.auth.Stored {
		return l.auth.Username
	}
	return ""
}

func (l *Launcher) refreshAccount(emit, afterAuth bool) shared.AuthStatus {
	l.mu.Lock()
	if l.auth.Running && !afterAuth {
		defer l.mu.Unlock()
		return shared.Clone(l.auth)
	}
	l.mu.Unlock()
	status := l.backend.Current()
	key := l.accountRefreshKey()
	account.Run(&l.accountRefresh, key, func(ctx context.Context) (accountState, error) {
		switch {
		case !status.Available || status.BinaryPath == "":
			return accountState{note: "Account status is unavailable because the Xodus backend is offline."}, nil
		case !status.Capabilities.Account:
			if status.Source == "external" {
				return accountState{note: "This external Xodus build cannot report the stored account. Install the Nexus backend from Settings."}, nil
			}
			return accountState{note: "This managed backend predates read-only account status. Update it from Settings."}, nil
		}
		result, err := account.ReadAccount(ctx, status.BinaryPath)
		if err != nil || result.Code != 0 {
			return accountState{note: "Xodus could not read its system-keychain account record."}, nil
		}
		lines := strings.Split(result.Output, "\n")
		var line string
		for index := len(lines) - 1; index >= 0; index-- {
			if entry := strings.TrimSpace(lines[index]); strings.HasPrefix(entry, "{") {
				line = entry
				break
			}
		}
		var value struct {
			Stored   any `json:"stored"`
			Username any `json:"username"`
		}
		unreadable := accountState{note: "Xodus returned an unreadable account-status response."}
		if json.Unmarshal([]byte(line), &value) != nil {
			return unreadable, nil
		}
		username, _ := value.Username.(string)
		switch {
		case value.Stored == true && strings.TrimSpace(username) != "":
			note := "Credentials are stored by Xodus in the system keychain. This does not mean in-game Xbox platform sign-in is implemented."
			if strings.Contains(l.runtime.Current().Name, "-xgamesave-pr28-") {
				note = "Credentials are stored by Xodus. The experimental PR #28 runtime can try in-game Xbox sign-in and two-way cloud save sync; this account status alone does not verify either."
			}
			return accountState{stored: boolPointer(true), username: strings.TrimSpace(username), note: note}, nil
		case value.Stored == false:
			return accountState{stored: boolPointer(false), note: "No Microsoft account is stored by Xodus."}, nil
		}
		return unreadable, nil
	}, func(next accountState) error {
		l.mu.Lock()
		l.auth.Stored, l.auth.Username, l.auth.Note = next.stored, next.username, next.note
		l.authVersion++
		l.mu.Unlock()
		user := l.storedAccount()
		binary := func(supported bool) string {
			if supported {
				return status.BinaryPath
			}
			return ""
		}
		l.library.Select(user, binary(status.Capabilities.Library), l.currentMarket())
		l.activities.Select(user, binary(status.Capabilities.GameActivity))
		l.achievements.Select(user, binary(status.Capabilities.Achievements))
		l.profile.Select(user, binary(status.Capabilities.Profile), emit)
		return nil
	}, func() bool {
		l.mu.Lock()
		running := l.auth.Running
		l.mu.Unlock()
		return key == l.accountRefreshKey() && (!running || afterAuth)
	})
	if emit {
		l.emitSnapshot()
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return shared.Clone(l.auth)
}

var keyringBlocked = []string{"locked", "stopped", "disabled", "missing", "needs-setup"}

func (l *Launcher) refreshInitialAccount() error {
	l.mu.Lock()
	initial := l.authVersion
	l.mu.Unlock()
	initialKey := l.accountRefreshKey()
	// Never overwrite a sign-in/refresh that the user started while metadata
	// was being checked, even if that newer operation has already completed.
	stillCurrent := func() bool {
		l.mu.Lock()
		defer l.mu.Unlock()
		return !l.shutdownStarted && !l.auth.Running && l.authVersion == initial && initialKey == l.accountRefreshKey()
	}
	status := l.keyring.Check()
	if !stillCurrent() {
		return nil
	}
	l.mu.Lock()
	attempt := status.GameMode && status.State != "unavailable" && !l.walletAttempted && l.host.WindowVisible() &&
		l.backend.Current().Available && l.backendCommand("keyring-unlock")
	if attempt {
		// The launcher is already visible, so the native password prompt has a
		// parent. Attempt once: cancellation must not trigger repeated prompts.
		l.walletAttempted = true
	}
	l.mu.Unlock()
	if attempt {
		status = l.keyring.StartForStartup()
		if !stillCurrent() {
			return nil
		}
		l.host.Emit("keyring-state", status)
		if status.State != "ready" {
			l.mu.Lock()
			l.auth.Stored, l.auth.Note = nil, status.Message
			l.mu.Unlock()
			l.emitSnapshot()
			return nil
		}
	}
	if status.GameMode && slices.Contains(keyringBlocked, status.State) {
		l.mu.Lock()
		l.auth.Stored, l.auth.Note = nil, status.Message
		l.mu.Unlock()
		l.emitSnapshot()
		return nil
	}
	l.refreshAccount(true, false)
	return nil
}

func (l *Launcher) prepareSystemWallet() error {
	status := l.keyring.Check()
	if !status.GameMode || status.State == "ready" || status.State == "unavailable" {
		return nil
	}
	// Direct Steam game shortcuts have no launcher window yet. Give the wallet
	// dialog a visible parent before any backend account read can block on it.
	l.mu.Lock()
	direct := l.shortcutOnly
	l.mu.Unlock()
	if !l.host.WindowVisible() {
		l.showLauncher()
		l.mu.Lock()
		l.shortcutOnly = direct
		l.mu.Unlock()
	}
	result := l.keyring.Start()
	if result.State != "ready" {
		return errors.New(result.Message)
	}
	if direct {
		l.host.HideWindow()
	}
	return nil
}

func (l *Launcher) authViewFactory() (auth.ViewFactory, error) {
	browser, err := auth.FindBrowser(launcherenv.FromOS())
	if err != nil {
		return nil, err
	}
	return auth.BrowserViewFactory(browser, launcherenv.External(nil), false), nil
}

func (l *Launcher) runAuth(command string) (err error) {
	if err := l.ensureIdle(""); err != nil {
		return err
	}
	binary, err := l.requireBackend(command)
	if err != nil {
		return err
	}
	id := "auth-" + install.NewUUID()
	l.accountRefresh.Invalidate()
	l.library.Reset()
	l.activities.Select("", "")
	l.achievements.Select("", "")
	l.mu.Lock()
	l.auth.Running, l.auth.LastResult = true, ""
	l.authVersion++
	l.mu.Unlock()
	l.emitSnapshot()
	defer func() {
		l.mu.Lock()
		delete(l.authKills, id)
		l.auth.Running = false
		l.mu.Unlock()
		l.emitSnapshot()
	}()
	if command == "login" && !l.backend.Current().Capabilities.ExternalAuth {
		return errors.New("Update the Nexus backend in Settings before signing in with this launcher.")
	}
	if command == "login" {
		if err := l.prepareSystemWallet(); err != nil {
			return err
		}
	}
	if l.closing() {
		return errors.New("The launcher is closing.")
	}
	l.profile.Select("", "", false)
	if l.closing() {
		return errors.New("The launcher is closing.")
	}
	var code int
	if command == "login" {
		factory, err := l.authViewFactory()
		if err != nil {
			return err
		}
		login, err := auth.Start(binary, factory, launcherenv.FromOS(), 10*time.Minute)
		if err != nil {
			return err
		}
		l.mu.Lock()
		l.authKills[id] = login.Kill
		l.mu.Unlock()
		if code, err = login.Wait(); err != nil {
			return err
		}
	} else {
		// Explicit sign-out removes the local device registration too. Ordinary
		// sign-in must keep it, including when renewing an existing account.
		tracked, err := proc.Spawn(binary, []string{"logout", "--device"}, proc.RunOptions{OnLine: func(line string) { l.emitLog("auth", line) }})
		if err != nil {
			return err
		}
		l.mu.Lock()
		l.authKills[id] = func() { tracked.Kill() }
		l.mu.Unlock()
		code = tracked.Wait().Code
	}
	l.accountRefresh.Invalidate()
	if code == 0 {
		l.refreshAccount(false, true)
	}
	l.mu.Lock()
	l.auth.LastAction = command
	l.auth.LastResult = "failed"
	if code == 0 {
		l.auth.LastResult = "success"
	}
	l.mu.Unlock()
	if code != 0 {
		return fmt.Errorf("xodus-cli %s exited with code %d.", command, code)
	}
	return nil
}

func (l *Launcher) requestAuth(command string) error {
	l.mu.Lock()
	if l.authJob != nil {
		l.mu.Unlock()
		return errors.New("A sign-in operation is already in progress.")
	}
	done := make(chan struct{})
	l.authJob = done
	l.mu.Unlock()
	defer func() {
		l.mu.Lock()
		if l.authJob == done {
			l.authJob = nil
		}
		l.mu.Unlock()
		close(done)
	}()
	return l.runAuth(command)
}

func (l *Launcher) checkGameUpdates(id string) error {
	if err := l.ensureIdle(""); err != nil {
		return err
	}
	if l.storedAccount() == "" {
		return errors.New("Sign in before checking game updates.")
	}
	binary, err := l.requireBackend("account")
	if err != nil {
		return err
	}
	games := l.store.Games()
	if id != "" {
		games = slices.DeleteFunc(games, func(entry shared.Game) bool { return entry.ID != id })
		if len(games) == 0 {
			return errors.New("Game not found.")
		}
	}
	temporary, err := os.MkdirTemp("", "nexus-updates-")
	if err != nil {
		return err
	}
	ctx, cancel := context.WithCancel(context.Background())
	l.mu.Lock()
	l.checkingGameUpdates, l.gameUpdateCancel = true, cancel
	for _, entry := range games {
		l.gameUpdates[entry.ID] = shared.GameUpdate{Checking: true}
	}
	l.mu.Unlock()
	var session *service.Session
	defer func() {
		cancel()
		if session != nil {
			session.Stop()
		}
		os.RemoveAll(temporary)
		l.mu.Lock()
		l.checkingGameUpdates, l.gameUpdateCancel = false, nil
		if err != nil {
			for _, entry := range games {
				if l.gameUpdates[entry.ID].Checking {
					l.gameUpdates[entry.ID] = shared.GameUpdate{Error: err.Error()}
				}
			}
		}
		l.mu.Unlock()
		l.emitSnapshot()
	}()
	l.emitSnapshot()
	if session, err = service.Start(ctx, binary, temporary, func(string) {}); err != nil {
		return err
	}
	for _, entry := range games {
		if ctx.Err() != nil {
			err = errors.New("Update check cancelled.")
			return err
		}
		result, checkErr := func() (shared.GameUpdate, error) {
			inspected, err := state.InspectGameDirectory(entry.Path)
			if err != nil {
				return shared.GameUpdate{}, err
			}
			if inspected.StoreID == "" || inspected.InstalledVersion == "" {
				return shared.GameUpdate{}, errors.New("This manifest has no Store ID or installed package version.")
			}
			return game.QueryUpdate(ctx, session.SocketPath, inspected.StoreID, inspected.InstalledVersion)
		}()
		if checkErr != nil {
			result = shared.GameUpdate{CheckedAt: now(), Error: checkErr.Error()}
		}
		l.mu.Lock()
		l.gameUpdates[entry.ID] = result
		l.mu.Unlock()
		l.emitSnapshot()
	}
	return nil
}
