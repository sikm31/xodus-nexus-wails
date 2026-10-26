// Package protonruntime ports runtime.ts: installation of the shared Xodus
// Proton runtime from GitHub Actions snapshots or published Nexus releases.
package protonruntime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"xodus-nexus-wails/internal/install"
	"xodus-nexus-wails/internal/proc"
	"xodus-nexus-wails/internal/shared"
)

const (
	repository             = "xodus-gaming/Proton"
	branch                 = "xodus/bleeding-edge"
	repositoryURL          = "https://github.com/xodus-gaming/Proton.git"
	nexusRepository        = "domsvoboda/xodus-nexus"
	nexusRuntimeRepository = "domsvoboda/nexus-runtime"
)

type pointer struct {
	Name        string `json:"name"`
	WinePath    string `json:"winePath"`
	Commit      string `json:"commit"`
	InstalledAt string `json:"installedAt"`
}

type artifactInfo struct {
	runID                  int64
	commit, name, checksum string
	expiresAt              string
}

type nexusArtifact struct {
	version, name, url, checksumName, checksumURL string
}

// CanonicalSnapshotName: release directories carry simple, user-facing version
// names; patch provenance lives in current.json and the release notes.
func CanonicalSnapshotName(commit string) string {
	return "xodus-proton-" + commit[:min(8, len(commit))]
}

func NexusReleaseName(archive string) string { return strings.TrimSuffix(archive, "-x86_64.tar.xz") }

var previewPattern = regexp.MustCompile(`^(proton-xodus-[A-Za-z0-9._-]+)-xgamesave-pr28-[0-9a-f]{10}(?:-bridge-v1(?:-userstate-v1(?:-userguest-v1)?)?)?(?:-locale-v1)?$`)

// PreviewBaseRelease returns the release a cloud-save preview was built on, or "".
func PreviewBaseRelease(name string) string {
	if match := previewPattern.FindStringSubmatch(name); match != nil {
		return match[1]
	}
	return ""
}

// Fetch performs an HTTP GET.
type Fetch func(ctx context.Context, url string, header http.Header) (*http.Response, error)

func HTTPFetch(ctx context.Context, url string, header http.Header) (*http.Response, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	for key, values := range header {
		request.Header[key] = values
	}
	return http.DefaultClient.Do(request)
}

type Manager struct {
	emitLog           func(scope, line string)
	isOperationActive func() bool
	fetch             Fetch
	// commandOK is replaced in tests.
	commandOK func(command string, args ...string) bool

	mu     sync.Mutex
	status shared.RuntimeStatus
	latest *artifactInfo
	nexus  *nexusArtifact
}

func NewManager(runtimeRoot string, emitLog func(scope, line string), isOperationActive func() bool, fetch Fetch) *Manager {
	if fetch == nil {
		fetch = HTTPFetch
	}
	return &Manager{emitLog: emitLog, isOperationActive: isOperationActive, fetch: fetch, commandOK: commandOK,
		status: shared.RuntimeStatus{RuntimeRoot: runtimeRoot}}
}

func commandOK(command string, args ...string) bool {
	result, err := proc.Run(command, args, proc.RunOptions{MaxOutput: 30_000})
	return err == nil && result.Code == 0
}

func (m *Manager) Current() shared.RuntimeStatus {
	m.mu.Lock()
	defer m.mu.Unlock()
	return shared.Clone(m.status)
}

func (m *Manager) update(change func(*shared.RuntimeStatus)) {
	m.mu.Lock()
	change(&m.status)
	m.mu.Unlock()
}

func (m *Manager) SetRoot(root string) {
	m.update(func(s *shared.RuntimeStatus) { s.RuntimeRoot = root })
}

func (m *Manager) root() string { return m.Current().RuntimeRoot }

func (m *Manager) pointerFile() string { return filepath.Join(m.root(), "current.json") }

func (m *Manager) readPointer() *pointer {
	data, err := os.ReadFile(m.pointerFile())
	if err != nil {
		return nil
	}
	var value pointer
	if json.Unmarshal(data, &value) != nil {
		return nil
	}
	return &value
}

var wineVersion = regexp.MustCompile(`(?i)wine`)

func validateWine(winePath string) bool {
	if info, err := os.Stat(winePath); err != nil || info.Mode()&0o111 == 0 {
		return false
	}
	result, err := proc.Run(winePath, []string{"--version"}, proc.RunOptions{MaxOutput: 30_000})
	return err == nil && result.Code == 0 && wineVersion.MatchString(result.Output)
}

func (m *Manager) Refresh() shared.RuntimeStatus {
	var prerequisites shared.RuntimePrerequisites
	var group sync.WaitGroup
	checks := []struct {
		target *bool
		args   []string
	}{
		{&prerequisites.GH, []string{"gh", "--version"}},
		{&prerequisites.Authenticated, []string{"gh", "auth", "status", "--hostname", "github.com"}},
		{&prerequisites.Tar, []string{"tar", "--version"}},
		{&prerequisites.SHA512Sum, []string{"sha512sum", "--version"}},
	}
	for _, check := range checks {
		group.Add(1)
		go func() { defer group.Done(); *check.target = m.commandOK(check.args[0], check.args[1:]...) }()
	}
	group.Wait()
	current := m.readPointer()
	valid := current != nil && validateWine(current.WinePath)
	m.update(func(s *shared.RuntimeStatus) {
		s.Prerequisites = prerequisites
		if valid {
			s.Installed, s.Name, s.WinePath, s.Commit, s.Error = true, current.Name, current.WinePath, current.Commit, ""
		} else {
			s.Installed, s.Name, s.WinePath, s.Commit = false, "", "", ""
		}
	})
	return m.Current()
}

func (m *Manager) discoverArtifact() (*artifactInfo, error) {
	remote, err := proc.Run("git", []string{"ls-remote", repositoryURL, "refs/heads/" + branch}, proc.RunOptions{MaxOutput: 30_000})
	if err != nil || remote.Code != 0 {
		return nil, errors.New("Could not resolve the Xodus Proton branch HEAD.")
	}
	fields := strings.Fields(remote.Output)
	if len(fields) == 0 {
		return nil, errors.New("Could not resolve the Xodus Proton branch HEAD.")
	}
	upstream := fields[0]
	m.update(func(s *shared.RuntimeStatus) { s.UpstreamCommit = upstream })
	runs, err := proc.Run("gh", []string{"api", "--method", "GET", "repos/" + repository + "/actions/runs",
		"-f", "branch=" + branch, "-f", "status=success", "-f", "per_page=20"}, proc.RunOptions{MaxOutput: 2_000_000})
	if err != nil || runs.Code != 0 {
		return nil, errors.New("GitHub Actions runs are unavailable. Is gh authenticated?")
	}
	var parsedRuns struct {
		WorkflowRuns []struct {
			ID      int64  `json:"id"`
			Name    string `json:"name"`
			HeadSHA string `json:"head_sha"`
		} `json:"workflow_runs"`
	}
	json.Unmarshal([]byte(runs.Output), &parsedRuns)
	index := slices.IndexFunc(parsedRuns.WorkflowRuns, func(run struct {
		ID      int64  `json:"id"`
		Name    string `json:"name"`
		HeadSHA string `json:"head_sha"`
	}) bool {
		return run.Name == "Snapshot" && run.HeadSHA == upstream
	})
	if index < 0 {
		index = slices.IndexFunc(parsedRuns.WorkflowRuns, func(run struct {
			ID      int64  `json:"id"`
			Name    string `json:"name"`
			HeadSHA string `json:"head_sha"`
		}) bool {
			return run.Name == "Snapshot"
		})
	}
	if index < 0 {
		return nil, errors.New("No successful Xodus Proton snapshot was found.")
	}
	run := parsedRuns.WorkflowRuns[index]
	artifacts, err := proc.Run("gh", []string{"api", fmt.Sprintf("repos/%s/actions/runs/%d/artifacts", repository, run.ID)}, proc.RunOptions{MaxOutput: 2_000_000})
	if err != nil || artifacts.Code != 0 {
		return nil, errors.New("Could not load Xodus Proton snapshot artifacts.")
	}
	var parsed struct {
		Artifacts []struct {
			Name      string `json:"name"`
			Expired   bool   `json:"expired"`
			ExpiresAt string `json:"expires_at"`
		} `json:"artifacts"`
	}
	json.Unmarshal([]byte(artifacts.Output), &parsed)
	var archive, checksum string
	expires := ""
	for _, entry := range parsed.Artifacts {
		if entry.Expired {
			continue
		}
		if archive == "" && strings.HasSuffix(entry.Name, "-x86_64.tar.xz") {
			archive, expires = entry.Name, entry.ExpiresAt
		}
		if checksum == "" && strings.HasSuffix(entry.Name, "-x86_64.sha512sum") {
			checksum = entry.Name
		}
	}
	if archive == "" || checksum == "" {
		return nil, errors.New("The x86_64 snapshot is missing or has expired.")
	}
	return &artifactInfo{runID: run.ID, commit: run.HeadSHA, name: archive, checksum: checksum, expiresAt: expires}, nil
}

func (m *Manager) Check() shared.RuntimeStatus {
	m.update(func(s *shared.RuntimeStatus) { s.Checking, s.Error = true, "" })
	if current := m.Current().Prerequisites; !current.GH || !current.Authenticated {
		m.Refresh()
	}
	prerequisites := m.Current().Prerequisites
	var artifact *artifactInfo
	var err error
	switch {
	case !prerequisites.GH:
		err = errors.New("GitHub CLI (gh) is missing.")
	case !prerequisites.Authenticated:
		err = errors.New("GitHub CLI is not authenticated; run gh auth login.")
	default:
		artifact, err = m.discoverArtifact()
	}
	m.update(func(s *shared.RuntimeStatus) {
		s.Checking = false
		m.latest = artifact
		if err != nil {
			s.ArtifactAvailable, s.Error = false, err.Error()
			return
		}
		s.ArtifactAvailable, s.ArtifactName, s.ArtifactExpiresAt = true, artifact.name, artifact.expiresAt
	})
	return m.Current()
}

func (m *Manager) setPhase(phase string, onStatus func()) {
	m.update(func(s *shared.RuntimeStatus) { s.Phase = phase })
	m.emitLog("runtime", phase)
	onStatus()
}

func (m *Manager) begin(onStatus func()) {
	m.update(func(s *shared.RuntimeStatus) { s.Updating, s.Error = true, "" })
	onStatus()
}

func (m *Manager) finish(staging string, err error, onStatus func()) {
	m.update(func(s *shared.RuntimeStatus) {
		s.Updating = false
		if err != nil {
			s.Error = err.Error()
		}
	})
	os.RemoveAll(staging)
	onStatus()
}

// Install downloads and activates the newest GitHub Actions snapshot.
func (m *Manager) Install(onStatus func()) (winePath string, err error) {
	if m.isOperationActive() {
		return "", errors.New("The runtime cannot be changed while a Xodus operation is active.")
	}
	m.mu.Lock()
	artifact := m.latest
	m.mu.Unlock()
	if artifact == nil {
		m.Check()
		m.mu.Lock()
		artifact = m.latest
		m.mu.Unlock()
	}
	if artifact == nil {
		message := m.Current().Error
		if message == "" {
			message = "The current snapshot is unavailable."
		}
		return "", errors.New(message)
	}
	if prerequisites := m.Current().Prerequisites; !prerequisites.Tar || !prerequisites.SHA512Sum {
		return "", errors.New("tar or sha512sum is missing.")
	}
	m.begin(onStatus)
	staging := filepath.Join(m.root(), ".staging-"+install.NewUUID())
	defer func() { m.finish(staging, err, onStatus) }()
	downloads := filepath.Join(staging, "downloads")
	if err := os.MkdirAll(downloads, 0o755); err != nil {
		return "", err
	}
	m.setPhase("Downloading the official x86_64 snapshot from GitHub Actions…", onStatus)
	log := func(line string) { m.emitLog("runtime", line) }
	result, err := proc.Run("gh", []string{"run", "download", fmt.Sprint(artifact.runID), "--repo", repository, "--name", artifact.name, "--dir", downloads},
		proc.RunOptions{OnLine: log, MaxOutput: 1_000_000})
	if err != nil || result.Code != 0 {
		return "", fmt.Errorf("Runtime download failed (gh code %d).", result.Code)
	}
	result, err = proc.Run("gh", []string{"run", "download", fmt.Sprint(artifact.runID), "--repo", repository, "--name", artifact.checksum, "--dir", downloads},
		proc.RunOptions{OnLine: log, MaxOutput: 100_000})
	if err != nil || result.Code != 0 {
		return "", errors.New("Checksum download failed.")
	}
	m.setPhase("Verifying the SHA-512 checksum…", onStatus)
	result, err = proc.Run("sha512sum", []string{"-c", artifact.checksum}, proc.RunOptions{Dir: downloads, MaxOutput: 100_000})
	if err != nil || result.Code != 0 {
		return "", errors.New("Snapshot SHA-512 verification failed.")
	}
	return m.activateArchive(downloads, artifact.name, artifact.commit, CanonicalSnapshotName(artifact.commit), onStatus)
}

var runtimeTop = regexp.MustCompile(`^(proton-xodus-|xodus-nexus-runtime-|nexus-runtime-)`)

// activateArchive verifies the archive structure, extracts, validates Wine and
// atomically switches the active runtime pointer. The release directory uses
// the canonical name; the archive's top-level folder is only provenance.
func (m *Manager) activateArchive(downloads, archiveName, commit, releaseName string, onStatus func()) (string, error) {
	root := m.root()
	staging := filepath.Join(root, ".staging-"+install.NewUUID())
	defer os.RemoveAll(staging)
	extracted := filepath.Join(staging, "extracted")
	archivePath := filepath.Join(downloads, archiveName)
	// Only tar's stdout contains paths. Loader/overlay warnings on stderr must
	// not become archive members, even when tar itself exits successfully.
	listing, err := proc.Run("tar", []string{"-tJf", archivePath}, proc.RunOptions{MaxOutput: 20_000_000, StdoutOnly: true})
	if err != nil || listing.Code != 0 {
		return "", errors.New("The runtime is not a valid tar.xz archive.")
	}
	var entries []string
	for _, entry := range strings.Split(listing.Output, "\n") {
		if entry != "" {
			entries = append(entries, entry)
		}
	}
	if len(entries) == 0 || slices.ContainsFunc(entries, func(entry string) bool {
		return filepath.IsAbs(entry) || slices.Contains(strings.Split(entry, "/"), "..")
	}) {
		return "", errors.New("The archive contains unsafe paths.")
	}
	top := strings.Split(entries[0], "/")[0]
	if !runtimeTop.MatchString(top) || slices.ContainsFunc(entries, func(entry string) bool { return strings.Split(entry, "/")[0] != top }) {
		return "", errors.New("Unexpected Xodus Proton runtime structure.")
	}
	m.setPhase("Extracting approximately 1.4 GB of runtime files…", onStatus)
	if err := os.MkdirAll(extracted, 0o755); err != nil {
		return "", err
	}
	result, err := proc.Run("tar", []string{"-xJf", archivePath, "-C", extracted}, proc.RunOptions{OnLine: func(line string) { m.emitLog("runtime", line) }, MaxOutput: 1_000_000})
	if err != nil || result.Code != 0 {
		return "", errors.New("Runtime extraction failed.")
	}
	candidate := filepath.Join(extracted, top)
	if !validateWine(filepath.Join(candidate, "files", "bin", "wine")) {
		return "", errors.New("The downloaded Wine failed --version validation.")
	}
	if m.isOperationActive() {
		return "", errors.New("Another operation started during installation; the runtime was not activated.")
	}
	releases := filepath.Join(root, "releases")
	releaseRoot := filepath.Join(releases, releaseName)
	if err := os.MkdirAll(releases, 0o755); err != nil {
		return "", err
	}
	if _, err := os.Stat(releaseRoot); err != nil {
		if err := os.Rename(candidate, releaseRoot); err != nil {
			return "", err
		}
	}
	winePath := filepath.Join(releaseRoot, "files", "bin", "wine")
	if !validateWine(winePath) {
		return "", errors.New("The activated runtime is not executable.")
	}
	if err := m.writePointer(pointer{Name: releaseName, WinePath: winePath, Commit: commit, InstalledAt: shared.JSISOString(time.Now())}); err != nil {
		return "", err
	}
	m.update(func(s *shared.RuntimeStatus) {
		s.Installed, s.Name, s.WinePath, s.Commit, s.Phase, s.Error = true, releaseName, winePath, commit, "Runtime is ready.", ""
	})
	m.emitLog("runtime", "Active Wine: "+winePath)
	return winePath, nil
}

func (m *Manager) writePointer(value pointer) error {
	if err := os.MkdirAll(m.root(), 0o755); err != nil {
		return err
	}
	data, _ := json.MarshalIndent(value, "", "  ")
	temporary := m.pointerFile() + ".tmp-" + install.NewUUID()
	defer os.Remove(temporary)
	if err := os.WriteFile(temporary, data, 0o600); err != nil {
		return err
	}
	return os.Rename(temporary, m.pointerFile())
}

type release struct {
	TagName     any  `json:"tag_name"`
	PublishedAt any  `json:"published_at"`
	Draft       bool `json:"draft"`
	Prerelease  bool `json:"prerelease"`
	Assets      []struct {
		Name any `json:"name"`
		URL  any `json:"browser_download_url"`
	} `json:"assets"`
}

var (
	nexusRuntimeAsset = regexp.MustCompile(`^nexus-runtime-[A-Za-z0-9._-]+-x86_64\.tar\.xz$`)
	legacyNexusAsset  = regexp.MustCompile(`^xodus-nexus-runtime-[A-Za-z0-9._-]+-x86_64\.tar\.xz$`)
)

// CheckNexus finds the newest published Nexus runtime. Only published releases
// are installable here, never unapproved CI artifacts. The complete fork has
// its own release repository; the existing release source stays a fallback.
func (m *Manager) CheckNexus(ctx context.Context) shared.RuntimeStatus {
	m.update(func(s *shared.RuntimeStatus) { s.NexusChecking, s.NexusError = true, "" })
	artifact, err := m.findNexus(ctx)
	m.update(func(s *shared.RuntimeStatus) {
		s.NexusChecking = false
		m.nexus = artifact
		if err != nil {
			s.NexusAvailable, s.NexusName, s.NexusVersion, s.NexusError = false, "", "", err.Error()
			return
		}
		s.NexusAvailable, s.NexusName, s.NexusVersion, s.NexusError = true, artifact.name, artifact.version, ""
	})
	return m.Current()
}

func (m *Manager) findNexus(ctx context.Context) (*nexusArtifact, error) {
	for _, repo := range []string{nexusRuntimeRepository, nexusRepository} {
		requestCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
		response, err := m.fetch(requestCtx, "https://api.github.com/repos/"+repo+"/releases?per_page=30", http.Header{"Accept": {"application/vnd.github+json"}})
		if err != nil {
			cancel()
			return nil, err
		}
		body, readErr := io.ReadAll(io.LimitReader(response.Body, 8_000_000))
		response.Body.Close()
		cancel()
		if response.StatusCode == 404 && repo == nexusRuntimeRepository {
			continue
		}
		if response.StatusCode < 200 || response.StatusCode > 299 || readErr != nil {
			return nil, errors.New("The Nexus runtime release list could not be read.")
		}
		var releases []release
		if err := json.Unmarshal(body, &releases); err != nil {
			return nil, err
		}
		pattern := legacyNexusAsset
		if repo == nexusRuntimeRepository {
			pattern = nexusRuntimeAsset
		}
		var found *nexusArtifact
		newest := math.Inf(-1)
		for _, item := range releases {
			if item.Draft || item.Prerelease {
				continue
			}
			tag, _ := item.TagName.(string)
			version := strings.TrimSpace(tag)
			type asset struct{ name, url string }
			var assets []asset
			for _, entry := range item.Assets {
				name, _ := entry.Name.(string)
				url, _ := entry.URL.(string)
				assets = append(assets, asset{name, url})
			}
			checksumFor := func(name string) (asset, bool) {
				index := slices.IndexFunc(assets, func(a asset) bool {
					return a.name == strings.TrimSuffix(name, ".tar.xz")+".sha512sum" && strings.HasPrefix(a.url, "https://")
				})
				if index < 0 {
					return asset{}, false
				}
				return assets[index], true
			}
			index := slices.IndexFunc(assets, func(a asset) bool {
				_, ok := checksumFor(a.name)
				return pattern.MatchString(a.name) && strings.HasPrefix(a.url, "https://") && ok
			})
			if index < 0 {
				continue
			}
			archive := assets[index]
			checksum, _ := checksumFor(archive.name)
			// GitHub's list order is not necessarily publication order, and the
			// commit suffix in runtime tags is not a sortable version number.
			publication := math.Inf(-1)
			if published, ok := item.PublishedAt.(string); ok {
				if parsed, ok := shared.ParseJSDate(published); ok {
					publication = float64(parsed.UnixMilli())
				}
			}
			// Keep an undated fallback only when no dated candidate is available.
			if found == nil || publication > newest {
				found = &nexusArtifact{version: version, name: archive.name, url: archive.url, checksumName: checksum.name, checksumURL: checksum.url}
				newest = publication
			}
		}
		if found == nil && repo == nexusRuntimeRepository {
			continue
		}
		if found == nil {
			return nil, errors.New("No Nexus runtime release has been published yet.")
		}
		return found, nil
	}
	return nil, errors.New("No Nexus runtime release has been published yet.")
}

func (m *Manager) downloadFile(url, target string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	response, err := m.fetch(ctx, url, nil)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode > 299 {
		return fmt.Errorf("Download failed with HTTP %d.", response.StatusCode)
	}
	file, err := os.Create(target)
	if err != nil {
		return err
	}
	_, err = io.Copy(file, response.Body)
	if closeErr := file.Close(); err == nil {
		err = closeErr
	}
	return err
}

var checksumLine = regexp.MustCompile(`(?i)^([a-f0-9]{128})[ \t]+\*?([^\r\n]+)$`)

// InstallNexus downloads, verifies and activates the published Nexus runtime.
func (m *Manager) InstallNexus(ctx context.Context, onStatus func()) (winePath string, err error) {
	if m.isOperationActive() {
		return "", errors.New("The runtime cannot be changed while a Xodus operation is active.")
	}
	m.mu.Lock()
	artifact := m.nexus
	m.mu.Unlock()
	if artifact == nil {
		m.CheckNexus(ctx)
		m.mu.Lock()
		artifact = m.nexus
		m.mu.Unlock()
	}
	if artifact == nil {
		message := m.Current().NexusError
		if message == "" {
			message = "No Nexus runtime is available."
		}
		return "", errors.New(message)
	}
	if prerequisites := m.Current().Prerequisites; !prerequisites.Tar || !prerequisites.SHA512Sum {
		return "", errors.New("tar or sha512sum is missing.")
	}
	m.begin(onStatus)
	staging := filepath.Join(m.root(), ".staging-"+install.NewUUID())
	defer func() { m.finish(staging, err, onStatus) }()
	downloads := filepath.Join(staging, "downloads")
	if err := os.MkdirAll(downloads, 0o755); err != nil {
		return "", err
	}
	m.setPhase("Downloading the Nexus runtime "+artifact.version+"…", onStatus)
	if err := m.downloadFile(artifact.url, filepath.Join(downloads, artifact.name)); err != nil {
		return "", err
	}
	m.setPhase("Verifying the SHA-512 checksum…", onStatus)
	if err := m.downloadFile(artifact.checksumURL, filepath.Join(downloads, artifact.checksumName)); err != nil {
		return "", err
	}
	data, err := os.ReadFile(filepath.Join(downloads, artifact.checksumName))
	if err != nil {
		return "", err
	}
	match := checksumLine.FindStringSubmatch(strings.TrimSpace(string(data)))
	if match == nil || match[2] != artifact.name {
		return "", errors.New("The SHA-512 file does not identify this Nexus runtime archive.")
	}
	result, err := proc.Run("sha512sum", []string{"-c", artifact.checksumName}, proc.RunOptions{Dir: downloads, MaxOutput: 100_000})
	if err != nil || result.Code != 0 {
		return "", errors.New("Nexus runtime SHA-512 verification failed.")
	}
	return m.activateArchive(downloads, artifact.name, artifact.version, NexusReleaseName(artifact.name), onStatus)
}

// RollbackCloudSavePreview restores the release a cloud-save preview was built on.
func (m *Manager) RollbackCloudSavePreview(onStatus func()) (string, error) {
	if m.isOperationActive() {
		return "", errors.New("The runtime cannot be changed while a Xodus operation is active.")
	}
	current := m.readPointer()
	if current == nil || PreviewBaseRelease(current.Name) == "" {
		return "", errors.New("No cloud-save preview is active.")
	}
	base := PreviewBaseRelease(current.Name)
	winePath := filepath.Join(m.root(), "releases", base, "files", "bin", "wine")
	if !validateWine(winePath) {
		return "", errors.New("The previous runtime is missing or failed validation. The preview remains active.")
	}
	m.update(func(s *shared.RuntimeStatus) { s.Updating = true })
	onStatus()
	defer func() {
		m.update(func(s *shared.RuntimeStatus) { s.Updating = false })
		onStatus()
	}()
	restored := *current
	restored.Name, restored.WinePath, restored.InstalledAt = base, winePath, shared.JSISOString(time.Now())
	if err := m.writePointer(restored); err != nil {
		return "", err
	}
	m.update(func(s *shared.RuntimeStatus) {
		s.Name, s.WinePath, s.Phase, s.Error = base, winePath, "Previous runtime restored.", ""
	})
	m.emitLog("runtime", "Restored Wine: "+winePath)
	return winePath, nil
}
