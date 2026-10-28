// Package maxima builds and drives Maxima, the open-source EA launcher
// (GPL-3.0, github.com/ArmchairDevelopers/Maxima). It always runs as a
// separate program: Nexus only exchanges arguments and JSON lines with it.
package maxima

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"xodus-nexus-wails/assets"
	"xodus-nexus-wails/internal/install"
	"xodus-nexus-wails/internal/launcherenv"
	"xodus-nexus-wails/internal/proc"
	"xodus-nexus-wails/internal/shared"
)

type Patch struct {
	File   string `json:"file"`
	SHA256 string `json:"sha256"`
}

// Lock pins the Maxima source built by this launcher version.
type Lock struct {
	Schema          int     `json:"schema"`
	Repository      string  `json:"repository"`
	Commit          string  `json:"commit"`
	CargoLockSHA256 string  `json:"cargoLockSha256"`
	PatchID         string  `json:"patchId"`
	Patches         []Patch `json:"patches"`
}

var SourceLock = func() Lock {
	data, err := assets.Maxima.ReadFile("maxima/maxima-source.lock.json")
	if err != nil {
		panic(err)
	}
	var lock Lock
	if err := json.Unmarshal(data, &lock); err != nil || lock.Schema != 1 || len(lock.Commit) != 40 {
		panic("invalid Maxima source lock")
	}
	return lock
}()

type pointer struct {
	Commit     string `json:"commit"`
	PatchID    string `json:"patchId"`
	BinaryPath string `json:"binaryPath"`
	BuiltAt    string `json:"builtAt"`
}

// DataDir is where Maxima keeps its account, downloads queue and Proton.
func DataDir() string {
	data := os.Getenv("XDG_DATA_HOME")
	if !filepath.IsAbs(data) {
		home, _ := os.UserHomeDir()
		data = filepath.Join(home, ".local", "share")
	}
	return filepath.Join(data, "maxima")
}

type Manager struct {
	root     string // managed builds: <root>/releases/…, <root>/current.json
	emitLog  func(scope, line string)
	onChange func()

	mu     sync.Mutex
	status shared.MaximaStatus
}

func NewManager(root string, emitLog func(scope, line string), onChange func()) *Manager {
	m := &Manager{root: root, emitLog: emitLog, onChange: onChange}
	m.Refresh()
	return m
}

func (m *Manager) pointerFile() string { return filepath.Join(m.root, "current.json") }

func (m *Manager) readPointer() *pointer {
	data, err := os.ReadFile(m.pointerFile())
	if err != nil {
		return nil
	}
	var value pointer
	if json.Unmarshal(data, &value) != nil || value.BinaryPath == "" {
		return nil
	}
	if info, err := os.Stat(value.BinaryPath); err != nil || !info.Mode().IsRegular() {
		return nil
	}
	return &value
}

func (m *Manager) Current() shared.MaximaStatus {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.status
}

func (m *Manager) update(change func(*shared.MaximaStatus)) {
	m.mu.Lock()
	change(&m.status)
	m.mu.Unlock()
	m.onChange()
}

func signedIn() bool {
	info, err := os.Stat(filepath.Join(DataDir(), "auth.toml"))
	return err == nil && info.Size() > 0
}

func commandExists(command string) bool {
	_, err := exec.LookPath(command)
	return err == nil
}

// Refresh re-reads the active build and sign-in state from disk.
func (m *Manager) Refresh() shared.MaximaStatus {
	current := m.readPointer()
	m.mu.Lock()
	s := &m.status
	s.PinnedCommit, s.SignedIn, s.CanBuild = SourceLock.Commit, signedIn(), commandExists("git") && commandExists("cargo")
	s.Available, s.Commit = current != nil, ""
	if current != nil {
		s.Commit = current.Commit
	}
	s.BuildRequired = current == nil || current.Commit != SourceLock.Commit || current.PatchID != SourceLock.PatchID
	value := *s
	m.mu.Unlock()
	return value
}

// Binary returns the active maxima-cli, or an error asking for a build.
func (m *Manager) Binary() (string, error) {
	current := m.readPointer()
	if current == nil {
		return "", errors.New("Maxima is not built yet. Build it in Settings → Maxima.")
	}
	return current.BinaryPath, nil
}

// CheckUpstream records the newest Maxima commit. Nexus only builds the
// pinned one; a newer upstream commit arrives with a launcher update.
func (m *Manager) CheckUpstream(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	output, err := exec.CommandContext(ctx, "git", "ls-remote", SourceLock.Repository, "HEAD").Output()
	if err != nil {
		return errors.New("The Maxima repository could not be checked.")
	}
	commit, _, _ := strings.Cut(strings.TrimSpace(string(output)), "\t")
	if len(commit) != 40 {
		return errors.New("The Maxima repository returned no commit.")
	}
	m.update(func(s *shared.MaximaStatus) { s.UpstreamCommit = commit })
	return nil
}

func (m *Manager) begin(phase string, flag func(*shared.MaximaStatus) *bool) error {
	m.mu.Lock()
	if m.status.Building || m.status.SigningIn {
		m.mu.Unlock()
		return errors.New("Maxima is busy. Wait for the current operation to finish.")
	}
	*flag(&m.status) = true
	m.status.Phase, m.status.Error = phase, ""
	m.mu.Unlock()
	m.onChange()
	return nil
}

func (m *Manager) end(err error, flag func(*shared.MaximaStatus) *bool) {
	m.Refresh()
	m.update(func(s *shared.MaximaStatus) {
		*flag(s) = false
		s.Phase = ""
		if err != nil {
			s.Error = err.Error()
		}
	})
}

func building(s *shared.MaximaStatus) *bool  { return &s.Building }
func signingIn(s *shared.MaximaStatus) *bool { return &s.SigningIn }

// Build clones the pinned commit, applies the embedded patches and builds
// maxima-cli plus maxima-bootstrap (the qrc:// sign-in handler) with Cargo.
// The previous build stays active until the new one is complete.
func (m *Manager) Build() (err error) {
	if !commandExists("git") || !commandExists("cargo") {
		return errors.New("Git and Cargo are required to build Maxima. Install the Rust toolchain (for example: sudo dnf install cargo rust).")
	}
	if err := m.begin("Building Maxima…", building); err != nil {
		return err
	}
	staging := filepath.Join(m.root, ".staging-"+install.NewUUID())
	defer func() {
		os.RemoveAll(staging)
		m.end(err, building)
	}()
	log := func(line string) { m.emitLog("maxima", line) }
	patches := map[string][]byte{}
	for _, patch := range SourceLock.Patches {
		data, err := assets.Maxima.ReadFile("maxima/" + patch.File)
		if err != nil {
			return err
		}
		if sum := sha256.Sum256(data); hex.EncodeToString(sum[:]) != patch.SHA256 {
			return errors.New("Maxima patch checksum mismatch.")
		}
		patches[patch.File] = data
	}
	if err := os.MkdirAll(filepath.Join(m.root, "releases"), 0o755); err != nil {
		return err
	}
	streamed := proc.RunOptions{OnLine: log, MaxOutput: 1_000_000}
	log(fmt.Sprintf("Cloning the pinned Maxima source %s…", SourceLock.Commit[:10]))
	if result, err := proc.Run("git", []string{"clone", "--no-checkout", SourceLock.Repository, staging}, streamed); err != nil || result.Code != 0 {
		return fmt.Errorf("git clone exited with code %d", result.Code)
	}
	if result, err := proc.Run("git", []string{"-C", staging, "checkout", "--detach", SourceLock.Commit}, streamed); err != nil || result.Code != 0 {
		return errors.New("The pinned Maxima checkout failed.")
	}
	// Maxima does not commit Cargo.lock, so Nexus ships the one it was
	// tested with; --locked then pins every dependency version.
	cargoLock, err := assets.Maxima.ReadFile("maxima/Cargo.lock")
	if sum := sha256.Sum256(cargoLock); err != nil || hex.EncodeToString(sum[:]) != SourceLock.CargoLockSHA256 {
		return errors.New("The embedded Maxima Cargo.lock does not match the source lock.")
	}
	if err := os.WriteFile(filepath.Join(staging, "Cargo.lock"), cargoLock, 0o644); err != nil {
		return err
	}
	for _, patch := range SourceLock.Patches {
		file := filepath.Join(staging, ".nexus-"+patch.File)
		if err := os.WriteFile(file, patches[patch.File], 0o644); err != nil {
			return err
		}
		log("Applying " + patch.File + "…")
		if result, err := proc.Run("git", []string{"-C", staging, "apply", file}, streamed); err != nil || result.Code != 0 {
			return fmt.Errorf("The Maxima patch %s does not apply; the previous build remains active.", patch.File)
		}
	}
	m.update(func(s *shared.MaximaStatus) {
		s.Phase = "Compiling Maxima with Cargo. The first build takes several minutes…"
	})
	env := launcherenv.FromOS()
	// Maxima uses nightly-only language features; this lets a stable
	// toolchain accept them instead of requiring a nightly install.
	env["RUSTC_BOOTSTRAP"] = "1"
	// The target directory outlives the staging checkout, so an update only
	// recompiles what changed instead of every dependency.
	target := filepath.Join(m.root, "cargo-target")
	env["CARGO_TARGET_DIR"] = target
	result, err := proc.Run("cargo", []string{"build", "--locked", "--release", "-p", "maxima-cli", "-p", "maxima-bootstrap"},
		proc.RunOptions{Dir: staging, Env: env, OnLine: log, MaxOutput: 5_000_000})
	if err != nil || result.Code != 0 {
		return fmt.Errorf("cargo build exited with code %d. See the Maxima build log.", result.Code)
	}
	release := filepath.Join(m.root, "releases", SourceLock.Commit[:12]+"-"+SourceLock.PatchID+"-"+install.NewUUID()[:8])
	if err := os.MkdirAll(release, 0o755); err != nil {
		return err
	}
	// maxima-bootstrap must sit next to maxima-cli: Maxima registers it as
	// the qrc:// handler that hands the EA sign-in code back.
	for _, name := range []string{"maxima-cli", "maxima-bootstrap"} {
		data, err := os.ReadFile(filepath.Join(target, "release", name))
		if err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(release, name), data, 0o755); err != nil {
			return err
		}
	}
	data, _ := json.MarshalIndent(pointer{Commit: SourceLock.Commit, PatchID: SourceLock.PatchID,
		BinaryPath: filepath.Join(release, "maxima-cli"), BuiltAt: time.Now().UTC().Format(time.RFC3339)}, "", "  ")
	temporary := m.pointerFile() + ".tmp"
	if err := os.WriteFile(temporary, data, 0o644); err != nil {
		return err
	}
	if err := os.Rename(temporary, m.pointerFile()); err != nil {
		return err
	}
	log("Maxima was built and activated.")
	return nil
}

var loggedInAs = regexp.MustCompile(`Logged in as (.+)!`)

// SignIn runs Maxima's own EA sign-in: it opens the EA page in the browser
// and receives the code through its qrc:// handler. Nexus never sees EA
// credentials; Maxima stores its token in its own data directory.
func (m *Manager) SignIn(ctx context.Context) (err error) {
	binary, err := m.Binary()
	if err != nil {
		return err
	}
	if err := m.begin("Sign in to EA in the browser window. Allow it to open the link in Maxima Launcher.", signingIn); err != nil {
		return err
	}
	defer func() { m.end(err, signingIn) }()
	ctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	tracked, err := proc.Spawn(binary, []string{"account-info"}, proc.RunOptions{MaxOutput: 200_000,
		OnLine: func(line string) { m.emitLog("maxima", line) }})
	if err != nil {
		return err
	}
	select {
	case <-tracked.Done():
	case <-ctx.Done():
		tracked.Kill()
		return errors.New("EA sign-in timed out.")
	}
	result := tracked.Wait()
	if match := loggedInAs.FindStringSubmatch(stripANSI(result.Output)); match != nil {
		m.update(func(s *shared.MaximaStatus) { s.Account = match[1] })
		return nil
	}
	return fmt.Errorf("EA sign-in did not complete (Maxima exited with code %d).", result.Code)
}

// SignOut removes Maxima's stored EA token. Installed games are kept.
func (m *Manager) SignOut() error {
	if err := os.Remove(filepath.Join(DataDir(), "auth.toml")); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	m.Refresh()
	m.update(func(s *shared.MaximaStatus) { s.Account = "" })
	return nil
}

var ansi = regexp.MustCompile(`\x1b\[[0-9;]*m`)

func stripANSI(value string) string { return ansi.ReplaceAllString(value, "") }

// command runs maxima-cli when signed in; without a stored token Maxima would
// block on an interactive browser sign-in, so that is refused here.
func (m *Manager) command(ctx context.Context, args []string, options proc.RunOptions) (*proc.Tracked, error) {
	binary, err := m.Binary()
	if err != nil {
		return nil, err
	}
	if !signedIn() {
		return nil, errors.New("Sign in to EA in Settings → Maxima first.")
	}
	tracked, err := proc.Spawn(binary, args, options)
	if err != nil {
		return nil, err
	}
	go func() {
		select {
		case <-tracked.Done():
		case <-ctx.Done():
			tracked.Kill()
		}
	}()
	return tracked, nil
}

// Library lists the EA account's games, including EA Play titles.
func (m *Manager) Library(ctx context.Context) ([]shared.EATitle, error) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	tracked, err := m.command(ctx, []string{"library"}, proc.RunOptions{StdoutOnly: true, MaxOutput: 4_000_000})
	if err != nil {
		return nil, err
	}
	result := tracked.Wait()
	lines := strings.Split(strings.TrimSpace(result.Output), "\n")
	var titles []shared.EATitle
	if result.Code != 0 || json.Unmarshal([]byte(lines[len(lines)-1]), &titles) != nil {
		return nil, fmt.Errorf("The EA library could not be loaded (Maxima exited with code %d).", result.Code)
	}
	return titles, nil
}

// Progress is one JSON line printed by `maxima-cli install`.
type Progress struct {
	Event      string  `json:"event"` // build | progress | done
	Downloaded int64   `json:"downloaded"`
	Total      int64   `json:"total"`
	Percent    float64 `json:"percent"`
}

// Install downloads an EA game into path, reporting progress per second.
// onStart receives the process so the caller can stop it.
func (m *Manager) Install(ctx context.Context, offerID, path string, onStart func(*proc.Tracked), onProgress func(Progress)) error {
	if !filepath.IsAbs(path) {
		return errors.New("The EA game install path must be absolute.")
	}
	var done bool
	tracked, err := m.command(ctx, []string{"install", offerID, "--path", path}, proc.RunOptions{MaxOutput: 200_000,
		OnLine: func(line string) {
			var progress Progress
			if json.Unmarshal([]byte(line), &progress) != nil || progress.Event == "" {
				m.emitLog("maxima", stripANSI(line))
				return
			}
			done = done || progress.Event == "done"
			onProgress(progress)
		}})
	if err != nil {
		return err
	}
	onStart(tracked)
	result := tracked.Wait()
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if result.Code != 0 || !done {
		return fmt.Errorf("Maxima install exited with code %d: %s", result.Code, lastLine(stripANSI(result.Output)))
	}
	return nil
}

// Launch starts an installed EA game. Maxima stays running as the game's EA
// client (licence, LSX) until the game exits.
// env adds variables to the game's environment.
func (m *Manager) Launch(ctx context.Context, slug string, env map[string]string, onLine func(string)) (*proc.Tracked, error) {
	environment := launcherenv.FromOS()
	for name, value := range env {
		environment[name] = value
	}
	return m.command(ctx, []string{"launch", slug}, proc.RunOptions{MaxOutput: 2_000_000, Detached: true, Env: environment,
		OnLine: func(line string) { onLine(stripANSI(line)) }})
}

func lastLine(output string) string {
	scanner := bufio.NewScanner(strings.NewReader(output))
	last := ""
	for scanner.Scan() {
		if line := strings.TrimSpace(scanner.Text()); line != "" && !strings.HasPrefix(line, "{") {
			last = line
		}
	}
	return last
}
