package backend

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"xodus-nexus-wails/assets"
	"xodus-nexus-wails/internal/install"
	"xodus-nexus-wails/internal/launcherenv"
	"xodus-nexus-wails/internal/proc"
	"xodus-nexus-wails/internal/shared"
)

type pointer struct {
	Commit        string `json:"commit"`
	BinaryPath    string `json:"binaryPath"`
	BuiltAt       string `json:"builtAt"`
	LinuxRunPatch string `json:"linuxRunPatch,omitempty"`
	RecipeSHA256  string `json:"recipeSha256,omitempty"`
	Version       string `json:"version,omitempty"`
	Delivery      string `json:"delivery,omitempty"` // release | local
}

func emptyCapabilities() shared.BackendCapabilities {
	return shared.BackendCapabilities{Commands: []string{}}
}

var (
	progressJSON     = regexp.MustCompile(`(?m)(?:^|\s)--progress-json(?:\s|$)`)
	controlStdin     = regexp.MustCompile(`(?m)(?:^|\s)--control-stdin(?:\s|$)`)
	downloadLimit    = regexp.MustCompile(`(?m)(?:^|\s)--download-limit-kib(?:\s|$)`)
	selectedContent  = regexp.MustCompile(`(?m)(?:^|\s)--selected-content-id(?:\s|$)`)
	includeDLC       = regexp.MustCompile(`(?m)(?:^|\s)--include-dlc(?:\s|$)`)
	streamingProcess = regexp.MustCompile(`\s(streaming|download)\s`)
)

type Manager struct {
	managedRoot, pointerFile string
	emitLog                  func(scope, line string)
	isOperationActive        func() bool
	fetch                    Fetch

	// Hooks replaced in tests.
	validate         func(binary string) *shared.BackendCapabilities
	validateRelease  func(cli string) error
	externalDownload func() bool
	commandExists    func(command string) bool

	mu       sync.Mutex
	status   shared.BackendStatus
	artifact *Artifact
}

func NewManager(userData, externalRoot string, emitLog func(scope, line string), isOperationActive func() bool, fetch Fetch) *Manager {
	if fetch == nil {
		fetch = HTTPFetch
	}
	root := filepath.Join(userData, "xodus-backends")
	return &Manager{managedRoot: root, pointerFile: filepath.Join(root, "current.json"), emitLog: emitLog, isOperationActive: isOperationActive, fetch: fetch,
		validate: Validate, validateRelease: validateRelease, externalDownload: ExternalDownloadRunning, commandExists: commandExists,
		status: shared.BackendStatus{Source: "none", ExternalRoot: externalRoot, Capabilities: emptyCapabilities()}}
}

func (m *Manager) Current() shared.BackendStatus {
	m.mu.Lock()
	defer m.mu.Unlock()
	return shared.Clone(m.status)
}

func (m *Manager) update(change func(*shared.BackendStatus)) {
	m.mu.Lock()
	change(&m.status)
	m.mu.Unlock()
}

func (m *Manager) SetExternalRoot(root string) {
	m.update(func(s *shared.BackendStatus) { s.ExternalRoot = root })
}

func commandExists(command string) bool {
	result, err := proc.Run(command, []string{"--version"}, proc.RunOptions{MaxOutput: 20_000})
	return err == nil && result.Code == 0
}

func run(binary string, args ...string) (proc.Result, error) {
	return proc.Run(binary, args, proc.RunOptions{MaxOutput: 100_000})
}

// Validate probes a backend binary's capabilities; nil when unusable.
func Validate(binary string) *shared.BackendCapabilities {
	if info, err := os.Stat(binary); err != nil || info.Mode()&0o111 == 0 {
		return nil
	}
	result, err := run(binary, "--help")
	if err != nil || result.Code != 0 {
		return nil
	}
	capabilities := shared.ParseBackendHelp(result.Output)
	if capabilities.Streaming {
		streaming, err := run(binary, "streaming", "--help")
		ok := err == nil && streaming.Code == 0
		capabilities.StreamingProgressJSON = ok && progressJSON.MatchString(streaming.Output)
		capabilities.StreamingControls = capabilities.StreamingProgressJSON && controlStdin.MatchString(streaming.Output)
		capabilities.StreamingBandwidthLimit = capabilities.StreamingControls && downloadLimit.MatchString(streaming.Output)
		capabilities.PackageSelection = ok && slices.Contains(capabilities.Commands, "resolve-package") && selectedContent.MatchString(streaming.Output)
		if capabilities.PackageSelection {
			help, err := run(binary, "resolve-package", "--help")
			capabilities.ContentPackageSelection = err == nil && help.Code == 0 && includeDLC.MatchString(help.Output)
		}
	}
	if !capabilities.Login || !capabilities.Streaming {
		return nil
	}
	return &capabilities
}

// validateRelease requires every command and the launch chain the launcher uses.
func validateRelease(cli string) error {
	caps := Validate(cli)
	if caps == nil || !caps.Account || !caps.Run || !caps.Streaming || !caps.Login || !caps.Logout || !caps.Download || !caps.ExternalAuth || !caps.PackageSelection {
		return errors.New("Backend validation failed. The prebuilt build requires glibc 2.35 or newer, OpenSSL 3 and D-Bus runtime libraries.")
	}
	help, err := run(cli, "run", "--help")
	if err != nil || help.Code != 0 || !strings.Contains(help.Output, "--materialize-chain") {
		return errors.New("Backend launch-chain validation failed.")
	}
	return nil
}

func gitCommit(root string) string {
	result, err := proc.Run("git", []string{"-C", root, "rev-parse", "HEAD"}, proc.RunOptions{MaxOutput: 10_000})
	if err != nil || result.Code != 0 {
		return ""
	}
	return strings.TrimSpace(result.Output)
}

func (m *Manager) readPointer() *pointer {
	data, err := os.ReadFile(m.pointerFile)
	if err != nil {
		return nil
	}
	var value pointer
	if json.Unmarshal(data, &value) != nil {
		return nil
	}
	return &value
}

func (m *Manager) Refresh() shared.BackendStatus {
	prerequisites := shared.BackendPrerequisites{}
	var group sync.WaitGroup
	for name, target := range map[string]*bool{"git": &prerequisites.Git, "cargo": &prerequisites.Cargo, "rustc": &prerequisites.Rustc, "protoc": &prerequisites.Protoc} {
		group.Add(1)
		go func() { defer group.Done(); *target = m.commandExists(name) }()
	}
	group.Wait()
	m.update(func(s *shared.BackendStatus) { s.Prerequisites = prerequisites })

	if current := m.readPointer(); current != nil {
		if capabilities := m.validate(current.BinaryPath); capabilities != nil {
			m.update(func(s *shared.BackendStatus) {
				s.Available, s.Source, s.BinaryPath, s.Commit, s.Capabilities, s.BlockReason = true, "managed", current.BinaryPath, current.Commit, *capabilities, ""
				s.LinuxRunPatch, s.Version, s.Delivery = current.LinuxRunPatch, current.Version, current.Delivery
			})
			return m.Current()
		}
	}
	externalRoot := m.Current().ExternalRoot
	external := filepath.Join(externalRoot, "target", "release", "xodus-cli")
	if capabilities := m.validate(external); capabilities != nil {
		commit := gitCommit(externalRoot)
		m.update(func(s *shared.BackendStatus) {
			s.Available, s.Source, s.BinaryPath, s.Commit, s.Capabilities, s.BlockReason = true, "external", external, commit, *capabilities, ""
			s.LinuxRunPatch, s.Version, s.Delivery = "", "", ""
		})
	} else {
		m.update(func(s *shared.BackendStatus) {
			s.Available, s.Source, s.BinaryPath, s.Commit, s.Version, s.Delivery, s.LinuxRunPatch = false, "none", "", "", "", "", ""
			s.Capabilities = emptyCapabilities()
			s.BlockReason = "No validated xodus-cli with login and streaming commands was found."
		})
	}
	return m.Current()
}

// ExternalDownloadRunning reports an xodus-cli download started outside the launcher.
func ExternalDownloadRunning() bool {
	result, err := proc.Run("ps", []string{"-eo", "args="}, proc.RunOptions{MaxOutput: 2_000_000})
	if err != nil {
		return false
	}
	for _, line := range strings.Split(result.Output, "\n") {
		lower := strings.ToLower(line)
		if strings.Contains(lower, "xodus-cli") && streamingProcess.MatchString(lower) {
			return true
		}
	}
	return false
}

func (m *Manager) assertUpdateAllowed() error {
	if m.isOperationActive() {
		return errors.New("The backend cannot be updated while authentication, a download, or a game is active.")
	}
	if m.externalDownload() {
		return errors.New("An external Xodus download is active. The update was blocked.")
	}
	return nil
}

func (m *Manager) CheckUpdate(ctx context.Context) shared.BackendStatus {
	m.update(func(s *shared.BackendStatus) { s.Checking = true })
	artifact, err := Discover(ctx, m.fetch)
	current := m.readPointer()
	m.update(func(s *shared.BackendStatus) {
		s.Checking = false
		if err != nil {
			m.artifact = nil
			s.ReleaseAvailable, s.UpdateAvailable = false, false
			if strings.HasPrefix(err.Error(), "No compatible Nexus backend release") {
				s.BlockReason = err.Error()
			} else {
				s.BlockReason = "Backend release check failed: " + err.Error()
			}
			return
		}
		m.artifact = &artifact
		s.ReleaseAvailable, s.ReleaseVersion, s.BlockReason = true, artifact.Manifest.Version, ""
		s.UpdateAvailable = !s.Available || current == nil || current.Delivery != "release" || current.Version == "" ||
			CompareVersions(current.Version, artifact.Manifest.Version) < 0
	})
	return m.Current()
}

func writeJSONAtomic(file string, value any) error {
	data, _ := json.MarshalIndent(value, "", "  ")
	temporary := file + "." + install.NewUUID() + ".tmp"
	defer os.Remove(temporary)
	if err := os.WriteFile(temporary, data, 0o644); err != nil {
		return err
	}
	return os.Rename(temporary, file)
}

func (m *Manager) beginUpdate(onStatus func()) error {
	if err := m.assertUpdateAllowed(); err != nil {
		return err
	}
	m.mu.Lock()
	if m.status.Updating {
		m.mu.Unlock()
		return errors.New("A backend installation is already running.")
	}
	m.status.Updating, m.status.BlockReason = true, ""
	m.mu.Unlock()
	onStatus()
	return nil
}

// Update installs the newest compatible prebuilt release.
func (m *Manager) Update(ctx context.Context, onStatus func()) (err error) {
	if err := m.beginUpdate(onStatus); err != nil {
		return err
	}
	defer func() {
		if err != nil {
			m.update(func(s *shared.BackendStatus) {
				s.BlockReason, s.Phase = err.Error(), "Backend update failed; the previous backend was kept."
			})
		}
		m.update(func(s *shared.BackendStatus) { s.Updating = false })
		onStatus()
	}()
	// Refresh discovery at install time; do not use a stale candidate.
	artifact, err := Discover(ctx, m.fetch)
	if err != nil {
		return err
	}
	m.mu.Lock()
	m.artifact = &artifact
	m.mu.Unlock()
	binary, err := Install(ctx, m.managedRoot, artifact, m.fetch, m.validateRelease, m.assertUpdateAllowed, func(message string) {
		m.update(func(s *shared.BackendStatus) { s.Phase = message })
		m.emitLog("backend", message)
		onStatus()
	}, nil)
	if err != nil {
		return err
	}
	if err := m.assertUpdateAllowed(); err != nil {
		return err
	}
	manifest := artifact.Manifest
	if err := writeJSONAtomic(m.pointerFile, pointer{Commit: manifest.Commit, BinaryPath: binary, BuiltAt: time.Now().UTC().Format(time.RFC3339Nano),
		LinuxRunPatch: manifest.Compatibility, RecipeSHA256: manifest.RecipeSHA256, Version: manifest.Version, Delivery: "release"}); err != nil {
		return err
	}
	m.Refresh()
	m.update(func(s *shared.BackendStatus) {
		s.ReleaseAvailable, s.ReleaseVersion, s.UpdateAvailable, s.Phase = true, manifest.Version, false, "Backend is ready."
	})
	return nil
}

// Rebuild builds the pinned backend source with the embedded patches.
func (m *Manager) Rebuild(onStatus func()) (err error) {
	if err := m.assertUpdateAllowed(); err != nil {
		return err
	}
	prerequisites := m.Current().Prerequisites
	if !prerequisites.Git || !prerequisites.Cargo || !prerequisites.Rustc || !prerequisites.Protoc {
		return errors.New("Git, Cargo, Rust, or protoc is missing. Check the build dependencies in Settings.")
	}
	if err := m.beginUpdate(onStatus); err != nil {
		return err
	}
	staging := filepath.Join(m.managedRoot, ".staging-"+install.NewUUID())
	defer func() {
		if err != nil {
			m.update(func(s *shared.BackendStatus) { s.BlockReason = err.Error() })
		}
		m.update(func(s *shared.BackendStatus) { s.Updating = false })
		os.RemoveAll(staging)
		onStatus()
	}()
	if err := os.MkdirAll(filepath.Join(m.managedRoot, "releases"), 0o755); err != nil {
		return err
	}
	log := func(line string) { m.emitLog("backend", line) }
	patches := map[string][]byte{}
	for _, patch := range SourceLock.Patches {
		data, err := assets.Patches.ReadFile(patch.File)
		if err != nil {
			return err
		}
		sum := sha256.Sum256(data)
		if hex.EncodeToString(sum[:]) != patch.SHA256 {
			return errors.New("Backend patch checksum mismatch.")
		}
		patches[patch.File] = data
	}
	streamed := proc.RunOptions{OnLine: log}
	git := func(args ...string) (proc.Result, error) { return proc.Run("git", args, streamed) }
	log(fmt.Sprintf("Cloning the pinned Xodus source %s…", SourceLock.Commit))
	if result, err := git("clone", "--no-checkout", SourceLock.Repository, staging); err != nil || result.Code != 0 {
		return fmt.Errorf("git clone exited with code %d", result.Code)
	}
	if result, err := git("-C", staging, "checkout", "--detach", SourceLock.Commit); err != nil || result.Code != 0 {
		return errors.New("Pinned backend checkout failed.")
	}
	head, _ := proc.Run("git", []string{"-C", staging, "rev-parse", "HEAD"}, proc.RunOptions{})
	commit := strings.TrimSpace(head.Output)
	cargoLock, err := os.ReadFile(filepath.Join(staging, "Cargo.lock"))
	sum := sha256.Sum256(cargoLock)
	if err != nil || commit != SourceLock.Commit || hex.EncodeToString(sum[:]) != SourceLock.CargoLockSHA256 {
		return errors.New("Backend source lock mismatch.")
	}
	for _, patch := range SourceLock.Patches {
		file := filepath.Join(staging, ".nexus-"+patch.File)
		if err := os.WriteFile(file, patches[patch.File], 0o644); err != nil {
			return err
		}
		log(fmt.Sprintf("Applying %s…", patch.File))
		limited := proc.RunOptions{OnLine: log, MaxOutput: 100_000}
		if result, err := proc.Run("git", []string{"-C", staging, "apply", "--check", file}, limited); err != nil || result.Code != 0 {
			return fmt.Errorf("The pinned backend patch %s does not apply; the previous backend remains active.", patch.File)
		}
		if result, err := proc.Run("git", []string{"-C", staging, "apply", file}, limited); err != nil || result.Code != 0 {
			return fmt.Errorf("Backend patch %s could not be applied.", patch.File)
		}
	}
	target := filepath.Join(staging, "target")
	log(fmt.Sprintf("Building %s with Cargo…", commit[:10]))
	env := launcherenv.FromOS()
	env["CARGO_TARGET_DIR"] = target
	result, err := proc.Run("cargo", []string{"build", "--locked", "--release", "--no-default-features", "-p", "xodus-cli", "-p", "xodus-service"},
		proc.RunOptions{Dir: staging, Env: env, OnLine: log, MaxOutput: 5_000_000})
	if err != nil || result.Code != 0 {
		return fmt.Errorf("cargo build exited with code %d", result.Code)
	}
	builtCLI := filepath.Join(target, "release", "xodus-cli")
	capabilities := m.validate(builtCLI)
	if capabilities == nil || !capabilities.Login || !capabilities.Streaming || !capabilities.Account || !capabilities.ExternalAuth || !capabilities.PackageSelection {
		return errors.New("The built backend failed command validation.")
	}
	if help, err := run(builtCLI, "run", "--help"); err != nil || help.Code != 0 || !strings.Contains(help.Output, "--materialize-chain") {
		return errors.New("The built backend lacks launch-chain support.")
	}
	if err := m.assertUpdateAllowed(); err != nil {
		return err
	}
	releaseDir := filepath.Join(m.managedRoot, "releases", commit+"-"+SourceLock.PatchID+"-local-"+install.NewUUID())
	if err := os.MkdirAll(releaseDir, 0o755); err != nil {
		return err
	}
	for _, name := range []string{"xodus-cli", "xodus-service"} {
		data, err := os.ReadFile(filepath.Join(target, "release", name))
		if err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(releaseDir, name), data, 0o755); err != nil {
			return err
		}
	}
	managedCLI := filepath.Join(releaseDir, "xodus-cli")
	now := time.Now().UTC().Format(time.RFC3339Nano)
	metadata, _ := json.MarshalIndent(map[string]any{"commit": commit, "builtAt": now, "capabilities": capabilities, "linuxRunPatch": LinuxRunPatch}, "", "  ")
	if err := os.WriteFile(filepath.Join(releaseDir, "metadata.json"), metadata, 0o644); err != nil {
		return err
	}
	if err := writeJSONAtomic(m.pointerFile, pointer{Commit: commit, BinaryPath: managedCLI, BuiltAt: now, LinuxRunPatch: LinuxRunPatch,
		RecipeSHA256: RecipeSHA256, Version: SourceLock.Version, Delivery: "local"}); err != nil {
		return err
	}
	m.update(func(s *shared.BackendStatus) {
		s.Available, s.Source, s.BinaryPath, s.Commit, s.UpstreamCommit, s.UpdateAvailable = true, "managed", managedCLI, commit, commit, false
		s.Capabilities, s.LinuxRunPatch = *capabilities, LinuxRunPatch
	})
	log("The new backend passed validation and was activated atomically. Older builds were preserved.")
	return nil
}
