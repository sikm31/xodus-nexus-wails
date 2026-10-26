package keyring

import (
	"context"
	"encoding/json"
	"errors"
	"os/exec"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"xodus-nexus-wails/internal/launcherenv"
	"xodus-nexus-wails/internal/proc"
	"xodus-nexus-wails/internal/shared"
)

type Actions struct {
	StartProvider func(provider KDEProvider, env launcherenv.Env, owned func() bool) error
	CanUnlock     func() bool
	Unlock        func() string
	CanCreate     func() bool
	Create        func() string
}

// Execute runs a metadata command and returns its stdout.
type Execute func(command string, args ...string) (string, error)

var (
	busService     = []string{"org.freedesktop.DBus", "/org/freedesktop/DBus", "org.freedesktop.DBus"}
	serviceObject  = "/org/freedesktop/secrets"
	objectPath     = regexp.MustCompile(`^/(?:[A-Za-z0-9_]+/?)*$`)
	falseSetting   = regexp.MustCompile(`(?i)^(false|0)$`)
	enabledSetting = regexp.MustCompile(`(?i)^(UNSET|true|1)$`)
	// KDE 6 can advertise its compatibility activation name rather than the
	// standard name. Never start another provider when one already owns secrets.
	activationNames = []string{serviceName, "org.kde.secretservicecompat", "org.kde.kwalletd6", "org.kde.kwalletd5"}
)

// Support checks are metadata-only. Preparation never reads credentials or
// changes policy.
type Support struct {
	env              launcherenv.Env
	execute          Execute
	gameMode         bool
	identifyProvider func() Provider
	actions          Actions

	mu       sync.Mutex
	checking chan struct{}
	checked  shared.KeyringStatus
	starting chan struct{}
	started  shared.KeyringStatus
}

func NewSupport(environment launcherenv.Env, execute Execute, identify func() Provider, actions Actions) *Support {
	if environment == nil {
		environment = launcherenv.FromOS()
	}
	gameMode := slices.ContainsFunc(strings.Split(environment["XDG_CURRENT_DESKTOP"], ":"), func(s string) bool { return strings.EqualFold(s, "gamescope") })
	env := launcherenv.External(environment)
	s := &Support{env: env, gameMode: gameMode, actions: actions}
	s.identifyProvider = identify
	if identify == nil {
		s.identifyProvider = func() Provider { return SecretServiceProvider(env, "") }
	}
	s.execute = execute
	if execute == nil {
		s.execute = func(command string, args ...string) (string, error) {
			ctx, cancel := context.WithTimeout(context.Background(), 3500*time.Millisecond)
			defer cancel()
			cmd := exec.CommandContext(ctx, command, args...)
			cmd.Env = env.List()
			out, err := cmd.Output()
			// Provider stderr and config paths never cross the renderer boundary.
			if err != nil || len(out) > 128_000 {
				return "", errors.New("Keyring metadata check unavailable.")
			}
			return string(out), nil
		}
	}
	return s
}

func (s *Support) Busy() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.starting != nil
}

func (s *Support) Check() shared.KeyringStatus {
	s.mu.Lock()
	if s.starting != nil {
		done := s.starting
		s.mu.Unlock()
		<-done
		s.mu.Lock()
		defer s.mu.Unlock()
		return s.started
	}
	if s.checking != nil {
		done := s.checking
		s.mu.Unlock()
		<-done
		s.mu.Lock()
		defer s.mu.Unlock()
		return s.checked
	}
	done := make(chan struct{})
	s.checking = done
	s.mu.Unlock()
	status := s.inspect().status
	s.mu.Lock()
	s.checked, s.checking = status, nil
	s.mu.Unlock()
	close(done)
	return status
}

// exclusive runs one preparation at a time; concurrent callers share its result.
func (s *Support) exclusive(run func() shared.KeyringStatus) shared.KeyringStatus {
	s.mu.Lock()
	if s.starting != nil {
		done := s.starting
		s.mu.Unlock()
		<-done
		s.mu.Lock()
		defer s.mu.Unlock()
		return s.started
	}
	done := make(chan struct{})
	s.starting = done
	checking := s.checking
	s.mu.Unlock()
	if checking != nil {
		<-checking
	}
	status := run()
	s.mu.Lock()
	s.started, s.starting = status, nil
	s.mu.Unlock()
	close(done)
	return status
}

func (s *Support) Start() shared.KeyringStatus {
	return s.exclusive(func() shared.KeyringStatus { return s.prepare(false) })
}

// StartForStartup is the one visible-launcher startup attempt. Unknown
// providers stay explicit.
func (s *Support) StartForStartup() shared.KeyringStatus {
	return s.exclusive(func() shared.KeyringStatus { return s.prepare(true) })
}

// CreateCollection runs on an explicit click only. It never replaces a default
// or another collection.
func (s *Support) CreateCollection() shared.KeyringStatus {
	return s.exclusive(func() shared.KeyringStatus {
		before := s.inspect().status
		if before.State != "needs-setup" || !before.CanCreate || s.actions.Create == nil {
			return before
		}
		result := s.actions.Create()
		after := s.inspect().status
		if after.State == "ready" {
			return after
		}
		switch result {
		case "cancelled":
			after.Message = "Wallet setup was cancelled. You can try again when ready."
		case "timed-out":
			after.Message = "Wallet setup timed out. Check for the system wallet window, then try again."
		case "unavailable":
			after.Message = "Wallet setup could not be completed. Check the backend diagnostic log and try again."
		}
		return after
	})
}

func (s *Support) prepare(automatic bool) shared.KeyringStatus {
	before := s.inspect()
	if automatic && (!s.gameMode || (before.status.State == "stopped" && !before.autoStartAllowed)) {
		return before.status
	}
	if before.status.State == "locked" {
		return s.unlock(before.status)
	}
	if before.target == "" || !before.status.CanStart {
		return before.status
	}
	failed := s.status("unavailable", "The installed keyring could not be started. Open your desktop wallet settings, enable and unlock it, then check again.", false)
	// Startup is limited to a known enabled KDE provider; other activation
	// remains explicit. Never enable a wallet, create one or change passwords.
	if s.gameMode && (before.provider == KWallet || before.provider == KSecretD) {
		provider := KDEProvider(before.provider)
		if before.target == "org.kde.kwalletd5" {
			provider = ProviderKWallet5
		}
		start := s.actions.StartProvider
		if start == nil {
			start = func(provider KDEProvider, env launcherenv.Env, owned func() bool) error {
				return StartSessionKeyring(provider, env, owned, "")
			}
		}
		owned := func() bool {
			value, err := s.bus("b", append(slices.Clone(busService), "NameHasOwner", "s", serviceName)...)
			return err == nil && value == true
		}
		if err := start(provider, s.env, owned); err != nil {
			return failed
		}
	} else {
		reply, err := s.bus("u", append(slices.Clone(busService), "StartServiceByName", "su", before.target, "0")...)
		if number, ok := reply.(float64); err != nil || !ok || (number != 1 && number != 2) {
			return failed
		}
	}
	after := s.inspect()
	deadline := time.Now().Add(4 * time.Second)
	for after.status.State == "stopped" && time.Now().Before(deadline) {
		time.Sleep(100 * time.Millisecond)
		after = s.inspect()
	}
	if after.status.State == "locked" {
		return s.unlock(after.status)
	}
	if after.status.State == "stopped" {
		return s.status("unavailable", "The keyring did not become available. Open your desktop wallet settings, then check again.", false)
	}
	return after.status
}

func (s *Support) unlock(status shared.KeyringStatus) shared.KeyringStatus {
	if !status.CanUnlock || s.actions.Unlock == nil {
		return status
	}
	result := s.actions.Unlock()
	after := s.inspect().status
	if after.State == "ready" {
		return after
	}
	switch result {
	case "cancelled":
		after.Message = "Wallet unlocking was cancelled. Your saved credentials are unchanged."
	case "timed-out":
		after.Message = "Wallet unlocking timed out. Check for the wallet password window, then try again."
	case "unavailable":
		after.Message = "The wallet password window could not be completed. Check the backend diagnostic log and try again."
	}
	return after
}

func (s *Support) status(state, message string, canStart bool) shared.KeyringStatus {
	return shared.KeyringStatus{State: state, Message: message, CanStart: canStart, GameMode: s.gameMode}
}

func (s *Support) bus(kind string, args ...string) (any, error) {
	raw, err := s.execute("busctl", append([]string{"--user", "--json=short", "--auto-start=no", "--allow-interactive-authorization=no", "--timeout=2s", "call"}, args...)...)
	if err != nil {
		return nil, err
	}
	var response struct {
		Type any   `json:"type"`
		Data []any `json:"data"`
	}
	if json.Unmarshal([]byte(raw), &response) != nil || response.Type != kind || len(response.Data) != 1 {
		return nil, errors.New("Unexpected metadata reply")
	}
	return response.Data[0], nil
}

type inspection struct {
	status           shared.KeyringStatus
	target           string
	provider         Provider
	autoStartAllowed bool
}

func validPath(value any) bool {
	text, ok := value.(string)
	return ok && objectPath.MatchString(text)
}

func (s *Support) inspect() inspection {
	unavailable := inspection{status: s.status("unavailable", "Keyring status could not be checked. Ensure your user D-Bus session and busctl are available, and that your desktop wallet is enabled and unlocked.", false)}
	owned, err := s.bus("b", append(slices.Clone(busService), "NameHasOwner", "s", serviceName)...)
	isOwned, ok := owned.(bool)
	if err != nil || !ok {
		return unavailable
	}
	if isOwned {
		collection, err := s.bus("o", serviceName, serviceObject, "org.freedesktop.Secret.Service", "ReadAlias", "s", "default")
		if err != nil || !validPath(collection) {
			return unavailable
		}
		if collection == "/" {
			status := s.status("needs-setup", "The keyring is running, but has no default wallet. Complete wallet setup in Desktop Mode, then check again.", false)
			if s.actions.CanCreate != nil && s.actions.CanCreate() {
				value, err := s.bus("v", serviceName, serviceObject, "org.freedesktop.DBus.Properties", "Get", "ss", "org.freedesktop.Secret.Service", "Collections")
				variant, _ := value.(map[string]any)
				paths, isList := variant["data"].([]any)
				if err != nil || variant["type"] != "ao" || !isList || slices.ContainsFunc(paths, func(p any) bool { return !validPath(p) }) {
					return unavailable
				}
				if len(paths) == 0 {
					status.CanCreate = true
					status.Message = "Create a wallet in the system dialog. Choose a password there, or leave it empty if supported. An empty password does not protect saved credentials."
				}
			}
			return inspection{status: status}
		}
		value, err := s.bus("v", serviceName, collection.(string), "org.freedesktop.DBus.Properties", "Get", "ss", "org.freedesktop.Secret.Collection", "Locked")
		// busctl represents a variant as a nested typed value.
		variant, _ := value.(map[string]any)
		locked, isBool := variant["data"].(bool)
		if err != nil || variant["type"] != "b" || !isBool {
			return unavailable
		}
		if locked {
			status := s.status("locked", "Your default wallet is locked. Unlock it to use saved sign-in credentials.", false)
			status.CanUnlock = s.actions.CanUnlock != nil && s.actions.CanUnlock()
			return inspection{status: status}
		}
		return inspection{status: s.status("ready", "The default system wallet is running and unlocked. Sign-in will still verify that credentials can be saved.", false)}
	}
	value, err := s.bus("as", append(slices.Clone(busService), "ListActivatableNames")...)
	names, isList := value.([]any)
	if err != nil || !isList || slices.ContainsFunc(names, func(name any) bool { _, ok := name.(string); return !ok }) {
		return unavailable
	}
	target := ""
	for _, name := range activationNames {
		if slices.Contains(names, any(name)) {
			target = name
			break
		}
	}
	// The standard name can activate KDE too, but merely having KDE installed
	// does not establish which provider owns that activation.
	provider := Unknown
	switch {
	case target == serviceName:
		provider = s.identifyProvider()
	case target == "org.kde.secretservicecompat":
		provider = KSecretD
	case strings.HasPrefix(target, "org.kde.kwalletd"):
		provider = KWallet
	}
	configurationKnown := false
	if provider == KWallet || provider == KSecretD {
		for _, command := range []string{"kreadconfig6", "kreadconfig5"} {
			group := "Wallet"
			if provider == KSecretD {
				group = "KSecretD"
			}
			known, completed := true, true
			for _, setting := range [][2]string{{group, "Enabled"}, {"org.freedesktop.secrets", "apiEnabled"}} {
				output, err := s.execute(command, "--file", "kwalletrc", "--group", setting[0], "--key", setting[1], "--default", "UNSET")
				if err != nil {
					// Older/non-KDE desktops may have no KConfig tools.
					completed = false
					break
				}
				enabled := strings.TrimSpace(output)
				if falseSetting.MatchString(enabled) {
					return inspection{status: s.status("disabled", "KDE Wallet or its Secret Service is disabled. In Desktop Mode, open System Settings > KDE Wallet, enable the wallet subsystem and unlock your wallet. Then check again.", false)}
				}
				known = known && enabledSetting.MatchString(enabled)
			}
			if completed {
				configurationKnown = known
				break
			}
		}
	}
	if target != "" {
		return inspection{target: target, provider: provider, autoStartAllowed: configurationKnown && (provider == KWallet || provider == KSecretD),
			status: s.status("stopped", "Start the installed keyring for this session. Its own password window may appear; wallet security settings are unchanged.", true)}
	}
	return inspection{status: s.status("missing", "No compatible Secret Service is running or available for activation. Install or enable your desktop keyring provider, then check again.", false)}
}

var windowID = regexp.MustCompile(`^\d{0,20}$`)

// Unlocker lets only the provider see the wallet password. The helper
// exchanges fixed wallet-operation states, never credentials, secret values
// or provider error text.
type Unlocker struct {
	mu      sync.Mutex
	tracked *proc.Tracked
}

func (u *Unlocker) Busy() bool {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.tracked != nil
}

func (u *Unlocker) Unlock(binary, window string) string {
	return u.run(binary, window, "keyring-unlock")
}
func (u *Unlocker) Create(binary, window string) string {
	return u.run(binary, window, "keyring-create")
}

var walletStates = []string{"ready", "locked", "stopped", "needs-setup", "cancelled", "timed-out", "unavailable"}

func (u *Unlocker) run(binary, window, command string) string {
	u.mu.Lock()
	if u.tracked != nil || !windowID.MatchString(window) {
		u.mu.Unlock()
		return "unavailable"
	}
	tracked, err := proc.Spawn(binary, []string{command, "--window-id", window}, proc.RunOptions{StdoutOnly: true, MaxOutput: 4096, DrainAfterExit: true, OutputDrain: 100 * time.Millisecond})
	if err != nil {
		u.mu.Unlock()
		return "unavailable"
	}
	u.tracked = tracked
	u.mu.Unlock()
	timer := time.AfterFunc(95*time.Second, u.Cancel)
	defer func() {
		timer.Stop()
		u.mu.Lock()
		u.tracked = nil
		u.mu.Unlock()
	}()
	result := tracked.Wait()
	if result.Code != 0 {
		return "unavailable"
	}
	var value struct {
		Schema json.RawMessage `json:"schema"`
		State  string          `json:"state"`
	}
	if json.Unmarshal([]byte(result.Output), &value) != nil || strings.TrimSpace(string(value.Schema)) != "1" || !slices.Contains(walletStates, value.State) {
		return "unavailable"
	}
	return value.State
}

func (u *Unlocker) Cancel() {
	u.mu.Lock()
	tracked := u.tracked
	u.mu.Unlock()
	if tracked == nil {
		return
	}
	tracked.Signal(2)
	time.AfterFunc(4*time.Second, func() {
		if !tracked.Exited() {
			tracked.Kill()
		}
	})
}

func (u *Unlocker) Stop() {
	u.mu.Lock()
	tracked := u.tracked
	u.mu.Unlock()
	u.Cancel()
	if tracked != nil {
		tracked.Wait()
	}
}
